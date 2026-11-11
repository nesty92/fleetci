package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// startFunc starts a class's plugin and initialises it. It calls exited
// when the plugin process ends.
type startFunc func(ctx context.Context, exited func()) (provider.InstanceGroup, provider.ProviderInfo, func(), error)

// supervised is a class's instance group that outlives its plugin process.
// taskscaler holds it for the class's lifetime. When the plugin process
// ends (killed, crashed, out of memory), or a call finds it gone, the plugin
// is started and initialised again, and later calls go to the new one. The new plugin
// lists what it finds; taskscaler drops instances it no longer sees, so
// VMs that died with the old plugin are removed and their jobs fail, while
// the class keeps serving new jobs.
//
// Failed calls are not retried: taskscaler calls Update every second or
// two and retries the rest itself, and retrying Increase or Decrease on a
// new plugin could create or remove twice.
type supervised struct {
	start startFunc
	id    string // the group's ID; a restarted plugin must report the same
	log   hclog.Logger

	mu       sync.RWMutex
	group    provider.InstanceGroup
	kill     func()
	gen      int
	closed   bool
	next     time.Time // no restart before this
	backoff  time.Duration
	retry    *time.Timer // a restart after a failed one
	restarts int
}

const (
	restartBackoffMin = time.Second
	restartBackoffMax = time.Minute
)

func supervise(ctx context.Context, start startFunc, log hclog.Logger) (*supervised, provider.ProviderInfo, error) {
	s := &supervised{start: start, log: log, backoff: restartBackoffMin}
	// Held through the start: a plugin that exits while it starts has its
	// restart wait for this, which sets what the restart replaces.
	s.mu.Lock()
	defer s.mu.Unlock()
	group, info, kill, err := start(ctx, s.exited(0))
	if err != nil {
		// A plugin that exits as its start fails is not restarted.
		s.closed = true
		return nil, provider.ProviderInfo{}, err
	}
	s.id, s.group, s.kill = info.ID, group, kill
	return s, info, nil
}

// exitWatch is a plugin's logger that calls exited when go-plugin reports
// the plugin process ended, the moment it does, rather than at the next
// call: taskscaler calls Update only once a minute while a class is quiet.
type exitWatch struct {
	hclog.Logger
	once   sync.Once
	exited func()
}

func watchExit(log hclog.Logger, exited func()) *exitWatch {
	return &exitWatch{Logger: log, exited: exited}
}

func (w *exitWatch) seen(msg string) {
	if msg == "plugin process exited" {
		w.once.Do(w.exited)
	}
}

func (w *exitWatch) Error(msg string, args ...any) { w.seen(msg); w.Logger.Error(msg, args...) }
func (w *exitWatch) Info(msg string, args ...any)  { w.seen(msg); w.Logger.Info(msg, args...) }
func (w *exitWatch) Log(level hclog.Level, msg string, args ...any) {
	w.seen(msg)
	w.Logger.Log(level, msg, args...)
}

// exited is the exit callback for the plugin of generation gen.
func (s *supervised) exited(gen int) func() {
	return func() { go s.restart(gen, errors.New("plugin process exited")) }
}

// pluginGone is whether err says the plugin process is no longer there.
func pluginGone(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == codes.Unavailable {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection is shut down") || strings.Contains(msg, "plugin exited")
}

func (s *supervised) current() (provider.InstanceGroup, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.group, s.gen
}

// check restarts the plugin when err says it is gone.
func (s *supervised) check(gen int, err error) {
	if pluginGone(err) {
		s.restart(gen, err)
	}
}

// restart replaces the plugin of generation gen, unless another call
// already did. A failed start is retried after a growing backoff.
func (s *supervised) restart(gen int, why error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || gen != s.gen {
		return
	}
	if wait := time.Until(s.next); wait > 0 {
		if s.retry == nil {
			s.retry = time.AfterFunc(wait, func() { s.restart(gen, why) })
		}
		return
	}
	s.retry = nil
	s.log.Error("plugin gone, starting it again", "err", why, "restarts", s.restarts)
	s.kill()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	group, info, kill, err := s.start(ctx, s.exited(gen+1))
	if err == nil && info.ID != s.id {
		kill()
		err = fmt.Errorf("restarted plugin reports group %s, not %s", info.ID, s.id)
	}
	if err != nil {
		s.next = time.Now().Add(s.backoff)
		s.log.Error("plugin restart failed", "err", err, "retry_in", s.backoff.String())
		s.backoff = min(s.backoff*2, restartBackoffMax)
		s.retry = time.AfterFunc(time.Until(s.next), func() { s.restart(gen, why) })
		return
	}
	s.group, s.kill = group, kill
	s.gen++
	s.restarts++
	s.backoff, s.next = restartBackoffMin, time.Time{}
	s.log.Info("plugin restarted", "group", info.ID, "restarts", s.restarts)
}

// Close kills the plugin and stops restarts.
func (s *supervised) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.retry != nil {
			s.retry.Stop()
		}
		s.kill()
	}
}

// Init was done by start; taskscaler's call gets the group's info back.
func (s *supervised) Init(context.Context, hclog.Logger, provider.Settings) (provider.ProviderInfo, error) {
	return provider.ProviderInfo{}, errors.New("supervised: Init is done by start")
}

func (s *supervised) Update(ctx context.Context, fn func(string, provider.State)) error {
	g, gen := s.current()
	err := g.Update(ctx, fn)
	s.check(gen, err)
	return err
}

func (s *supervised) Increase(ctx context.Context, n int) (int, error) {
	g, gen := s.current()
	ok, err := g.Increase(ctx, n)
	s.check(gen, err)
	return ok, err
}

func (s *supervised) Decrease(ctx context.Context, ids []string) ([]string, error) {
	g, gen := s.current()
	ok, err := g.Decrease(ctx, ids)
	s.check(gen, err)
	return ok, err
}

func (s *supervised) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	g, gen := s.current()
	info, err := g.ConnectInfo(ctx, id)
	s.check(gen, err)
	return info, err
}

func (s *supervised) Heartbeat(ctx context.Context, id string) error {
	g, gen := s.current()
	err := g.Heartbeat(ctx, id)
	s.check(gen, err)
	return err
}

func (s *supervised) Suspend(ctx context.Context, ids []string) ([]string, error) {
	g, gen := s.current()
	ok, err := g.Suspend(ctx, ids)
	s.check(gen, err)
	return ok, err
}

func (s *supervised) Resume(ctx context.Context, ids []string) ([]string, error) {
	g, gen := s.current()
	ok, err := g.Resume(ctx, ids)
	s.check(gen, err)
	return ok, err
}

func (s *supervised) Shutdown(ctx context.Context) error {
	g, gen := s.current()
	err := g.Shutdown(ctx)
	s.check(gen, err)
	return err
}
