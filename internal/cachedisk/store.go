// Package cachedisk keeps a disk of caches per project and ref across
// single-use VMs: Docker's and containerd's data, and the tools' caches.
//
// Each cache is a chain of qcow2 layers. A job's VM gets a new, empty layer
// on the newest of its scope's (Begin); after the job, the layer is the
// scope's newest (Commit), or is thrown away when the VM didn't give the
// disk back whole (Abort). Two jobs of one scope at once each get a layer
// on the same parent, and the last to finish wins.
//
// A job reads its own scope, or, when that has none yet, its project's
// default branch's; it only ever writes its own. A protected ref's scope
// is its own: a job on an unprotected ref, such as a merge request's, can
// read a protected one's cache but never write it.
//
// Chains are flattened into one layer before cloud-hypervisor's nesting
// limit (MaxDepth). The store keeps to a budget: past it, or with less than
// MinFree left on its filesystem, the least recently used scopes go first,
// never one a VM is using; a scope unused for MaxAge goes too.
package cachedisk

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
)

// Scope says whose cache a job uses.
type Scope struct {
	Forge    string `json:"forge"`    // gitlab, github
	Instance string `json:"instance"` // the forge's host
	Project  string `json:"project"`  // its id or path
	Ref      string `json:"ref"`
	// Protected: only trusted pipelines run on Ref, so its cache can be
	// read by the project's other jobs.
	Protected bool `json:"protected"`
	// Default is the project's default branch, whose cache a ref without
	// one starts from. Empty, a new ref starts empty.
	Default string `json:"default,omitempty"`
}

// key names the scope's directory.
func (s Scope) key() string {
	trust := "u"
	if s.Protected {
		trust = "p"
	}
	h := sha256.Sum256([]byte(strings.Join([]string{s.Forge, s.Instance, s.Project, trust, s.Ref}, "\x00")))
	return hex.EncodeToString(h[:12])
}

// fallback is the scope a new ref reads: its default branch's, protected.
func (s Scope) fallback() (Scope, bool) {
	if s.Default == "" || (s.Ref == s.Default && s.Protected) {
		return Scope{}, false
	}
	f := s
	f.Ref, f.Protected, f.Default = s.Default, true, ""
	return f, true
}

// Config sizes a store.
type Config struct {
	// Budget is what the caches may take on disk, all together.
	Budget int64
	// MinFree is what the store leaves free on its filesystem.
	MinFree int64
	// MaxPerCache is the most one scope's chain may take: past it, the
	// scope's next job starts empty.
	MaxPerCache int64
	// MaxAge drops a scope no job has used for that long.
	MaxAge time.Duration
	// Size is the cache disk's size as the guest sees it. Only what is
	// written takes space.
	Size int64
	// MaxDepth is the longest chain kept: a longer one is flattened.
	MaxDepth int
}

// Defaults fills in what c leaves unset.
func (c Config) withDefaults() Config {
	const gb = 1 << 30
	if c.Budget == 0 {
		c.Budget = 100 * gb
	}
	if c.MinFree == 0 {
		c.MinFree = 20 * gb
	}
	if c.MaxPerCache == 0 {
		c.MaxPerCache = 30 * gb
	}
	if c.MaxAge == 0 {
		c.MaxAge = 7 * 24 * time.Hour
	}
	if c.Size == 0 {
		c.Size = 100 * gb
	}
	if c.MaxDepth == 0 {
		// cloud-hypervisor refuses about 16.
		c.MaxDepth = 10
	}
	return c
}

// Images makes the layers; QemuImg does it with qemu-img.
type Images interface {
	// Create makes an empty qcow2 file at path of size bytes, on parent
	// when it isn't "".
	Create(path, parent string, size int64) error
	// Flatten writes the whole chain ending in src to dst, a file of its own.
	Flatten(src, dst string) error
}

// head is a scope's newest layer, in its head.json.
type head struct {
	Scope Scope `json:"scope"`
	// Chain is the layer and its backing files, oldest first.
	Chain []string `json:"chain"`
	// Used is when a job last read or wrote it.
	Used time.Time `json:"used"`
}

func (h *head) layer() string { return h.Chain[len(h.Chain)-1] }

// Store is the caches in one directory: fleetingd's vm_cache_directory,
// which its VMs may open.
type Store struct {
	dir string
	cfg Config
	img Images
	log hclog.Logger
	now func() time.Time

	mu     sync.Mutex
	leases map[*Lease]struct{}
	// flattening: scopes whose chain is being flattened.
	flattening map[string]bool
	wg         sync.WaitGroup
}

// Open opens the store in dir, making it if needed.
func Open(dir string, cfg Config, img Images, log hclog.Logger) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("cache directory %q: want an absolute path", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scopes"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, cfg: cfg.withDefaults(), img: img, log: log, now: time.Now,
		leases: map[*Lease]struct{}{}, flattening: map[string]bool{}}
	// Layers a stopped fleetci left half-made.
	s.mu.Lock()
	s.sweep()
	s.mu.Unlock()
	return s, nil
}

// Lease is a layer a VM has, until Commit or Abort.
type Lease struct {
	scope Scope
	// chain is the new layer last; its parents came from from.
	chain []string
	from  string // the scope key the parents are of, or ""
	done  bool
}

// Layer is the file to attach to the VM.
func (l *Lease) Layer() string { return l.chain[len(l.chain)-1] }

// From says what the layer starts from: "own", "default" or "empty".
func (l *Lease) From() string {
	switch l.from {
	case "":
		return "empty"
	case l.scope.key():
		return "own"
	}
	return "default"
}

// Begin makes the scope's next layer, on its newest or its default
// branch's.
func (s *Store) Begin(scope Scope) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evict()
	l := &Lease{scope: scope}
	parent, from := s.readable(scope)
	if parent != nil {
		l.chain, l.from = append(l.chain, parent.Chain...), from
		parent.Used = s.now()
		if err := s.writeHead(from, parent); err != nil {
			s.log.Warn("cache: could not note a cache's use", "err", err)
		}
	}
	dir := filepath.Join(s.dir, "scopes", scope.key())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	layer := filepath.Join(dir, randomName()+".qcow2")
	p := ""
	if parent != nil {
		p = parent.layer()
	}
	// The lease is held before the file exists, so sweep keeps it.
	l.chain = append(l.chain, layer)
	s.leases[l] = struct{}{}
	if err := s.img.Create(layer, p, s.cfg.Size); err != nil {
		delete(s.leases, l)
		_ = os.Remove(layer)
		return nil, fmt.Errorf("cache layer: %w", err)
	}
	return l, nil
}

// readable is the head a scope's next layer goes on, and its key, or nil
// for none.
func (s *Store) readable(scope Scope) (*head, string) {
	try := []Scope{scope}
	if f, ok := scope.fallback(); ok {
		try = append(try, f)
	}
	for _, sc := range try {
		h, err := s.readHead(sc.key())
		if err != nil {
			continue
		}
		if size := s.chainSize(h.Chain); size > s.cfg.MaxPerCache {
			s.log.Info("cache: over max_per_cache, starting again", "ref", sc.Ref, "size", size)
			continue
		}
		return h, sc.key()
	}
	return nil, ""
}

// Commit makes the lease's layer its scope's newest. The VM must have let
// go of it, its filesystem unmounted.
func (s *Store) Commit(l *Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.done {
		return errors.New("cache: lease already ended")
	}
	l.done = true
	delete(s.leases, l)
	key := l.scope.key()
	h := &head{Scope: l.scope, Chain: l.chain, Used: s.now()}
	if err := s.writeHead(key, h); err != nil {
		_ = os.Remove(l.Layer())
		return err
	}
	if len(h.Chain) > s.cfg.MaxDepth && !s.flattening[key] {
		s.flattening[key] = true
		s.wg.Add(1)
		go s.flatten(key, h.layer())
	}
	s.sweep()
	s.evict()
	return nil
}

// Abort throws the lease's layer away.
func (s *Store) Abort(l *Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	delete(s.leases, l)
	_ = os.Remove(l.Layer())
}

// flatten writes a scope's chain into one layer, which becomes its head if
// no job has committed since.
func (s *Store) flatten(key, layer string) {
	defer s.wg.Done()
	dst := filepath.Join(s.dir, "scopes", key, randomName()+".qcow2")
	// Held as a lease so sweep leaves it while it is written.
	hold := &Lease{chain: []string{dst}}
	s.mu.Lock()
	s.leases[hold] = struct{}{}
	s.mu.Unlock()
	start := s.now()
	err := s.img.Flatten(layer, dst)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, hold)
	delete(s.flattening, key)
	h, herr := s.readHead(key)
	switch {
	case err != nil:
		s.log.Warn("cache: flattening failed", "err", err)
		_ = os.Remove(dst)
	case herr != nil || h.layer() != layer:
		// Committed over or evicted meanwhile: the next commit flattens.
		_ = os.Remove(dst)
	default:
		h.Chain = []string{dst}
		if err := s.writeHead(key, h); err != nil {
			_ = os.Remove(dst)
			return
		}
		s.log.Info("cache: flattened", "ref", h.Scope.Ref, "took", s.now().Sub(start).Round(time.Second).String())
		s.sweep()
	}
}

// Wait waits for flattening under way.
func (s *Store) Wait() { s.wg.Wait() }

// inUse is every layer a head or a lease needs.
func (s *Store) inUse() map[string]bool {
	keep := map[string]bool{}
	for l := range s.leases {
		for _, f := range l.chain {
			keep[f] = true
		}
	}
	for _, key := range s.keys() {
		if h, err := s.readHead(key); err == nil {
			for _, f := range h.Chain {
				keep[f] = true
			}
		}
	}
	return keep
}

// sweep removes the layers nothing needs, and empty scopes.
func (s *Store) sweep() {
	keep := s.inUse()
	for _, key := range s.keys() {
		dir := filepath.Join(s.dir, "scopes", key)
		entries, _ := os.ReadDir(dir)
		left := 0
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if strings.HasSuffix(e.Name(), ".qcow2") && !keep[p] {
				_ = os.Remove(p)
				continue
			}
			left++
		}
		if left == 0 {
			_ = os.Remove(dir)
		}
	}
}

// evict drops scopes, least recently used first, until the store is in
// budget, and those past MaxAge. A scope with a lease on its layers stays.
func (s *Store) evict() {
	type scope struct {
		key  string
		used time.Time
	}
	var heads []scope
	for _, key := range s.keys() {
		h, err := s.readHead(key)
		if err != nil {
			continue
		}
		heads = append(heads, scope{key, h.Used})
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].used.Before(heads[j].used) })
	leased := map[string]bool{}
	for l := range s.leases {
		for _, f := range l.chain {
			leased[filepath.Base(filepath.Dir(f))] = true
		}
	}
	for _, h := range heads {
		old := s.now().Sub(h.used) > s.cfg.MaxAge
		if !old && s.fits() {
			break
		}
		if leased[h.key] {
			continue
		}
		s.log.Info("cache: evicting", "scope", h.key, "unused for", s.now().Sub(h.used).Round(time.Minute).String())
		_ = os.Remove(filepath.Join(s.dir, "scopes", h.key, "head.json"))
		s.sweep()
	}
}

// fits says whether the store is within its budget and leaves MinFree.
func (s *Store) fits() bool {
	if s.used() > s.cfg.Budget {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err == nil {
		if int64(st.Bavail)*int64(st.Bsize) < s.cfg.MinFree {
			return false
		}
	}
	return true
}

// used is what the store's files take on disk.
func (s *Store) used() int64 {
	var n int64
	_ = filepath.WalkDir(filepath.Join(s.dir, "scopes"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n += allocated(d)
		}
		return nil
	})
	return n
}

func (s *Store) chainSize(chain []string) int64 {
	var n int64
	for _, f := range chain {
		if st, err := os.Lstat(f); err == nil {
			n += allocated(fs.FileInfoToDirEntry(st))
		}
	}
	return n
}

// allocated is the space a file takes, not its apparent size: layers are
// sparse.
func allocated(d fs.DirEntry) int64 {
	info, err := d.Info()
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

func (s *Store) keys() []string {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "scopes"))
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			keys = append(keys, e.Name())
		}
	}
	return keys
}

func (s *Store) readHead(key string) (*head, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "scopes", key, "head.json"))
	if err != nil {
		return nil, err
	}
	var h head
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	if len(h.Chain) == 0 {
		return nil, errors.New("empty chain")
	}
	for _, f := range h.Chain {
		if _, err := os.Stat(f); err != nil {
			return nil, err
		}
	}
	return &h, nil
}

// writeHead replaces a scope's head.json in one rename.
func (s *Store) writeHead(key string, h *head) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, "scopes", key)
	tmp := filepath.Join(dir, "head.json."+randomName())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "head.json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
