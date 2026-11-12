package cluster

import (
	"net"
	"strings"
	"testing"
)

func ipnet(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

// TestResolvePeerNet: by default the default route's network, never a
// VPN's; peer_network names another by CIDR or interface.
func TestResolvePeerNet(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	eth := netIface{name: "eth0", hw: mac, nets: []*net.IPNet{ipnet(t, "192.168.1.5/24")}}
	san := netIface{name: "eth1", hw: mac, nets: []*net.IPNet{ipnet(t, "10.20.0.5/16")}}
	wg := netIface{name: "wg0", nets: []*net.IPNet{ipnet(t, "10.8.0.2/24")}}
	tap := netIface{name: "tap0", hw: mac, nets: []*net.IPNet{ipnet(t, "10.9.0.2/24")}}
	ppp := netIface{name: "uplink", hw: mac, flags: net.FlagPointToPoint, nets: []*net.IPNet{ipnet(t, "10.7.0.2/24")}}
	ifs := []netIface{eth, san, wg, tap, ppp}
	ip := net.ParseIP

	n, err := resolvePeerNet("", ifs, []net.IP{ip("192.168.1.5")})
	if err != nil || n.name != "eth0" || !n.contains(ip("192.168.1.77")) || n.contains(ip("10.20.0.6")) || n.contains(ip("10.8.0.3")) {
		t.Fatalf("default: %v, %v", n, err)
	}
	if got := n.within([]string{"10.8.0.3:9483", "192.168.1.6:9483", "w2.example:9483", "[2001:db8::6]:9483"}); len(got) != 1 || got[0] != "192.168.1.6:9483" {
		t.Fatalf("within: %v", got)
	}
	for _, via := range []string{"10.8.0.2", "10.9.0.2", "10.7.0.2"} {
		if _, err := resolvePeerNet("", ifs, []net.IP{ip(via)}); err == nil || !strings.Contains(err.Error(), "VPN") {
			t.Fatalf("default route by %s: %v", via, err)
		}
	}
	if _, err := resolvePeerNet("", ifs, nil); err == nil {
		t.Fatal("no default route, yet a network")
	}

	// Named: no VPN check, the user chose it.
	if n, err := resolvePeerNet("wg0", ifs, nil); err != nil || !n.contains(ip("10.8.0.9")) {
		t.Fatalf("by interface: %v, %v", n, err)
	}
	n, err = resolvePeerNet("10.20.0.0/16", ifs, []net.IP{ip("192.168.1.5")})
	if err != nil || len(n.addrs) != 1 || !n.addrs[0].Equal(ip("10.20.0.5")) || n.contains(ip("192.168.1.6")) {
		t.Fatalf("by CIDR: %v, %v", n, err)
	}
	for _, bad := range []string{"172.16.0.0/12", "eth9"} {
		if _, err := resolvePeerNet(bad, ifs, nil); err == nil {
			t.Fatalf("%s: no error", bad)
		}
	}
	var none *peerNet
	if none.contains(ip("192.168.1.6")) || len(none.within([]string{"192.168.1.6:9483"})) != 0 {
		t.Fatal("no network contains anything")
	}
}

// TestNetListener: connections from outside the network are dropped.
func TestNetListener(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accept := func(pn *peerNet) bool {
		l := netListener{lis, pn}
		got := make(chan bool, 1)
		go func() {
			c, err := l.Accept()
			if err == nil {
				c.Close()
			}
			got <- err == nil
		}()
		c, err := net.Dial("tcp", lis.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		buf := make([]byte, 1)
		if _, err := c.Read(buf); err == nil {
			t.Fatal("read data")
		}
		if pn.contains(net.ParseIP("127.0.0.1")) {
			return <-got
		}
		return false
	}
	if !accept(&peerNet{nets: []*net.IPNet{ipnet(t, "127.0.0.0/8")}}) {
		t.Fatal("refused a connection from inside")
	}
	if accept(&peerNet{nets: []*net.IPNet{ipnet(t, "192.168.1.0/24")}}) {
		t.Fatal("took a connection from outside")
	}
}
