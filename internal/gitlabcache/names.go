// Package gitlabcache keeps a GitLab job's cache in the per-file store
// (internal/filestore) in the runner's own bucket, with nothing changed in
// the job: the runner's cache adapter hands the job's helper links to a
// service on the job's VM, which restores a snapshot as the tar.zst the
// helper unpacks and saves the tar.zst it uploads, file by file. The node
// signs the VM's links to the bucket, for the cache names the runner gave
// the job only; the VM never holds the bucket's credentials.
//
// Each GitLab project keeps two stores, as GitLab keeps its caches: one
// for keys GitLab gives protected refs ("-protected"), one for the rest.
// A job that may not write a protected key never writes to the store its
// contents are in.
package gitlabcache

import (
	"fmt"
	"path"
	"strings"
)

// filesDir is the stores' folder under a project's caches.
const filesDir = "fleetci-files"

// store is the store a cache object's name falls in, and its key there.
// name is the object's name as the runner makes it, under its Path:
// [runner/<token>/]project/<id>/[<shard>/]<key>.
func store(name string) (prefix, key string, err error) {
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
		return path.Join(path.Join(parts[:i+2]...), filesDir, class), key, nil
	}
	return "", "", fmt.Errorf("cache name %q: no project/<id>/<key>", name)
}
