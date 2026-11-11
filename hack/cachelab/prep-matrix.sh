#!/bin/sh
# Runs on the host: fetches each repo of matrix.txt at its pinned head and
# writes its last commits as tarballs for replay.sh.
#
#   prep-matrix.sh <download dir> <out dir> [name...]
#
# The download dir holds untrusted clones: give it a fresh, empty one.
set -eu
here=$(dirname "$0")
dl=$1 out=$2; shift 2
mkdir -p "$dl" "$out"
grep -v '^#' "$here/matrix.txt" | while read -r name url head count; do
  [ -n "$name" ] || continue
  if [ $# -gt 0 ]; then
    case " $* " in *" $name "*) ;; *) continue ;; esac
  fi
  d=$dl/$name
  if [ ! -d "$d" ]; then
    git init -q "$d"
    git -C "$d" fetch -q --depth "$count" "$url" "$head"
  fi
  sh "$here/prep.sh" "$d" "$name" "$head" "$count" "$out"
  echo "$name: $(tr '\n' ' ' < "$out/$name.list")"
done
