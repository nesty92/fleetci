// Package cachedisk keeps disks of caches across single-use VMs: Docker's
// and containerd's data, and whatever else a job keeps on them.
//
// Each cache is a chain of qcow2 layers. A job's VM gets a new, empty layer
// on the newest of its cache's (Begin); after the job, the layer is the
// cache's newest (Commit), or is thrown away when the VM didn't give the
// disk back whole (Abort). Two jobs on one cache at once each get a layer
// on the same parent, and the last to finish wins.
//
// A cache has a name, its key, within its project. A job its forge trusts
// (a writer: by default one on a protected ref) reads and writes the
// project's shared cache of that key. Any other job, such as a merge
// request's, gets a cache of its own ref: it starts from the shared one,
// and what it writes no other ref reads. A cache with nothing yet starts
// from the shared caches its From names, the default branch's, say.
//
// Chains are flattened into one layer well before cloud-hypervisor's
// nesting limit (MaxDepth, chainLimit). The store keeps to a budget: past it, or with less than
// MinFree left on its filesystem, the least recently used caches go first,
// never one a VM is using; a cache unused for MaxAge goes too.
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

// Scope says which cache a job uses.
type Scope struct {
	Forge    string `json:"forge"`    // gitlab, github
	Instance string `json:"instance"` // the forge's host
	Project  string `json:"project"`  // its id
	// Ref is the job's ref. A job that isn't a writer has caches of its
	// ref's own.
	Ref string `json:"ref"`
	// Key names the cache within the project; "" is Ref and Job: each job
	// has its own, so jobs that run at once don't undo each other's.
	Key string `json:"key,omitempty"`
	// Job is the job's name, matrix values included.
	Job string `json:"job,omitempty"`
	// Writer: the job writes the project's shared cache of Key.
	Writer bool `json:"writer"`
	// From are the shared caches a cache with nothing yet starts from, the
	// first that exists.
	From []string `json:"from,omitempty"`
}

func (s Scope) name() string {
	if s.Key == "" {
		return s.named(s.Ref)
	}
	return s.Key
}

// named is the cache of ref for the scope's job; a Key the job chose is
// its own and names the cache whole.
func (s Scope) named(ref string) string {
	if s.Key != "" || s.Job == "" {
		return ref
	}
	return ref + "\x00" + s.Job
}

// Label is the cache's name for people.
func (s Scope) Label() string {
	return strings.ReplaceAll(s.name(), "\x00", " · ")
}

func hashKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

// shared is the directory of the project's shared cache of key.
func (s Scope) shared(key string) string {
	return hashKey(s.Forge, s.Instance, s.Project, "shared", key)
}

// key is the directory the job writes.
func (s Scope) key() string {
	if s.Writer {
		return s.shared(s.name())
	}
	return hashKey(s.Forge, s.Instance, s.Project, "ref", s.Ref, s.name())
}

// source is a cache a job may start from: its directory and its name.
type source struct{ dir, name string }

// sources are the caches the job may start from, in order: its own, the
// shared one of its key, then From's.
func (s Scope) sources() []source {
	out := []source{{s.key(), ""}}
	seen := map[string]bool{s.key(): true}
	names := []string{s.name()}
	for _, k := range s.From {
		if k != "" {
			names = append(names, s.named(k))
		}
	}
	for _, k := range names {
		if d := s.shared(k); k != "" && !seen[d] {
			seen[d] = true
			out = append(out, source{d, strings.ReplaceAll(k, "\x00", " · ")})
		}
	}
	return out
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
	// ReaderMaxAge drops sooner a scope only readers use, a merge or pull
	// request's: with a disk for each job, they are most of the disks.
	ReaderMaxAge time.Duration
	// Size is the cache disk's size as the guest sees it. Only what is
	// written takes space.
	Size int64
	// MaxDepth is the longest chain kept: a longer one is flattened.
	MaxDepth int
}

// chainLimit is the most layers a VM's disk may have, its own included:
// cloud-hypervisor opens a qcow2 file and at most ten backing files below
// it (MAX_NESTING_DEPTH in its block crate, v54), and fails the disk with
// "Overflow" past that.
const chainLimit = 11

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
	if c.ReaderMaxAge == 0 {
		c.ReaderMaxAge = 2 * 24 * time.Hour
	}
	c.ReaderMaxAge = min(c.ReaderMaxAge, c.MaxAge)
	if c.Size == 0 {
		c.Size = 100 * gb
	}
	if c.MaxDepth == 0 || c.MaxDepth >= chainLimit {
		// Well short of chainLimit: the jobs that finish while a chain
		// is flattened still add their layers.
		c.MaxDepth = 6
	}
	return c
}

// Images makes the layers; QemuImg does it with qemu-img.
type Images interface {
	// Create makes an empty qcow2 file at path of size bytes, on parent
	// when it isn't "".
	Create(path, parent string, size int64) error
	// Flatten writes the chain ending in src to dst: on base, a layer of
	// the chain, what differs from it; with base "", all of it, in a file
	// of its own.
	Flatten(src, base, dst string) error
	// Rebase points the layer at path to parent, its data unchanged: for
	// a layer copied from another node, whose parent is elsewhere here.
	Rebase(path, parent string) error
}

// head is a scope's newest layer, in its head.json.
type head struct {
	Scope Scope `json:"scope"`
	// Chain is the layer and its backing files, oldest first.
	Chain []string `json:"chain"`
	// Used is when a job last read or wrote it.
	Used time.Time `json:"used"`
	// Committed is when its newest layer was kept, on whichever node:
	// which of two nodes' copies is newer.
	Committed time.Time `json:"committed,omitzero"`
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
	// pulling: layers being copied from another node, as .part files.
	pulling map[string]bool
	wg      sync.WaitGroup
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
		leases: map[*Lease]struct{}{}, flattening: map[string]bool{}, pulling: map[string]bool{}}
	// Layers a stopped fleetci left half-made, and chains it left deep.
	s.mu.Lock()
	s.sweep()
	for _, key := range s.keys() {
		if h, err := s.readHead(key); err == nil {
			s.keepShallow(key, h)
		}
	}
	s.mu.Unlock()
	return s, nil
}

// Lease is a layer a VM has, until Commit or Abort.
type Lease struct {
	scope Scope
	// chain is the new layer last; its parents came from from.
	chain []string
	from  *source // nil: it started empty
	done  bool
}

// Layer is the file to attach to the VM.
func (l *Lease) Layer() string { return l.chain[len(l.chain)-1] }

// Start says what a job's cache disk starts from.
type Start struct {
	// Empty: it starts empty, its cache's first job.
	Empty bool
	// Own: it starts from the job's own cache's last layer.
	Own bool
	// From is otherwise the shared cache it starts from.
	From string
}

// Start says what the layer starts from.
func (l *Lease) Start() Start {
	switch {
	case l.from == nil:
		return Start{Empty: true}
	case l.from.dir == l.scope.key():
		return Start{Own: true}
	}
	return Start{From: l.from.name}
}

// From is Start in a word: "" when empty, "own", or the shared cache's
// name.
func (l *Lease) From() string {
	st := l.Start()
	if st.Own {
		return "own"
	}
	return st.From
}

// Begin makes the scope's next layer, on the newest of the first cache of
// its sources that exists.
func (s *Store) Begin(scope Scope) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evict()
	l := &Lease{scope: scope}
	parent, from := s.readable(scope)
	if parent != nil && len(parent.Chain) >= chainLimit {
		// Its flattening failed, or is behind: this job goes without,
		// and the next gets the flattened chain.
		s.flattenLater(from.dir, parent.Chain)
		return nil, fmt.Errorf("cache: %s is %d layers, too deep for a VM; it is being flattened for the next job", parent.Scope.Label(), len(parent.Chain))
	}
	if parent != nil {
		s.keepShallow(from.dir, parent)
		l.chain, l.from = append(l.chain, parent.Chain...), from
		parent.Used = s.now()
		if err := s.writeHead(from.dir, parent); err != nil {
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

// readable is the head a scope's next layer goes on, and where it is, or
// nil for none.
func (s *Store) readable(scope Scope) (*head, *source) {
	for _, src := range scope.sources() {
		h, err := s.readHead(src.dir)
		if err != nil {
			continue
		}
		if size := s.chainSize(h.Chain); size > s.cfg.MaxPerCache {
			s.log.Info("cache: over max_per_cache, starting again", "cache", h.Scope.Label(), "size", size)
			continue
		}
		return h, &src
	}
	return nil, nil
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
	h := &head{Scope: l.scope, Chain: l.chain, Used: s.now(), Committed: s.now()}
	if err := s.writeHead(key, h); err != nil {
		_ = os.Remove(l.Layer())
		return err
	}
	s.keepShallow(key, h)
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

// keepShallow flattens the scope's chain in the background once it is
// deeper than MaxDepth, well before a VM can't take it: whichever way the
// chain got deep (a commit, a neighbour's copy, a restart that stopped a
// flattening). s.mu is held.
func (s *Store) keepShallow(key string, h *head) {
	if len(h.Chain) > s.cfg.MaxDepth {
		s.flattenLater(key, h.Chain)
	}
}

// flattenRetry is how long a failed flattening waits to try again.
var flattenRetry = 30 * time.Second

// flattenLater starts flattening a scope's chain, unless it is already
// under way. s.mu is held.
func (s *Store) flattenLater(key string, chain []string) {
	if s.flattening[key] {
		return
	}
	s.flattening[key] = true
	s.wg.Add(1)
	go s.flatten(key, append([]string(nil), chain...))
}

// shared is how many of the chain's first layers are another scope's, as
// a merge request's job's are the default branch's it started from. They
// are kept when the chain is flattened, so the two disks still share their
// blocks, unless that would leave it too deep.
func (s *Store) shared(key string, chain []string) int {
	dir := filepath.Join(s.dir, "scopes", key)
	n := 0
	for n < len(chain) && filepath.Dir(chain[n]) != dir {
		n++
	}
	if n == len(chain) || n+1 > s.cfg.MaxDepth/2 {
		return 0
	}
	return n
}

// flatten writes a scope's own layers into one, on those it shares with
// another scope; it becomes the head if no job has committed since; if one
// has, it flattens the new head.
func (s *Store) flatten(key string, chain []string) {
	layer := chain[len(chain)-1]
	keep := chain[:s.shared(key, chain)]
	base := ""
	if len(keep) > 0 {
		base = keep[len(keep)-1]
	}
	defer s.wg.Done()
	dst := filepath.Join(s.dir, "scopes", key, randomName()+".qcow2")
	// Held as a lease so sweep leaves it while it is written.
	hold := &Lease{chain: []string{dst}}
	s.mu.Lock()
	s.leases[hold] = struct{}{}
	s.mu.Unlock()
	start := s.now()
	err := s.img.Flatten(layer, base, dst)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, hold)
	delete(s.flattening, key)
	h, herr := s.readHead(key)
	switch {
	case err != nil:
		s.log.Warn("cache: flattening failed; trying again", "cache", key, "in", flattenRetry, "err", err)
		_ = os.Remove(dst)
		time.AfterFunc(flattenRetry, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if h, err := s.readHead(key); err == nil {
				s.keepShallow(key, h)
			}
		})
	case herr != nil:
		// Evicted meanwhile.
		_ = os.Remove(dst)
	case h.layer() != layer:
		// Committed over meanwhile: the new head, if it is still deep.
		_ = os.Remove(dst)
		s.keepShallow(key, h)
	default:
		h.Chain = append(keep, dst)
		if err := s.writeHead(key, h); err != nil {
			_ = os.Remove(dst)
			return
		}
		s.log.Info("cache: flattened", "cache", h.Scope.Label(), "on", len(keep), "took", s.now().Sub(start).Round(time.Second).String())
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
			if strings.HasSuffix(e.Name(), ".qcow2") && !keep[p] ||
				strings.HasSuffix(e.Name(), ".part") && !s.pulling[p] {
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
func (s *Store) evict() { s.evictFor(0) }

// evictFor is evict with room left for need more bytes.
func (s *Store) evictFor(need int64) {
	type scope struct {
		key    string
		used   time.Time
		maxAge time.Duration
	}
	var heads []scope
	for _, key := range s.keys() {
		h, err := s.readHead(key)
		if err != nil {
			continue
		}
		age := s.cfg.MaxAge
		if !h.Scope.Writer {
			age = s.cfg.ReaderMaxAge
		}
		heads = append(heads, scope{key, h.Used, age})
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].used.Before(heads[j].used) })
	leased := map[string]bool{}
	for l := range s.leases {
		for _, f := range l.chain {
			leased[filepath.Base(filepath.Dir(f))] = true
		}
	}
	fits := false
	for _, h := range heads {
		old := s.now().Sub(h.used) > h.maxAge
		fits = fits || s.fitsWith(need)
		if !old && fits {
			continue
		}
		if leased[h.key] {
			continue
		}
		s.log.Info("cache: evicting", "scope", h.key, "unused for", s.now().Sub(h.used).Round(time.Minute).String())
		_ = os.Remove(filepath.Join(s.dir, "scopes", h.key, "head.json"))
		s.sweep()
	}
}

// fitsWith says whether the store, need bytes more, is within its budget
// and leaves MinFree.
func (s *Store) fitsWith(need int64) bool {
	if s.used()+need > s.cfg.Budget {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err == nil {
		if int64(st.Bavail)*int64(st.Bsize)-need < s.cfg.MinFree {
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

// Held are the caches the store has, by the name Dirs gives them, and when
// a job last used each: what a node tells the controllers, which place a
// job where its cache is.
func (s *Store) Held() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for _, key := range s.keys() {
		if h, err := s.readHead(key); err == nil {
			out[key] = h.Used
		}
	}
	return out
}

// Dirs are the caches a job of the scope may start from, the best first,
// by the names Held gives them.
func (s Scope) Dirs() []string {
	var out []string
	for _, src := range s.sources() {
		out = append(out, src.dir)
	}
	return out
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
