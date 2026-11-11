package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/vmjob"
)

// runScript runs actions/runner with the just-in-time config read from
// stdin, which keeps it out of the SSH command line. The runner takes one
// job and exits. It is the version asked for (%[1]s), or with none the
// newest in the image's ~/actions-runner-<version>, or else %[3]s;
// one the image lacks is downloaded. A second line "cache" has each job
// wait at its start while fleetci gives it its cache disk (jobStartedHook).
const runScript = `set -eu
read -r jit
read -r hook || hook=
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
if [ "$hook" = cache ] && sudo -n true 2>/dev/null; then
  sudo mkdir -p /run/fleetci && sudo chown "$(id -u):$(id -g)" /run/fleetci
  cat > fleetci-job-started.sh <<'HOOK'
%[4]s
HOOK
  export ACTIONS_RUNNER_HOOK_JOB_STARTED="$dir/fleetci-job-started.sh"
fi
exec ./run.sh --jitconfig "$jit"
`

// jobStartedHook runs before the job's first step: it says which run the
// job is in, and waits while fleetci gives the job its cache disk
// (Dispatcher.attachCache). fleetci or the hook, whichever makes "decided"
// first, decides: the hook gives up after a minute only if fleetci has not
// begun, so fleetci never stops Docker under a job that has started.
const jobStartedHook = `d=/run/fleetci
printf '%s %s\n' "$GITHUB_REPOSITORY" "$GITHUB_RUN_ID" > "$d/job.new" && mv "$d/job.new" "$d/job"
i=0
until [ -e "$d/done" ]; do
  i=$((i+1))
  if [ $i -ge 600 ] && mkdir "$d/decided" 2>/dev/null; then
    echo "No cache disk: fleetci did not answer"
    exit 0
  fi
  if [ $i -ge 6000 ]; then
    echo "No cache disk: fleetci did not finish attaching it"
    exit 0
  fi
  sleep 0.1
done
cat "$d/done"
`

// SSHStart runs the runner on the VM over SSH and logs its output. An
// empty version is the newest in the image, or DefaultRunnerVersion.
// With cacheURL set, jobs use that cache server instead of GitHub's.
//
// The runner runs on the VM apart from fleetci's sessions (see vmjob):
// fleetci follows its output, and losing the connection, or fleetci
// stopping, leaves it running. A VM handed over to the next fleetci is
// followed by that one to the job's end.
func SSHStart(version, cacheURL string, log hclog.Logger) StartFunc {
	return sshStart(fmt.Sprintf(runScript, version, cacheURL, DefaultRunnerVersion, jobStartedHook), log)
}

// followTries is how many times in a row following the runner may fail
// on a VM that still answers before the runner counts as lost.
const followTries = 5

func sshStart(script string, log hclog.Logger) StartFunc {
	return func(ctx context.Context, vm VM, jit string, took func(job string)) error {
		// The machine's connection, shared with its reclaim watch: the job
		// opens sessions on it and leaves it open. A VM that dies mid-job
		// closes lost in seconds, not when TCP gives up.
		client, lost, err := vm.Conn.Client(ctx)
		if err != nil {
			return err
		}
		stdin := jit + "\n"
		if vm.Cache {
			stdin += "cache\n"
		}
		if err := vmjob.Start(ctx, client, vm.ID, script, []byte(stdin)); err != nil {
			return err
		}
		log := log.With("vm", vm.ID)
		line := func(l string) {
			l = strings.TrimSpace(l)
			if l == "" {
				return
			}
			log.Info("runner: " + l)
			if job, ok := runningJob(l); ok && took != nil {
				took(job)
			}
		}
		next, failed := 1, 0
		for {
			status, n, err := vmjob.Follow(ctx, client, vm.ID, next, line)
			switch {
			case err == nil && status == 0:
				return nil
			case err == nil:
				return fmt.Errorf("runner exited: status %d", status)
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, vmjob.ErrLost), errors.Is(err, vmjob.ErrNoJob):
				return fmt.Errorf("runner exited: %w", err)
			}
			if n > next {
				failed = 0
			}
			next = n
			if failed++; failed > followTries {
				return fmt.Errorf("lost the vm: following the runner failed %d times: %w", failed, err)
			}
			// The session went, and perhaps the connection: the runner goes
			// on on the VM. A connection that is gone closes lost.
			select {
			case <-lost:
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			c, l, derr := vm.Conn.Client(ctx)
			if derr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("lost the vm: %w (following the runner: %v)", derr, err)
			}
			client, lost = c, l
			log.Warn("following the runner again", "err", err)
		}
	}
}

// runningJob reads the job's name from the runner's line
// "2026-11-05 00:49:12Z: Running job: with-service".
func runningJob(line string) (string, bool) {
	_, job, ok := strings.Cut(line, "Running job: ")
	job = strings.TrimSpace(job)
	return job, ok && job != ""
}
