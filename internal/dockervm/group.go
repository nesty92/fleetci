// Package dockervm is a fleeting plugin whose instances are local
// Docker-in-Docker containers, for trying fleetci on one machine. It
// behaves like a cloud plugin where it matters to fleetci: every
// ConnectInfo carries a new key that the instance accepts for two minutes,
// the way EC2 Instance Connect does.
package dockervm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/nesty92/fleetci/internal/sshx"
)

const (
	labelGroup = "fleetci.group"
	vmUser     = "fleetci"
	keyTTL     = 2 * time.Minute
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

// InstanceGroup is configured from plugin_config.
type InstanceGroup struct {
	// Name keeps groups apart; it labels every container.
	Name    string `json:"name"`
	Image   string `json:"image"`
	Network string `json:"network"`
	CPUs    string `json:"cpus"`
	Memory  string `json:"memory"`
	// MaxSize caps the group. Default 10.
	MaxSize int `json:"max_size"`

	settings provider.Settings
	log      hclog.Logger

	mu      sync.Mutex
	deleted map[string]bool // removed by Decrease, not yet reported
}

func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	if g.Name == "" || g.Image == "" || g.Network == "" || g.CPUs == "" || g.Memory == "" {
		return provider.ProviderInfo{}, errors.New("name, image, network, cpus and memory are required")
	}
	if g.MaxSize == 0 {
		g.MaxSize = 10
	}
	g.log = log
	g.settings = settings
	g.deleted = map[string]bool{}

	if _, err := docker(ctx, "network", "inspect", g.Network); err != nil {
		return provider.ProviderInfo{}, err
	}
	return provider.ProviderInfo{ID: "dockervm/" + g.Name, MaxSize: g.MaxSize}, nil
}

func (g *InstanceGroup) Update(ctx context.Context, fn func(string, provider.State)) error {
	out, err := docker(ctx, "ps", "--all", "--filter", "label="+labelGroup+"="+g.Name, "--format", "{{.Names}} {{.State}}")
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for line := range strings.Lines(out) {
		id, state, _ := strings.Cut(strings.TrimSpace(line), " ")
		seen[id] = true
		switch state {
		case "running":
			fn(id, provider.StateRunning)
		case "created", "restarting":
			fn(id, provider.StateCreating)
		case "removing":
			fn(id, provider.StateDeleting)
		default:
			// Exited or dead: the instance is gone for good.
			_, _ = docker(ctx, "rm", "--force", "--volumes", id)
			fn(id, provider.StateDeleted)
		}
	}

	g.mu.Lock()
	for id := range g.deleted {
		if !seen[id] {
			fn(id, provider.StateDeleted)
			delete(g.deleted, id)
		}
	}
	g.mu.Unlock()
	return nil
}

func (g *InstanceGroup) Increase(ctx context.Context, n int) (int, error) {
	var errs []error
	done := 0
	for range n {
		id := "dockervm-" + g.Name + "-" + randomHex(4)
		_, err := docker(ctx, "run", "--detach", "--privileged",
			"--name", id,
			"--hostname", id,
			"--network", g.Network,
			"--cpus", g.CPUs,
			"--memory", g.Memory,
			"--label", labelGroup+"="+g.Name,
			g.Image)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		g.log.Info("instance created", "instance", id, "cpus", g.CPUs, "memory", g.Memory)
		done++
	}
	return done, errors.Join(errs...)
}

func (g *InstanceGroup) Decrease(ctx context.Context, ids []string) ([]string, error) {
	var done []string
	var errs []error
	for _, id := range ids {
		if err := g.own(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := docker(ctx, "rm", "--force", "--volumes", id); err != nil {
			errs = append(errs, err)
			continue
		}
		g.mu.Lock()
		g.deleted[id] = true
		g.mu.Unlock()
		g.log.Info("instance removed", "instance", id)
		done = append(done, id)
	}
	return done, errors.Join(errs...)
}

// ConnectInfo installs a new key on the instance that works for keyTTL, and
// returns it. Connections made with it stay up after it expires.
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	if err := g.own(ctx, id); err != nil {
		return provider.ConnectInfo{}, err
	}
	out, err := docker(ctx, "inspect", "--format",
		fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, g.Network), id)
	if err != nil {
		return provider.ConnectInfo{}, err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return provider.ConnectInfo{}, fmt.Errorf("instance %s has no address yet", id)
	}

	cc := g.settings.ConnectorConfig
	cc.Protocol = provider.ProtocolSSH
	if cc.Username == "" {
		cc.Username = vmUser
	}
	if cc.OS == "" {
		cc.OS = "linux"
	}
	info := provider.ConnectInfo{ConnectorConfig: cc, ID: id, InternalAddr: ip, ExternalAddr: ip}
	if cc.UseStaticCredentials {
		return info, nil
	}

	key, signer, err := sshx.NewKey()
	if err != nil {
		return provider.ConnectInfo{}, err
	}
	expires := time.Now().Add(keyTTL)
	line := fmt.Sprintf("expiry-time=%q %s", expires.UTC().Format("20060102150405")+"Z",
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	if err := g.addKey(ctx, id, line); err != nil {
		return provider.ConnectInfo{}, err
	}

	info.Key = key
	// Ask for a new key a little before this one stops working.
	refresh := expires.Add(-30 * time.Second)
	info.Expires = &refresh
	return info, nil
}

// addKey appends one authorized_keys line, dropping lines whose expiry has
// passed.
func (g *InstanceGroup) addKey(ctx context.Context, id, line string) error {
	script := `set -eu
f=/home/fleetci/.ssh/authorized_keys
now=$(date -u +%Y%m%d%H%M%S)
touch "$f"
awk -v now="$now" '{ if (match($0, /expiry-time="[0-9]+Z"/)) { t = substr($0, RSTART+13, 14); if (t < now) next } print }' "$f" > "$f.new"
cat >> "$f.new"
chown fleetci:fleetci "$f.new"
chmod 600 "$f.new"
mv "$f.new" "$f"`
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", id, "sh", "-c", script)
	cmd.Stdin = strings.NewReader(line + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing key on %s: %w: %s", id, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (g *InstanceGroup) Heartbeat(ctx context.Context, id string) error {
	out, err := docker(ctx, "inspect", "--format", "{{.State.Running}}", id)
	if err != nil || strings.TrimSpace(out) != "true" {
		return provider.ErrInstanceUnhealthy
	}
	return nil
}

func (g *InstanceGroup) Suspend(context.Context, []string) ([]string, error) { return nil, nil }
func (g *InstanceGroup) Resume(context.Context, []string) ([]string, error)  { return nil, nil }
func (g *InstanceGroup) Shutdown(context.Context) error                      { return nil }

// own refuses ids that are not this group's containers.
func (g *InstanceGroup) own(ctx context.Context, id string) error {
	out, err := docker(ctx, "inspect", "--format", `{{index .Config.Labels "`+labelGroup+`"}}`, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != g.Name {
		return fmt.Errorf("%s is not in group %s", id, g.Name)
	}
	return nil
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
