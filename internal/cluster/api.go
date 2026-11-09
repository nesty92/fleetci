package cluster

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"
)

// handler is a controller's API; wire.go lists it.
func (c *Controller) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/join", c.handleJoin)
	mux.HandleFunc("POST /v1/ping", c.auth(RoleController, c.handlePing))
	mux.HandleFunc("GET /v1/status", c.auth(RoleController, func(w http.ResponseWriter, _ *http.Request, _ Identity) {
		writeJSON(w, http.StatusOK, c.Status())
	}))
	mux.HandleFunc("POST /v1/tokens", c.auth(RoleController, c.handleToken))
	for _, op := range []string{"drain", "undrain", "revoke"} {
		mux.HandleFunc("POST /v1/workers/{id}/"+op, c.auth(RoleController, func(w http.ResponseWriter, r *http.Request, _ Identity) {
			if err := c.workerOp(op, r.PathValue("id")); err != nil {
				writeErr(w, http.StatusNotFound, err)
				return
			}
			writeJSON(w, http.StatusOK, struct{}{})
		}))
	}
	mux.HandleFunc("PUT /v1/workers/{id}/status", c.auth(RoleWorker, c.handleWorkerStatus))
	mux.HandleFunc("GET /v1/workers/{id}/assignments", c.auth(RoleWorker, c.handleAssignments))
	mux.HandleFunc("POST /v1/workers/{id}/certificate", c.auth(RoleWorker, c.handleRenew))
	mux.HandleFunc("GET /v1/artifacts/{name}", c.auth("", c.handleArtifact))
	return mux
}

// auth lets in callers with a certificate of the role ("" for any) that is
// not revoked; a worker reaches only its own paths.
func (c *Controller) auth(role string, h func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := peerIdentity(r)
		switch {
		case !ok:
			writeErr(w, http.StatusUnauthorized, errors.New("this needs the fleet's certificate: join first"))
			return
		case c.isRevoked(id.Serial):
			writeErr(w, http.StatusForbidden, errors.New("this certificate is revoked"))
			return
		case role != "" && id.Role != role:
			writeErr(w, http.StatusForbidden, fmt.Errorf("this is for a %s's certificate, not a %s's", role, id.Role))
			return
		case id.Role == RoleWorker && r.PathValue("id") != "" && r.PathValue("id") != id.ID:
			writeErr(w, http.StatusForbidden, fmt.Errorf("%s may not act for %s", id.ID, r.PathValue("id")))
			return
		}
		h(w, r, id)
	}
}

func (c *Controller) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp, err := c.join(req)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (c *Controller) handlePing(w http.ResponseWriter, r *http.Request, id Identity) {
	var p pingMsg
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if p.ID != id.ID {
		writeErr(w, http.StatusForbidden, errors.New("the ping names another controller"))
		return
	}
	c.merge(p)
	writeJSON(w, http.StatusOK, c.pingMsg())
}

func (c *Controller) handleToken(w http.ResponseWriter, r *http.Request, _ Identity) {
	var req tokenReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := c.CreateToken(req.Role, req.TTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResp{Token: t, Controllers: c.controllers()})
}

// workerOp drains, undrains or revokes a worker.
func (c *Controller) workerOp(op, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.workers[id]
	if w == nil {
		return fmt.Errorf("no worker %q", id)
	}
	switch op {
	case "drain", "undrain":
		// Saved, and shared with the other controllers by the pings.
		w.draining = op == "drain"
		c.drains[id] = DrainRecord{Drained: w.draining, At: time.Now()}
		c.saveLocked()
	case "revoke":
		c.revoked[w.serial] = Revoked{ID: w.id, Until: time.Now().Add(certLifetime)}
		w.seen = time.Time{}
		c.saveLocked()
		c.log.Warn("worker certificate revoked", "worker", id)
	}
	return nil
}

// handleWorkerStatus takes a worker's facts and report, its heartbeat, and
// answers with its classes and whether it is drained.
func (c *Controller) handleWorkerStatus(w http.ResponseWriter, r *http.Request, id Identity) {
	var up StatusUpdate
	if err := readJSON(r, &up); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	fit := c.fit(up.Facts)
	c.mu.Lock()
	wc := c.workerLocked(id.ID)
	back := wc.state() == Unreachable
	wc.serial, wc.facts, wc.fit, wc.report, wc.seen = id.Serial, up.Facts, fit, up.Report, time.Now()
	c.observeLocked(wc)
	resp := StatusResponse{Fit: fit, Draining: wc.draining}
	c.mu.Unlock()
	if back {
		c.log.Info("worker reporting", "worker", id.ID, "version", up.Facts.Version, "state", up.Report.State, "classes", classNames(fit.Classes), "refused", fit.Refused)
	}
	c.wake()
	writeJSON(w, http.StatusOK, resp)
}

// handleAssignments lists a worker's assignments. With ?rev= at the
// current revision it waits, up to watchTimeout, for the next.
func (c *Controller) handleAssignments(w http.ResponseWriter, r *http.Request, id Identity) {
	since := r.URL.Query().Get("rev")
	timer := time.NewTimer(watchTimeout)
	defer timer.Stop()
	for waited := false; ; waited = true {
		c.mu.Lock()
		wc := c.workerLocked(id.ID)
		list := AssignmentList{Rev: c.revLocked(wc), Assignments: []Assignment{}}
		if list.Rev != since || since == "" || waited {
			for _, a := range wc.assigns {
				list.Assignments = append(list.Assignments, a.Assignment)
			}
			c.mu.Unlock()
			slices.SortFunc(list.Assignments, func(a, b Assignment) int { return a.Created.Compare(b.Created) })
			writeJSON(w, http.StatusOK, list)
			return
		}
		ch := wc.revWake
		c.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
}

func (c *Controller) handleRenew(w http.ResponseWriter, r *http.Request, id Identity) {
	var req renewReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cert, err := c.pki.Sign(req.CSR, RoleWorker, id.ID, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c.log.Info("worker certificate renewed", "worker", id.ID)
	writeJSON(w, http.StatusOK, renewResp{Cert: cert})
}

// handleArtifact serves a binary to a machine that joined; ranges work, so
// a broken download goes on where it stopped.
func (c *Controller) handleArtifact(w http.ResponseWriter, r *http.Request, _ Identity) {
	name := r.PathValue("name")
	path, ok := c.artifactPath(name)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no artifact %q", name))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
