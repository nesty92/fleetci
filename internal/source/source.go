// Package source is the boundary between fleetci's core and where jobs come
// from, drawn as fleeting draws it between taskscaler and a cloud: the core
// owns the classes, the host budget, the queue and the VMs, and offers them
// as Capacity; a job source (GitLab, GitHub) takes jobs from its forge and
// runs them on Capacity's VMs. Sources can be swapped without touching the
// core.
//
// Everything that crosses the boundary is data or an id, except Conn: a
// VM's one SSH connection, which stays in the core. So a source can later
// run as a plugin binary of its own over hashicorp/go-plugin, as fleeting's
// plugins do, with Capacity reaching it through go-plugin's broker and each
// session or forward a source opens on Conn crossing it as a stream.
package source

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
)

// ErrNoRoom is Reserve's answer when no VM of the class fits now.
var ErrNoRoom = fleet.ErrNoRoom

// Class is one of the core's classes, as a source sees it.
type Class struct {
	Name string `json:"name"`
	// Slots is how many of the class's jobs run at once.
	Slots int `json:"slots"`
	// Overflow is the class that takes the class's jobs when it is full.
	Overflow string `json:"overflow,omitempty"`
	// Arch is the GOARCH of the class's VMs, "" if not known.
	Arch string `json:"arch,omitempty"`
}

// VM is a job's VM.
type VM struct {
	ID    string `json:"id"`
	Class string `json:"class"`
	// Instance is the fleeting plugin's id for the machine behind the VM.
	Instance string `json:"instance"`
}

// Reservation is room for one VM, held until Acquire or Release.
type Reservation struct {
	ID string `json:"id"`
	// Class is the class asked for; the VM may be its overflow's.
	Class string `json:"class"`
}

// Capacity is what the core gives a source.
type Capacity interface {
	// Classes are the core's classes and the default class.
	Classes(ctx context.Context) ([]Class, string, error)
	// Reserve holds room for one VM of class without waiting, or fails
	// with ErrNoRoom.
	Reserve(ctx context.Context, class string) (Reservation, error)
	// Acquire turns a reservation into its VM. Once called, the VM is
	// Deleted, not Released, whether or not Acquire succeeds.
	Acquire(ctx context.Context, r Reservation) (VM, error)
	// Release gives an unused reservation back.
	Release(ctx context.Context, r Reservation) error
	// Create waits for room for a VM of class and takes it.
	Create(ctx context.Context, class string) (VM, error)
	// Conn is the SSH connection to a VM's machine, which every job on it
	// and fleetci itself share. Callers open sessions on it and never close
	// it.
	Conn(vm string) (*proxy.Conn, error)
	// Delete removes a VM.
	Delete(ctx context.Context, vm string) error
}

// Source takes jobs from a forge and runs them on Capacity's VMs.
type Source interface {
	// Init reads the source's table of fleetci.toml, as JSON.
	Init(ctx context.Context, config []byte, capacity Capacity, log hclog.Logger) error
	// Run takes and runs jobs until ctx ends, then stops its jobs.
	Run(ctx context.Context) error
}

// Configure marshals a source's table for Init.
func Configure(table any) ([]byte, error) {
	b, err := json.Marshal(table)
	if err != nil {
		return nil, fmt.Errorf("source config: %w", err)
	}
	return b, nil
}

// HasClass reports whether classes holds name.
func HasClass(classes []Class, name string) bool {
	for _, c := range classes {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Names are the classes' names.
func Names(classes []Class) []string {
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = c.Name
	}
	return out
}
