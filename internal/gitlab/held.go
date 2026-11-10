package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/network"
)

// A job the leader took from GitLab is known only to it until a worker has
// it: a leader killed while the job waits for a VM leaves it running on
// GitLab with no one to end it, until GitLab drops it as stuck, an hour
// on. So each job waiting for a VM is noted in a file, its id, its own
// token and its GitLab, and the controller's next start fails the ones
// still noted, at once, as a runner system failure, which a job's retry
// can take.

// heldJob is the note of a job taken and not yet placed.
type heldJob struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
	URL   string `json:"url"`
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
	if err == nil {
		err = os.WriteFile(h.path(j.ID), b, 0o600)
	}
	if err != nil {
		log.Warn("noting a job taken; a controller killed before it has a VM leaves it to GitLab's stuck-job timeout", "err", err)
	}
}

// drop removes a job's note: a worker has it, or it was failed.
func (h held) drop(id int64) {
	if h != "" {
		_ = os.Remove(h.path(id))
	}
}

// FailHeld lists the jobs dir notes, taken by a controller that stopped
// before it placed them, and returns what fails them and drops their notes.
// It lists them at once, so the returned func, which waits on GitLab, can
// run beside a new leader without failing the jobs that one takes. A job
// GitLab can't be told of keeps its note, for the next start.
func FailHeld(dir string, log hclog.Logger) func() {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("reading the jobs taken before a restart", "dir", dir, "err", err)
		}
		return func() {}
	}
	var jobs []heldJob
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
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
		jobs = append(jobs, j)
	}
	return func() {
		h, netw := held(dir), network.NewGitLabClient()
		for _, j := range jobs {
			rc := common.RunnerConfig{RunnerCredentials: common.RunnerCredentials{URL: j.URL}}
			r := netw.UpdateJob(rc, &common.JobCredentials{ID: j.ID, Token: j.Token},
				common.UpdateJobInfo{ID: j.ID, State: common.Failed, FailureReason: common.RunnerSystemFailure})
			if r.State == common.UpdateFailed {
				log.Warn("failing a job taken before a restart; trying again at the next start", "job", j.ID)
				continue
			}
			log.Info("failed a job taken before a restart, which never had a VM", "job", j.ID)
			h.drop(j.ID)
		}
	}
}
