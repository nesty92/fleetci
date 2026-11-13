#!/bin/sh
# One job of a merge-train car: fetches the repo at the car's commit, from
# train.env, and runs its CI, timing each step. The tools' caches are in
# .tcache, which the job's actions/cache or cache: keeps, as a project's
# own would.
#
#   job.sh go    fleetci: go vet and go test -race
#   job.sh web   vercel/chatbot: pnpm install, its checks and next build
#
# The last line of its log says what it did:
#   TRAIN <run> car=<n> job=<go|web> ref=<ref> total=<s> <step>=<s>... cache=<MB>
set -u
what=$1
. ./train.env
C=$PWD/.tcache
mkdir -p "$C"
now() { cut -d' ' -f1 /proc/uptime; }
since() { awk -v a="$(now)" -v b="$1" 'BEGIN { printf "%.1f", a - b }'; }
t0=$(now) steps="" failed=0
step() { # <label> <command>
  echo "::: $1"
  t=$(now)
  sh -c "$2" || failed=$((failed + 1))
  steps="$steps $1=$(since "$t")"
}

case $what in
go) url=https://github.com/nesty92/fleetci commit=$GO_COMMIT ;;
web) url=https://github.com/vercel/chatbot commit=$WEB_COMMIT ;;
esac
src=$PWD/train-src
t=$(now)
rm -rf "$src" && git init -q "$src" &&
  git -C "$src" fetch -q --depth 1 "$url" "$commit" &&
  git -C "$src" -c advice.detachedHead=false checkout -q FETCH_HEAD || exit 1
steps=" fetch=$(since "$t")"
cd "$src"

case $what in
go)
  export GOCACHE=$C/go/build GOMODCACHE=$C/go/mod GOFLAGS=-buildvcs=false
  step vet "go vet ./..."
  # This test waits for a killed process that nothing reaps in a job
  # container without an init: it times out there, cache or not.
  step test "go test -race -skip TestAJobKilledWithoutAStatusIsLost ./..."
  ;;
web)
  export COREPACK_HOME=$C/corepack COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1
  step install "corepack enable && pnpm install --frozen-lockfile --store-dir $C/pnpm-store"
  step lint "corepack enable && pnpm check"
  # The build needs its services' settings, not the services.
  step build "corepack enable && mkdir -p $C/next .next && ln -sfn $C/next .next/cache &&
    AUTH_SECRET=placeholder POSTGRES_URL=postgres://ci@localhost/ci REDIS_URL=redis://localhost BLOB_READ_WRITE_TOKEN=placeholder pnpm exec next build"
  ;;
esac

ref=${GITHUB_REF_NAME:-${CI_COMMIT_REF_NAME:-?}}
# The most memory the job's container took, page cache included: what its class needs.
peak=$(( $(cat /sys/fs/cgroup/memory.peak 2>/dev/null || echo 0) >> 20 ))
echo "TRAIN $RUN car=$CAR job=$what ref=$ref total=$(since "$t0")$steps failed=$failed cache=$(du -sm "$C" | cut -f1) peak=${peak}MB"
[ $failed = 0 ]
