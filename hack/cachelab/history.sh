#!/bin/sh
# Runs in the cachelab container. For each repo: its oldest commit with no
# cache disk, then all 8 commits on one chain of cache layers (the first
# fills it), then its newest commit with no cache disk again. The chain is
# merged into one file after 6 layers, as fleetingd would have to.
set -u
for repo in fleetci fleetingd; do
  list=$(cat /srcs/$repo.list)
  first=$(echo "$list" | head -1); last=$(echo "$list" | tail -1)
  sh /lab/replay.sh 0 none $repo $first no
  parent=none i=1
  for c in $list; do
    sh /lab/replay.sh $i $parent $repo $c yes || echo "$repo $c job $i FAILED"
    parent=/data/cache-$i.qcow2
    if [ $i = 6 ]; then
      qemu-img convert -O qcow2 $parent /data/flat.qcow2 && rm -f /data/cache-*.qcow2 &&
        mv /data/flat.qcow2 /data/cache-6.qcow2 && echo "$repo: merged 6 layers"
    fi
    i=$((i + 1))
  done
  sh /lab/replay.sh 0 none $repo $last no
  echo "$repo: cache disk $(du -cm /data/cache-*.qcow2 | tail -1 | cut -f1) MB"
  rm -f /data/cache-*.qcow2
done
echo replay done
