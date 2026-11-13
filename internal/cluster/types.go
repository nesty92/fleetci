package cluster

import (
	"encoding/json"
	"time"

	"github.com/nesty92/fleetci/internal/cachebucket"
	"github.com/nesty92/fleetci/internal/cachedisk"
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
	// Protocol is the fleet protocol it speaks.
	Protocol int `json:"protocol"`
	// Hosts are a joining controller's addresses, for its certificate.
	Hosts []string `json:"hosts,omitempty"`
	// PeerAddrs are where it answers other workers.
	PeerAddrs []string `json:"peer_addrs,omitempty"`
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
	Room       map[string]int `json:"room"`
	UsedCPUs   int            `json:"used_cpus"`
	UsedMemory int64          `json:"used_memory_mb"`
	VMs        []VMInfo       `json:"vms,omitempty"`
	// Idle is how many VMs of each class are booted with no job and no
	// assignment, until their idle_time: none, mostly.
	Idle   map[string]int    `json:"idle,omitempty"`
	Checks map[string]string `json:"checks,omitempty"` // class → "ok …" or the failure
	// Assignments are the assignments it has, and those it finished
	// lately.
	Assignments []AssignmentStatus `json:"assignments,omitempty"`
	Update      *UpdateStatus      `json:"update,omitempty"`
	// Caches are the cache disks it keeps (cachedisk.Scope.Dirs' names):
	// the controllers place a job where its cache is.
	Caches []string `json:"caches,omitempty"`
	// Neighbours are the workers it reaches, of those the controllers
	// name to it.
	Neighbours []Neighbour `json:"neighbours,omitempty"`
}

// VMInfo is one of a worker's VMs.
type VMInfo struct {
	ID    string    `json:"id"`
	Class string    `json:"class"`
	Since time.Time `json:"since"`
	// Job is what runs on it: "github" or "gitlab".
	Job  string `json:"job,omitempty"`
	Took string `json:"took,omitempty"` // the GitHub job its runner took
	// Assignment is the assignment it was made for.
	Assignment string `json:"assignment,omitempty"`
}

// StatusUpdate is what a worker sends every controller when something
// changes and every 5 s: its facts and its report. It is also its
// heartbeat.
type StatusUpdate struct {
	Facts  Facts  `json:"facts"`
	Report Report `json:"report"`
}

// StatusResponse is a controller's answer to a status update: the classes
// the worker runs, and whether it is drained.
type StatusResponse struct {
	Fit      Fit  `json:"fit"`
	Draining bool `json:"draining,omitempty"`
	// Update is how far the worker may update itself; UpdateCheck, when
	// later than its last check, asks it to check now; UpdateGo is the
	// version the leader lets it install now, its update slot.
	Update      string    `json:"update,omitempty"`
	UpdateCheck time.Time `json:"update_check,omitzero"`
	UpdateGo    string    `json:"update_go,omitempty"`
	// Version and Protocol are the controller's, and Artifacts the binaries
	// it serves: a worker whose [update] source is "controllers" takes them
	// as its release.
	Version   string     `json:"version,omitempty"`
	Protocol  int        `json:"protocol,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Verify, when later than the worker's last request, asks it to run
	// its microVM checks again: fleetci fleet verify.
	Verify time.Time `json:"verify,omitzero"`
	// Peers are the other workers, for it to find which it reaches.
	Peers []PeerHint `json:"peers,omitempty"`
}

// The update policies: how far a worker may update itself, set on the
// controllers. A version, "v0.7.0", pins it.
const (
	UpdateOff    = "off"
	UpdatePatch  = "patch"
	UpdateMinor  = "minor"
	UpdateLatest = "latest"
)

// The states of a worker's self-update.
const (
	UpdateIdle     = ""
	UpdateStaging  = "downloading"
	UpdateReady    = "ready"    // downloaded; waits for its slot
	UpdateDraining = "draining" // has its slot; waits for its jobs to end
	UpdateTrial    = "trial"    // runs the new version, on trial
	UpdateFailed   = "failed"   // the last try failed, and rolled back
)

// UpdateStatus is a worker's self-update, in its reports.
type UpdateStatus struct {
	State  string `json:"state,omitempty"`
	Target string `json:"target,omitempty"`
	// Protocol is the target's, which the leader checks before it gives
	// the slot: a worker must not outrun its controllers.
	Protocol int    `json:"protocol,omitempty"`
	Err      string `json:"err,omitempty"`
}

// Assignment is a VM the leader wants on a worker, and later the job to run
// on it. The worker watches its assignments, makes them so, and reports
// each one's status; the leader never calls a worker.
type Assignment struct {
	ID      string    `json:"id"`
	Class   string    `json:"class"`
	Created time.Time `json:"created"`
	// Run is the job, set once the VM is Ready.
	Run *RunSpec `json:"run,omitempty"`
	// Cancel asks the worker to stop it and remove its VM. An assignment
	// that is no longer listed is not cancelled: its controller may have
	// gone, and the worker sees it through.
	Cancel bool `json:"cancel,omitempty"`
	// Cache answers a GitHub job's hook (AssignmentStatus.JobHint): the
	// disk to attach before the job goes on.
	Cache *CacheGrant `json:"cache,omitempty"`
}

// CacheGrant is the cache disk the leader decided on for a GitHub job.
type CacheGrant struct {
	// Scope is the disk to attach; nil for none, and Note says why.
	Scope *cachedisk.Scope `json:"scope,omitempty"`
	Note  string           `json:"note,omitempty"`
}

// RunSpec is the job an assignment runs: one of GitHub and GitLab.
type RunSpec struct {
	GitHub *GitHubRun `json:"github,omitempty"`
	GitLab *GitLabRun `json:"gitlab,omitempty"`
}

// GitHubRun is a just-in-time runner.
type GitHubRun struct {
	JITConfig     string `json:"jit_config"`
	RunnerVersion string `json:"runner_version,omitempty"`
	CacheURL      string `json:"cache_url,omitempty"`
	// CacheStore, CacheBucket and CacheTokenIssuer are the [github]
	// table's; the worker serves the bucket to its VMs, as the leader
	// would. The bucket's credentials go only to workers, as a GitLab
	// job's [runners.cache] does.
	CacheStore       string              `json:"cache_store,omitempty"`
	CacheBucket      *cachebucket.Config `json:"cache_bucket,omitempty"`
	CacheTokenIssuer string              `json:"cache_token_issuer,omitempty"`
	IdleTimeout      time.Duration       `json:"idle_timeout"`
	// Cache: the VM's class has a cache disk, and each job waits at its
	// start for the leader to say which (Assignment.Cache).
	Cache bool `json:"cache,omitempty"`
}

// GitLabRun is a GitLab job, run with the job's own token.
type GitLabRun struct {
	Job    json.RawMessage `json:"job"`    // the job as GitLab gave it
	Runner json.RawMessage `json:"runner"` // the [[runners]] table, without its token
	// Cache is the job's cache disk, which the worker attaches when the
	// VM's class has one; nil for none.
	Cache *cachedisk.Scope `json:"cache,omitempty"`
}

// An assignment's phases.
const (
	PhasePending   = "Pending" // accepted; the VM is booting
	PhaseReady     = "Ready"   // the VM is up; waiting for its job
	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// ReasonNoRoom is a failed assignment's reason when the worker had no room.
const ReasonNoRoom = "NoRoom"

// AssignmentStatus is an assignment as its worker reports it.
type AssignmentStatus struct {
	ID       string `json:"id"`
	Class    string `json:"class"`
	Phase    string `json:"phase"`
	Reason   string `json:"reason,omitempty"`
	Err      string `json:"err,omitempty"`
	VM       string `json:"vm,omitempty"`
	Instance string `json:"instance,omitempty"`
	Took     string `json:"took,omitempty"` // the GitHub job its runner took
	// JobHint is what the GitHub job's hook said of the job, "owner/repo
	// run-id", for the leader to decide its cache disk; CacheNote is what
	// the job was told of its disk, once it was.
	JobHint   string    `json:"job_hint,omitempty"`
	CacheNote string    `json:"cache_note,omitempty"`
	Since     time.Time `json:"since"`
	// Job is the forge of the job it ran, "github" or "gitlab"; the times
	// are when the leader wrote it, its VM was up, its job started, and it
	// ended. Every controller keeps its history from these.
	Job     string    `json:"job,omitempty"`
	Created time.Time `json:"created,omitzero"`
	Booted  time.Time `json:"booted,omitzero"`
	Started time.Time `json:"started,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
}

func (s AssignmentStatus) terminal() bool {
	return s.Phase == PhaseSucceeded || s.Phase == PhaseFailed
}

// AssignmentList is a worker's assignments on one controller, at a
// revision: a watch with that revision waits for the next.
type AssignmentList struct {
	Rev         string       `json:"rev"`
	Assignments []Assignment `json:"assignments"`
}

type (
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
	Version   string                 `json:"version,omitempty"`
	Protocol  int                    `json:"protocol,omitempty"`
	Leader    string                 `json:"leader"`
	Advertise []string               `json:"advertise,omitempty"`
	Revoked   map[string]Revoked     `json:"revoked,omitempty"`
	Tokens    map[string]TokenRecord `json:"tokens,omitempty"`
	Drains    map[string]DrainRecord `json:"drains,omitempty"`
}

// DrainRecord is whether a worker is drained, as of when that was said:
// the controllers share them, and the latest wins.
type DrainRecord struct {
	Drained bool      `json:"drained"`
	At      time.Time `json:"at"`
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
	// Versions are the controllers' fleetci versions.
	Versions map[string]string `json:"versions,omitempty"`
	Workers  []WorkerStatus    `json:"workers"`
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

// tokenReq and tokenResp are POST /v1/tokens's.
type (
	tokenReq struct {
		Role string        `json:"role,omitempty"`
		TTL  time.Duration `json:"ttl,omitempty"`
	}
	tokenResp struct {
		Token       string   `json:"token"`
		Controllers []string `json:"controllers"`
		// Pin is the answering controller's key, the first of
		// Controllers, as curl's --pinnedpubkey takes it.
		Pin string `json:"pin,omitempty"`
	}
)
