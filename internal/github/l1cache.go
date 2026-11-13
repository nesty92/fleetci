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

// l1Bins are the actions/cache L1 (internal/l1cache, cmd/fleetci-l1) for
// the VMs, gzipped, one per architecture: jobs restore and save through
// it, and their archives stay on the cache disk. go generate builds them;
// a fleetci built without them runs jobs without the L1.
//
//go:generate sh ../../hack/build-l1.sh
//go:embed l1bin
var l1Bins embed.FS

// l1Path is where the L1 goes on the VM, under the runner's home.
const l1Path = ".fleetci/fleetci-l1"

// l1CachePort is where the L1 listens on the VM.
const l1CachePort = 31849

// githubResultsURL is GitHub's cache service, the L2 when no cache_url is set.
const githubResultsURL = "https://results-receiver.actions.githubusercontent.com/"

// errNoL1 is a fleetci built without the L1, or a VM of an architecture
// it has none for.
var errNoL1 = errors.New("no L1 for this VM")

// l1For is the gzipped L1 for the architecture uname -m names.
func l1For(machine string) ([]byte, error) {
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(machine)]
	if arch == "" {
		return nil, fmt.Errorf("%w: architecture %q", errNoL1, strings.TrimSpace(machine))
	}
	b, err := fs.ReadFile(l1Bins, "l1bin/fleetci-l1-linux-"+arch+".gz")
	if err != nil {
		return nil, fmt.Errorf("%w: fleetci was built without it (go generate ./internal/github)", errNoL1)
	}
	return b, nil
}

// installL1 copies the L1 into the VM, to ~/l1Path, before the runner
// starts; the VM's script runs it when it is there. A VM without it runs
// its jobs without the L1, as before a cache disk.
func installL1(ctx context.Context, client *ssh.Client, log hclog.Logger) {
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
		log.Warn("cache disk: the actions/cache L1 isn't used", "err", err)
		return
	}
	bin, err := l1For(string(machine))
	if err != nil {
		log.Warn("cache disk: the actions/cache L1 isn't used", "err", err)
		return
	}
	cmd := fmt.Sprintf(`set -e; f="$HOME/%[1]s"; mkdir -p "${f%%/*}"; gzip -dc > "$f.new"; chmod 755 "$f.new"; mv -f "$f.new" "$f"`, l1Path)
	if out, err := run(cmd, bin); err != nil {
		log.Warn("cache disk: the actions/cache L1 isn't used", "err", err, "out", strings.TrimSpace(string(out)))
	}
}
