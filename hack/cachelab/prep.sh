#!/bin/sh
# Runs on the host: a repo's last <count> commits as tarballs for
# replay.sh, oldest first, and their list.
#
#   prep.sh <git dir> <name> <ref> <count> <out dir>
set -eu
dir=$1 name=$2 ref=$3 count=$4 out=$5
mkdir -p "$out"
# Every file, tests too: many repos export-ignore them, and info/attributes
# goes over the tree's .gitattributes.
gd=$(git -C "$dir" rev-parse --absolute-git-dir)
mkdir -p "$gd/info" && echo '* -export-ignore -export-subst' > "$gd/info/attributes"
git -C "$dir" log --format=%h -"$count" --reverse "$ref" > "$out/$name.list"
for c in $(cat "$out/$name.list"); do
  git -C "$dir" archive -o "$out/$name-$c.tar" "$c"
done
