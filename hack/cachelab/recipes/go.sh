# fleetci: go vet, go test -race and a GoReleaser snapshot, as its CI.
G="$RUN -e GOCACHE=/cache/go/build -e GOMODCACHE=/cache/go/mod -e GOFLAGS=-buildvcs=false"
pull golang:1.27 goreleaser/goreleaser:v2.14.0
step vet "$G golang:1.27 go vet ./..."
step test "$G golang:1.27 go test -race ./..."
# GoReleaser's image has Go 1.26; fleetci's go.mod asks for 1.27.
step release "$G -e GOTOOLCHAIN=auto --entrypoint sh goreleaser/goreleaser:v2.14.0 -c 'git config --global --add safe.directory /src && goreleaser release --snapshot --clean'"
