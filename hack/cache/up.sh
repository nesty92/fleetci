#!/bin/sh
# A local stand-in for the Actions cache on S3: RustFS as the bucket and
# falcondev-oss/github-actions-cache-server in front of it, on the network
# of hack/local, and a Docker Hub pull-through cache (distribution's
# registry) on the same storage.
# Prints the cache_url to put under [github] in fleetci.toml, and the
# registry_mirror for the classes.
set -eu
DATA=${FLEETCI_CACHE_DATA:-$HOME/.local/share/fleetci-cache}
NET=fleetci-local
BUCKET=gh-actions-cache
MIRROR_BUCKET=registry-mirror

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
mirror_ip=$prefix.250.3

docker rm -f fleetci-cache fleetci-mirror fleetci-s3 >/dev/null 2>&1 || true
docker run -d --name fleetci-s3 --network "$NET" --ip "$s3_ip" \
  -e RUSTFS_ACCESS_KEY=fleetci -e RUSTFS_SECRET_KEY="$secret" \
  rustfs/rustfs:latest >/dev/null
for b in "$BUCKET" "$MIRROR_BUCKET"; do
  until docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
    -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url "http://$s3_ip:9000" \
    s3 mb "s3://$b" >/dev/null 2>&1 ||
    docker run --rm --network "$NET" -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
    -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url "http://$s3_ip:9000" \
    s3 ls "s3://$b" >/dev/null 2>&1; do sleep 1; done
done

docker run -d --name fleetci-cache --network "$NET" --ip "$cache_ip" \
  -e API_BASE_URL="http://$cache_ip:3000" \
  -e RUST_LOG="${FLEETCI_CACHE_LOG:-info}" \
  -e STORAGE_DRIVER=s3 -e STORAGE_S3_BUCKET="$BUCKET" \
  -e AWS_ENDPOINT_URL="http://$s3_ip:9000" -e AWS_ALLOW_HTTP=true \
  -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY="$secret" \
  ghcr.io/falcondev-oss/github-actions-cache-server:latest >/dev/null

# Anonymous pulls from Docker Hub, one per image the first time any VM
# asks for it.
docker run -d --name fleetci-mirror --network "$NET" --ip "$mirror_ip" \
  -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
  -e REGISTRY_STORAGE=s3 -e REGISTRY_STORAGE_S3_BUCKET="$MIRROR_BUCKET" \
  -e REGISTRY_STORAGE_S3_REGION=us-east-1 -e REGISTRY_STORAGE_S3_REGIONENDPOINT="http://$s3_ip:9000" \
  -e REGISTRY_STORAGE_S3_FORCEPATHSTYLE=true -e REGISTRY_STORAGE_S3_SECURE=false \
  -e REGISTRY_STORAGE_S3_ACCESSKEY=fleetci -e REGISTRY_STORAGE_S3_SECRETKEY="$secret" \
  -e REGISTRY_STORAGE_REDIRECT_DISABLE=true \
  registry:3 >/dev/null

echo "cache_url = \"http://$cache_ip:3000/\""
echo "registry_mirror = \"http://$mirror_ip:5000\""
echo "  objects: docker run --rm --network $NET -e AWS_ACCESS_KEY_ID=fleetci -e AWS_SECRET_ACCESS_KEY=\$(cat $DATA/s3.secret) -e AWS_DEFAULT_REGION=us-east-1 amazon/aws-cli --endpoint-url http://$s3_ip:9000 s3 ls --recursive s3://$BUCKET"
