// Package l1cache is fleetci's L1 for actions/cache, run in the job's VM
// (cmd/fleetci-l1): the job's ACTIONS_RESULTS_URL points here, and every
// request goes on to the cache server it would have used (the L2: GitHub's,
// or the cache_url server), which still decides what each key restores and
// what a job may save.
//
// The archives travel through here, and a copy of each stays on the job's
// cache disk (/cache/actions). An entry, a key and version, never changes
// once saved, so the next job that restores it reads the disk's copy and
// skips the download. Saving still uploads to the L2.
//
// The disk is a convenience: when it is missing, full or failing, requests
// go on as without it. When the L2 can't be reached, a restore is answered
// from the disk, as the L2 would have chosen among the same entries.
//
// In ModeOff the cache server isn't used for actions/cache at all: a
// restore is answered from the disk alone, as the cache server would have
// chosen among the same entries, and a save stays on the disk. The job's
// other calls, artifacts', still go to it.
//
// In ModeBucket the cache is in the user's own bucket, behind the node at
// Config.Node (fleetci, through the VM's SSH connection): the node checks
// the job's token, says which entry a restore gets and signs a link to it.
// A save stays on the disk; after the job, fleetci asks for /fleetci/flush,
// which uploads the job's saves with links the node signs. The job doesn't
// wait for its uploads.
//
// On the disk each entry is two files: <id>.blob, the archive, and
// <id>.json, what it is (meta). The node trims the blobs, oldest first,
// when the disk is over its size (internal/fleet's unmount script).
package l1cache

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Mode is where actions/cache's entries live besides the disk.
type Mode int

const (
	// ModeProxy: the cache server the job would have used (the L2).
	ModeProxy Mode = iota
	// ModeOff: the disk alone.
	ModeOff
	// ModeBucket: the user's bucket, behind the node.
	ModeBucket
)

// Config is how the L1 runs.
type Config struct {
	// Upstream is the cache server the job would have used.
	Upstream string
	// Store is the directory on the cache disk the archives go to.
	Store string
	// AnyStore stores in Store whether or not its parent is a mount
	// (tests); otherwise a Store on the VM's root disk isn't used.
	AnyStore bool
	Mode     Mode
	// Node is the node's cache service, in ModeBucket.
	Node string
	// Port is where the L1 listens, for the URLs it hands out when a
	// request names no host.
	Port int
	// Log gets what the L1 does; nil logs to stderr.
	Log *log.Logger
}

// Server is the L1, an http.Handler.
type Server struct {
	cfg    Config
	log    *log.Logger
	client *http.Client

	mu       sync.Mutex
	links    map[string]any // fillLink or putLink, by the token in its local URL
	uploads  map[string]string
	pending  map[string]pendingSave
	stats    stats
	storeWhy string
	storeSet bool
}

// The cache service's twirp path.
const svc = "/twirp/github.actions.results.api.v1.CacheService/"

// share is the part of the disk the archives stay under, the least
// recently used out first.
const share = 0.5

// A miss is fetched in ranges this big, this many at a time, as
// actions/cache would fetch it from the cache server itself.
const (
	part        = 8 << 20
	partsAtOnce = 8
)

// fillLink is a download handed out as a local URL: the L2's or the
// bucket's URL of the entry, and the entry.
type fillLink struct {
	url, key, version string
	// src names the entry where it came from: the bucket's name for it,
	// or, when empty, the cache server's URL without its signature.
	src, scope string
}

// putLink is an upload handed out as a local URL: the L2's URL, empty
// when the save stays on the disk, and the entry.
type putLink struct {
	url, eid string
}

// pendingSave is a save the bucket doesn't have yet. The job's token stays
// in memory, never on the disk.
type pendingSave struct {
	key, version, auth string
}

type stats struct {
	hit, hitBytes                  int64
	fill, fillBytes                int64
	fillSecs                       float64
	remote, none                   int64
	saved, savedBytes, savedRemote int64
}

// meta is an entry's <id>.json.
type meta struct {
	Key     string  `json:"key"`
	Version string  `json:"version"`
	Size    int64   `json:"size"`
	Saved   float64 `json:"saved"`
	// Src names the entry where it came from (fillLink.src).
	Src string `json:"src"`
	// Scope is the ref's scope a save went to; empty when unknown.
	Scope string `json:"scope,omitempty"`
}

// New makes the L1 for cfg.
func New(cfg Config) *Server {
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	cfg.Node = strings.TrimRight(cfg.Node, "/")
	lg := cfg.Log
	if lg == nil {
		lg = log.New(os.Stderr, "", log.Ltime)
	}
	tr := &http.Transport{
		Proxy:                 nil, // as the runner reaches its cache server
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   partsAtOnce * 2,
		DisableCompression:    true, // bodies go on as they come
	}
	return &Server{
		cfg: cfg, log: lg,
		client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		links: map[string]any{}, uploads: map[string]string{}, pending: map[string]pendingSave{},
	}
}

func (s *Server) logf(format string, a ...any) { s.log.Printf(format, a...) }

// diskOnlySaves: a save stays on the disk, not on a cache server.
func (s *Server) diskOnlySaves() bool { return s.cfg.Mode == ModeOff || s.cfg.Mode == ModeBucket }

func (s *Server) link(v any) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	s.mu.Lock()
	s.links[t] = v
	s.mu.Unlock()
	return t
}

func (s *Server) linked(t string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links[t]
}

// --- the disk ---

// storeOK says the store is usable: on a disk of its own, not the VM's
// root.
func (s *Server) storeOK() bool {
	parent := filepath.Dir(strings.TrimRight(s.cfg.Store, "/"))
	if !s.cfg.AnyStore && devOf(parent) == devOf("/") {
		return s.storeSays("no cache disk at " + parent)
	}
	if err := os.MkdirAll(filepath.Join(s.cfg.Store, "tmp"), 0o755); err != nil {
		return s.storeSays("the disk can't be used: " + err.Error())
	}
	return s.storeSays("")
}

// devOf is the device path is on; -1 when it can't be read.
func devOf(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return -1
	}
	return int64(st.Dev)
}

// storeSays logs why the disk isn't used when that changes; true when it
// is.
func (s *Server) storeSays(why string) bool {
	s.mu.Lock()
	changed := !s.storeSet || why != s.storeWhy
	s.storeWhy, s.storeSet = why, true
	s.mu.Unlock()
	if changed {
		if why == "" {
			s.logf("using the disk at %s", s.cfg.Store)
		} else {
			s.logf("%s", why)
		}
	}
	return why == ""
}

// EntryID names an entry's files: its key and version, hashed.
func EntryID(key, version string) string {
	sum := sha256.Sum256([]byte(version + "\n" + key))
	return hex.EncodeToString(sum[:])[:32]
}

func (s *Server) blobPath(eid string) string { return filepath.Join(s.cfg.Store, eid+".blob") }
func (s *Server) metaPath(eid string) string { return filepath.Join(s.cfg.Store, eid+".json") }
func (s *Server) tmpDir(eid string) string   { return filepath.Join(s.cfg.Store, "tmp", eid) }

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (s *Server) have(eid string) bool {
	return isFile(s.blobPath(eid)) && isFile(s.metaPath(eid))
}

func (s *Server) readMeta(eid string) (meta, error) {
	var m meta
	b, err := os.ReadFile(s.metaPath(eid))
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

func (s *Server) writeMeta(eid string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := s.metaPath(eid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(eid))
}

// srcOf is the cache server's entry a URL names, without its signature.
func srcOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.Host + p.Path
}

// sameEntry says the disk's copy is of the entry src names now. The same
// key and version may be another entry in another ref's scope: a copy of
// the default branch's isn't served for a pull request's own.
func (s *Server) sameEntry(eid, src string) bool {
	m, err := s.readMeta(eid)
	return err == nil && m.Src == src
}

// commit moves a whole archive into the store, then trims the store.
// scope is the ref's scope a save went to, empty when unknown.
func (s *Server) commit(tmp, key, version, src, scope string) error {
	eid := EntryID(key, version)
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	m := meta{Key: key, Version: version, Size: st.Size(), Saved: float64(time.Now().UnixNano()) / 1e9, Src: src, Scope: scope}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(s.metaPath(eid)+".tmp", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.blobPath(eid)); err != nil {
		return err
	}
	if err := os.Rename(s.metaPath(eid)+".tmp", s.metaPath(eid)); err != nil {
		return err
	}
	s.logf("kept %s %d bytes", key, st.Size())
	s.trim()
	return nil
}

// trim keeps the archives under share of the disk, the least recently
// used out first.
func (s *Server) trim() {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.Store, &fs); err != nil {
		s.logf("trim: %v", err)
		return
	}
	limit := float64(fs.Blocks) * float64(fs.Bsize) * share
	des, err := os.ReadDir(s.cfg.Store)
	if err != nil {
		s.logf("trim: %v", err)
		return
	}
	type ent struct {
		mtime time.Time
		size  int64
		eid   string
	}
	var ents []ent
	var total int64
	for _, de := range des {
		if eid, ok := strings.CutSuffix(de.Name(), ".blob"); ok {
			if info, err := de.Info(); err == nil {
				ents = append(ents, ent{info.ModTime(), info.Size(), eid})
				total += info.Size()
			}
		}
	}
	slices.SortFunc(ents, func(a, b ent) int { return a.mtime.Compare(b.mtime) })
	for _, e := range ents {
		if float64(total) <= limit {
			break
		}
		_ = os.Remove(s.blobPath(e.eid))
		_ = os.Remove(s.metaPath(e.eid))
		total -= e.size
	}
}

// localMatch is what GitHub would restore, among the disk's entries: scope
// by scope, in the order the token gives, the key and then each restore
// key, each first exactly and else as the prefix of the newest entry. Keys
// compare byte for byte. Entries of no known scope come after the token's
// scopes; without scopes, every entry is one scope.
func (s *Server) localMatch(key string, restoreKeys []string, version string, scopes []string) (string, bool) {
	des, err := os.ReadDir(s.cfg.Store)
	if err != nil {
		return "", false
	}
	var metas []meta
	for _, de := range des {
		eid, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !s.have(eid) {
			continue
		}
		m, err := s.readMeta(eid)
		if err != nil {
			return "", false
		}
		if m.Version == version {
			metas = append(metas, m)
		}
	}
	var groups [][]meta
	if len(scopes) > 0 {
		for _, sc := range scopes {
			var g []meta
			for _, m := range metas {
				if m.Scope == sc {
					g = append(g, m)
				}
			}
			groups = append(groups, g)
		}
		var none []meta
		for _, m := range metas {
			if m.Scope == "" {
				none = append(none, m)
			}
		}
		groups = append(groups, none)
	} else {
		groups = [][]meta{metas}
	}
	newest := func(ms []meta) string {
		best := ms[0]
		for _, m := range ms[1:] {
			if m.Saved > best.Saved {
				best = m
			}
		}
		return best.Key
	}
	for _, ms := range groups {
		for _, k := range append([]string{key}, restoreKeys...) {
			var exact, prefixed []meta
			for _, m := range ms {
				if m.Key == k {
					exact = append(exact, m)
				}
				if strings.HasPrefix(m.Key, k) {
					prefixed = append(prefixed, m)
				}
			}
			if len(exact) > 0 {
				return newest(exact), true
			}
			if len(prefixed) > 0 {
				return newest(prefixed), true
			}
		}
	}
	return "", false
}

// tokenScopes are the cache scopes the job's token grants, as GitHub's
// cache server reads them: the scope a save goes to, and the scopes a
// restore searches, in order. The token's "ac" claim lists {"Scope": ref,
// "Permission": bits}, 1 to read and 2 to write. Empty when the token
// doesn't say.
//
// The token isn't verified: this runs in the job, for the job's own token,
// and only decides what the job's own disk answers it.
func tokenScopes(auth string) (write string, reads []string) {
	_, tok, ok := strings.Cut(auth, " ")
	if !ok {
		return "", nil
	}
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return "", nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", nil
	}
	var claims struct {
		AC string `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.AC == "" {
		return "", nil
	}
	var grants []struct {
		Scope      string
		Permission int
	}
	if json.Unmarshal([]byte(claims.AC), &grants) != nil {
		return "", nil
	}
	for _, g := range grants {
		if g.Permission&2 != 0 {
			write = g.Scope
			break
		}
	}
	slices.SortStableFunc(grants, func(a, b struct {
		Scope      string
		Permission int
	}) int {
		return b.Permission - a.Permission
	})
	for _, g := range grants {
		if g.Permission&1 != 0 {
			reads = append(reads, g.Scope)
		}
	}
	return write, reads
}

// --- the counts the job's log ends with ---

func (s *Server) count(f func(*stats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

// Summary is what the job's cache steps got, the line its log ends with.
func (s *Server) Summary() string {
	mb := func(n int64) string {
		if n >= 1e6 {
			return fmt.Sprintf("%d MB", int64(float64(n)/1e6+0.5))
		}
		return fmt.Sprintf("%d KB", max(1, int64(float64(n)/1e3+0.5)))
	}
	s.mu.Lock()
	st, pending, why := s.stats, len(s.pending), s.storeWhy
	s.mu.Unlock()
	var parts []string
	if st.hit > 0 {
		parts = append(parts, fmt.Sprintf("%d restored from the cache disk (%s)", st.hit, mb(st.hitBytes)))
	}
	if st.fill > 0 {
		parts = append(parts, fmt.Sprintf("%d downloaded to it (%s in %.1fs)", st.fill, mb(st.fillBytes), st.fillSecs))
	}
	if st.remote > 0 {
		parts = append(parts, fmt.Sprintf("%d restored from the cache server only", st.remote))
	}
	if st.none > 0 {
		parts = append(parts, fmt.Sprintf("%d not in the cache", st.none))
	}
	if st.saved > 0 {
		parts = append(parts, fmt.Sprintf("%d saved, kept on the disk (%s)", st.saved, mb(st.savedBytes)))
	}
	if st.savedRemote > 0 {
		parts = append(parts, fmt.Sprintf("%d saved to the cache server only", st.savedRemote))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d to upload to the cache bucket after the job", pending))
	}
	line := "fleetci cache: no actions/cache calls"
	if len(parts) > 0 {
		line = "fleetci cache: " + strings.Join(parts, ", ")
	}
	if why != "" {
		line += "; the cache disk wasn't used: " + why
	}
	return line + "\n"
}

// --- HTTP plumbing ---

// hop are the headers that don't go on.
var hop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailers": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Host": true, "Content-Length": true,
}

// errWritten is an error after the answer began: nothing more is sent.
type writer struct {
	http.ResponseWriter
	began bool
}

func (w *writer) WriteHeader(code int) {
	w.began = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(b []byte) (int, error) {
	w.began = true
	return w.ResponseWriter.Write(b)
}

// ReadFrom keeps net/http's own, which sends a file with sendfile.
func (w *writer) ReadFrom(r io.Reader) (int64, error) {
	w.began = true
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
}

// upstream sends r's method and headers on to u, with body.
func (s *Server) upstream(r *http.Request, method, u string, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if !hop[http.CanonicalHeaderKey(k)] {
			req.Header[k] = vs
		}
	}
	if body != nil {
		req.ContentLength = length
	}
	return s.client.Do(req)
}

func copyHeaders(w http.ResponseWriter, h http.Header, skip ...string) {
	for k, vs := range h {
		k = http.CanonicalHeaderKey(k)
		if hop[k] || slices.Contains(skip, k) {
			continue
		}
		w.Header()[k] = vs
	}
}

func reply(w http.ResponseWriter, status int, h http.Header, body []byte) {
	copyHeaders(w, h)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func replyJSON(w http.ResponseWriter, status int, h http.Header, v any) {
	b, _ := json.Marshal(v)
	h = h.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/json")
	reply(w, status, h, b)
}

// streamReply copies the L2's answer to the client, and to tee while it
// can. It says whether tee got the whole body.
func (s *Server) streamReply(w http.ResponseWriter, resp *http.Response, tee *os.File) bool {
	copyHeaders(w, resp.Header)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 1<<20)
	var got int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got += int64(n)
			if tee != nil {
				if _, werr := tee.Write(buf[:n]); werr != nil {
					s.logf("not keeping: %v", werr)
					tee = nil
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return false
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
	}
	return tee != nil && (resp.ContentLength < 0 || got == resp.ContentLength)
}

func (s *Server) localURL(r *http.Request, path string) string {
	host := r.Host
	if host == "" {
		host = fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	}
	return "http://" + host + path
}

// --- routes ---

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if !allowed(r) {
		http.Error(rw, "only this VM", http.StatusForbidden)
		return
	}
	w := &writer{ResponseWriter: rw}
	p := r.URL.Path
	if p != "/fleetci/ping" && p != "/fleetci/stats" {
		s.logf("%s %.80s from %s", r.Method, p, r.RemoteAddr)
	}
	err := s.route(w, r)
	if err != nil {
		s.logf("%s %s failed: %v", r.Method, p, err)
		if !w.began {
			reply(w, http.StatusBadGateway, nil, []byte(err.Error()))
		}
	}
}

func (s *Server) route(w *writer, r *http.Request) error {
	p := r.URL.Path
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case r.Method == http.MethodPost && p == svc+"GetCacheEntryDownloadURL":
		return s.restore(w, r)
	case r.Method == http.MethodPost && p == svc+"CreateCacheEntry":
		return s.create(w, r)
	case r.Method == http.MethodPost && p == svc+"FinalizeCacheEntryUpload":
		return s.finalize(w, r)
	case r.Method == http.MethodPost && p == "/fleetci/flush" && fromLoopback(r):
		return s.flush(w, r)
	case r.Method == http.MethodGet && p == "/fleetci/ping":
		reply(w, http.StatusOK, nil, []byte("ok"))
		return nil
	case r.Method == http.MethodGet && p == "/fleetci/stats":
		reply(w, http.StatusOK, nil, []byte(s.Summary()))
		return nil
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/fleetci/l1/"):
		s.serveLocal(w, r, last, true)
		return nil
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/fleetci/fill/"):
		return s.fill(w, r, last)
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/fleetci/put/"):
		return s.put(w, r, last)
	}
	return s.passthrough(w, r, r.Method, s.cfg.Upstream+r.URL.RequestURI())
}

// allowed: only this VM, by loopback, and its containers, by Docker's
// bridges.
func allowed(r *http.Request) bool {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	tcp, ok := la.(*net.TCPAddr)
	if !ok {
		return true // not over TCP: a test's
	}
	if tcp.IP.IsLoopback() {
		return true
	}
	return onBridge(tcp.IP)
}

// onBridge says ip is one of Docker's bridges here, docker0 or a
// network's br-…, looked at now: a job's compose network comes and goes.
func onBridge(ip net.IP) bool {
	ifs, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, i := range ifs {
		if i.Name != "docker0" && !strings.HasPrefix(i.Name, "br-") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) passthrough(w *writer, r *http.Request, method, u string) error {
	var body io.Reader
	if r.ContentLength != 0 {
		body = r.Body
	}
	resp, err := s.upstream(r, method, u, body, r.ContentLength)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	s.streamReply(w, resp, nil)
	return nil
}

// call sends the twirp call on with body, and reads the answer.
func (s *Server) call(r *http.Request, body []byte) (int, http.Header, []byte, error) {
	resp, err := s.upstream(r, http.MethodPost, s.cfg.Upstream+r.URL.RequestURI(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out, err
}

// nodeCall is a call to the node's cache service with the job's token.
func (s *Server) nodeCall(path string, v any, auth string) (int, map[string]any, error) {
	b, _ := json.Marshal(v)
	req, err := http.NewRequest(http.MethodPost, s.cfg.Node+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	c := *s.client
	c.Timeout = 30 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	out := map[string]any{}
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out, nil
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func truthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case nil:
		return false
	}
	return true
}

type restoreReq struct {
	Key            string   `json:"key"`
	Version        string   `json:"version"`
	RestoreKeys    []string `json:"restore_keys"`
	RestoreKeysAlt []string `json:"restoreKeys"`
}

func (s *Server) restore(w *writer, r *http.Request) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var req restoreReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	rks := req.RestoreKeys
	if len(rks) == 0 {
		rks = req.RestoreKeysAlt
	}
	disk := s.storeOK()
	auth := r.Header.Get("Authorization")
	_, scopes := tokenScopes(auth)
	l1URL := func(m string) string { return s.localURL(r, "/fleetci/l1/"+EntryID(m, req.Version)) }

	if s.cfg.Mode == ModeBucket {
		st, out, err := s.nodeCall("/lookup", map[string]any{"key": req.Key, "restore_keys": rks, "version": req.Version}, auth)
		if err != nil {
			s.logf("the node's cache unreachable: %v", err)
		}
		if err == nil && st == http.StatusOK && truthy(out["ok"]) {
			m, name, u := str(out, "matched_key"), str(out, "name"), str(out, "url")
			eid := EntryID(m, req.Version)
			var local string
			switch {
			case disk && s.have(eid) && s.sameEntry(eid, name):
				s.logf("hit %s", m)
				local = l1URL(m)
			case disk:
				s.logf("miss %s", m)
				local = s.localURL(r, "/fleetci/fill/"+s.link(fillLink{url: u, key: m, version: req.Version, src: name, scope: str(out, "scope")}))
			default:
				s.count(func(st *stats) { st.remote++ })
				local = u
			}
			replyJSON(w, http.StatusOK, nil, map[string]any{"ok": true, "matched_key": m, "signed_download_url": local})
			return nil
		}
		if err == nil && st != http.StatusOK {
			s.logf("the node's cache answered %d", st)
		}
		// Not in the bucket, or no answer: this job's own saves and what
		// the disk came with, as the bucket would have chosen.
	}
	if s.diskOnlySaves() {
		m, ok := "", false
		if disk {
			m, ok = s.localMatch(req.Key, rks, req.Version, scopes)
		}
		if !ok {
			s.count(func(st *stats) { st.none++ })
			replyJSON(w, http.StatusOK, nil, map[string]any{"ok": false})
			return nil
		}
		s.logf("hit %s", m)
		replyJSON(w, http.StatusOK, nil, map[string]any{"ok": true, "matched_key": m, "signed_download_url": l1URL(m)})
		return nil
	}

	status, h, body, err := s.call(r, raw)
	if err != nil {
		// The L2 is out of reach: the disk answers, if it can.
		m, ok := "", false
		if disk {
			m, ok = s.localMatch(req.Key, rks, req.Version, scopes)
		}
		if !ok {
			s.logf("L2 unreachable: %v", err)
			reply(w, http.StatusBadGateway, nil, []byte(err.Error()))
			return nil
		}
		s.logf("L2 unreachable (%v): restoring %s from the disk", err, m)
		replyJSON(w, http.StatusOK, nil, map[string]any{"ok": true, "matched_key": m, "signed_download_url": l1URL(m)})
		return nil
	}
	if status != http.StatusOK {
		reply(w, status, h, body)
		return nil
	}
	out := map[string]any{}
	if json.Unmarshal(body, &out) != nil {
		reply(w, status, h, body)
		return nil
	}
	m, u := str(out, "matched_key", "matchedKey"), str(out, "signed_download_url", "signedDownloadUrl")
	if !truthy(out["ok"]) || m == "" || u == "" {
		s.count(func(st *stats) { st.none++ })
		reply(w, status, h, body)
		return nil
	}
	if !disk {
		s.count(func(st *stats) { st.remote++ })
		reply(w, status, h, body)
		return nil
	}
	eid := EntryID(m, req.Version)
	var local string
	if s.have(eid) && s.sameEntry(eid, srcOf(u)) {
		s.logf("hit %s", m)
		local = l1URL(m)
	} else {
		s.logf("miss %s", m)
		local = s.localURL(r, "/fleetci/fill/"+s.link(fillLink{url: u, key: m, version: req.Version}))
	}
	delete(out, "signedDownloadUrl")
	out["signed_download_url"] = local
	replyJSON(w, status, h, out)
	return nil
}

var rangeRE = regexp.MustCompile(`^\s*bytes=(\d*)-(\d*)\s*$`)

func (s *Server) serveLocal(w http.ResponseWriter, r *http.Request, eid string, hit bool) {
	f, err := os.Open(s.blobPath(eid))
	if err != nil {
		reply(w, http.StatusNotFound, nil, []byte("not on the disk"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		reply(w, http.StatusNotFound, nil, []byte("not on the disk"))
		return
	}
	size := info.Size()
	now := time.Now()
	_ = os.Chtimes(s.blobPath(eid), now, now)
	// BuildKit reads at offsets, through Azure's SDK (x-ms-range); one
	// range is served, anything else whole.
	rg := r.Header.Get("x-ms-range")
	if rg == "" {
		rg = r.Header.Get("Range")
	}
	m := rangeRE.FindStringSubmatch(rg)
	if hit && (m == nil || m[1] == "" || m[1] == "0") {
		s.count(func(st *stats) { st.hit++; st.hitBytes += size }) // once per restore, not per range
	}
	start, end := int64(0), size-1
	status := http.StatusOK
	if m != nil && (m[1] != "" || m[2] != "") {
		if m[1] != "" {
			start, _ = strconv.ParseInt(m[1], 10, 64)
			if m[2] != "" {
				e, _ := strconv.ParseInt(m[2], 10, 64)
				end = min(e, size-1)
			}
		} else {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			start = max(0, size-n)
		}
		if start >= size || end < start {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	// A LimitedReader of the file itself, so net/http sends it with
	// sendfile, not through this process.
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.Copy(w, &io.LimitedReader{R: f, N: end - start + 1})
}

// fill serves a download the disk lacks and keeps a copy.
func (s *Server) fill(w *writer, r *http.Request, token string) error {
	l, ok := s.linked(token).(fillLink)
	if !ok {
		reply(w, http.StatusNotFound, nil, []byte("no such download"))
		return nil
	}
	// A bucket's entry is named by the node; the cache server's by its URL.
	src := l.src
	if src == "" {
		src = srcOf(l.url)
	}
	eid := EntryID(l.key, l.version)
	if s.storeOK() {
		// To the disk in parallel ranges, then from it: one stream from
		// the cache server would be several times slower.
		tmp := filepath.Join(s.cfg.Store, "tmp", eid+"."+randHex(4)+".ranges")
		start := time.Now()
		ok, err := s.fetchRanges(r, l.url, tmp)
		if err == nil && ok {
			if err = s.commit(tmp, l.key, l.version, src, l.scope); err == nil {
				secs := time.Since(start).Seconds()
				s.logf("fetched %s in %.1fs", l.key, secs)
				var size int64
				if st, err := os.Stat(s.blobPath(eid)); err == nil {
					size = st.Size()
				}
				s.count(func(st *stats) { st.fill++; st.fillBytes += size; st.fillSecs += secs })
				s.serveLocal(w, r, eid, false)
				return nil
			}
		}
		if err != nil {
			s.logf("not keeping (ranges): %v", err)
		}
		_ = os.Remove(tmp)
	}
	start := time.Now()
	resp, err := s.upstream(r, http.MethodGet, l.url, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tmp string
	var tee *os.File
	if resp.StatusCode == http.StatusOK && s.storeOK() {
		tmp = filepath.Join(s.cfg.Store, "tmp", eid+"."+randHex(4)+".fill")
		if tee, err = os.Create(tmp); err != nil {
			s.logf("not keeping: %v", err)
			tee = nil
		}
	}
	whole := s.streamReply(w, resp, tee)
	kept := false
	if tee != nil {
		if err := tee.Close(); err != nil {
			whole = false
		}
		if whole {
			if err := s.commit(tmp, l.key, l.version, src, l.scope); err != nil {
				s.logf("not keeping: %v", err)
			} else {
				kept = true
				var size int64
				if st, err := os.Stat(s.blobPath(eid)); err == nil {
					size = st.Size()
				}
				secs := time.Since(start).Seconds()
				s.count(func(st *stats) { st.fill++; st.fillBytes += size; st.fillSecs += secs })
			}
		}
		if !kept {
			_ = os.Remove(tmp)
		}
	}
	if !kept && resp.StatusCode == http.StatusOK {
		s.count(func(st *stats) { st.remote++ })
	}
	return nil
}

var contentRangeRE = regexp.MustCompile(`^bytes 0-0/(\d+)$`)

// fetchRanges fetches u into tmp in parallel ranges. ok is false when the
// server doesn't take ranges (nothing written).
func (s *Server) fetchRanges(r *http.Request, u, tmp string) (bool, error) {
	get := func(lo, hi int64) (*http.Response, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi))
		return s.client.Do(req)
	}
	resp, err := get(0, 0)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	m := contentRangeRE.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if resp.StatusCode != http.StatusPartialContent || m == nil {
		return false, nil
	}
	size, _ := strconv.ParseInt(m[1], 10, 64)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	type rng struct{ lo, hi int64 }
	parts := make(chan rng, size/part+1)
	for o := int64(0); o < size; o += part {
		parts <- rng{o, min(o+part, size) - 1}
	}
	close(parts)
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		first error
	)
	failed := func() bool { errMu.Lock(); defer errMu.Unlock(); return first != nil }
	for range min(partsAtOnce, int(size/part+1)) {
		wg.Go(func() {
			for p := range parts {
				if failed() {
					return
				}
				if err := func() error {
					resp, err := get(p.lo, p.hi)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusPartialContent {
						return fmt.Errorf("range %d-%d: %d", p.lo, p.hi, resp.StatusCode)
					}
					n, err := io.Copy(io.NewOffsetWriter(f, p.lo), io.LimitReader(resp.Body, p.hi-p.lo+1))
					if err == nil && n != p.hi-p.lo+1 {
						err = fmt.Errorf("range %d-%d ended at %d", p.lo, p.hi, p.lo+n)
					}
					return err
				}(); err != nil {
					errMu.Lock()
					if first == nil {
						first = err
					}
					errMu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if first != nil {
		return false, first
	}
	return true, f.Close()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type entryReq struct {
	Key          string `json:"key"`
	Version      string `json:"version"`
	SizeBytes    any    `json:"size_bytes"`
	SizeBytesAlt any    `json:"sizeBytes"`
}

func (s *Server) create(w *writer, r *http.Request) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var req entryReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	eid := EntryID(req.Key, req.Version)
	if s.diskOnlySaves() {
		if !s.storeOK() {
			replyJSON(w, http.StatusOK, nil, map[string]any{"ok": false, "message": "no cache disk"})
			return nil
		}
		if s.have(eid) && s.savedInScope(eid, r.Header.Get("Authorization")) {
			// An entry never changes once saved; GitHub's answer.
			replyJSON(w, http.StatusConflict, nil, map[string]any{"code": "already_exists",
				"msg": "cache entry with the same key, version, and scope already exists"})
			return nil
		}
		_ = os.RemoveAll(s.tmpDir(eid))
		replyJSON(w, http.StatusOK, nil, map[string]any{"ok": true, "signed_upload_url": s.localURL(r, "/fleetci/put/"+s.link(putLink{eid: eid}))})
		return nil
	}
	status, h, body, err := s.call(r, raw)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !s.storeOK() {
		reply(w, status, h, body)
		return nil
	}
	out := map[string]any{}
	if json.Unmarshal(body, &out) != nil {
		reply(w, status, h, body)
		return nil
	}
	u := str(out, "signed_upload_url", "signedUploadUrl")
	if !truthy(out["ok"]) || u == "" {
		reply(w, status, h, body)
		return nil
	}
	_ = os.RemoveAll(s.tmpDir(eid))
	delete(out, "signedUploadUrl")
	s.mu.Lock()
	s.uploads[eid] = srcOf(u)
	s.mu.Unlock()
	out["signed_upload_url"] = s.localURL(r, "/fleetci/put/"+s.link(putLink{url: u, eid: eid}))
	replyJSON(w, status, h, out)
	return nil
}

// savedInScope says the disk's entry is in the scope this job saves to. A
// pull request's disk starts with its base branch's entries, and the pull
// request may save the same key in its own scope, as on GitHub.
func (s *Server) savedInScope(eid, auth string) bool {
	write, _ := tokenScopes(auth)
	m, err := s.readMeta(eid)
	if err != nil {
		return true
	}
	return write == "" || m.Scope == "" || m.Scope == write
}

var blockIDRE = regexp.MustCompile(`<(?:Latest|Committed|Uncommitted)>([^<]*)</(?:Latest|Committed|Uncommitted)>`)

func blockName(id string) string {
	sum := sha1.Sum([]byte(id))
	return "block-" + hex.EncodeToString(sum[:])
}

// put is the archive's upload, Azure's way: one PUT, or blocks and then
// their list. Each goes on to the L2, a copy to the disk.
func (s *Server) put(w *writer, r *http.Request, token string) error {
	l, ok := s.linked(token).(putLink)
	if !ok {
		reply(w, http.StatusNotFound, nil, []byte("no such upload"))
		return nil
	}
	q := r.URL.Query()
	u := l.url
	if len(q) > 0 && u != "" {
		extra := url.Values{}
		for k, vs := range q {
			extra.Set(k, vs[0])
		}
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + extra.Encode()
	}
	comp := q.Get("comp")
	d := s.tmpDir(l.eid)
	var name string
	switch comp {
	case "blocklist":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		status, h, body := http.StatusCreated, http.Header{}, []byte{}
		if u != "" {
			resp, err := s.upstream(r, http.MethodPut, u, bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				return err
			}
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			status, h = resp.StatusCode, resp.Header
		}
		if st, err := os.Stat(d); status < 300 && err == nil && st.IsDir() {
			if err := joinBlocks(d, raw); err != nil {
				s.logf("not keeping: %v", err)
				_ = os.RemoveAll(d)
			}
		}
		putReply(w, status, h, body)
		return nil
	case "block":
		name = blockName(q.Get("blockid"))
	case "":
		name = "whole"
	default:
		if u == "" {
			reply(w, http.StatusBadRequest, nil, []byte("not an upload"))
			return nil
		}
		return s.passthrough(w, r, http.MethodPut, u)
	}
	if u == "" {
		s.keep(w, r, d, name)
		return nil
	}
	var tee *os.File
	if s.storeOK() {
		if err := os.MkdirAll(d, 0o755); err == nil {
			tee, err = os.Create(filepath.Join(d, name+".part"))
			if err != nil {
				s.logf("not keeping: %v", err)
				tee = nil
			}
		} else {
			s.logf("not keeping: %v", err)
		}
	}
	body := &teeReader{r: r.Body, tee: tee, onFail: func(err error) {
		s.logf("not keeping: %v", err)
		_ = os.RemoveAll(d)
	}}
	resp, err := s.upstream(r, http.MethodPut, u, body, r.ContentLength)
	if err != nil {
		if body.tee != nil {
			body.tee.Close()
		}
		return err
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if body.tee != nil {
		if err := body.tee.Close(); err != nil {
			s.logf("not keeping: %v", err)
		} else if resp.StatusCode < 300 {
			if err := os.Rename(filepath.Join(d, name+".part"), filepath.Join(d, name)); err != nil {
				s.logf("not keeping: %v", err)
			}
		}
	}
	putReply(w, resp.StatusCode, resp.Header, out)
	return nil
}

// joinBlocks writes d/whole from the blocks the list names, in order.
func joinBlocks(d string, list []byte) error {
	part := filepath.Join(d, "whole.part")
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	for _, m := range blockIDRE.FindAllSubmatch(list, -1) {
		f, err := os.Open(filepath.Join(d, blockName(string(m[1]))))
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, f)
		f.Close()
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(part, filepath.Join(d, "whole"))
}

// teeReader copies what is read to tee until writing it fails.
type teeReader struct {
	r      io.Reader
	tee    *os.File
	onFail func(error)
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.tee != nil {
		if _, werr := t.tee.Write(p[:n]); werr != nil {
			t.tee.Close()
			t.tee = nil
			t.onFail(werr)
		}
	}
	return n, err
}

// putReply is an upload's answer, with the request id Azure's would
// carry: BuildKit's client intermittently fails without one.
func putReply(w http.ResponseWriter, status int, h http.Header, body []byte) {
	h = h.Clone()
	if h == nil {
		h = http.Header{}
	}
	if h.Get("x-ms-request-id") == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		h.Set("x-ms-request-id", fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]))
	}
	reply(w, status, h, body)
}

// keep is an upload with no cache server behind: to the disk alone.
func (s *Server) keep(w http.ResponseWriter, r *http.Request, d, name string) {
	err := func() error {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(d, name+".part"))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r.Body); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(filepath.Join(d, name+".part"), filepath.Join(d, name))
	}()
	if err != nil {
		s.logf("not keeping: %v", err)
		_ = os.RemoveAll(d)
		reply(w, http.StatusInternalServerError, nil, []byte(err.Error()))
		return
	}
	putReply(w, http.StatusCreated, nil, nil)
}

// flush uploads the job's saves to the bucket, with links the node signs
// for the job's own token; fleetci asks for it after the job.
func (s *Server) flush(w *writer, r *http.Request) error {
	s.mu.Lock()
	todo := make(map[string]pendingSave, len(s.pending))
	for k, v := range s.pending {
		todo[k] = v
	}
	s.mu.Unlock()
	done := map[string]int64{"uploaded": 0, "bytes": 0, "exists": 0, "failed": 0}
	for eid, p := range todo {
		size, exists, err := s.upload(r, eid, p)
		switch {
		case err != nil:
			done["failed"]++
			s.logf("not uploaded: %s %v", p.key, err)
			continue
		case exists:
			done["exists"]++
		default:
			done["uploaded"]++
			done["bytes"] += size
		}
		s.mu.Lock()
		delete(s.pending, eid)
		s.mu.Unlock()
	}
	s.logf("flushed %v", done)
	replyJSON(w, http.StatusOK, nil, done)
	return nil
}

// upload sends one save to the bucket.
func (s *Server) upload(r *http.Request, eid string, p pendingSave) (int64, bool, error) {
	st, out, err := s.nodeCall("/upload", map[string]any{"key": p.key, "version": p.version}, p.auth)
	if err != nil {
		return 0, false, err
	}
	if st == http.StatusConflict {
		return 0, true, nil
	}
	if st != http.StatusOK || !truthy(out["ok"]) {
		return 0, false, fmt.Errorf("the node answered %d", st)
	}
	f, err := os.Open(s.blobPath(eid))
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, str(out, "url"), f)
	if err != nil {
		return 0, false, err
	}
	req.ContentLength = info.Size()
	if hs, ok := out["headers"].(map[string]any); ok {
		for k, v := range hs {
			if v, ok := v.(string); ok {
				req.Header.Set(k, v)
			}
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, false, fmt.Errorf("upload: %d", resp.StatusCode)
	}
	if m, err := s.readMeta(eid); err == nil {
		m.Src, m.Scope = str(out, "name"), str(out, "scope")
		_ = s.writeMeta(eid, m)
	}
	return info.Size(), false, nil
}

func sizeOf(v any) int64 {
	switch v := v.(type) {
	case float64:
		return int64(v)
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return -1
}

func (s *Server) finalize(w *writer, r *http.Request) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var req entryReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	var (
		status int
		h      http.Header
		body   []byte
	)
	if s.diskOnlySaves() {
		status, body = http.StatusOK, []byte(`{"ok":true,"entry_id":"0"}`)
	} else if status, h, body, err = s.call(r, raw); err != nil {
		return err
	}
	auth := r.Header.Get("Authorization")
	eid := EntryID(req.Key, req.Version)
	d := s.tmpDir(eid)
	out := map[string]any{}
	ok := status == http.StatusOK && json.Unmarshal(body, &out) == nil && truthy(out["ok"])
	whole := filepath.Join(d, "whole")
	kept := false
	if ok && isFile(whole) {
		want := sizeOf(req.SizeBytes)
		if req.SizeBytes == nil {
			want = sizeOf(req.SizeBytesAlt)
		}
		if info, err := os.Stat(whole); err == nil && (want < 0 || info.Size() == want) {
			s.mu.Lock()
			src := s.uploads[eid]
			delete(s.uploads, eid)
			s.mu.Unlock()
			write, _ := tokenScopes(auth)
			if err := s.commit(whole, req.Key, req.Version, src, write); err != nil {
				s.logf("not keeping: %v", err)
			} else {
				kept = true
				size := info.Size()
				s.count(func(st *stats) { st.saved++; st.savedBytes += size })
				if s.cfg.Mode == ModeBucket {
					s.mu.Lock()
					s.pending[eid] = pendingSave{key: req.Key, version: req.Version, auth: auth}
					s.mu.Unlock()
				}
			}
		}
	}
	if s.diskOnlySaves() && !kept {
		_ = os.RemoveAll(d)
		replyJSON(w, http.StatusOK, nil, map[string]any{"ok": false, "message": "not kept on the cache disk"})
		return nil
	}
	if ok && !kept {
		s.count(func(st *stats) { st.savedRemote++ })
	}
	_ = os.RemoveAll(d)
	reply(w, status, h, body)
	return nil
}

// ParseArgs reads the L1's arguments after its port, as the VM's script
// passes them: UPSTREAM_URL STORE_DIR [any] [remote=off | remote=bucket
// node=URL].
func ParseArgs(args []string) (Config, error) {
	if len(args) < 2 {
		return Config{}, errors.New("usage: fleetci-l1 PORT UPSTREAM_URL STORE_DIR [any] [remote=off | remote=bucket node=URL]")
	}
	c := Config{Upstream: args[0], Store: args[1]}
	for _, a := range args[2:] {
		switch {
		case a == "any":
			c.AnyStore = true
		case a == "remote=off":
			c.Mode = ModeOff
		case a == "remote=bucket":
			c.Mode = ModeBucket
		case strings.HasPrefix(a, "node="):
			c.Node = strings.TrimPrefix(a, "node=")
		default:
			return Config{}, fmt.Errorf("unknown argument %q", a)
		}
	}
	if c.Mode == ModeBucket && c.Node == "" {
		return Config{}, errors.New("remote=bucket needs node=URL")
	}
	return c, nil
}
