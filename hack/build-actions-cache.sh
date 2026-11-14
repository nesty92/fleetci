#!/bin/sh
# Builds the actions cache (cmd/fleetci-actions-cache) for the VMs, gzipped, into
# internal/github/actionscachebin, where fleetci embeds it: run by
# "go generate ./internal/github" before building fleetci.
set -eu
cd "$(dirname "$0")/.."
out=internal/github/actionscachebin
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$out/fleetci-actions-cache-linux-$arch" ./cmd/fleetci-actions-cache
  gzip -9nf "$out/fleetci-actions-cache-linux-$arch"
done
