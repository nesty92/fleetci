# fleeting-plugin-fleetingd: its CI's cross-build.
G="$RUN -e GOCACHE=/cache/go/build -e GOMODCACHE=/cache/go/mod -e GOFLAGS=-buildvcs=false"
pull golang:1.27
# The CI's license step downloads a tool; LICENSE and the template stand in.
step build "$G golang:1.27 sh -c 'cp LICENSE NOTICE.tpl cmd/fleeting-plugin-fleetingd/ && mv cmd/fleeting-plugin-fleetingd/NOTICE.tpl cmd/fleeting-plugin-fleetingd/NOTICE && go get . && for a in amd64 arm64 riscv64; do CGO_ENABLED=0 GOOS=linux GOARCH=\$a go build -o dist/x ./cmd/fleeting-plugin-fleetingd/ || exit 1; done'"
