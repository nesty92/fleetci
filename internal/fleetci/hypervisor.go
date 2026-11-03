package fleetci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/nesting/hypervisor"

	"github.com/nesty92/fleetci/internal/dockervm"
	"github.com/nesty92/fleetci/internal/proxy"
)

// namePrefix starts every VM name. The runner config sets
// vm_isolation.image = "class-${FLEETCI_CLASS}", so an unset variable
// arrives as "class-".
const namePrefix = "class-"

// Hypervisor serves the nesting API: one fresh VM per job, of the class the
// job asked for.
type Hypervisor struct {
	cfg     Config
	backend *dockervm.Backend
	log     hclog.Logger

	mu  sync.Mutex
	vms map[string]*vmEntry
}

type vmEntry struct {
	name  string
	proxy *proxy.VM
}

var _ hypervisor.Hypervisor = (*Hypervisor)(nil)

func NewHypervisor(cfg Config, backend *dockervm.Backend, log hclog.Logger) *Hypervisor {
	return &Hypervisor{cfg: cfg, backend: backend, log: log, vms: map[string]*vmEntry{}}
}

// ClassFor maps a VM name from the runner to a class.
func (h *Hypervisor) ClassFor(name string) (string, dockervm.Size, error) {
	key, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return "", dockervm.Size{}, fmt.Errorf("vm name %q must start with %q", name, namePrefix)
	}
	if key == "" {
		key = h.cfg.DefaultClass
	}
	size, ok := h.cfg.Classes[key]
	if !ok {
		return "", dockervm.Size{}, fmt.Errorf("unknown class %q", key)
	}
	return key, size, nil
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
	class, size, err := h.ClassFor(name)
	if err != nil {
		return nil, err
	}
	vm, err := h.backend.Create(ctx, class, size)
	if err != nil {
		return nil, err
	}

	target := vm.Target
	pv := proxy.NewVM(vm.ID, func(context.Context) (proxy.Target, error) { return target, nil }, h.log)

	h.mu.Lock()
	h.vms[vm.ID] = &vmEntry{name: name, proxy: pv}
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

// Shutdown removes every VM. Each one served a single job, so none is worth
// keeping.
func (h *Hypervisor) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	ids := make([]string, 0, len(h.vms))
	for id := range h.vms {
		ids = append(ids, id)
	}
	h.mu.Unlock()

	var errs []error
	for _, id := range ids {
		errs = append(errs, h.Delete(ctx, id))
	}
	errs = append(errs, h.backend.DeleteAll(ctx))
	return errors.Join(errs...)
}
