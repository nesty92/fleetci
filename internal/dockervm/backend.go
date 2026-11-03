// Package dockervm is a VM backend whose "VMs" are local Docker-in-Docker
// containers, for trying fleetci on one machine. Each VM gets its own SSH
// key, made when the VM is, and accepts nothing else.
package dockervm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/proxy"
	"github.com/nesty92/fleetci/internal/sshx"
)

const (
	labelOwner = "fleetci.vm"
	labelClass = "fleetci.class"
	idPrefix   = "fleetci-vm-"
	vmUser     = "fleetci"
)

// Settings configure the backend.
type Settings struct {
	Image   string `json:"image"`
	Network string `json:"network"`
}

// Size is what one class gets.
type Size struct {
	CPUs   string `json:"cpus"`
	Memory string `json:"memory"`
}

// VM is a running VM and how to reach it.
type VM struct {
	ID     string
	Target proxy.Target
}

type Backend struct {
	s   Settings
	log hclog.Logger
}

func New(s Settings, log hclog.Logger) (*Backend, error) {
	if s.Image == "" || s.Network == "" {
		return nil, errors.New("docker.image and docker.network are required")
	}
	return &Backend{s: s, log: log}, nil
}

func (b *Backend) Create(ctx context.Context, class string, size Size) (VM, error) {
	id := idPrefix + randomHex(4)
	log := b.log.With("vm", id, "class", class)
	start := time.Now()

	key, signer, err := sshx.NewKey()
	if err != nil {
		return VM{}, err
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))

	log.Info("creating vm", "cpus", size.CPUs, "memory", size.Memory)
	_, err = docker(ctx, "run", "--detach", "--privileged",
		"--name", id,
		"--hostname", id,
		"--network", b.s.Network,
		"--cpus", size.CPUs,
		"--memory", size.Memory,
		"--label", labelOwner+"=1",
		"--label", labelClass+"="+class,
		"--env", "VM_AUTHORIZED_KEY="+pub,
		b.s.Image)
	if err != nil {
		return VM{}, err
	}

	vm, err := b.waitReady(ctx, id, key)
	if err != nil {
		_ = b.Delete(context.WithoutCancel(ctx), id)
		return VM{}, err
	}
	log.Info("vm ready", "addr", vm.Target.Info.InternalAddr, "took", time.Since(start).Round(time.Millisecond).String())
	return vm, nil
}

// waitReady waits until the VM's SSH port answers and its Docker daemon
// runs, and reads its host key so the proxy can pin it.
func (b *Backend) waitReady(ctx context.Context, id string, key []byte) (VM, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for {
		vm, err := b.probe(ctx, id, key)
		if err == nil {
			return vm, nil
		}
		select {
		case <-ctx.Done():
			return VM{}, fmt.Errorf("vm %s not ready: %w (last error: %v)", id, ctx.Err(), err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (b *Backend) probe(ctx context.Context, id string, key []byte) (VM, error) {
	out, err := docker(ctx, "inspect", "--format",
		fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, b.s.Network), id)
	if err != nil {
		return VM{}, err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return VM{}, errors.New("no address yet")
	}
	addr := net.JoinHostPort(ip, "22")

	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return VM{}, err
	}
	_ = conn.Close()

	if _, err := docker(ctx, "exec", id, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return VM{}, err
	}

	hk, err := docker(ctx, "exec", id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return VM{}, err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hk))
	if err != nil {
		return VM{}, fmt.Errorf("parsing host key: %w", err)
	}

	return VM{ID: id, Target: proxy.Target{
		Info: provider.ConnectInfo{
			ConnectorConfig: provider.ConnectorConfig{
				OS:       "linux",
				Protocol: provider.ProtocolSSH,
				Username: vmUser,
				Key:      key,
				Timeout:  time.Minute,
			},
			ID:           id,
			InternalAddr: addr,
		},
		HostKey: hostKey,
	}}, nil
}

func (b *Backend) Delete(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, idPrefix) {
		return fmt.Errorf("refusing to delete %q: not a fleetci vm", id)
	}
	b.log.Info("deleting vm", "vm", id)
	_, err := docker(ctx, "rm", "--force", "--volumes", id)
	return err
}

// DeleteAll removes every VM this backend made, including ones left behind
// by an earlier run.
func (b *Backend) DeleteAll(ctx context.Context) error {
	out, err := docker(ctx, "ps", "--all", "--quiet", "--filter", "label="+labelOwner+"=1", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range strings.Fields(out) {
		errs = append(errs, b.Delete(ctx, id))
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

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
