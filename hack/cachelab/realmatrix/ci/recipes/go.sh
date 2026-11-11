# fleetci: go vet, go test -race and a GoReleaser snapshot, as its CI.
# GoReleaser runs in its own image, so not on GitLab.
IMAGE=golang:1.27 IMAGES=goreleaser/goreleaser:v2.14.0
ENV="GOCACHE=$C/go/build GOMODCACHE=$C/go/mod GOFLAGS=-buildvcs=false"
recipe() {
  step vet "go vet ./..."
  step test "go test -race ./..."
  # GoReleaser's image has Go 1.26; fleetci's go.mod asks for 1.27.
  step_image goreleaser/goreleaser:v2.14.0 release "git config --global --add safe.directory /src && GOTOOLCHAIN=auto goreleaser release --snapshot --clean"
}
