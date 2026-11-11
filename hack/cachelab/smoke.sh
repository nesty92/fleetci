#!/bin/sh
# Runs in the cachelab container: one cold job of each language's newest
# commit, to check its recipe before a matrix run. A failed step prints
# its last lines.
set -u
names=${*:-$(grep -v '^#' /lab/matrix.txt | awk 'NF {print $1}')}
for name in $names; do
  sh /lab/replay.sh 0 none $name "$(tail -1 /srcs/$name.list)" no
done
