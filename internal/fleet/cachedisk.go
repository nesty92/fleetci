package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/cachedisk"
	"github.com/nesty92/fleetci/internal/proxy"
)

// CacheDisk is a class's [classes.<name>.cache_disk] table: each job's VM
// gets a disk of its project's and ref's caches, kept across VMs (see
// package cachedisk). Docker's and containerd's data go on it, so images
// pulled and volumes made, gitlab-runner's local cache among them, are
// there for the next job of that ref. The VMs must be fleetingd's, with a
// vm_cache_directory in plugin_config: that is where the caches are kept.
type CacheDisk struct {
	// Budget is what all of the class's caches may take on disk. Default
	// 100GB.
	Budget Size `json:"budget" toml:"budget"`
	// MinFree is what is left free on the disk they are on. Default 20GB.
	MinFree Size `json:"min_free" toml:"min_free"`
	// MaxPerCache is the most one ref's cache may take; past it, its next
	// job starts empty. Default 30GB.
	MaxPerCache Size `json:"max_per_cache" toml:"max_per_cache"`
	// MaxAge drops a cache no job has used for that long. Default 7 days.
	MaxAge Duration `json:"max_age" toml:"max_age"`
	// ReaderMaxAge drops sooner the caches of jobs that don't write the
	// shared ones, a merge or pull request's. Default 2 days, and never
	// past MaxAge.
	ReaderMaxAge Duration `json:"reader_max_age" toml:"reader_max_age"`
	// QemuImg is the qemu-img binary; default, the one on PATH.
	QemuImg string `json:"qemu_img" toml:"qemu_img"`
}

// cacheDirectory is fleetingd's vm_cache_directory in a class's
// plugin_config, or "".
func (c Class) cacheDirectory() string {
	var pc struct {
		Dir string `json:"vm_cache_directory"`
	}
	_ = json.Unmarshal(c.PluginConfig, &pc)
	return pc.Dir
}

func (c Class) validateCacheDisk(name string) error {
	if c.CacheDisk == nil {
		return nil
	}
	if !strings.Contains(filepath.Base(c.Plugin), "fleetingd") {
		return fmt.Errorf("class %q: cache_disk needs fleetingd's VMs", name)
	}
	if !filepath.IsAbs(c.cacheDirectory()) {
		return fmt.Errorf("class %q: cache_disk needs an absolute vm_cache_directory in plugin_config", name)
	}
	if c.Reuses() {
		return fmt.Errorf("class %q: cache_disk needs a VM per job: leave capacity_per_instance and max_use_count at 1", name)
	}
	d := c.CacheDisk
	if d.Budget < 0 || d.MinFree < 0 || d.MaxPerCache < 0 || d.MaxAge < 0 || d.ReaderMaxAge < 0 {
		return fmt.Errorf("class %q: cache_disk sizes and max_age can't be negative", name)
	}
	return nil
}

func (d CacheDisk) config() cachedisk.Config {
	const mb = 1 << 20
	return cachedisk.Config{Budget: int64(d.Budget) * mb, MinFree: int64(d.MinFree) * mb,
		MaxPerCache: int64(d.MaxPerCache) * mb, MaxAge: time.Duration(d.MaxAge), ReaderMaxAge: time.Duration(d.ReaderMaxAge)}
}

// openCache opens the class's store. Classes on one directory share it.
func openCache(stores map[string]*cachedisk.Store, c Class, log hclog.Logger) (*cachedisk.Store, error) {
	dir := filepath.Clean(c.cacheDirectory())
	if s, ok := stores[dir]; ok {
		return s, nil
	}
	s, err := cachedisk.Open(dir, c.CacheDisk.config(), cachedisk.QemuImg{Path: c.CacheDisk.QemuImg}, log.Named("cache"))
	if err != nil {
		return nil, err
	}
	stores[dir] = s
	return s, nil
}

// cacheVM is a VM's cache disk, from attach to Delete.
type cacheVM struct {
	store *cachedisk.Store
	lease *cachedisk.Lease
	class Class
	inst  string
	conn  *proxy.Conn
}

// AttachCache gives the VM its scope's cache disk, mounted at /cache with
// Docker's and containerd's data on it, and says what the disk starts
// from. A class without cache_disk returns nil and no error. The VM gives
// the disk back in Delete.
func (b *Backend) AttachCache(ctx context.Context, vm VM, scope cachedisk.Scope) (*cachedisk.Start, error) {
	id, inst := vm.ID, vm.Instance
	b.mu.Lock()
	p, ok := b.vms[id]
	_, has := b.caches[id]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no vm %q", id)
	}
	if p.cache == nil || has {
		return nil, nil
	}
	lease, err := p.cache.Begin(scope)
	if err != nil {
		return nil, err
	}
	cv := &cacheVM{store: p.cache, lease: lease, class: p.class, inst: inst, conn: p.conns.get(inst)}
	start := time.Now()
	if err := pluginDisk(ctx, p.class, "attach", inst, lease.Layer()); err != nil {
		p.cache.Abort(lease)
		return nil, err
	}
	if out, err := guest(ctx, cv.conn, mountScript); err != nil {
		// The disk may be half-mounted: it goes with the VM, unused.
		_ = pluginDisk(context.WithoutCancel(ctx), p.class, "detach", inst)
		p.cache.Abort(lease)
		return nil, fmt.Errorf("mounting the cache: %w: %s", err, out)
	}
	b.mu.Lock()
	if b.caches == nil {
		b.caches = map[string]*cacheVM{}
	}
	b.caches[id] = cv
	b.mu.Unlock()
	st := lease.Start()
	b.log.Info("cache attached", "vm", id, "ref", scope.Ref, "key", scope.Key, "job", scope.Job, "writer", scope.Writer, "from", lease.From(), "took", time.Since(start).Round(time.Millisecond).String())
	return &st, nil
}

// CacheStores are this backend's cache stores, one per
// vm_cache_directory.
func (b *Backend) CacheStores() []*cachedisk.Store {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[*cachedisk.Store]bool{}
	var stores []*cachedisk.Store
	for _, ps := range b.pools {
		for _, p := range ps {
			if p.cache != nil && !seen[p.cache] {
				seen[p.cache] = true
				stores = append(stores, p.cache)
			}
		}
	}
	return stores
}

// HeldCaches are the caches this backend's stores have, and when a job
// last used each (cachedisk.Store.Held).
func (b *Backend) HeldCaches() map[string]time.Time {
	out := map[string]time.Time{}
	for _, s := range b.CacheStores() {
		for k, t := range s.Held() {
			if t.After(out[k]) {
				out[k] = t
			}
		}
	}
	return out
}

// HasCache says whether the VM's class has a cache disk.
func (b *Backend) HasCache(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.vms[id]
	return ok && p.cache != nil
}

// detachCache takes the cache disk back from a VM whose job is over. The
// layer is kept only when the guest unmounted it cleanly and the plugin
// took it out; otherwise it is thrown away and the cache stays as it was.
func (b *Backend) detachCache(id string) {
	b.mu.Lock()
	cv, ok := b.caches[id]
	delete(b.caches, id)
	b.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	if out, err := guest(ctx, cv.conn, unmountScript(cv.store.KeepUnder())); err != nil {
		b.log.Warn("cache not unmounted; its layer is dropped", "vm", id, "err", err, "out", out)
		_ = pluginDisk(ctx, cv.class, "detach", cv.inst)
		cv.store.Abort(cv.lease)
		return
	}
	if err := pluginDisk(ctx, cv.class, "detach", cv.inst); err != nil {
		b.log.Warn("cache not detached; its layer is dropped", "vm", id, "err", err)
		cv.store.Abort(cv.lease)
		return
	}
	if err := cv.store.Commit(cv.lease); err != nil {
		b.log.Warn("cache layer not kept", "vm", id, "err", err)
		return
	}
	b.log.Info("cache kept", "vm", id, "took", time.Since(start).Round(time.Millisecond).String())
}

// pluginDisk runs the class's plugin's disk command, its plugin_config on
// stdin, in fleetci's environment, as fleeting runs the plugin itself: the
// plugin finds the VM's API socket where it made it.
func pluginDisk(ctx context.Context, c Class, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Plugin, append([]string{"disk", "-config", "-"}, args...)...)
	cmd.Stdin = bytes.NewReader(c.PluginConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s disk %s: %w: %s", filepath.Base(c.Plugin), args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// guest runs a script on the VM, as root.
func guest(ctx context.Context, conn *proxy.Conn, script string) (string, error) {
	if conn == nil {
		return "", errors.New("no connection to the vm")
	}
	client, _, err := conn.Client(ctx)
	if err != nil {
		return "", err
	}
	s, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-done:
		}
	}()
	out, err := s.CombinedOutput(`s=; [ "$(id -u)" = 0 ] || s="sudo -n"; exec $s sh -euc '` + script + `'`)
	return strings.TrimSpace(string(out)), err
}

// mountScript waits for the disk, formats it the first time, and puts
// Docker's and containerd's data on it. The disk's serial names it.
// No single quotes: guest wraps it in them.
const mountScript = `
dev=/dev/disk/by-id/virtio-cache
i=0
until [ -b $dev ]; do
  i=$((i+1)); [ $i -lt 200 ] || { echo "no $dev"; exit 1; }
  sleep 0.05
done
blkid $dev >/dev/null 2>&1 || mkfs.ext4 -q -E lazy_itable_init=1,lazy_journal_init=1 $dev
systemctl stop docker.socket docker.service containerd.service
mkdir -p /cache
mount -o noatime $dev /cache
mkdir -p /cache/docker /cache/containerd
mount --bind /cache/docker /var/lib/docker
mount --bind /cache/containerd /var/lib/containerd
systemctl start containerd.service docker.service
`

// unmountScript stops what uses the disk, trims it and unmounts it, so the
// layer is whole when the plugin takes it out. Without the trim, what the
// job wrote and deleted (a container's layers, an archive already
// extracted) stays allocated in the job's layer on the node, and every
// job's layer grows the disk by that much: 1 to 3 GB a job in the merge
// train. Trimmed, the layer keeps only what is on the disk at the end.
//
// First it keeps what the disk holds under keep bytes, the least recently
// used first: actions/cache's archives, then Docker's build cache, then
// images older than a week. Otherwise a cache grows until the store
// starts it again from empty (max_per_cache), images and all.
func unmountScript(keep int64) string {
	return strings.ReplaceAll(unmountTmpl, "@KEEP@", strconv.FormatInt(keep, 10))
}

const unmountTmpl = `
keep=@KEEP@
over() { [ "$(df -B1 --output=used /cache | tail -1)" -gt $keep ]; }
if over && [ -d /cache/actions ]; then
  for f in $(ls -tr /cache/actions/*.blob 2>/dev/null); do
    over || break
    rm -f "$f" "${f%.blob}.json"
  done
fi
if over; then docker builder prune -f --keep-storage $((keep / 4)) >/dev/null 2>&1 || true; fi
if over; then docker image prune -af --filter until=168h >/dev/null 2>&1 || true; fi
systemctl stop docker.socket docker.service containerd.service
sync
fstrim /cache || true
umount /var/lib/docker /var/lib/containerd /cache
`
