// Package dockervm is a nesting hypervisor whose "VMs" are local
// Docker-in-Docker containers. It exists to prove the fleetci flow on one
// machine: gitlab-runner asks for a VM named after a class, and the class
// decides the CPU and memory the job gets. A cloud backend replaces it later.
package dockervm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"time"

	"gitlab.com/gitlab-org/fleeting/nesting/hypervisor"
)

const (
	labelOwner = "fleetci.vm"
	labelClass = "fleetci.class"
	namePrefix = "class-"
)

// Class is one machine size.
type Class struct {
	CPUs   string `json:"cpus"`
	Memory string `json:"memory"`
}

// Config is the daemon's own configuration.
type Config struct {
	Image        string           `json:"image"`
	Network      string           `json:"network"`
	Password     string           `json:"-"`
	DefaultClass string           `json:"default_class"`
	Classes      map[string]Class `json:"classes"`
	ReadyTimeout time.Duration    `json:"-"`
}

type Hypervisor struct {
	cfg Config
	log *slog.Logger
}

var _ hypervisor.Hypervisor = (*Hypervisor)(nil)

func New(cfg Config, log *slog.Logger) (*Hypervisor, error) {
	if cfg.Image == "" || cfg.Network == "" || cfg.Password == "" {
		return nil, errors.New("image, network and password are required")
	}
	if _, ok := cfg.Classes[cfg.DefaultClass]; !ok {
		return nil, fmt.Errorf("default class %q is not defined", cfg.DefaultClass)
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}
	return &Hypervisor{cfg: cfg, log: log}, nil
}

// Init receives the runner's vm_isolation.nesting_config. The classes live in
// the daemon's own config, so the runner's settings are only logged.
func (h *Hypervisor) Init(ctx context.Context, config []byte) error {
	h.log.Info("init from runner", "nesting_config", string(config))
	return h.removeAll(ctx)
}

// ClassFor maps a VM name from gitlab-runner to a class. The runner expands
// "class-${FLEETCI_CLASS}", so an unset variable arrives as "class-".
func (h *Hypervisor) ClassFor(name string) (string, Class, error) {
	key, ok := strings.CutPrefix(name, namePrefix)
	if !ok {
		return "", Class{}, fmt.Errorf("vm name %q must start with %q", name, namePrefix)
	}
	if key == "" {
		key = h.cfg.DefaultClass
	}
	c, ok := h.cfg.Classes[key]
	if !ok {
		return "", Class{}, fmt.Errorf("unknown class %q", key)
	}
	return key, c, nil
}

func (h *Hypervisor) Create(ctx context.Context, name string) (hypervisor.VirtualMachine, error) {
	class, c, err := h.ClassFor(name)
	if err != nil {
		return nil, err
	}

	id := "fleetci-vm-" + randomHex(4)
	log := h.log.With("vm", id, "class", class)
	log.Info("creating vm", "cpus", c.CPUs, "memory", c.Memory)
	start := time.Now()

	_, err = docker(ctx, "run", "--detach", "--privileged",
		"--name", id,
		"--hostname", id,
		"--network", h.cfg.Network,
		"--cpus", c.CPUs,
		"--memory", c.Memory,
		"--label", labelOwner+"=1",
		"--label", labelClass+"="+class,
		"--env", "VM_PASSWORD="+h.cfg.Password,
		"--env", "VM_CLASS="+class,
		h.cfg.Image)
	if err != nil {
		return nil, err
	}

	addr, err := h.waitReady(ctx, id)
	if err != nil {
		_ = h.Delete(context.WithoutCancel(ctx), id)
		return nil, err
	}

	log.Info("vm ready", "addr", addr, "took", time.Since(start).Round(time.Millisecond))
	return hypervisor.VirtualMachineInfo{Id: id, Name: name, Addr: addr}, nil
}

// waitReady waits until the VM's SSH port answers and its Docker daemon runs.
func (h *Hypervisor) waitReady(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.ReadyTimeout)
	defer cancel()

	for {
		addr, err := h.addr(ctx, id)
		if err == nil {
			if err = dialOnce(ctx, addr); err == nil {
				_, err = docker(ctx, "exec", id, "docker", "info", "--format", "{{.ServerVersion}}")
			}
		}
		if err == nil {
			return addr, nil
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("vm %s not ready: %w (last error: %v)", id, ctx.Err(), err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (h *Hypervisor) addr(ctx context.Context, id string) (string, error) {
	out, err := docker(ctx, "inspect", "--format",
		fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, h.cfg.Network), id)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("vm %s has no address yet", id)
	}
	return net.JoinHostPort(ip, "22"), nil
}

func (h *Hypervisor) Delete(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, "fleetci-vm-") {
		return fmt.Errorf("refusing to delete %q: not a fleetci vm", id)
	}
	h.log.Info("deleting vm", "vm", id)
	_, err := docker(ctx, "rm", "--force", "--volumes", id)
	return err
}

func (h *Hypervisor) List(ctx context.Context) ([]hypervisor.VirtualMachine, error) {
	out, err := docker(ctx, "ps", "--all", "--filter", "label="+labelOwner+"=1", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var vms []hypervisor.VirtualMachine
	for line := range strings.Lines(out) {
		var row struct {
			Names  string
			Labels string
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		addr, _ := h.addr(ctx, row.Names)
		vms = append(vms, hypervisor.VirtualMachineInfo{Id: row.Names, Name: namePrefix + labelValue(row.Labels, labelClass), Addr: addr})
	}
	return vms, nil
}

// Shutdown removes every VM this daemon owns. Each VM serves one job, so
// none is worth keeping once the daemon stops.
func (h *Hypervisor) Shutdown(ctx context.Context) error {
	return h.removeAll(ctx)
}

func (h *Hypervisor) removeAll(ctx context.Context) error {
	vms, err := h.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, vm := range vms {
		errs = append(errs, h.Delete(ctx, vm.GetId()))
	}
	return errors.Join(errs...)
}

func docker(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func dialOnce(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func labelValue(labels, key string) string {
	for kv := range strings.SplitSeq(labels, ",") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
