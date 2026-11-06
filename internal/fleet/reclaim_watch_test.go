package fleet

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/sshx"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"gitlab.com/gitlab-org/fleeting/taskscaler"
)

// fakeIMDS answers like EC2's IMDSv2: a token first, then metadata for
// requests that carry it. A notice is served once set.
type fakeIMDS struct {
	mu       sync.Mutex
	token    string
	tokens   int
	requests int
	notices  map[string]string // meta-data path → body
	hang     bool              // accept, never answer
	failing  bool              // answer 500
}

func (f *fakeIMDS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeIMDS) setFailing(v bool) {
	f.mu.Lock()
	f.failing = v
	f.mu.Unlock()
}

func newFakeIMDS(t *testing.T) (*fakeIMDS, string) {
	f := &fakeIMDS{notices: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.Listener.Addr().String()
}

func (f *fakeIMDS) set(path, body string) {
	f.mu.Lock()
	f.notices[path] = body
	f.mu.Unlock()
}

// expire drops the current token, as IMDS does at its TTL.
func (f *fakeIMDS) expire() {
	f.mu.Lock()
	f.token = ""
	f.mu.Unlock()
}

func (f *fakeIMDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	if f.hang {
		f.mu.Unlock()
		<-r.Context().Done()
		return
	}
	defer f.mu.Unlock()
	if f.failing {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/latest/api/token" {
		if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		f.tokens++
		f.token = "tok" + strconv.Itoa(f.tokens)
		_, _ = w.Write([]byte(f.token))
		return
	}
	if f.token == "" || r.Header.Get("X-aws-ec2-metadata-token") != f.token {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	body, ok := f.notices[strings.TrimPrefix(r.URL.Path, "/latest/meta-data/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(body + "\n"))
}

// vmSSH is a VM's SSH server: it forwards TCP to the metadata address
// (served by imds), or refuses as a VM's sshd can.
type vmSSH struct {
	addr   string
	imds   string
	refuse ssh.RejectionReason // 0: forward
	opened atomic.Int32
	mu     sync.Mutex
	conns  []net.Conn
}

func newVMSSH(t *testing.T, imds string, refuse ssh.RejectionReason) *vmSSH {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(nil)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v := &vmSSH{addr: ln.Addr().String(), imds: imds, refuse: refuse}
	t.Cleanup(func() { ln.Close(); v.drop() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			v.mu.Lock()
			v.conns = append(v.conns, c)
			v.mu.Unlock()
			go v.serve(c, cfg)
		}
	}()
	return v
}

func (v *vmSSH) serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(true, nil)
			}
		}
	}()
	for nc := range chans {
		var to struct {
			Host     string
			Port     uint32
			OrigHost string
			OrigPort uint32
		}
		if nc.ChannelType() != "direct-tcpip" || ssh.Unmarshal(nc.ExtraData(), &to) != nil {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		if v.refuse != 0 {
			_ = nc.Reject(v.refuse, "refused")
			continue
		}
		if net.JoinHostPort(to.Host, strconv.Itoa(int(to.Port))) != imdsAddr {
			_ = nc.Reject(ssh.ConnectionFailed, "only the metadata service here")
			continue
		}
		up, err := net.Dial("tcp", v.imds)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			up.Close()
			continue
		}
		go ssh.DiscardRequests(creqs)
		go sshx.Pipe(ch, up)
	}
}

// drop cuts every connection, as a VM that went away does.
func (v *vmSSH) drop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.conns {
		c.Close()
	}
	v.conns = nil
}

// open is an opener that reaches the VM as sshTunnel does, minus the
// plugin's ConnectInfo.
func (v *vmSSH) open(_ context.Context, _ string) (tunnel, error) {
	v.opened.Add(1)
	client, err := ssh.Dial("tcp", v.addr, &ssh.ClientConfig{
		User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		return tunnel{}, err
	}
	return tunnel{
		dial:  client.DialContext,
		lost:  proxy.KeepAlive(client, time.Second, time.Second),
		close: func() { _ = client.Close() },
	}, nil
}

func fastWatch(open opener, rebalance bool) *reclaimWatch {
	w := newReclaimWatch(open, rebalance, hclog.NewNullLogger())
	w.every, w.retry = 20*time.Millisecond, 10*time.Millisecond
	return w
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestANoticeReadThroughTheTunnelFailsTheHeartbeat(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	w := fastWatch(vm.open, false)
	defer w.close()
	g := reclaimGroup{newCloud("i-1"), w}

	w.start("i-1")
	eventually(t, "never read the metadata", func() bool {
		imds.mu.Lock()
		defer imds.mu.Unlock()
		return imds.tokens > 0
	})
	if err := g.Heartbeat(context.Background(), "i-1"); err != nil {
		t.Fatalf("no notice yet: %v", err)
	}

	imds.set("spot/instance-action", `{"action": "terminate", "time": "2026-11-06T18:00:00Z"}`)
	eventually(t, "notice never seen", func() bool { return w.notice("i-1") != "" })
	err := g.Heartbeat(context.Background(), "i-1")
	if !errors.Is(err, errReclaimNotice) || !strings.Contains(err.Error(), "terminate") {
		t.Fatalf("heartbeat %v, want the reclaim notice", err)
	}
	if err := g.Heartbeat(context.Background(), "i-2"); err != nil {
		t.Fatalf("another vm: %v", err)
	}
	if n := vm.opened.Load(); n != 1 {
		t.Fatalf("opened %d connections, want one for the whole watch", n)
	}

	w.stop("i-1")
	if err := g.Heartbeat(context.Background(), "i-1"); err != nil {
		t.Fatalf("after stop: %v", err)
	}
}

func TestAnExpiredTokenIsRenewed(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	w := fastWatch(newVMSSH(t, addr, 0).open, false)
	defer w.close()

	w.start("i-1")
	eventually(t, "never read the metadata", func() bool {
		imds.mu.Lock()
		defer imds.mu.Unlock()
		return imds.tokens > 0
	})
	imds.expire()
	imds.set("spot/instance-action", `{"action": "stop"}`)
	eventually(t, "notice never seen after the token expired", func() bool { return w.notice("i-1") != "" })
}

func TestARebalanceRecommendationCountsOnlyWhenTheClassAsks(t *testing.T) {
	for _, asks := range []bool{false, true} {
		imds, addr := newFakeIMDS(t)
		imds.set("events/recommendations/rebalance", `{"noticeTime": "2026-11-06T18:00:00Z"}`)
		w := fastWatch(newVMSSH(t, addr, 0).open, asks)
		w.start("i-1")
		if asks {
			eventually(t, "recommendation never seen", func() bool { return w.notice("i-1") != "" })
		} else {
			eventually(t, "recommendation never read", func() bool {
				w.mu.Lock()
				defer w.mu.Unlock()
				_, noted := w.notices["i-1"]
				return noted
			})
			if n := w.notice("i-1"); n != "" {
				t.Fatalf("rebalance off: notice %q", n)
			}
		}
		w.close()
	}
}

func TestAWatchEndsForGoodWhenTheVMCantReachTheMetadata(t *testing.T) {
	for _, refuse := range []ssh.RejectionReason{ssh.Prohibited, ssh.ConnectionFailed} {
		_, addr := newFakeIMDS(t)
		vm := newVMSSH(t, addr, refuse)
		w := fastWatch(vm.open, false)
		w.start("i-1")
		time.Sleep(200 * time.Millisecond)
		w.close()
		if n := vm.opened.Load(); n != 1 {
			t.Fatalf("refused with %v: opened %d times, want once", refuse, n)
		}
	}
}

func TestALostConnectionIsOpenedAgain(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	w := fastWatch(vm.open, false)
	defer w.close()

	w.start("i-1")
	eventually(t, "never connected", func() bool { return vm.opened.Load() == 1 })
	vm.drop()
	eventually(t, "never connected again", func() bool { return vm.opened.Load() >= 2 })
	imds.set("spot/instance-action", `{"action": "hibernate"}`)
	eventually(t, "notice never seen on the new connection", func() bool { return w.notice("i-1") != "" })
}

func TestTheWatchEndsWithTheVM(t *testing.T) {
	_, addr := newFakeIMDS(t)
	w := fastWatch(newVMSSH(t, addr, 0).open, false)
	defer w.close()
	g := reclaimGroup{newCloud("i-1", "i-2"), w}

	w.start("i-1")
	w.start("i-2")
	if _, err := g.Decrease(context.Background(), []string{"i-1"}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	_, one := w.stops["i-1"]
	_, two := w.stops["i-2"]
	w.mu.Unlock()
	if one || !two {
		t.Fatalf("watched after decrease: i-1 %v, i-2 %v; want only i-2", one, two)
	}
}

func TestReclaimIsWatchedForTheAWSPluginByDefault(t *testing.T) {
	off := false
	for _, c := range []struct {
		class Class
		want  bool
	}{
		{Class{Plugin: "fleeting-plugin-aws"}, true},
		{Class{Plugin: "/usr/local/bin/fleeting-plugin-aws"}, true},
		{Class{Plugin: "fleeting-plugin-dockervm"}, false},
		{Class{Plugin: "fleeting-plugin-aws", Reclaim: Reclaim{Watch: &off}}, false},
	} {
		if got := c.class.watchesReclaim(); got != c.want {
			t.Errorf("%s watch=%v: %v, want %v", c.class.Plugin, c.class.Reclaim.Watch, got, c.want)
		}
	}
	bad := Class{Plugin: "fleeting-plugin-dockervm", MaxInstances: 1, Reclaim: Reclaim{Rebalance: true}}
	if err := bad.validate("x"); err == nil || !strings.Contains(err.Error(), "reclaim.rebalance") {
		t.Fatalf("rebalance without watch: %v", err)
	}
}

func TestAVMWhoseMetadataNeverAnswersIsLeftAlone(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	imds.hang = true
	vm := newVMSSH(t, addr, 0)
	w := fastWatch(vm.open, false)
	w.timeout = 20 * time.Millisecond
	defer w.close()

	w.start("i-1")
	eventually(t, "never asked", func() bool { return imds.count() >= 3 })
	time.Sleep(200 * time.Millisecond)
	before := imds.count()
	time.Sleep(200 * time.Millisecond)
	if after := imds.count(); after != before {
		t.Fatalf("still asking (%d, then %d requests); want the watch to end", before, after)
	}
	if n := vm.opened.Load(); n != 1 {
		t.Fatalf("opened %d times, want once", n)
	}
}

func TestAMetadataHiccupDoesNotEndTheWatch(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	w := fastWatch(vm.open, false)
	defer w.close()

	w.start("i-1")
	eventually(t, "never answered", func() bool { return imds.count() >= 2 })
	imds.setFailing(true)
	failingFrom := imds.count()
	eventually(t, "stopped asking while it failed", func() bool { return imds.count() >= failingFrom+5 })
	imds.setFailing(false)
	imds.set("spot/instance-action", `{"action": "terminate"}`)
	eventually(t, "notice never seen after the hiccup", func() bool { return w.notice("i-1") != "" })
	if n := vm.opened.Load(); n != 1 {
		t.Fatalf("opened %d times, want the same connection throughout", n)
	}
}

// reporting is a plugin that reports one VM in one state.
type reporting struct {
	cloud
	id    string
	state provider.State
}

func (r *reporting) Update(_ context.Context, fn func(string, provider.State)) error {
	fn(r.id, r.state)
	return nil
}

func TestTheWatchEndsWhenThePluginReportsTheVMDeleted(t *testing.T) {
	_, addr := newFakeIMDS(t)
	w := fastWatch(newVMSSH(t, addr, 0).open, false)
	defer w.close()

	for _, c := range []struct {
		state   provider.State
		watched bool
	}{{provider.StateRunning, true}, {provider.StateDeleting, true}, {provider.StateDeleted, false}} {
		w.start("i-1")
		g := reclaimGroup{&reporting{id: "i-1", state: c.state}, w}
		var got provider.State
		if err := g.Update(context.Background(), func(_ string, s provider.State) { got = s }); err != nil {
			t.Fatal(err)
		}
		if got != c.state {
			t.Fatalf("taskscaler saw %v, want %v passed on", got, c.state)
		}
		w.mu.Lock()
		_, watched := w.stops["i-1"]
		w.mu.Unlock()
		if watched != c.watched {
			t.Fatalf("%v: watched %v, want %v", c.state, watched, c.watched)
		}
	}
}

func TestAnAdmittedVMIsWatched(t *testing.T) {
	_, addr := newFakeIMDS(t)
	w := fastWatch(newVMSSH(t, addr, 0).open, false)
	defer w.close()
	watched := func(id string) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		_, ok := w.stops[id]
		return ok
	}

	ok := watchOnceUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil }, w)
	if err := ok(nil, taskscaler.UpFuncInstance{ID: "i-up"}); err != nil || !watched("i-up") {
		t.Fatalf("admitted vm: err %v, watched %v", err, watched("i-up"))
	}

	refused := errors.New("docker never answered")
	bad := watchOnceUp(func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return refused }, w)
	if err := bad(nil, taskscaler.UpFuncInstance{ID: "i-down"}); !errors.Is(err, refused) || watched("i-down") {
		t.Fatalf("refused vm: err %v, watched %v", err, watched("i-down"))
	}
}

func TestAWatchedClassCordonsAVMWithANotice(t *testing.T) {
	imds, addr := newFakeIMDS(t)
	imds.set("spot/instance-action", `{"action": "terminate"}`)
	vm := newVMSSH(t, addr, 0)
	up := func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { return nil }
	on := true

	group, admit, w := withReclaim(Class{Plugin: "fleeting-plugin-x", Reclaim: Reclaim{Watch: &on}},
		newCloud("i-1"), up, vm.open, hclog.NewNullLogger())
	if w == nil {
		t.Fatal("no watch for a class that asks for one")
	}
	defer w.close()
	if err := admit(nil, taskscaler.UpFuncInstance{ID: "i-1"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the group taskscaler sees never failed the heartbeat", func() bool {
		return errors.Is(group.Heartbeat(context.Background(), "i-1"), errReclaimNotice)
	})
}

func TestAnUnwatchedClassGetsItsGroupAsItIs(t *testing.T) {
	plain := newCloud("i-1")
	called := false
	up := func(taskscaler.Taskscaler, taskscaler.UpFuncInstance) error { called = true; return nil }

	group, admit, w := withReclaim(Class{Plugin: "fleeting-plugin-dockervm"}, plain, up,
		func(context.Context, string) (tunnel, error) { t.Fatal("opened a tunnel"); return tunnel{}, nil },
		hclog.NewNullLogger())
	if w != nil || group != provider.InstanceGroup(plain) {
		t.Fatalf("watch %v, group %T: want neither changed", w, group)
	}
	if err := admit(nil, taskscaler.UpFuncInstance{ID: "i-1"}); err != nil || !called {
		t.Fatalf("up: err %v, called %v", err, called)
	}
}
