package github

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

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
dir=$HOME/actions-runner-%[1]s
if [ ! -x "$dir/run.sh" ]; then
  mkdir -p "$dir"
  curl -fsSL "https://github.com/actions/runner/releases/download/v%[1]s/actions-runner-linux-$arch-%[1]s.tar.gz" | tar -xz -C "$dir"
fi
cd "$dir"
cache='%[2]s'
if [ -n "$cache" ]; then
  # The runner sets ACTIONS_RESULTS_URL for each job from GitHub's answer.
  # Renaming the variable it writes (same length, UTF-16) keeps ours.
  sed -i 's/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00L\x00/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00O\x00R\x00L\x00/g' bin/Runner.Worker.dll
  export ACTIONS_RESULTS_URL="$cache"
fi
if ! ls /usr/lib*/libicuuc.so.* /usr/lib/*/libicuuc.so.* 2>/dev/null | grep -q .; then
  if command -v dnf >/dev/null 2>&1; then sudo dnf install -y -q libicu >/dev/null
  else sudo ./bin/installdependencies.sh >/dev/null
  fi
fi
exec ./run.sh --jitconfig "$jit"
`

// SSHStart runs the runner on the VM over SSH and logs its output.
// With cacheURL set, jobs use that cache server instead of GitHub's.
func SSHStart(version, cacheURL string, log hclog.Logger) StartFunc {
	return func(ctx context.Context, vm VM, jit string, took func(job string)) error {
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
		// A VM that dies mid-job ends the session in seconds, not when TCP
		// gives up.
		alive := proxy.KeepAlive(client, proxy.KeepAliveInterval, proxy.KeepAliveTimeout)

		s, err := client.NewSession()
		if err != nil {
			return err
		}
		defer s.Close()

		out := logWriter(log.With("vm", vm.ID), took)
		defer out.Close()
		s.Stdin = strings.NewReader(jit + "\n")
		s.Stdout = out
		s.Stderr = out
		if err := s.Run(fmt.Sprintf(runScript, version, cacheURL)); err != nil {
			select {
			case <-alive:
				if ctx.Err() == nil {
					return fmt.Errorf("lost the vm: no answer for %s: %w", proxy.KeepAliveTimeout, err)
				}
			case <-time.After(time.Second):
			}
			return fmt.Errorf("runner exited: %w", err)
		}
		return nil
	}
}

// logWriter logs each line the runner prints, and calls took with the job
// named in its "Running job: <name>" line.
func logWriter(log hclog.Logger, took func(job string)) io.WriteCloser {
	r, w := io.Pipe()
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			log.Info("runner: " + line)
			if job, ok := runningJob(line); ok && took != nil {
				took(job)
			}
		}
	}()
	return w
}

// runningJob reads the job's name from the runner's line
// "2026-11-05 00:49:12Z: Running job: with-service".
func runningJob(line string) (string, bool) {
	_, job, ok := strings.Cut(line, "Running job: ")
	job = strings.TrimSpace(job)
	return job, ok && job != ""
}
