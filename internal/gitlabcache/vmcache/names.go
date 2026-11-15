// Package vmcache is the part of a GitLab job's per-file cache that runs
// on the job's VM, and what it shares with the node: the store a cache
// name falls in, and how the VM asks the node for links. It imports no
// bucket client and nothing of the runner, so the VM's binary stays small.
package vmcache

import (
	"fmt"
	"path"
	"strings"
)

// FilesDir is the stores' folder under a project's caches.
const FilesDir = "fleetci-files"

// Store is the store a cache object's name falls in, and its key there.
// name is the object's name as the runner makes it, under its Path:
// [runner/<token>/]project/<id>/[<shard>/]<key>.
func Store(name string) (prefix, key string, err error) {
	c := path.Clean(name)
	parts := strings.Split(c, "/")
	if c != name || path.IsAbs(c) {
		return "", "", fmt.Errorf("cache name %q: not a clean relative path", name)
	}
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] != "project" {
			continue
		}
		key = parts[len(parts)-1]
		if key == "." || key == ".." || parts[i+1] == ".." {
			break
		}
		class := "other"
		if strings.HasSuffix(key, "-protected") {
			class = "protected"
		}
		return path.Join(path.Join(parts[:i+2]...), FilesDir, class), key, nil
	}
	return "", "", fmt.Errorf("cache name %q: no project/<id>/<key>", name)
}
