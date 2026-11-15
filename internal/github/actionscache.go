package github

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/crypto/ssh"
)

// actionsCacheBins are the actions cache (internal/actionscache, cmd/fleetci-actions-cache) for
// the VMs, gzipped, one per architecture: jobs restore and save through
// it, and their archives stay on the cache disk. go generate builds them;
// a fleetci built without them runs jobs without the actions cache.
//
//go:generate sh ../../hack/build-actions-cache.sh
//go:embed actionscachebin
var actionsCacheBins embed.FS

// actionsCachePath is where the actions cache goes on the VM, under the runner's home.
const actionsCachePath = ".fleetci/fleetci-actions-cache"

// actionsCachePort is where the actions cache listens on the VM.
const actionsCachePort = 31849

// githubResultsURL is GitHub's cache service, the L2 when no cache_url is set.
const githubResultsURL = "https://results-receiver.actions.githubusercontent.com/"

// errNoActionsCache is a fleetci built without the actions cache, or a VM of an architecture
// it has none for.
var errNoActionsCache = errors.New("no actions cache for this VM")

// actionsCacheFor is the gzipped actions cache for the architecture uname -m names.
func actionsCacheFor(machine string) ([]byte, error) {
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(machine)]
	if arch == "" {
		return nil, fmt.Errorf("%w: architecture %q", errNoActionsCache, strings.TrimSpace(machine))
	}
	b, err := fs.ReadFile(actionsCacheBins, "actionscachebin/fleetci-actions-cache-linux-"+arch+".gz")
	if err != nil {
		return nil, fmt.Errorf("%w: fleetci was built without it (go generate ./internal/github)", errNoActionsCache)
	}
	return b, nil
}

// installActionsCache copies the actions cache into the VM, to ~/actionsCachePath, before the runner
// starts; the VM's script runs it when it is there. A VM without it runs
// its jobs without the actions cache, as before a cache disk.
func installActionsCache(ctx context.Context, client *ssh.Client, log hclog.Logger) {
	run := func(cmd string, stdin []byte) ([]byte, error) {
		s, err := client.NewSession()
		if err != nil {
			return nil, err
		}
		defer s.Close()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = s.Close()
			case <-done:
			}
		}()
		if stdin != nil {
			s.Stdin = bytes.NewReader(stdin)
		}
		return s.CombinedOutput(cmd)
	}
	machine, err := run("uname -m", nil)
	if err != nil {
		log.Warn("cache disk: the actions cache isn't used", "err", err)
		return
	}
	bin, err := actionsCacheFor(string(machine))
	if err != nil {
		log.Warn("cache disk: the actions cache isn't used", "err", err)
		return
	}
	cmd := fmt.Sprintf(`set -e; f="$HOME/%[1]s"; mkdir -p "${f%%/*}"; gzip -dc > "$f.new"; chmod 755 "$f.new"; mv -f "$f.new" "$f"`, actionsCachePath)
	if out, err := run(cmd, bin); err != nil {
		log.Warn("cache disk: the actions cache isn't used", "err", err, "out", strings.TrimSpace(string(out)))
	}
}
