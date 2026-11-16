package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

// TestTailcat runs a command on a fake VM through a real tailcat address,
// as fleetci ssh would: it needs the network (tailcat.dev's relays), so
// it runs only with FLEETCI_TAILCAT_TEST=1.
func TestTailcat(t *testing.T) {
	if os.Getenv("FLEETCI_TAILCAT_TEST") != "1" {
		t.Skip("set FLEETCI_TAILCAT_TEST=1: it uses tailcat.dev's relays")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vm := fakeVM(t)
	key := newSigner(t)
	srv, err := Start(ctx, Options{
		Keys: string(ssh.MarshalAuthorizedKey(key.PublicKey())),
		Who:  "@tester",
		VM:   func(context.Context) (*ssh.Client, error) { return vm, nil },
		Log:  hclog.New(&hclog.LoggerOptions{Level: hclog.Info, Output: os.Stderr}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	start := time.Now()
	conn, err := Dial(ctx, srv.Addr(), "", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, "fleetci-vm", &ssh.ClientConfig{
		User:            "me",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := ssh.NewClient(cc, chans, reqs)
	defer c.Close()
	// A session waits for Open: the VM is let out only once kept.
	type opened struct {
		s   *ssh.Session
		err error
	}
	got := make(chan opened, 1)
	go func() { s, err := c.NewSession(); got <- opened{s, err} }()
	select {
	case <-got:
		t.Fatal("a session opened before Open")
	case <-time.After(500 * time.Millisecond):
	}
	srv.Open()
	o := <-got
	s, err := o.s, o.err
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Stdout = &out
	err = s.Run("hostname")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || out.String() != "ran: hostname\n" {
		t.Fatalf("Run: %v, out %q", err, out.String())
	}
	t.Logf("connected and ran a command in %s", time.Since(start).Round(time.Millisecond))
}
