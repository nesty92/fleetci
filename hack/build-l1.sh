#!/bin/sh
# Builds the actions/cache L1 (cmd/fleetci-l1) for the VMs, gzipped, into
# internal/github/l1bin, where fleetci embeds it: run by
# "go generate ./internal/github" before building fleetci.
set -eu
cd "$(dirname "$0")/.."
out=internal/github/l1bin
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$out/fleetci-l1-linux-$arch" ./cmd/fleetci-l1
  gzip -9nf "$out/fleetci-l1-linux-$arch"
done
