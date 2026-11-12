package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/network"

	"github.com/nesty92/fleetci/internal/cachedisk"
)

// A job the leader took from GitLab is known only to it until a worker has
// it, and GitLab gives a job out once: a leader killed or restarted while
// the job waits for a VM would leave it running on GitLab with no one to
// run or end it, until GitLab drops it as stuck, an hour on, and a job
// with no retry would need someone to run it again. So each job waiting
// for a VM is noted in a file, with what a worker runs it from, and the
// controller's next start places it on a VM as it would have; a job is
// failed only when it can't be: no VM for it, or no lead within
// ResumeWithin, as another controller leads and this one can't give it the
// job.

// ResumeWithin is how long a controller's start waits to lead, and place
// the jobs its last process left waiting, before it fails them.
const ResumeWithin = 5 * time.Minute

// heldJob is the note of a job taken and not yet placed: the job and the
// runner table, without its token, as a worker takes them. They hold the
// job's variables and its token, so the note is the controller's alone.
type heldJob struct {
	ID     int64           `json:"id"`
	Token  string          `json:"token"`
	URL    string          `json:"url"`
	Class  string          `json:"class,omitempty"`
	Job    json.RawMessage `json:"job,omitempty"`
	Runner json.RawMessage `json:"runner,omitempty"`
	// Cache is the job's cache disk, as the leader decided it.
	Cache *cachedisk.Scope `json:"cache,omitempty"`
}

// held is the directory of notes; "" keeps none.
type held string

func (h held) path(id int64) string { return filepath.Join(string(h), fmt.Sprintf("%d.json", id)) }

// put notes a job; a note that can't be written is logged, and the job
// goes on without one.
func (h held) put(j heldJob, log hclog.Logger) {
	if h == "" {
		return
	}
	b, err := json.Marshal(j)
	if err == nil {
		err = os.MkdirAll(string(h), 0o700)
	}
	// Written whole or not at all: a kill mid-write leaves a .tmp, which
	// the next start removes, never a note cut short.
	tmp := h.path(j.ID) + ".tmp"
	if err == nil {
		err = os.WriteFile(tmp, b, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, h.path(j.ID))
	}
	if err != nil {
		log.Warn("noting a job taken; a controller restarted before it has a VM can't place it", "err", err)
	}
}

// drop removes a job's note: a worker has it, or it was failed.
func (h held) drop(id int64) {
	if h != "" {
		_ = os.Remove(h.path(id))
	}
}

// Held is the jobs a controller's last process took and never placed, as
// its start found their notes: the leader places them, or FailAfter fails
// them, whichever comes first.
type Held struct {
	dir held
	log hclog.Logger

	mu    sync.Mutex
	jobs  []heldJob
	taken bool
}

// LoadHeld reads the notes in dir now, before this controller can lead and
// note jobs of its own.
func LoadHeld(dir string, log hclog.Logger) *Held {
	h := &Held{dir: held(dir), log: log}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("reading the jobs taken before a restart", "dir", dir, "err", err)
		}
		return h
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if filepath.Ext(p) != ".json" {
			_ = os.Remove(p) // a note its writer was killed writing
			continue
		}
		b, err := os.ReadFile(p)
		var j heldJob
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID == 0 || j.Token == "" {
			log.Warn("dropping a note of a job that can't be read", "file", p, "err", err)
			_ = os.Remove(p)
			continue
		}
		h.jobs = append(h.jobs, j)
	}
	if len(h.jobs) > 0 {
		log.Info("jobs taken before a restart, and never placed, wait for this controller to lead", "jobs", len(h.jobs), "within", ResumeWithin.String())
	}
	return h
}

// take hands over the jobs, once: to the leader, or to FailAfter.
func (h *Held) take() []heldJob {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.taken {
		return nil
	}
	h.taken = true
	jobs := h.jobs
	h.jobs = nil
	return jobs
}

// FailAfter fails the jobs no leader took within d, as a runner system
// failure, with their own tokens. When ctx ends first the notes stay, for
// the next start; a job GitLab can't be told of keeps its note too.
func (h *Held) FailAfter(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(d):
	}
	netw := network.NewGitLabClient()
	for _, j := range h.take() {
		rc := common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: j.URL}}
		r := netw.UpdateJob(rc, &common.JobCredentials{ID: j.ID, Token: j.Token},
			common.UpdateJobInfo{ID: j.ID, State: common.Failed, FailureReason: common.RunnerSystemFailure})
		if r.State == common.UpdateFailed {
			h.log.Warn("failing a job taken before a restart; trying again at the next start", "job", j.ID)
			continue
		}
		h.log.Warn("failed a job taken before a restart: this controller did not lead again to place it", "job", j.ID, "waited", d.String())
		h.dir.drop(j.ID)
	}
}
