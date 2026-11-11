package gocache

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/jobtoken"
)

// goCmd plays the go command: it writes requests to a client and reads
// its responses.
type goCmd struct {
	t    *testing.T
	in   *io.PipeWriter
	dec  *json.Decoder
	done chan error
	id   int64
}

func startClient(t *testing.T, c *Client) *goCmd {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	g := &goCmd{t: t, in: inW, dec: json.NewDecoder(bufio.NewReader(outR)), done: make(chan error, 1)}
	go func() { g.done <- c.Serve(inR, outW); outW.Close() }()
	var hello response
	if err := g.dec.Decode(&hello); err != nil || len(hello.KnownCommands) != 3 {
		t.Fatalf("hello = %+v, %v", hello, err)
	}
	return g
}

func (g *goCmd) do(req request, body []byte) response {
	g.t.Helper()
	g.id++
	req.ID = g.id
	b, _ := json.Marshal(req)
	fmt.Fprintf(g.in, "%s\n", b)
	if len(body) > 0 {
		fmt.Fprintf(g.in, "%q\n", base64.StdEncoding.EncodeToString(body))
	}
	var r response
	if err := g.dec.Decode(&r); err != nil {
		g.t.Fatal(err)
	}
	if r.ID != req.ID || r.Err != "" {
		g.t.Fatalf("%s: response %+v", req.Command, r)
	}
	return r
}

func (g *goCmd) close() {
	g.t.Helper()
	g.do(request{Command: "close"}, nil)
	if err := <-g.done; err != nil {
		g.t.Fatal(err)
	}
}

func id(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// TestShared puts entries from one job and gets them from others with
// empty local caches, as on fresh VMs, across refs and projects.
func TestShared(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(&Server{Store: Dir(t.TempDir()), Keys: []ed25519.PublicKey{pub}, Log: hclog.NewNullLogger()})
	defer srv.Close()
	token := func(project, ref string, protected bool) string {
		tok, err := jobtoken.Sign(priv, jobtoken.Claims{Forge: "gitlab", Instance: "gitlab.com", Project: project, Ref: ref, Protected: protected, Job: "1", Expires: time.Now().Add(time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	job := func(tok string) *goCmd {
		return startClient(t, &Client{Dir: t.TempDir(), URL: srv.URL, Token: tok})
	}
	put := func(tok, action, body string) {
		g := job(tok)
		r := g.do(request{Command: "put", ActionID: id(action), OutputID: id(body), BodySize: int64(len(body))}, []byte(body))
		if b, err := os.ReadFile(r.DiskPath); err != nil || string(b) != body {
			t.Fatalf("put's DiskPath holds %q, %v", b, err)
		}
		g.close()
	}
	// get returns the body a fresh job gets for action, or "miss".
	get := func(tok, action string) string {
		g := job(tok)
		defer g.close()
		r := g.do(request{Command: "get", ActionID: id(action)}, nil)
		if r.Miss {
			return "miss"
		}
		b, err := os.ReadFile(r.DiskPath)
		if err != nil || !bytes.Equal(r.OutputID, id(string(b))) || r.Size != int64(len(b)) || r.Time == nil {
			t.Fatalf("get %s: %+v, %q, %v", action, r, b, err)
		}
		return string(b)
	}

	main, mr, other := token("42", "main", true), token("42", "feature", false), token("43", "main", true)
	put(main, "a", "built on main")
	put(mr, "b", "built on feature")
	put(mr, "a", "planted by feature")
	put(main, "empty", "")

	for _, tc := range []struct {
		name, tok, action, want string
	}{
		{"main reads its own", main, "a", "built on main"},
		{"a merge request reads main's", token("42", "other", false), "a", "built on main"},
		{"main never reads a merge request's", main, "b", "miss"},
		{"a ref reads its own first", mr, "a", "planted by feature"},
		{"a ref reads its own", mr, "b", "built on feature"},
		{"another project reads nothing", other, "a", "miss"},
		{"an empty entry", main, "empty", ""},
	} {
		if got := get(tc.tok, tc.action); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestLocalOnly: with no server, or one that refuses the token, the
// client is a local cache and the build goes on.
func TestLocalOnly(t *testing.T) {
	refuse := httptest.NewServer(&Server{Store: Dir(t.TempDir()), Log: hclog.NewNullLogger()})
	defer refuse.Close()
	for name, c := range map[string]*Client{
		"no server":     {Dir: t.TempDir()},
		"refused token": {Dir: t.TempDir(), URL: refuse.URL, Token: "fjt1.x.y"},
		"server down":   {Dir: t.TempDir(), URL: "http://127.0.0.1:1", Token: "fjt1.x.y"},
	} {
		var log strings.Builder
		c.Log = &log
		g := startClient(t, c)
		if r := g.do(request{Command: "get", ActionID: id("a")}, nil); !r.Miss {
			t.Errorf("%s: get before put = %+v", name, r)
		}
		g.do(request{Command: "put", ActionID: id("a"), OutputID: id("x"), BodySize: 1}, []byte("x"))
		if r := g.do(request{Command: "get", ActionID: id("a")}, nil); r.Miss || r.Size != 1 {
			t.Errorf("%s: get after put = %+v", name, r)
		}
		g.do(request{Command: "put", ActionID: id("empty"), OutputID: id("")}, nil)
		g.close()
		if name != "no server" && !strings.Contains(log.String(), "going on without it") {
			t.Errorf("%s: log %q", name, log.String())
		}
	}
}

func TestServerRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(&Server{Store: Dir(t.TempDir()), Keys: []ed25519.PublicKey{pub}, Log: hclog.NewNullLogger()})
	defer srv.Close()
	good, _ := jobtoken.Sign(priv, jobtoken.Claims{Forge: "gitlab", Instance: "gitlab.com", Project: "42", Ref: "main", Expires: time.Now().Add(time.Hour).Unix()})
	bad, _ := jobtoken.Sign(priv, jobtoken.Claims{Forge: "gitlab", Instance: "../x", Project: "42", Ref: "main", Expires: time.Now().Add(time.Hour).Unix()})
	action := fmt.Sprintf("%x", id("a"))
	for _, tc := range []struct {
		name, method, path, tok string
		want                    int
	}{
		{"no token", "GET", "/v1/cache/" + action, "", 401},
		{"path in the claims", "GET", "/v1/cache/" + action, bad, 401},
		{"not an action ID", "GET", "/v1/cache/../a", good, 404},
		{"put without output ID", "PUT", "/v1/cache/" + action, good, 400},
		{"miss", "GET", "/v1/cache/" + action, good, 404},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+tc.tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
}
