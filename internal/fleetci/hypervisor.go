package fleetci

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/nesting/hypervisor"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
)

// namePrefix starts every VM name. The runner config sets
// vm_isolation.image = "class-${FLEETCI_CLASS}", so an unset variable
// arrives as "class-".
const namePrefix = "class-"

// Backend hands out one VM per job.
type Backend interface {
	Has(class string) bool
	Create(ctx context.Context, class string) (fleet.VM, error)
	Delete(ctx context.Context, id string) error
	Shutdown(ctx context.Context) error
}

// Hypervisor serves the nesting API: one fresh VM per job, of the class the
// job asked for.
type Hypervisor struct {
	defaultClass string
	backend      Backend
	log          hclog.Logger

	mu  sync.Mutex
	vms map[string]*vmEntry
}

type vmEntry struct {
	name  string
	proxy *proxy.VM
}

var _ hypervisor.Hypervisor = (*Hypervisor)(nil)

func NewHypervisor(defaultClass string, backend Backend, log hclog.Logger) *Hypervisor {
	return &Hypervisor{defaultClass: defaultClass, backend: backend, log: log, vms: map[string]*vmEntry{}}
}

// ClassFor maps a VM name from the runner to a class.
func (h *Hypervisor) ClassFor(name string) (string, error) {
	class, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return "", fmt.Errorf("vm name %q must start with %q", name, namePrefix)
	}
	if class == "" {
		class = h.defaultClass
	}
	if !h.backend.Has(class) {
		return "", fmt.Errorf("unknown class %q", class)
	}
	return class, nil
}

// Get finds a VM for the gateway.
func (h *Hypervisor) Get(id string) (*proxy.VM, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.vms[id]
	if !ok {
		return nil, false
	}
	return e.proxy, true
}

// Init receives the runner's vm_isolation.nesting_config. Classes live in
// plugin_config, so it is only logged.
func (h *Hypervisor) Init(_ context.Context, config []byte) error {
	h.log.Info("nesting init from runner", "nesting_config", string(config))
	return nil
}

func (h *Hypervisor) Create(ctx context.Context, name string) (hypervisor.VirtualMachine, error) {
	class, err := h.ClassFor(name)
	if err != nil {
		return nil, err
	}
	vm, err := h.backend.Create(ctx, class)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	h.vms[vm.ID] = &vmEntry{name: name, proxy: proxy.NewVM(vm.ID, vm.Connect, h.log)}
	h.mu.Unlock()

	// The address is the VM's id, which only the gateway understands. The
	// runner's connection to it never leaves this process.
	return hypervisor.VirtualMachineInfo{Id: vm.ID, Name: name, Addr: net.JoinHostPort(vm.ID, "22")}, nil
}

func (h *Hypervisor) Delete(ctx context.Context, id string) error {
	h.mu.Lock()
	e, ok := h.vms[id]
	delete(h.vms, id)
	h.mu.Unlock()

	if ok {
		_ = e.proxy.Close()
	}
	return h.backend.Delete(ctx, id)
}

func (h *Hypervisor) List(context.Context) ([]hypervisor.VirtualMachine, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	vms := make([]hypervisor.VirtualMachine, 0, len(h.vms))
	for id, e := range h.vms {
		vms = append(vms, hypervisor.VirtualMachineInfo{Id: id, Name: e.name, Addr: net.JoinHostPort(id, "22")})
	}
	return vms, nil
}

// Shutdown releases every VM; the backend then removes them all.
func (h *Hypervisor) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	ids := make([]string, 0, len(h.vms))
	for id := range h.vms {
		ids = append(ids, id)
	}
	h.mu.Unlock()

	for _, id := range ids {
		_ = h.Delete(ctx, id)
	}
	return h.backend.Shutdown(ctx)
}
