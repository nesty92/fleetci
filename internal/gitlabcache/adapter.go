package gitlabcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
)

// Type is the runner's cache type for a job whose cache is in the store.
const Type = "fleetci"

// jobMark starts the cache Path ForJob gives a job: the runner hands the
// adapter its config and the object's name only, and copies the config,
// so the Path is what says whose the name is.
const jobMark = "fleetci-job-"

var jobs sync.Map // mark: *Job

func init() {
	if err := cache.Factories().Register(Type, factory); err != nil {
		panic(err)
	}
}

// Job is what one job may do with the store: the names the runner gave it
// links for, as it made them.
type Job struct {
	// url is the VM's service, as the job's containers reach it.
	url  string
	path string // the runner's cache Path

	mu    sync.Mutex
	read  map[string]bool // stores
	write map[string]bool // stores
	snaps map[string]bool // store/snaps/<key> the job may write
}

// ForJob is the runner's cache config for one job, cfg's in all but where
// it keeps the cache, and the job's grants; done forgets them. vm is the
// VM's service, as the job's containers reach it.
func ForJob(cfg *cacheconfig.Config, vm string) (*cacheconfig.Config, *Job, func()) {
	var b [12]byte
	_, _ = rand.Read(b[:])
	mark := hex.EncodeToString(b[:])
	j := &Job{url: strings.TrimSuffix(vm, "/"), path: strings.Trim(cfg.Path, "/"), read: map[string]bool{}, write: map[string]bool{}, snaps: map[string]bool{}}
	c := &cacheconfig.Config{
		Type:                   Type,
		Path:                   path.Join(jobMark+mark, cfg.Path),
		Shared:                 cfg.Shared,
		MaxUploadedArchiveSize: cfg.MaxUploadedArchiveSize,
		RedactURL:              cfg.RedactURL,
	}
	jobs.Store(mark, j)
	return c, j, func() { jobs.Delete(mark) }
}

func (j *Job) grant(prefix, key string, write bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.read[prefix] = true
	if write {
		j.write[prefix] = true
		j.snaps[prefix+"/snaps/"+key] = true
	}
}

// may says the job may read, or write, the bucket's object name.
func (j *Job) may(name string, write bool) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	// A listing's prefix ends in a slash; nothing else may be other than
	// clean.
	if path.Clean(name) != strings.TrimSuffix(name, "/") {
		return false
	}
	stores := j.read
	if write {
		stores = j.write
	}
	for s := range stores {
		if !strings.HasPrefix(name, s+"/") {
			continue
		}
		if write && strings.HasPrefix(name, s+"/snaps/") {
			return j.snaps[name]
		}
		return true
	}
	return false
}

func factory(_ *cacheconfig.Config, _ time.Duration, objectName string) (cache.Adapter, error) {
	first, rest, _ := strings.Cut(objectName, "/")
	v, ok := jobs.Load(strings.TrimPrefix(first, jobMark))
	if !strings.HasPrefix(first, jobMark) || !ok {
		return nil, fmt.Errorf("fleetci cache: %q is no running job's", objectName)
	}
	j := v.(*Job)
	if j.path != "" {
		var cut bool
		if rest, cut = strings.CutPrefix(rest, j.path+"/"); !cut {
			return nil, fmt.Errorf("fleetci cache: %q is not under the cache's Path", objectName)
		}
	}
	prefix, key, err := store(rest)
	if err != nil {
		return nil, err
	}
	return &adapter{job: j, name: rest, prefix: prefix, key: key}, nil
}

// adapter is one cache name's links, to the VM's service.
type adapter struct {
	job               *Job
	name, prefix, key string
}

func (a *adapter) link() cache.PresignedURL {
	u, err := url.Parse(a.job.url)
	if err != nil {
		return cache.PresignedURL{}
	}
	u.Path = "/gitlab/" + a.name
	return cache.PresignedURL{URL: u}
}

func (a *adapter) GetDownloadURL(context.Context) cache.PresignedURL {
	a.job.grant(a.prefix, a.key, false)
	return a.link()
}

func (a *adapter) GetHeadURL(ctx context.Context) cache.PresignedURL {
	return a.GetDownloadURL(ctx)
}

func (a *adapter) GetUploadURL(context.Context) cache.PresignedURL {
	a.job.grant(a.prefix, a.key, true)
	return a.link()
}

func (a *adapter) WithMetadata(map[string]string) {}

// GetGoCloudURL is none: the helper uses the links.
func (a *adapter) GetGoCloudURL(context.Context, bool) (cache.GoCloudURL, error) {
	return cache.GoCloudURL{}, nil
}
