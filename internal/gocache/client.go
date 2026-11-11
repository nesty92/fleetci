package gocache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/s2"
)

// The go command's GOCACHEPROG protocol (cmd/go/internal/cacheprog): JSON
// requests on stdin, each "put" with a body followed by the body as a
// JSON base64 string; responses on stdout, in any order, the first one
// listing the commands the program knows.
type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Client is the go command's cache program. It keeps entries in Dir, and
// with a URL and a token also gets and puts them on fleetci's server. The
// server failing makes a miss, never a failed build.
type Client struct {
	Dir   string
	URL   string // the server's base URL; "" is a local cache only
	Token string
	HTTP  *http.Client
	// Log, if set, gets one line a problem and the counts at the end.
	Log io.Writer

	out     sync.Mutex
	enc     *json.Encoder
	uploads sync.WaitGroup
	slots   chan struct{}
	down    atomic.Bool // the server failed: stop asking it

	localHits, remoteHits, misses, puts, putFails atomic.Int64
	remoteBytes                                   atomic.Int64
}

// Serve runs the protocol until the go command sends close or stdin ends.
func (c *Client) Serve(in io.Reader, out io.Writer) error {
	for _, d := range []string{"a", "o"} {
		if err := os.MkdirAll(filepath.Join(c.Dir, d), 0o755); err != nil {
			return err
		}
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: time.Minute}
	}
	c.slots = make(chan struct{}, 32)
	if c.URL == "" || c.Token == "" {
		c.down.Store(true)
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	bw := bufio.NewWriter(out)
	c.enc = json.NewEncoder(bw)
	c.send(bw, response{KnownCommands: []string{"get", "put", "close"}})

	dec := json.NewDecoder(bufio.NewReader(in))
	var gets sync.WaitGroup
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		switch req.Command {
		case "get":
			gets.Add(1)
			go func() {
				defer gets.Done()
				c.send(bw, c.get(req))
			}()
		case "put":
			var body []byte
			if req.BodySize > 0 {
				if err := dec.Decode(&body); err != nil {
					return fmt.Errorf("put body: %w", err)
				}
			}
			c.send(bw, c.put(req, body))
		case "close":
			gets.Wait()
			c.uploads.Wait()
			c.stats()
			c.send(bw, response{ID: req.ID})
			return nil
		default:
			c.send(bw, response{ID: req.ID, Err: "unknown command " + req.Command})
		}
	}
	gets.Wait()
	c.uploads.Wait()
	return nil
}

func (c *Client) send(bw *bufio.Writer, r response) {
	c.out.Lock()
	defer c.out.Unlock()
	_ = c.enc.Encode(r)
	_ = bw.Flush()
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, "fleetci-gocache: "+format+"\n", args...)
	}
}

// fail stops asking the server after its first failure: a cache that is
// down costs one timeout, not one per entry.
func (c *Client) fail(err error) {
	if !c.down.Swap(true) {
		c.logf("server: %v; going on without it", err)
	}
}

func (c *Client) stats() {
	c.logf("%d local hits, %d from the server (%d MB), %d misses, %d puts (%d not uploaded)",
		c.localHits.Load(), c.remoteHits.Load(), c.remoteBytes.Load()>>20, c.misses.Load(), c.puts.Load(), c.putFails.Load())
}

func (c *Client) actionPath(id string) string { return filepath.Join(c.Dir, "a", id) }
func (c *Client) outputPath(id string) string { return filepath.Join(c.Dir, "o", id) }

// local is an entry this machine has: "<output id> <size> <unix nano>".
func (c *Client) local(action string) (response, bool) {
	b, err := os.ReadFile(c.actionPath(action))
	if err != nil {
		return response{}, false
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		return response{}, false
	}
	out, err1 := hex.DecodeString(f[0])
	size, err2 := strconv.ParseInt(f[1], 10, 64)
	ns, err3 := strconv.ParseInt(f[2], 10, 64)
	p := c.outputPath(f[0])
	if st, err := os.Stat(p); err1 != nil || err2 != nil || err3 != nil || err != nil || st.Size() != size {
		return response{}, false
	}
	t := time.Unix(0, ns)
	return response{OutputID: out, Size: size, Time: &t, DiskPath: p}, true
}

func (c *Client) remember(action, output string, size int64, t time.Time) error {
	return writeAtomic(c.actionPath(action), func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s %d %d\n", output, size, t.UnixNano())
		return err
	})
}

func (c *Client) get(req request) response {
	action := hex.EncodeToString(req.ActionID)
	if r, ok := c.local(action); ok {
		c.localHits.Add(1)
		r.ID = req.ID
		return r
	}
	if r, ok := c.fetch(action); ok {
		c.remoteHits.Add(1)
		r.ID = req.ID
		return r
	}
	c.misses.Add(1)
	return response{ID: req.ID, Miss: true}
}

// fetch gets an entry from the server into Dir.
func (c *Client) fetch(action string) (response, bool) {
	if c.down.Load() {
		return response{}, false
	}
	c.slots <- struct{}{}
	defer func() { <-c.slots }()
	httpReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, c.URL+"/v1/cache/"+action, nil)
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		c.fail(err)
		return response{}, false
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return response{}, false
	case res.StatusCode != http.StatusOK:
		c.failOn(res, "get")
		return response{}, false
	}
	output := res.Header.Get(headerOutputID)
	t, terr := time.Parse(time.RFC3339Nano, res.Header.Get(headerTime))
	if !hexID.MatchString(output) || terr != nil {
		return response{}, false
	}
	p := c.outputPath(output)
	wire := &counter{r: res.Body}
	var n int64
	err = writeAtomic(p, func(w io.Writer) error {
		var err error
		n, err = io.Copy(w, s2.NewReader(wire))
		return err
	})
	if err != nil || wire.n != res.ContentLength {
		return response{}, false
	}
	c.remoteBytes.Add(wire.n)
	_ = c.remember(action, output, n, t)
	out, _ := hex.DecodeString(output)
	return response{OutputID: out, Size: n, Time: &t, DiskPath: p}, true
}

func (c *Client) put(req request, body []byte) response {
	c.puts.Add(1)
	action, output := hex.EncodeToString(req.ActionID), hex.EncodeToString(req.OutputID)
	if int64(len(body)) != req.BodySize {
		return response{ID: req.ID, Err: "body size mismatch"}
	}
	p := c.outputPath(output)
	if st, err := os.Stat(p); err != nil || st.Size() != req.BodySize {
		if err := writeAtomic(p, func(w io.Writer) error { _, err := w.Write(body); return err }); err != nil {
			return response{ID: req.ID, Err: err.Error()}
		}
	}
	now := time.Now()
	if err := c.remember(action, output, req.BodySize, now); err != nil {
		return response{ID: req.ID, Err: err.Error()}
	}
	if !c.down.Load() {
		c.uploads.Add(1)
		go func() {
			defer c.uploads.Done()
			if !c.upload(action, output, p, req.BodySize) {
				c.putFails.Add(1)
			}
		}()
	}
	return response{ID: req.ID, DiskPath: p}
}

func (c *Client) upload(action, output, path string, size int64) bool {
	if c.down.Load() {
		return false
	}
	c.slots <- struct{}{}
	defer func() { <-c.slots }()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// Entries go compressed: object files and binaries shrink to about a
	// third, and S2 compresses faster than a network carries.
	var body bytes.Buffer
	z := s2.NewWriter(&body, s2.WriterConcurrency(1))
	if n, err := io.Copy(z, f); err != nil || n != size || z.Close() != nil {
		return false
	}
	httpReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, c.URL+"/v1/cache/"+action, &body)
	httpReq.ContentLength = int64(body.Len())
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	httpReq.Header.Set(headerOutputID, output)
	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		c.fail(err)
		return false
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		c.failOn(res, "put")
		return false
	}
	return true
}

// failOn stops asking the server after a response that says it can't
// help this job: an error of its own or a refused token. A refused
// entry is that entry's problem only.
func (c *Client) failOn(res *http.Response, what string) {
	if res.StatusCode >= 500 || res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		c.fail(fmt.Errorf("%s: %s", what, res.Status))
		return
	}
	c.logf("server: %s: %s", what, res.Status)
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// writeAtomic writes path through a temporary file, so a reader never sees
// part of it.
func writeAtomic(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
