package cluster

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// Workers reach each other, and copy caches, over one network: peer_network
// in worker.toml, a CIDR or an interface, or by default the network of the
// interface the default route leaves by. A worker answers its peers only at
// its addresses in it, dials only theirs in it, and takes connections only
// from it, so a copy never goes through a router, a VPN or a container's
// bridge. A default route through what looks like a VPN gives no network:
// the worker then neither checks its peers nor copies, until peer_network
// names one.

// peerNet is the network workers reach each other over.
type peerNet struct {
	name  string // the interface, or the CIDR it was given
	nets  []*net.IPNet
	addrs []net.IP // this worker's own in it
}

func (n *peerNet) contains(ip net.IP) bool {
	return n != nil && ip != nil && slices.ContainsFunc(n.nets, func(c *net.IPNet) bool { return c.Contains(ip) })
}

// within keeps the addresses (host:port) in the network.
func (n *peerNet) within(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		if err == nil && n.contains(net.ParseIP(host)) {
			out = append(out, a)
		}
	}
	return out
}

func (n *peerNet) String() string {
	if len(n.nets) == 1 && n.nets[0].String() == n.name {
		return n.name
	}
	var cs []string
	for _, c := range n.nets {
		cs = append(cs, c.String())
	}
	return n.name + " " + strings.Join(cs, ",")
}

// netIface is what peerNet needs of an interface.
type netIface struct {
	name  string
	flags net.Flags
	hw    net.HardwareAddr
	nets  []*net.IPNet
}

func localIfaces() ([]netIface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netIface
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		ni := netIface{name: i.Name, flags: i.Flags, hw: i.HardwareAddr}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				ni.nets = append(ni.nets, n)
			}
		}
		out = append(out, ni)
	}
	return out, nil
}

// routeIPs are the source addresses of the default routes: connecting a
// UDP socket looks the route up, and sends nothing.
func routeIPs() []net.IP {
	var out []net.IP
	for _, dst := range []string{"192.0.2.1:9", "[2001:db8::1]:9"} { // documentation addresses
		c, err := net.Dial("udp", dst)
		if err != nil {
			continue
		}
		out = append(out, c.LocalAddr().(*net.UDPAddr).IP)
		c.Close()
	}
	return out
}

// vpnPrefixes start the names of tunnels' interfaces.
var vpnPrefixes = []string{"tun", "tap", "wg", "tailscale", "zt", "utun", "ppp", "ipsec", "vti", "nordlynx", "gpd"}

// vpnLike says whether an interface looks like a tunnel: point to point,
// without a hardware address, or named as tunnels are.
func vpnLike(i netIface) bool {
	if i.flags&net.FlagPointToPoint != 0 || len(i.hw) == 0 {
		return true
	}
	return slices.ContainsFunc(vpnPrefixes, func(p string) bool { return strings.HasPrefix(i.name, p) })
}

// resolvePeerNet finds the network spec names: a CIDR, an interface, or
// for "" the default route's.
func resolvePeerNet(spec string, ifs []netIface, route []net.IP) (*peerNet, error) {
	if _, cidr, err := net.ParseCIDR(spec); err == nil {
		n := &peerNet{name: spec, nets: []*net.IPNet{cidr}}
		for _, i := range ifs {
			for _, a := range i.nets {
				if cidr.Contains(a.IP) {
					n.addrs = append(n.addrs, a.IP)
				}
			}
		}
		if len(n.addrs) == 0 {
			return nil, fmt.Errorf("peer_network %s: this worker has no address in it", spec)
		}
		return n, nil
	}
	var pick *netIface
	if spec != "" {
		for k := range ifs {
			if ifs[k].name == spec {
				pick = &ifs[k]
			}
		}
		if pick == nil {
			return nil, fmt.Errorf("peer_network %q: neither a CIDR nor an interface that is up", spec)
		}
	} else {
		for k := range ifs {
			if slices.ContainsFunc(ifs[k].nets, func(a *net.IPNet) bool { return slices.ContainsFunc(route, a.IP.Equal) }) {
				pick = &ifs[k]
				break
			}
		}
		if pick == nil {
			return nil, errors.New("no default route: set peer_network")
		}
		if vpnLike(*pick) {
			return nil, fmt.Errorf("the default route leaves by %s, which looks like a VPN: set peer_network", pick.name)
		}
	}
	if len(pick.nets) == 0 {
		return nil, fmt.Errorf("%s has no address", pick.name)
	}
	n := &peerNet{name: pick.name, nets: pick.nets}
	for _, a := range pick.nets {
		n.addrs = append(n.addrs, a.IP)
	}
	return n, nil
}

// netListener drops connections from outside the network.
type netListener struct {
	net.Listener
	pn *peerNet
}

func (l netListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && l.pn.contains(a.IP) {
			return c, nil
		}
		c.Close()
	}
}
