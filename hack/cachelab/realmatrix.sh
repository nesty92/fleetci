#!/bin/sh
# Runs on the host: the language matrix (matrix.txt) as real CI jobs on
# fleetci, in the cachelab's e2e fleetci, one job at a time; two copies,
# each given its own names, run two at once. For each language, its
# oldest commit with no cache disk, every commit with the disk (the first
# fills it), then its newest with no disk again.
#
#   realmatrix.sh github|gitlab [name...]
#
# GitHub: WireZone/fleetci-playground, branch matrix/<name>, workflow
# matrix.yml; fleetci-cache has the disk, fleetci-plain not. GitLab:
# Nesty92/fleetci-playground, branch matrix, a pipeline per job from a
# push with options; cache: keyed matrix-<name>, or FLEETCI_CACHE=off.
# The jobs post to the sink (sink.py); each job's line goes to
# $OUT/<forge>.log. A job that doesn't start in time stops the matrix.
set -eu
forge=$1; shift
here=$(cd "$(dirname "$0")" && pwd)
DATA=${DATA:-$HOME/.local/share/fleetci-cachelab}
OUT=$DATA/realmatrix
SINK=$OUT/sink.log
FLEETCI_LOG=$DATA/e2e/fleetci.log
GL=${GL:-$OUT/gitlab-playground} # a clone of the GitLab playground, on branch matrix
names=${*:-$(grep -v '^#' "$here/matrix.txt" | awk 'NF {print $1}')}

wait_for() { # <what> <seconds> <command...>: until the command succeeds
  what=$1 secs=$2; shift 2
  end=$(($(date +%s) + secs))
  until "$@"; do
    [ "$(date +%s)" -lt $end ] || { echo "$(date +%T) $what: not within ${secs}s" >&2; return 1; }
    sleep 5
  done
}

job() { # <name> <url> <commit> <with disk: yes|no> <tag>
  name=$1 url=$2 commit=$3 disk=$4 tag=$5
  id="$forge-$name-$tag-$(date +%s)"
  case $forge in
  github)
    runner=fleetci-plain; [ $disk = no ] || runner=fleetci-cache
    gh workflow run matrix.yml -R WireZone/fleetci-playground --ref "matrix/$name" \
      -f id="$id" -f lang="$name" -f url="$url" -f commit="$commit" -f runner="$runner" >/dev/null
    ;;
  gitlab)
    image=$(sed -n 's/^IMAGE=\([^ ]*\).*/\1/p' "$here/realmatrix/ci/recipes/$name.sh")
    off=on; [ $disk = yes ] || off=off
    git -C "$GL" commit -q --allow-empty -m "matrix $id"
    git -C "$GL" push -q origin HEAD:matrix \
      -o ci.variable="MATRIX_ID=$id" -o ci.variable="MATRIX_LANG=$name" \
      -o ci.variable="MATRIX_URL=$url" -o ci.variable="MATRIX_COMMIT=$commit" \
      -o ci.variable="MATRIX_IMAGE=$image" -o ci.variable="FLEETCI_CACHE=$off"
    ;;
  esac
  t=$(date +%s)
  # Started: the job said so. The first may wait for the VM image.
  if ! wait_for "$id start" 1200 grep -q " $id start" "$SINK"; then
    echo "$id did not start: stopping the matrix" | tee -a "$OUT/$forge.log"
    exit 1
  fi
  echo "$(date +%T) $id started after $(($(date +%s) - t))s"
  wait_for "$id end" 4200 grep -q " $id end" "$SINK" || { echo "$id: no end" | tee -a "$OUT/$forge.log"; exit 1; }
  # Done: its VM is released, which fleetci logs once the disk's layer
  # is kept, before the next. The runner names the VM.
  start=$(grep " $id start" "$SINK" | cut -d' ' -f1)
  runner=$(grep " $id start" "$SINK" | sed -n 's/.* runner=\([^ ]*\).*/\1/p')
  vm=$(grep "runner=$runner " "$FLEETCI_LOG" | sed -n 's/.* vm=\([^ ]*\).*/\1/p' | head -1)
  if [ -n "$vm" ]; then
    wait_for "$id vm released" 600 grep -q "vm released: vm=$vm " "$FLEETCI_LOG" || true
  else
    echo "$id: which VM ran it is not known; waiting a minute" >&2
    sleep 60
  fi
  line=$(grep " $id end" "$SINK" | tail -1)
  echo "$line queued=$(awk -v a="$start" -v b="$t" 'BEGIN { printf "%.0f", a - b }')s" | tee -a "$OUT/$forge.log"
}

for name in $names; do
  url=$(awk -v n="$name" '$1 == n {print $2}' "$here/matrix.txt")
  repo=${url#https://github.com/}
  list=""
  for c in $(cat "$DATA/srcs/$name.list"); do
    list="$list $(gh api "repos/$repo/commits/$c" --jq .sha)"
  done
  first=$(echo $list | awk '{print $1}') last=$(echo $list | awk '{print $NF}')
  job "$name" "$url" "$first" no cold-first
  i=1
  for c in $list; do
    job "$name" "$url" "$c" yes "warm$i"
    i=$((i + 1))
  done
  job "$name" "$url" "$last" no cold-last
done
echo "$(date +%T) $forge matrix done" | tee -a "$OUT/$forge.log"
