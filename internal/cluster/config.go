package cluster

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/mod/semver"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/github"
	"github.com/nesty92/fleetci/internal/gitlab"
)

// DefaultImageURL is fleetci's small image, which has Docker and
// actions/runner: what the classes boot unless they name another.
const DefaultImageURL = "https://github.com/nesty92/fleetci/releases/download/vm-image-small/small-{arch}.SHA256SUMS"

// ControllerConfig is controller.toml: the fleet's classes, the forge
// credentials, and this controller.
type ControllerConfig struct {
	Controller   ControllerTable       `toml:"controller"`
	DefaultClass string                `toml:"default_class"`
	Classes      map[string]*ClassSpec `toml:"classes"`
	GitHub       *github.Config        `toml:"github"`
	GitLab       *gitlab.Config        `toml:"gitlab"`

	File string `toml:"-"`
}

// ControllerTable is controller.toml's [controller].
type ControllerTable struct {
	// ID names the controller; the live one with the lowest leads.
	ID string `toml:"id"`
	// Listen is the address to serve on. Default ":9481".
	Listen string `toml:"listen"`
	// Advertise are the addresses workers reach this controller at, which
	// join tokens print and its certificate names.
	Advertise []string `toml:"advertise"`
	// Peers are the other controllers.
	Peers []string `toml:"peers"`
	// PKIDir holds the CA and this controller's certificate. Default
	// /etc/fleetci/pki.
	PKIDir string `toml:"pki_dir"`
	// StateDir holds the join tokens and the revoked certificates.
	// Default /var/lib/fleetci.
	StateDir string `toml:"state_dir"`
	// Placement is "pack" (default) or "spread", across workers.
	Placement string `toml:"placement"`
	// Artifacts is a directory with the binaries joining workers install:
	// cloud-hypervisor and fleeting-plugin-fleetingd. fleetci itself is
	// this controller's own binary.
	Artifacts string `toml:"artifacts"`
	// Metrics is an address to serve Prometheus metrics on, plain HTTP at
	// /metrics, e.g. "127.0.0.1:9482". Default none.
	Metrics string `toml:"metrics"`
	// WorkerUpdate is how far workers with [update] auto may update
	// themselves: "off" (default), "patch", "minor", "latest", or a
	// version, "v0.7.0", to pin them to.
	WorkerUpdate string `toml:"worker_update"`
	// UpdateParallel is how many workers may update at once. Default 1.
	UpdateParallel int `toml:"update_parallel"`
	// CacheWait is how long a job of a class with cache_disk waits for
	// room on the worker that has its cache, before it goes to another,
	// where it starts from the cache it falls back on or from nothing.
	// Default 30s.
	CacheWait fleet.Duration `toml:"cache_wait"`
}

// ClassSpec is a class as the controllers define it, once for the fleet:
// what a VM of it needs. The workers whose resources fit it run it, as
// many at once as their budget has room for.
type ClassSpec struct {
	CPUs   int        `toml:"cpus" json:"cpus"`
	Memory fleet.Size `toml:"memory" json:"memory"`
	// Disk is each VM's disk. Default 30GB.
	Disk fleet.Size `toml:"disk" json:"disk"`
	// Image is a fleetingd vm_image_url. Default DefaultImageURL.
	Image string `toml:"image" json:"image"`
	// Arch is the VMs' GOARCH. Default amd64.
	Arch string `toml:"arch" json:"arch"`
	// Overflow is the class that takes the class's jobs when no worker
	// has room for them.
	Overflow string `toml:"overflow" json:"overflow,omitempty"`
	// MaxInstances caps the class's VMs across the fleet; 0 for none.
	MaxInstances int `toml:"max_instances" json:"max_instances,omitempty"`
	// On limits the class to workers with these labels.
	On map[string]string `toml:"on" json:"on,omitempty"`
	// CacheDisk gives each job's VM a disk of its project's and ref's
	// caches, kept on the worker (see fleet.CacheDisk). Each worker sizes
	// its own in its [cache_disk]; jobs go to a worker that has their
	// cache when it has room.
	CacheDisk bool `toml:"cache_disk" json:"cache_disk,omitempty"`
}

// LoadController reads controller.toml.
func LoadController(path string) (*ControllerConfig, error) {
	c := &ControllerConfig{File: path}
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys %s", path, strings.Join(keys, ", "))
	}
	t := &c.Controller
	if t.ID == "" {
		t.ID, _ = os.Hostname()
	}
	t.Listen = cmp.Or(t.Listen, ":"+DefaultPort)
	t.PKIDir = cmp.Or(t.PKIDir, "/etc/fleetci/pki")
	t.StateDir = cmp.Or(t.StateDir, "/var/lib/fleetci")
	t.Placement = cmp.Or(t.Placement, fleet.Pack)
	if t.Placement != fleet.Pack && t.Placement != fleet.Spread {
		return nil, fmt.Errorf("%s: placement %q: pack or spread", path, t.Placement)
	}
	t.WorkerUpdate = cmp.Or(t.WorkerUpdate, UpdateOff)
	switch t.WorkerUpdate {
	case UpdateOff, UpdatePatch, UpdateMinor, UpdateLatest:
	default:
		if !semver.IsValid(t.WorkerUpdate) || t.WorkerUpdate != semver.Canonical(t.WorkerUpdate) {
			return nil, fmt.Errorf("%s: worker_update %q: off, patch, minor, latest or a version as v0.7.0", path, t.WorkerUpdate)
		}
	}
	t.UpdateParallel = cmp.Or(t.UpdateParallel, 1)
	t.CacheWait = cmp.Or(t.CacheWait, fleet.Duration(30*time.Second))
	if t.CacheWait < 0 {
		return nil, fmt.Errorf("%s: cache_wait can't be negative", path)
	}
	if len(c.Classes) == 0 {
		return nil, fmt.Errorf("%s: no [classes]", path)
	}
	for name, cl := range c.Classes {
		if cl.CPUs <= 0 || cl.Memory <= 0 {
			return nil, fmt.Errorf("%s: class %s needs cpus and memory", path, name)
		}
		if cl.Disk == 0 {
			cl.Disk = 30 * 1024
		}
		cl.Image = cmp.Or(cl.Image, DefaultImageURL)
		cl.Arch = cmp.Or(cl.Arch, "amd64")
		if cl.Overflow != "" && c.Classes[cl.Overflow] == nil {
			return nil, fmt.Errorf("%s: class %s: overflow %q is no class", path, name, cl.Overflow)
		}
	}
	if c.DefaultClass == "" {
		c.DefaultClass = slices.Sorted(maps.Keys(c.Classes))[0]
	}
	if c.Classes[c.DefaultClass] == nil {
		return nil, fmt.Errorf("%s: default_class %q is no class", path, c.DefaultClass)
	}
	if c.GitHub == nil && c.GitLab == nil {
		return nil, fmt.Errorf("%s: a [github] or [gitlab] table is needed", path)
	}
	return c, nil
}

// WorkerConfig is worker.toml: what only this machine knows, written by
// fleetci join.
type WorkerConfig struct {
	ID          string   `toml:"id"`
	Controllers []string `toml:"controllers"`
	PKIDir      string   `toml:"pki_dir"`
	// StateDir holds the VMs' disks and images.
	StateDir string `toml:"state_dir"`
	// EgressInterface is the VMs' way out; default the default route's.
	EgressInterface string `toml:"egress_interface"`
	// Labels match classes' on.
	Labels map[string]string `toml:"labels"`
	// Classes, when set, are the only classes this worker runs.
	Classes []string `toml:"classes"`
	// Host is the budget, as fleetci.toml's [host]; detected by default.
	Host *fleet.HostConfig `toml:"host"`
	// FleetingdPlugin is fleeting-plugin-fleetingd. Default on the PATH.
	FleetingdPlugin string `toml:"fleetingd_plugin"`
	// Update is how the worker updates itself; off by default.
	Update UpdateConfig `toml:"update,omitempty"`
	// CacheDisk sizes this worker's cache disks, for the classes with
	// cache_disk; they are kept under StateDir/cache.
	CacheDisk fleet.CacheDisk `toml:"cache_disk"`

	File string `toml:"-"`
}

// LoadWorker reads worker.toml.
func LoadWorker(path string) (*WorkerConfig, error) {
	c := &WorkerConfig{File: path}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		c.ID, _ = os.Hostname()
	}
	if len(c.Controllers) == 0 {
		return nil, fmt.Errorf("%s: no controllers", path)
	}
	c.PKIDir = cmp.Or(c.PKIDir, "/etc/fleetci/pki")
	c.StateDir = cmp.Or(c.StateDir, "/var/lib/fleetci")
	c.FleetingdPlugin = cmp.Or(c.FleetingdPlugin, "fleeting-plugin-fleetingd")
	return c, nil
}

// Save writes worker.toml.
func (c *WorkerConfig) Save() error {
	if c.File == "" {
		return errors.New("worker config has no file")
	}
	f, err := os.OpenFile(c.File+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "# Written by fleetci join on %s. The classes come from the controllers.\n", time.Now().UTC().Format(time.RFC3339))
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(c.File+".tmp", c.File)
}
