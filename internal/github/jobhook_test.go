package github

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nesty92/fleetci/internal/proxy"
)

type exitErr int

func (e exitErr) Error() string   { return "exit" }
func (e exitErr) ExitStatus() int { return int(e) }

func stubHook(t *testing.T, results ...func() (string, error)) *int {
	t.Helper()
	run, retry := hookRun, hookRetry
	t.Cleanup(func() { hookRun, hookRetry = run, retry })
	hookRetry = time.Millisecond
	calls := 0
	hookRun = func(context.Context, *proxy.Conn, string, string) (string, error) {
		r := results[min(calls, len(results)-1)]
		calls++
		return r()
	}
	return &calls
}

func TestWaitJobHookRetriesTheConnection(t *testing.T) {
	refused := func() (string, error) { return "", errors.New("ssh: handshake failed: EOF") }
	calls := stubHook(t, refused, refused, func() (string, error) { return "o/r 7\n", nil })
	got, err := WaitJobHook(context.Background(), nil)
	if err != nil || got != "o/r 7" || *calls != 3 {
		t.Fatalf("got %q, %v after %d calls; want o/r 7 after 3", got, err, *calls)
	}
}

func TestWaitJobHookDoesNotRetryTheScript(t *testing.T) {
	calls := stubHook(t, func() (string, error) { return "", exitErr(4) })
	if _, err := WaitJobHook(context.Background(), nil); !errors.Is(err, ErrHookGaveUp) || *calls != 1 {
		t.Fatalf("err %v after %d calls; want ErrHookGaveUp after 1", err, *calls)
	}
	calls = stubHook(t, func() (string, error) { return "", exitErr(3) })
	if _, err := WaitJobHook(context.Background(), nil); err == nil || *calls != 1 {
		t.Fatalf("err %v after %d calls; want an error after 1", err, *calls)
	}
}

func TestWaitJobHookStopsWithItsContext(t *testing.T) {
	stubHook(t, func() (string, error) { return "", errors.New("connection refused") })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := WaitJobHook(ctx, nil); err == nil {
		t.Fatal("no error once the context is done")
	}
}
