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

// runScript runs actions/runner with the just-in-time config read from
// stdin, which keeps it out of the SSH command line. The runner takes one
// job and exits. It is the version asked for (%[1]s), or with none the
// newest in the image's ~/actions-runner-<version>, or else %[3]s;
// one the image lacks is downloaded.
const runScript = `set -eu
read -r jit
case $(uname -m) in
  x86_64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
version='%[1]s'
if [ -z "$version" ]; then
  version=$(for d in "$HOME"/actions-runner-*; do
    if [ -x "$d/run.sh" ]; then echo "${d##*/actions-runner-}"; fi
  done | sort -V | tail -n 1)
  [ -n "$version" ] || version='%[3]s'
fi
dir=$HOME/actions-runner-$version
if [ ! -x "$dir/run.sh" ]; then
  mkdir -p "$dir"
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 "https://github.com/actions/runner/releases/download/v$version/actions-runner-linux-$arch-$version.tar.gz" | tar -xz -C "$dir"
fi
cd "$dir"
# Paths workflows written for GitHub's runners name: the work directory as
# /home/runner/work and the tool cache at /opt/hostedtoolcache.
if sudo -n true 2>/dev/null; then
  if [ ! -e /home/runner ]; then
    sudo mkdir -p /home/runner && sudo chown "$(id -u):$(id -g)" /home/runner &&
    mkdir -p _work && ln -s "$dir/_work" /home/runner/work || true
  fi
  sudo mkdir -p /opt/hostedtoolcache && sudo chown "$(id -u):$(id -g)" /opt/hostedtoolcache || true
fi
if [ -w /opt/hostedtoolcache ]; then
  export RUNNER_TOOL_CACHE=/opt/hostedtoolcache AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
fi
if [ -r /etc/os-release ]; then . /etc/os-release; fi
if [ "${ID:-}" = ubuntu ]; then export ImageOS="ubuntu${VERSION_ID%%%%.*}"; fi
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

// SSHStart runs the runner on the VM over SSH and logs its output. An
// empty version is the newest in the image, or DefaultRunnerVersion.
// With cacheURL set, jobs use that cache server instead of GitHub's.
func SSHStart(version, cacheURL string, log hclog.Logger) StartFunc {
	return func(ctx context.Context, vm VM, jit string, took func(job string)) error {
		// The machine's connection, shared with its reclaim watch: the job
		// opens a session on it and leaves it open. A VM that dies mid-job
		// closes lost in seconds, not when TCP gives up.
		client, lost, err := vm.Conn.Client(ctx)
		if err != nil {
			return err
		}
		s, err := client.NewSession()
		if err != nil {
			return err
		}
		defer s.Close()
		stop := context.AfterFunc(ctx, func() { _ = s.Close() })
		defer stop()

		out := logWriter(log.With("vm", vm.ID), took)
		defer out.Close()
		s.Stdin = strings.NewReader(jit + "\n")
		s.Stdout = out
		s.Stderr = out
		if err := s.Run(fmt.Sprintf(runScript, version, cacheURL, DefaultRunnerVersion)); err != nil {
			select {
			case <-lost:
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
