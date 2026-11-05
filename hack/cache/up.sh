#!/bin/sh
# A local stand-in for the Actions cache on S3: RustFS as the bucket and
# falcondev-oss/github-actions-cache-server in front of it, on the network
# of hack/local.
# Prints the cache_url to put under [github] in fleetci.toml.
set -eu
DATA=${FLEETCI_CACHE_DATA:-$HOME/.local/share/fleetci-cache}
NET=fleetci-local
BUCKET=gh-actions-cache

umask 077
mkdir -p "$DATA"
[ -s "$DATA/s3.secret" ] || head -c 24 /dev/urandom | base64 | tr -d '/+=' > "$DATA/s3.secret"
secret=$(cat "$DATA/s3.secret")
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

# Fixed addresses: the microVMs resolve names through public DNS, not
# Docker's, so the cache server is reached by IP.
prefix=$(docker network inspect "$NET" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' | cut -d. -f1-2)
s3_ip=$prefix.250.1
cache_ip=$prefix.250.2

docker rm -f fleetci-cache fleetci-s3 >/dev/null 2>&1 || true
docker run -d --name fleetci-s3 --network "$NET" --ip "$s3_ip" \
  -e RUSTFS_ACCESS_KEY=fleetci -e RUSTFS_SECRET_KEY="$secret" \
  rustfs/rustfs:latest >/dev/null
until docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
  -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url "http://$s3_ip:9000" \
  s3 mb "s3://$BUCKET" >/dev/null 2>&1 ||
  docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
  -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url "http://$s3_ip:9000" \
  s3 ls "s3://$BUCKET" >/dev/null 2>&1; do sleep 1; done

docker run -d --name fleetci-cache --network "$NET" --ip "$cache_ip" \
  -e API_BASE_URL="http://$cache_ip:3000" \
  -e RUST_LOG="${FLEETCI_CACHE_LOG:-info}" \
  -e STORAGE_DRIVER=s3 -e STORAGE_S3_BUCKET="$BUCKET" \
  -e AWS_ENDPOINT_URL="http://$s3_ip:9000" -e AWS_ALLOW_HTTP=true \
  -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
  ghcr.io/falcondev-oss/github-actions-cache-server:latest >/dev/null

echo "cache_url = \"http://$cache_ip:3000/\""
echo "  objects: docker run --rm --network $NET -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY=\$(cat $DATA/s3.secret) -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url http://$s3_ip:9000 s3 ls --recursive s3://$BUCKET"
