package gitlabcache

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachebucket"
)

// NodePort is where the VM reaches the node's signer: a port on the VM's
// loopback that the node's SSH connection forwards to it.
const NodePort = 31852

// VMPort is where the VM's service listens, on the VM's Docker bridge.
const VMPort = 31853

const linkTTL = 15 * time.Minute

// signReq is the VM's ask: "list" a prefix, or a link to "get" or "put"
// an object.
type signReq struct {
	Op   string `json:"op"`
	Name string `json:"name"`
}

type signResp struct {
	Names []string          `json:"names,omitempty"`
	Link  *cachebucket.Link `json:"link,omitempty"`
}

// Signer is the node's side for one job: it lists the bucket and signs
// links, for the stores the job's names fall in only, and the snapshots
// it may write.
type Signer struct {
	B   cachebucket.Bucket
	J   *Job
	Log hclog.Logger
}

func (s *Signer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var q signReq
	if r.Method != http.MethodPost || r.URL.Path != "/sign" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil {
		http.Error(w, "want POST /sign", http.StatusBadRequest)
		return
	}
	write := q.Op == "put"
	if (q.Op != "list" && q.Op != "get" && !write) || !s.J.may(q.Name, write) {
		s.Log.Warn("gitlab cache: refused", "op", q.Op, "name", q.Name)
		http.Error(w, "not this job's", http.StatusForbidden)
		return
	}
	var out signResp
	var err error
	switch q.Op {
	case "list":
		var objs []cachebucket.Object
		objs, err = s.B.List(r.Context(), q.Name)
		for _, o := range objs {
			out.Names = append(out.Names, o.Name)
		}
	case "get":
		var l cachebucket.Link
		l, err = s.B.Download(r.Context(), q.Name, linkTTL)
		out.Link = &l
	case "put":
		var l cachebucket.Link
		l, err = s.B.Upload(r.Context(), q.Name, linkTTL)
		out.Link = &l
	}
	if err != nil {
		s.Log.Warn("gitlab cache: the bucket", "op", q.Op, "err", err)
		http.Error(w, "the bucket: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
