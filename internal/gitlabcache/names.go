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

import "github.com/nesty92/fleetci/internal/gitlabcache/vmcache"

var store = vmcache.Store
