package fleet

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/proxy"
)

// CheckMirror checks a registry_mirror: the root URL of a registry that
// mirrors Docker Hub, as Docker's registry-mirrors takes it, and returns
// it without a trailing slash.
func CheckMirror(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("registry_mirror %q: want an http(s) URL", s)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		// Docker takes a mirror at a registry's root only; ECR's
		// pull-through rules put Docker Hub under a prefix.
		return "", fmt.Errorf("registry_mirror %q: a mirror is a registry's root URL, without a path", s)
	}
	if strings.ContainsAny(s, "'\"\\ $`") {
		return "", fmt.Errorf("registry_mirror %q: want an http(s) URL", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

// mirrorScript points the VM's Docker at the mirror: registry-mirrors in
// daemon.json, which dockerd reloads on SIGHUP, and the mirror in buildx's
// default BuildKit config, which docker-container builders (as
// docker/setup-buildx-action makes) read, since they don't use the
// daemon's. A VM without dockerd is left as it is. It exits 0 when the
// daemon already had the mirror.
const mirrorScript = `set -eu
mirror='%s'
command -v dockerd >/dev/null 2>&1 || exit 0
run() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo -n "$@"; fi; }
daemon=${FLEETCI_DAEMON_JSON:-/etc/docker/daemon.json}
changed=1
if command -v python3 >/dev/null 2>&1; then
  rc=0
  run python3 -I - "$daemon" "$mirror" <<'PY' || rc=$?
import json, os, sys
path, mirror = sys.argv[1:]
try:
    with open(path) as f:
        c = json.load(f)
except FileNotFoundError:
    c = {}
m = c.get("registry-mirrors") or []
if mirror in m:
    sys.exit(3)
c["registry-mirrors"] = [mirror] + m
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path + ".fleetci", "w") as f:
    json.dump(c, f, indent=2)
    f.write("\n")
os.replace(path + ".fleetci", path)
PY
  case $rc in 0) ;; 3) changed= ;; *) exit "$rc" ;; esac
elif run grep -qF "\"$mirror\"" "$daemon" 2>/dev/null; then
  changed=
elif ! run test -s "$daemon"; then
  run mkdir -p "$(dirname "$daemon")"
  printf '{"registry-mirrors": ["%%s"]}\n' "$mirror" | run tee "$daemon" >/dev/null
else
  echo "registry mirror: $daemon exists and there's no python3 to add to it" >&2
  exit 1
fi
if [ -n "$changed" ]; then
  if pid=$(pidof dockerd); then run kill -HUP $pid; fi
fi
bk=$HOME/.docker/buildx/buildkitd.default.toml
if [ ! -e "$bk" ]; then
  host=${mirror#*://}
  mkdir -p "$(dirname "$bk")"
  {
    printf '[registry."docker.io"]\n  mirrors = ["%%s"]\n' "$host"
    case $mirror in http://*) printf '[registry."%%s"]\n  http = true\n' "$host" ;; esac
  } > "$bk"
fi
`

// mirrorTimeout bounds setting a VM's mirror, connecting included.
const mirrorTimeout = time.Minute

// setMirror points the VM's Docker at the mirror. It is best effort: a
// job on a VM without it pulls from Docker Hub, as without the setting.
func setMirror(ctx context.Context, conn *proxy.Conn, mirror string, log hclog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	err := func() error {
		client, _, err := conn.Client(ctx)
		if err != nil {
			return err
		}
		sess, err := client.NewSession()
		if err != nil {
			return err
		}
		defer sess.Close()
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = sess.Close()
			case <-done:
			}
		}()
		out, err := sess.CombinedOutput(fmt.Sprintf(mirrorScript, mirror))
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}()
	if err != nil {
		log.Warn("vm's registry mirror not set; its jobs pull from Docker Hub", "mirror", mirror, "err", err)
	}
}
