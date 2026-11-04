package github

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/fleet"
	"github.com/nesty92/fleetci/internal/proxy"
)

// runScript installs actions/runner on first use and runs it with the
// just-in-time config read from stdin, which keeps it out of the SSH
// command line. The runner takes one job and exits.
const runScript = `set -eu
read -r jit
case $(uname -m) in
  x86_64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
dir=$HOME/actions-runner
if [ ! -x "$dir/run.sh" ]; then
  mkdir -p "$dir"
  curl -fsSL "https://github.com/actions/runner/releases/download/v%[1]s/actions-runner-linux-$arch-%[1]s.tar.gz" | tar -xz -C "$dir"
fi
cd "$dir"
if ! ls /usr/lib*/libicuuc.so.* /usr/lib/*/libicuuc.so.* 2>/dev/null | grep -q .; then
  if command -v dnf >/dev/null 2>&1; then sudo dnf install -y -q libicu >/dev/null
  else sudo ./bin/installdependencies.sh >/dev/null
  fi
fi
exec ./run.sh --jitconfig "$jit"
`

// SSHStart runs the runner on the VM over SSH and logs its output.
func SSHStart(version string, log hclog.Logger) StartFunc {
	return func(ctx context.Context, vm fleet.VM, jit string) error {
		target, err := vm.Connect(ctx)
		if err != nil {
			return err
		}
		client, err := proxy.Dial(ctx, target)
		if err != nil {
			return err
		}
		defer client.Close()
		stop := context.AfterFunc(ctx, func() { _ = client.Close() })
		defer stop()

		s, err := client.NewSession()
		if err != nil {
			return err
		}
		defer s.Close()

		out := logWriter(log.With("vm", vm.ID))
		defer out.Close()
		s.Stdin = strings.NewReader(jit + "\n")
		s.Stdout = out
		s.Stderr = out
		if err := s.Run(fmt.Sprintf(runScript, version)); err != nil {
			return fmt.Errorf("runner exited: %w", err)
		}
		return nil
	}
}

// logWriter logs each line the runner prints.
func logWriter(log hclog.Logger) io.WriteCloser {
	r, w := io.Pipe()
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				log.Info("runner: " + line)
			}
		}
	}()
	return w
}
