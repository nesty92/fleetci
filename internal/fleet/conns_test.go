package fleet

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// reachable is a plugin whose instances all answer at one SSH server, with
// a fresh key on every ConnectInfo.
type reachable struct {
	cloud
	addr  string
	calls atomic.Int32
}

func newReachable(t *testing.T, addr string, ids ...string) *reachable {
	return &reachable{cloud: *newCloud(ids...), addr: addr}
}

func (r *reachable) ConnectInfo(_ context.Context, id string) (provider.ConnectInfo, error) {
	r.calls.Add(1)
	_, key, _ := ed25519.GenerateKey(nil)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return provider.ConnectInfo{}, err
	}
	return provider.ConnectInfo{ID: id, InternalAddr: r.addr, ConnectorConfig: provider.ConnectorConfig{
		Username: "u", Key: pem.EncodeToMemory(block), Timeout: 5 * time.Second,
	}}, nil
}

func (v *vmSSH) accepted() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.conns)
}

func gone(t *testing.T, lost <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

func TestEachInstanceHasOneConnection(t *testing.T) {
	_, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	group := newReachable(t, vm.addr, "i-1", "i-2")
	conns := newVMConns(group, false, hclog.NewNullLogger())
	defer conns.close()

	one := conns.get("i-1")
	if one == nil || conns.get("i-1") != one {
		t.Fatal("two gets of one instance gave different connections")
	}
	if conns.get("i-2") == one {
		t.Fatal("two instances share a connection")
	}
	for range 3 {
		if _, _, err := one.Client(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := vm.accepted(); n != 1 {
		t.Fatalf("%d connections for one instance, want 1", n)
	}
}

func TestAGoneInstanceDropsItsConnection(t *testing.T) {
	_, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	conns := newVMConns(newReachable(t, vm.addr, "i-1"), false, hclog.NewNullLogger())
	defer conns.close()

	first := conns.get("i-1")
	_, lost, err := first.Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conns.drop("i-1")
	gone(t, lost, "drop left the connection open")
	if conns.get("i-1") == first {
		t.Fatal("a dropped connection was handed out again")
	}
}

func TestClosedConnsRefuseEveryone(t *testing.T) {
	_, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	conns := newVMConns(newReachable(t, vm.addr, "i-1", "i-2"), false, hclog.NewNullLogger())

	_, lost, err := conns.get("i-1").Client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conns.close()
	gone(t, lost, "close left a connection open")
	if _, _, err := conns.get("i-2").Client(context.Background()); err == nil {
		t.Fatal("a connection made after close dialed")
	}
	if n := vm.accepted(); n != 1 {
		t.Fatalf("%d connections, want none after close", n)
	}
}

func TestAJobsVMCarriesItsInstancesConnection(t *testing.T) {
	_, addr := newFakeIMDS(t)
	vm := newVMSSH(t, addr, 0)
	group := newReachable(t, vm.addr, "i-1")
	conns := newVMConns(group, false, hclog.NewNullLogger())
	defer conns.close()
	c := Class{MaxInstances: 1, MaxUseCount: 100}
	ts := scaler(t, vmGroup{group, conns, nil}, c)

	b := &Backend{log: hclog.NewNullLogger(), vms: map[string]*pool{}}
	p := &pool{name: "c", class: c, conns: conns, ts: ts}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	job, err := b.acquire(ctx, p, "c", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Release(job.ID)
	if job.Conn == nil || job.Conn != conns.get(job.Instance) {
		t.Fatal("the job's vm does not carry its instance's connection")
	}
}

// noConns is a pool's connections for tests that never connect.
func noConns() *vmConns { return newVMConns(newCloud(), false, hclog.NewNullLogger()) }
