#!/bin/sh
# Runs in the cachelab container: a merge train on one host. The commits
# of /srcs/<repo>.list are merge requests merged in order; the first is
# main as the train starts, and each other is a car, whose pipeline runs on
# main plus the cars ahead of it, which is that commit.
#
#   train.sh <repo> <cars at once> [cache: yes|no] [cars]
#
# Up to <cars at once> pipelines run together, one VM each. A car merges
# when it passes and every car ahead of it has merged; then the next car
# starts. Its cache disk is a layer on the cache of the car that merged
# last, main's newest: last write wins, and no car reads a cache written by
# a car that hasn't merged. With no cache, every car starts cold.
set -u
repo=$1 width=$2 usecache=${3:-yes}
list=$(cat /srcs/$repo.list)
main=$(echo "$list" | head -1)
cars=$(echo "$list" | tail -n +2 | head -n "${4:-1000}")
T0=$(date +%s)
at() { echo $(($(date +%s) - T0)); }
log=/data/train
mkdir -p $log

parent=none
if [ "$usecache" = yes ]; then
  echo "main $main: filling its cache, $(at)s"
  SLOT=0 sh /lab/replay.sh 0 none $repo $main yes
  parent=/data/cache-0.qcow2
fi
T0=$(date +%s)

i=1
for c in $cars; do
  # The slot of the car <width> ahead: wait for it to merge.
  if [ $i -gt $width ]; then
    j=$((i - width))
    wait $(cat $log/pid-$j)
    echo "car $j merged at $(at)s"
    [ "$usecache" = no ] || parent=/data/cache-$j.qcow2
  fi
  slot=$(( (i - 1) % width ))
  echo "car $i ($c) starts at $(at)s on slot $slot, cache from ${parent##*/}"
  SLOT=$slot sh /lab/replay.sh $i $parent $repo $c $usecache > $log/car-$i.log 2>&1 &
  echo $! > $log/pid-$i
  i=$((i + 1))
done
j=$((i - width)); [ $j -ge 1 ] || j=1
while [ $j -lt $i ]; do
  wait $(cat $log/pid-$j); echo "car $j merged at $(at)s"; j=$((j + 1))
done
grep -h '^[a-z]* [0-9a-f]* job' $log/car-*.log
echo "train of $((i - 1)) cars, $width at once, cache $usecache: $(at)s"
