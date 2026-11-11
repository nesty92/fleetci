#!/bin/sh
# One job of the language matrix, on a fleetci VM: fetches the language's
# repo at a commit and runs its CI, recipes/<lang>.sh, timing each step.
# It posts a line to the lab's sink when it starts and when it ends.
#
#   run.sh github   on the VM; each step in the recipe's image, docker run,
#                   with the tools' caches under /cache, the cache disk
#   run.sh gitlab   in the job's container, the recipe's image; the
#                   caches under /fleetci, kept by FLEETCI_CACHE_PATHS
#
# MATRIX_ID, MATRIX_LANG, MATRIX_URL and MATRIX_COMMIT say which job.
set -u
mode=$1
SINK=${MATRIX_SINK:-http://10.231.29.20:8099/}
here=$(cd "$(dirname "$0")" && pwd)

post() { # <line>: with whatever the image has
  curl -fsS -m 10 --data-binary "$1" "$SINK" >/dev/null 2>&1 && return
  wget -q -T 10 -O /dev/null --post-data "$1" "$SINK" 2>/dev/null && return
  python3 -c 'import sys, urllib.request as u; u.urlopen(u.Request(sys.argv[1], data=sys.argv[2].encode()), timeout=10)' "$SINK" "$1" 2>/dev/null && return
  node -e 'fetch(process.argv[1], {method: "POST", body: process.argv[2]}).catch(() => process.exit(1))' "$SINK" "$1" 2>/dev/null && return
  php -r 'file_get_contents($argv[1], false, stream_context_create(["http" => ["method" => "POST", "header" => "Content-Type: text/plain", "content" => $argv[2]]]));' "$SINK" "$1" 2>/dev/null && return
  ruby -rnet/http -e 'Net::HTTP.post(URI(ARGV[0]), ARGV[1])' "$SINK" "$1" 2>/dev/null && return
  echo "could not post: $1" >&2
}
now() { cut -d' ' -f1 /proc/uptime; }
since() { awk -v a="$(now)" -v b="$1" 'BEGIN { printf "%.1f", a - b }'; }

t0=$(now)
id=$MATRIX_ID lang=$MATRIX_LANG
case $mode in
github)
  C=/cache
  if findmnt -n /cache >/dev/null 2>&1; then disk=yes; else disk=no; sudo mkdir -p /cache; fi
  sudo chmod 777 /cache
  ;;
gitlab)
  C=/fleetci
  if grep -q " $C " /proc/mounts; then disk=yes; else disk=no; mkdir -p $C; fi
  ;;
esac
post "$id start $mode $lang $MATRIX_COMMIT disk=$disk runner=${RUNNER_NAME:-$(hostname)}"

steps="" failed=0
SRC=$PWD/matrix-src
IMAGE="" IMAGES="" ENV=""
. "$here/recipes/$lang.sh" # IMAGE, IMAGES, ENV and the steps, as functions
eflags=""
for kv in $ENV; do eflags="$eflags -e $kv"; done

_run() { # <label> <user|-> <image> <command>
  label=$1 as=$2 img=$3 cmd=$4 t=$(now) st=""
  echo "::: $label"
  case $mode in
  github)
    u=""; [ "$as" = user ] && u="--user 1000:1000 -e HOME=/tmp"
    # shellcheck disable=SC2086
    docker run --rm --init -v "$SRC:/src" -v /cache:/cache -w /src $eflags $u --entrypoint sh "$img" -c "$cmd" || st=FAILED
    ;;
  gitlab)
    if [ "$img" != "$IMAGE" ]; then steps="$steps $label=skipped"; return; fi
    if [ "$as" = user ]; then
      chmod -R a+rwX "$SRC" $C
      (cd "$SRC" && env $ENV HOME=/tmp setpriv --reuid=1000 --regid=1000 --clear-groups sh -c "$cmd") || st=FAILED
    else
      (cd "$SRC" && env $ENV sh -c "$cmd") || st=FAILED
    fi
    ;;
  esac
  [ -z "$st" ] || failed=$((failed + 1))
  steps="$steps $label=$(since "$t")$st"
}
step() { _run "$1" - "$IMAGE" "$2"; }
step_user() { _run "$1" user "$IMAGE" "$2"; }
step_image() { _run "$2" - "$1" "$3"; }

# The source: a git checkout of the commit, as the repo's own CI has.
t=$(now)
command -v git >/dev/null || { apt-get update -qq && apt-get install -y -qq git >/dev/null; } 2>/dev/null ||
  apk add -q git >/dev/null 2>&1 || microdnf install -y git >/dev/null 2>&1 || true
rm -rf "$SRC" && git init -q "$SRC" &&
  git -C "$SRC" fetch -q --depth 1 "$MATRIX_URL" "$MATRIX_COMMIT" &&
  git -C "$SRC" -c advice.detachedHead=false checkout -q FETCH_HEAD || { post "$id end fetch FAILED"; exit 1; }
steps=" fetch=$(since "$t")"

if [ "$mode" = github ]; then
  t=$(now)
  for i in $IMAGE $IMAGES; do docker pull -q "$i" >/dev/null || failed=$((failed + 1)); done
  steps="$steps pull=$(since "$t")"
fi
recipe

size=$(du -sm $C 2>/dev/null | cut -f1)
post "$id end $mode $lang $MATRIX_COMMIT disk=$disk total=$(since "$t0")$steps failed=$failed cache=${size}MB"
[ $failed = 0 ]
