package cluster

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// JobRecord is a job a worker ran, as the controllers keep it. Every
// controller keeps its own history, from the workers' reports, which go
// to all of them: a job is in it whichever controller placed it, even one
// that stepped down.
type JobRecord struct {
	Assignment string    `json:"assignment"`
	Worker     string    `json:"worker"`
	Class      string    `json:"class"`
	Forge      string    `json:"forge"`
	Took       string    `json:"took,omitempty"` // the GitHub job
	Result     string    `json:"result"`         // succeeded or failed
	Err        string    `json:"err,omitempty"`
	Created    time.Time `json:"created,omitzero"`
	Booted     time.Time `json:"booted,omitzero"`
	Started    time.Time `json:"started,omitzero"`
	Ended      time.Time `json:"ended"`
}

// Boot is how long its VM took to be up, from the leader's assignment.
func (r JobRecord) Boot() time.Duration { return span(r.Created, r.Booted) }

// Duration is how long the job ran.
func (r JobRecord) Duration() time.Duration { return span(r.Started, r.Ended) }

func span(a, b time.Time) time.Duration {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return b.Sub(a)
}

const (
	// historyKeep is how many jobs a controller keeps.
	historyKeep = 1000
	// recordedFor is how long a recorded assignment is remembered, so its
	// worker reporting it again does not record it twice: longer than a
	// worker keeps a finished task (forgetDone).
	recordedFor = 2 * forgetDone
)

// history is a controller's record of finished jobs, kept in jobs.jsonl
// in its state directory.
type history struct {
	mu       sync.Mutex
	path     string
	jobs     []JobRecord          // oldest first
	recorded map[string]time.Time // assignment id → when it was recorded
	lines    int                  // in the file
	metrics  *metrics
}

func newHistory(dir string, m *metrics) *history {
	h := &history{path: filepath.Join(dir, "jobs.jsonl"), recorded: map[string]time.Time{}, metrics: m}
	f, err := os.Open(h.path)
	if err != nil {
		return h
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r JobRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		h.lines++
		h.jobs = append(h.jobs, r)
		if time.Since(r.Ended) < recordedFor {
			h.recorded[r.Assignment] = r.Ended
		}
	}
	if len(h.jobs) > historyKeep {
		h.jobs = slices.Clone(h.jobs[len(h.jobs)-historyKeep:])
	}
	return h
}

// observe takes in a worker's report: each assignment it finished that is
// not recorded yet is counted, and, if it ran a job, recorded.
func (h *history) observe(worker string, ss []AssignmentStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var add []JobRecord
	for _, s := range ss {
		if !s.terminal() {
			continue
		}
		if _, ok := h.recorded[s.ID]; ok {
			continue
		}
		h.recorded[s.ID] = time.Now()
		h.metrics.vmDone(s)
		if s.Job == "" {
			continue // a VM no job ran on
		}
		r := JobRecord{Assignment: s.ID, Worker: worker, Class: s.Class, Forge: s.Job, Took: s.Took,
			Result: s.Phase, Err: s.Err, Created: s.Created, Booted: s.Booted, Started: s.Started, Ended: s.Ended}
		if r.Ended.IsZero() {
			r.Ended = s.Since
		}
		h.metrics.jobDone(r)
		add = append(add, r)
	}
	for id, at := range h.recorded {
		if time.Since(at) > recordedFor {
			delete(h.recorded, id)
		}
	}
	if len(add) == 0 {
		return
	}
	h.jobs = append(h.jobs, add...)
	if len(h.jobs) > historyKeep {
		h.jobs = slices.Clone(h.jobs[len(h.jobs)-historyKeep:])
	}
	h.persistLocked(add)
}

// persistLocked appends the records to the file, which is rewritten with
// only the jobs kept once it has twice as many.
func (h *history) persistLocked(add []JobRecord) {
	write := func(path string, flag int, rs []JobRecord) error {
		f, err := os.OpenFile(path, flag|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		e := json.NewEncoder(f)
		for _, r := range rs {
			if err := e.Encode(r); err != nil {
				f.Close()
				return err
			}
		}
		return f.Close()
	}
	if h.lines+len(add) <= 2*historyKeep {
		if write(h.path, os.O_APPEND, add) == nil {
			h.lines += len(add)
		}
		return
	}
	tmp := h.path + ".tmp"
	if write(tmp, os.O_TRUNC, h.jobs) == nil && os.Rename(tmp, h.path) == nil {
		h.lines = len(h.jobs)
	}
}

// list is the last n jobs, newest first, that keep says to.
func (h *history) list(n int, keep func(JobRecord) bool) []JobRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []JobRecord{}
	for i := len(h.jobs) - 1; i >= 0 && len(out) < n; i-- {
		if keep == nil || keep(h.jobs[i]) {
			out = append(out, h.jobs[i])
		}
	}
	return out
}
