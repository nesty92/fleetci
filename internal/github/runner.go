package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
# actions/cache compresses with zstd only where zstd is: a job's container,
# often an image without it, falls back to gzip, one core, several times
# slower. The VM's zstd goes where the runner mounts into job containers
# (externals, at /__e), with its libraries and the loader, so that it runs
# in any of them, glibc or musl; the job-started hook puts it on the PATH.
zd=externals/fleetci-zstd
rm -rf "$zd"
if zb=$(command -v zstd 2>/dev/null) && [ -d externals ] && command -v ldd >/dev/null 2>&1; then
  ld=$(ldd "$zb" 2>/dev/null | awk '$1 ~ /^\/.*ld-linux/ { print $1 }')
  if [ -n "$ld" ] && mkdir -p "$zd/lib" "$zd/bin" && cp -L "$zb" "$zd/zstd" && cp -L "$ld" "$zd/ld.so" &&
    ldd "$zb" | awk '$3 ~ /^\// { print $3 }' | while read -r l; do cp -L "$l" "$zd/lib/"; done; then
    for w in zstd: zstdmt:-T0 unzstd:-d; do
      printf '#!/bin/sh\nexec /__e/fleetci-zstd/ld.so --library-path /__e/fleetci-zstd/lib /__e/fleetci-zstd/zstd %%s "$@"\n' "${w#*:}" > "$zd/bin/${w%%%%:*}"
      chmod 755 "$zd/bin/${w%%%%:*}"
    done
  else
    rm -rf "$zd"
  fi
fi
if [ -r /etc/os-release ]; then . /etc/os-release; fi
if [ "${ID:-}" = ubuntu ]; then export ImageOS="ubuntu${VERSION_ID%%%%.*}"; fi
cache='%[2]s'
# With a cache disk, actions/cache goes through the L1 (l1cache.py), which
# keeps the archives on the disk, in front of the cache server it would
# have used. Without python3, or if it doesn't start, jobs go straight on.
l1=
if [ "$hook" = cache ] && command -v python3 >/dev/null 2>&1; then
  cat > fleetci-l1cache.py <<'L1PY'
%[5]s
L1PY
  mkdir -p _diag
  nohup setsid python3 -I fleetci-l1cache.py %[6]d "${cache:-%[7]s}" /cache/actions %[8]s>> _diag/fleetci-l1cache.log 2>&1 < /dev/null &
  # The address the VM's containers reach it at too: Docker's bridge. A
  # firewall that drops what comes in lets them through to it, and only it.
  at=$(ip -4 -o addr show docker0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n 1)
  if command -v iptables >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    for i in docker0 br-+; do
      sudo -n iptables -I INPUT -i "$i" -p tcp --dport %[6]d -j ACCEPT 2>/dev/null || true
    done
  fi
  i=0
  while [ $i -lt 50 ]; do
    if curl -fsS -o /dev/null -m 1 "http://127.0.0.1:%[6]d/fleetci/ping" 2>/dev/null; then
      l1="http://${at:-127.0.0.1}:%[6]d/"
      break
    fi
    i=$((i+1))
    sleep 0.1
  done
fi
if [ -n "$l1$cache" ]; then
  # The runner sets ACTIONS_RESULTS_URL for each job from GitHub's answer.
  # Renaming the variable it writes (same length, UTF-16) keeps ours. A
  # runner that updates itself before its job brings an unpatched one: it
  # is patched too, within a second, before the job's message comes.
  patch() {
    if LC_ALL=C grep -qaP 'A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00L\x00' bin/Runner.Worker.dll 2>/dev/null; then
      sed -i 's/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00L\x00/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00O\x00R\x00L\x00/g' bin/Runner.Worker.dll
    fi
  }
  patch
  (while sleep 1; do patch; done) < /dev/null > /dev/null 2>&1 &
  export ACTIONS_RESULTS_URL="${l1:-$cache}" FLEETCI_RESULTS_URL="${l1:-$cache}"
fi
if [ -n "$l1" ]; then
  # The job's log ends with what its cache steps got.
  echo 'curl -fsS -m 3 http://127.0.0.1:%[6]d/fleetci/stats || true' > fleetci-job-completed.sh
  export ACTIONS_RUNNER_HOOK_JOB_COMPLETED="$dir/fleetci-job-completed.sh"
fi
if ! ls /usr/lib*/libicuuc.so.* /usr/lib/*/libicuuc.so.* 2>/dev/null | grep -q .; then
  if command -v dnf >/dev/null 2>&1; then sudo dnf install -y -q libicu >/dev/null
  else sudo ./bin/installdependencies.sh >/dev/null
  fi
fi
: > fleetci-job-started.sh
if [ -d "$zd" ]; then
  # A path that is only there in a job's container: a job on the VM itself
  # has the VM's zstd.
  echo 'if [ -n "${GITHUB_PATH:-}" ]; then echo /__e/fleetci-zstd/bin >> "$GITHUB_PATH"; fi' >> fleetci-job-started.sh
fi
if [ "$hook" = cache ] && sudo -n true 2>/dev/null; then
  sudo mkdir -p /run/fleetci && sudo chown "$(id -u):$(id -g)" /run/fleetci
  cat >> fleetci-job-started.sh <<'HOOK'
%[4]s
HOOK
fi
if [ -s fleetci-job-started.sh ]; then
  export ACTIONS_RUNNER_HOOK_JOB_STARTED="$dir/fleetci-job-started.sh"
fi
exec ./run.sh --jitconfig "$jit"
`

// jobStartedHook runs before the job's first step: it says which run the
// job is in, and its ref's type and protection as GitHub gave them, and waits while fleetci gives the job its cache disk
// (Dispatcher.attachCache). fleetci or the hook, whichever makes "decided"
// first, decides: the hook gives up after a minute only if fleetci has not
// begun, so fleetci never stops Docker under a job that has started.
const jobStartedHook = `d=/run/fleetci
printf '%s %s %s %s\n' "$GITHUB_REPOSITORY" "$GITHUB_RUN_ID" "${GITHUB_REF_TYPE:--}" "${GITHUB_REF_PROTECTED:--}" > "$d/job.new" && mv "$d/job.new" "$d/job"
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
# A job's container doesn't get the runner's environment, only the job's:
# the job's, through GITHUB_ENV, has the cache server too.
if [ -n "${FLEETCI_RESULTS_URL:-}" ] && [ -n "${GITHUB_ENV:-}" ]; then
  echo "ACTIONS_RESULTS_URL=$FLEETCI_RESULTS_URL" >> "$GITHUB_ENV"
fi
# An unpatched runner sends actions/cache to GitHub's cache and says nothing.
if [ -n "${FLEETCI_RESULTS_URL:-}" ] &&
  LC_ALL=C grep -qaP 'A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00L\x00' "$(dirname "$0")/bin/Runner.Worker.dll" 2>/dev/null; then
  echo "::warning::fleetci: this runner isn't patched, so actions/cache goes to GitHub's cache, not $FLEETCI_RESULTS_URL"
fi
# The L1 (l1cache.py) runs as this user and keeps its archives on the disk.
if mountpoint -q /cache 2>/dev/null; then
  sudo -n mkdir -p /cache/actions && sudo -n chown "$(id -u):$(id -g)" /cache/actions
fi
cat "$d/done"
`

// SSHStart runs the runner on the VM over SSH and logs its output. An
// empty version is the newest in the image, or DefaultRunnerVersion.
// The cache store says where actions/cache keeps its caches, besides the
// job's cache disk.
//
// The runner runs on the VM apart from fleetci's sessions (see vmjob):
// fleetci follows its output, and losing the connection, or fleetci
// stopping, leaves it running. A VM handed over to the next fleetci is
// followed by that one to the job's end.
func SSHStart(version string, cs CacheStore, log hclog.Logger) StartFunc {
	var node http.Handler
	if cs.bucket != nil {
		node = cs.bucket.Handler()
	}
	return startWith(vmScript(version, cs), node, log)
}

// vmScript is runScript for the runner version and cache store given.
func vmScript(version string, cs CacheStore) string {
	return fmt.Sprintf(runScript, version, cs.URL, DefaultRunnerVersion, jobStartedHook, l1CacheScript, l1CachePort, githubResultsURL, cs.l1Args())
}

// followTries is how many times in a row following the runner may fail
// on a VM that still answers before the runner counts as lost.
const followTries = 5

func sshStart(script string, log hclog.Logger) StartFunc {
	return startWith(script, nil, log)
}

// startWith is sshStart with node, the node's cache service for a VM
// with a cache disk, served on the VM through the SSH connection; the
// job's saves are uploaded through it after the runner exits.
func startWith(script string, node http.Handler, log hclog.Logger) StartFunc {
	return func(ctx context.Context, vm VM, jit string, took func(job string)) error {
		log := log.With("vm", vm.ID)
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
		if !vm.Cache {
			node = nil
		}
		stop := func() {}
		if node != nil {
			stop = serveNode(client, node, log)
		}
		defer func() { stop() }()
		if err := vmjob.Start(ctx, client, vm.ID, script, []byte(stdin)); err != nil {
			return err
		}
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
			if err == nil && node != nil {
				flushSaves(ctx, client, log)
			}
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
			if c != client && node != nil {
				stop()
				stop = serveNode(c, node, log)
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
