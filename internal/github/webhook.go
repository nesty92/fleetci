package github

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxDelivery caps a delivery's body. A workflow_job event is a few KB.
const maxDelivery = 1 << 20

// listen starts the webhook's server on ctx.
func (d *Dispatcher) listen(ctx context.Context) (*http.Server, error) {
	w := d.cfg.Webhook
	ln, err := net.Listen("tcp", w.Listen)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(cmp.Or(w.Path, "/"), d)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		var err error
		if w.TLSCertFile != "" {
			err = srv.ServeTLS(ln, w.TLSCertFile, w.TLSKeyFile)
		} else {
			err = srv.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("webhook stopped; polling only", "err", err)
		}
	}()
	d.log.Info("webhook listening", "addr", ln.Addr().String(), "path", cmp.Or(w.Path, "/"), "tls", w.TLSCertFile != "")
	select {
	case d.listening <- ln.Addr().String():
	default:
	}
	return srv, nil
}

// delivery is the part of a workflow_job event fleetci reads.
// https://docs.github.com/en/webhooks/webhook-events-and-payloads#workflow_job
type delivery struct {
	Action     string `json:"action"`
	Job        job    `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ServeHTTP takes one delivery: a signed workflow_job event.
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDelivery))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if !d.signed(body, r.Header.Get("X-Hub-Signature-256")) {
		d.log.Warn("webhook delivery with a bad signature", "remote", r.RemoteAddr, "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		_, _ = io.WriteString(w, "pong\n")
		return
	case "workflow_job":
	default:
		w.WriteHeader(http.StatusNoContent) // not one fleetci reads
		return
	}
	var ev delivery
	if err := json.Unmarshal(body, &ev); err != nil || ev.Job.ID == 0 || ev.Repository.FullName == "" {
		http.Error(w, "not a workflow_job event", http.StatusBadRequest)
		return
	}
	d.event(ev)
	w.WriteHeader(http.StatusAccepted)
}

// signed checks the delivery's HMAC-SHA256 against the secret.
func (d *Dispatcher) signed(body []byte, header string) bool {
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, d.secret)
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}

// event acts on a workflow_job event: a queued job is offered to a runner
// at once and watched until it starts; one that started or ended is no
// longer watched.
func (d *Dispatcher) event(ev delivery) {
	repo := ev.Repository.FullName
	if !d.serves(repo) {
		return
	}
	d.mu.Lock()
	d.active[repo] = time.Now()
	ctx := d.runCtx
	d.mu.Unlock()
	switch ev.Action {
	case "queued":
		ev.Job.Status = "queued"
		if _, ok := d.classFor(repo, ev.Job); !ok {
			return
		}
		d.mu.Lock()
		d.watched[ev.Job.ID] = repo
		d.mu.Unlock()
		if ctx != nil && ctx.Err() == nil {
			d.offer(ctx, repo, ev.Job, "webhook")
		}
	case "in_progress", "completed":
		d.mu.Lock()
		delete(d.watched, ev.Job.ID)
		delete(d.warned, ev.Job.ID)
		if a, ok := d.taken[ev.Job.ID]; ok && !a.running && ev.Action == "completed" {
			delete(d.taken, ev.Job.ID)
		}
		d.mu.Unlock()
	}
}

// recheck looks again at the watched jobs no runner is serving: one still
// queued gets a runner again, as a poll would give it. A runner that
// failed, or took another job, leaves its job queued with no event to
// say so.
func (d *Dispatcher) recheck(ctx context.Context) {
	type due struct {
		id   int64
		repo string
	}
	var list []due
	d.mu.Lock()
	for id, repo := range d.watched {
		a := d.taken[id]
		if a != nil && (a.running || a.tries >= maxTries || time.Now().Before(a.retryAt)) {
			continue
		}
		list = append(list, due{id, repo})
	}
	d.mu.Unlock()
	for _, w := range list {
		if ctx.Err() != nil {
			return
		}
		j, err := d.gh.job(ctx, w.repo, w.id)
		switch {
		case errors.As(err, new(rateLimited)):
			return
		case err != nil && !errors.Is(err, errNotFound):
			d.log.Warn("looking at a queued job", "repo", w.repo, "job_id", strconv.FormatInt(w.id, 10), "err", err)
			continue
		case err != nil || j.Status != "queued":
			d.mu.Lock()
			delete(d.watched, w.id)
			d.mu.Unlock()
			continue
		}
		d.offer(ctx, w.repo, j, "poll")
	}
}
