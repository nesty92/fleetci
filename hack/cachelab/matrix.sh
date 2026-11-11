#!/bin/sh
# Runs in the cachelab container: the language matrix of matrix.txt. For
# each language, its oldest commit with no cache disk, then every commit
# on one chain of cache layers (the first fills it), then its newest
# commit with no cache disk again. The layers are deleted after each
# language; its lines go to /data/matrix/<name>.log.
#
#   matrix.sh [name...]
#
# The sources come from prep-matrix.sh, in /srcs.
set -u
names=${*:-$(grep -v '^#' /lab/matrix.txt | awk 'NF {print $1}')}
out=/data/matrix
mkdir -p $out
for name in $names; do
  list=$(cat /srcs/$name.list)
  first=$(echo "$list" | head -1); last=$(echo "$list" | tail -1)
  {
    sh /lab/replay.sh 0 none $name $first no
    parent=none i=1
    for c in $list; do
      sh /lab/replay.sh $i $parent $name $c yes || echo "$name $c job $i FAILED"
      parent=/data/cache-$i.qcow2
      i=$((i + 1))
    done
    sh /lab/replay.sh 0 none $name $last no
    echo "$name: cache disk $(du -cm /data/cache-*.qcow2 | tail -1 | cut -f1) MB in $((i - 1)) layers"
  } 2>&1 | tee $out/$name.log
  rm -f /data/cache-*.qcow2
done
echo matrix done
