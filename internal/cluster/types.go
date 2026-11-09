package cluster

import (
	"encoding/json"
	"time"
)

// Facts are what a worker has, as it reports them on joining and on each
// connect.
type Facts struct {
	Arch string `json:"arch"`
	// CPUs and MemoryMB are its budget for VMs, the reserve for the host
	// taken off.
	CPUs     int   `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`
	// DiskMB is free where its VMs' disks and images go.
	DiskMB int64             `json:"disk_mb"`
	KVM    string            `json:"kvm,omitempty"` // "" when /dev/kvm works, else why not
	Labels map[string]string `json:"labels,omitempty"`
	// Only, when set, are the only classes it runs.
	Only    []string `json:"only,omitempty"`
	Version string   `json:"version"`
	// Hosts are a joining controller's addresses, for its certificate.
	Hosts []string `json:"hosts,omitempty"`
}

// Class is a class a worker runs: its name and spec.
type Class struct {
	Name string `json:"name"`
	ClassSpec
}

// Fit is what the controllers make of a worker's facts: the classes it
// runs, and why each of the others is left off.
type Fit struct {
	Classes []Class           `json:"classes"`
	Refused map[string]string `json:"refused,omitempty"`
	// Reach are the URLs the VMs must reach: the forges.
	Reach []string `json:"reach,omitempty"`
	// GitHub says the classes serve GitHub jobs, whose VMs need actions/runner.
	GitHub bool `json:"github,omitempty"`
}

// The worker's states.
const (
	Ready       = "Ready"
	NotReady    = "NotReady"
	Draining    = "Draining"
	Unreachable = "Unreachable"
)

// Report is a worker's room and jobs, sent when they change and every 5 s.
type Report struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Room is how many more VMs of each class fit now.
	Room       map[string]int    `json:"room"`
	UsedCPUs   int               `json:"used_cpus"`
	UsedMemory int64             `json:"used_memory_mb"`
	VMs        []VMInfo          `json:"vms,omitempty"`
	Checks     map[string]string `json:"checks,omitempty"` // class → "ok …" or the failure
}

// VMInfo is one of a worker's VMs.
type VMInfo struct {
	ID    string    `json:"id"`
	Class string    `json:"class"`
	Since time.Time `json:"since"`
	// Job is what runs on it: "github <runner>" or "gitlab <job id>".
	Job  string `json:"job,omitempty"`
	Took string `json:"took,omitempty"` // the GitHub job its runner took
}

// Hello is a worker's first message on its stream.
type Hello struct {
	Facts  Facts  `json:"facts"`
	Report Report `json:"report"`
}

// Event is something that happened on a worker's VM.
type Event struct {
	VM   string `json:"vm"`
	Kind string `json:"kind"` // "took" or "exit"
	Job  string `json:"job,omitempty"`
	Err  string `json:"err,omitempty"`
}

// The requests a controller sends a worker.
type (
	createReq struct {
		Class string `json:"class"`
	}
	createResp struct {
		VM       string `json:"vm"`
		Class    string `json:"class"`
		Instance string `json:"instance"`
	}
	vmReq struct {
		VM string `json:"vm"`
	}
	runGitHubReq struct {
		VM            string        `json:"vm"`
		JITConfig     string        `json:"jit_config"`
		RunnerVersion string        `json:"runner_version,omitempty"`
		CacheURL      string        `json:"cache_url,omitempty"`
		IdleTimeout   time.Duration `json:"idle_timeout"`
	}
	runGitLabReq struct {
		VM     string          `json:"vm"`
		Job    json.RawMessage `json:"job"`    // the job as GitLab gave it
		Runner json.RawMessage `json:"runner"` // the [[runners]] table, without its token
	}
	renewReq struct {
		CSR []byte `json:"csr"`
	}
	renewResp struct {
		Cert []byte `json:"cert"`
	}
)

// JoinRequest is a new machine's: its token, CSR and facts.
type JoinRequest struct {
	Token string `json:"token"` // id.secret; the CA hash stays with the joiner
	ID    string `json:"id"`
	CSR   []byte `json:"csr"`
	Facts Facts  `json:"facts"`
}

// JoinResponse is its certificate and the fleet's manifest.
type JoinResponse struct {
	Cert     []byte   `json:"cert"`
	CA       []byte   `json:"ca"`
	Role     string   `json:"role"`
	Manifest Manifest `json:"manifest"`
	// CAKey is the CA's key, for a joining controller.
	CAKey []byte `json:"ca_key,omitempty"`
}

// Manifest is what a worker installs and runs.
type Manifest struct {
	Version     string     `json:"version"`
	Artifacts   []Artifact `json:"artifacts"`
	Controllers []string   `json:"controllers"`
	Fit         Fit        `json:"fit"`
}

// Artifact is a binary a worker installs.
type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// pingMsg is a controller's heartbeat to another, and the answer.
type pingMsg struct {
	ID        string                 `json:"id"`
	Leader    string                 `json:"leader"`
	Advertise []string               `json:"advertise,omitempty"`
	Revoked   map[string]Revoked     `json:"revoked,omitempty"`
	Tokens    map[string]TokenRecord `json:"tokens,omitempty"`
}

// Revoked is a revoked certificate, until it would have expired.
type Revoked struct {
	ID    string    `json:"id"`
	Until time.Time `json:"until"`
}

// TokenRecord is a join token as controllers keep it.
type TokenRecord struct {
	Hash    string    `json:"hash"`
	Role    string    `json:"role"`
	Expires time.Time `json:"expires"`
	Used    string    `json:"used,omitempty"` // the machine that joined with it
}

// Status is fleetci fleet status's.
type Status struct {
	Controller  string               `json:"controller"`
	Leader      string               `json:"leader"`
	Controllers map[string]time.Time `json:"controllers"`
	Workers     []WorkerStatus       `json:"workers"`
}

// WorkerStatus is a worker as a controller sees it.
type WorkerStatus struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	Seen     time.Time `json:"seen"`
	Facts    Facts     `json:"facts"`
	Fit      Fit       `json:"fit"`
	Report   Report    `json:"report"`
	Draining bool      `json:"draining,omitempty"`
}

// adminReq is one of fleetci's commands, run on a controller.
type adminReq struct {
	Op     string        `json:"op"` // token, status, drain, undrain, revoke
	Worker string        `json:"worker,omitempty"`
	TTL    time.Duration `json:"ttl,omitempty"`
	Role   string        `json:"role,omitempty"`
}
