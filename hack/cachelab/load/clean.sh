#!/bin/sh
# clean.sh <run|all>: deletes a load run's caches from the lab bucket:
# GitHub's of its load/<run>/ branches, GitLab's keyed load-<run>-...
# With "all", every load run's; main's caches stay either way.
#
# S3_ENV is the bucket's env file (RUSTFS_ACCESS_KEY, RUSTFS_SECRET_KEY);
# S3_URL the bucket's address, S3_BUCKET its name.
set -eu
run=${1:?run or all}
S3_ENV=${S3_ENV:-$HOME/.local/share/fleetci-cachelab/e2e/secrets/labs3.env}
S3_URL=${S3_URL:-http://10.231.29.5:9000}
B=s3://${S3_BUCKET:-gitlab-cache}
set -a; . "$S3_ENV"; set +a
export AWS_ACCESS_KEY_ID="$RUSTFS_ACCESS_KEY" AWS_SECRET_ACCESS_KEY="$RUSTFS_SECRET_KEY" AWS_DEFAULT_REGION=us-east-1
unset AWS_PROFILE RUSTFS_ACCESS_KEY RUSTFS_SECRET_KEY
s3() { aws --endpoint-url "$S3_URL" s3 "$@"; }
dirs() { s3 ls "$1" 2>/dev/null | awk '$1=="PRE"{print $2}'; }
if [ "$run" = all ]; then gh="refs%2Fheads%2Fload%2F" gl="load-"
else gh="refs%2Fheads%2Fload%2F$run%2F" gl="load-$run-"; fi
n=0
# GitHub: github-lab/github/<repo id>/<ref>/...
for repo in $(dirs "$B/github-lab/github/"); do
  for ref in $(dirs "$B/github-lab/github/$repo"); do
    case "$ref" in "$gh"*) s3 rm --quiet --recursive "$B/github-lab/github/$repo$ref"; n=$((n+1)) ;; esac
  done
done
# GitLab: runner/<runner>/project/<id>/<key>
for r in $(dirs "$B/runner/"); do
  for p in $(dirs "$B/runner/${r}project/"); do
    for k in $(s3 ls "$B/runner/${r}project/$p" | awk '$1!="PRE"{print $4}'); do
      case "$k" in "$gl"*) s3 rm --quiet "$B/runner/${r}project/$p$k"; n=$((n+1)) ;; esac
    done
  done
done
echo "deleted $n caches of $run"
