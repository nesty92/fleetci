package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// An org's repos are listed and polled, archived ones left out, and their
// jobs' runners registered on the org.
func TestAnOrgsJobsGetRunnersOnTheOrg(t *testing.T) {
	f := newForge(t, queuedJob(1, 7), queuedJob(2, 8))
	f.orgRepos = []map[string]any{{"full_name": "o/r"}, {"full_name": "o/old", "archived": true}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dispatchWith(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"}, func(c *Config) {
		c.Repos = nil
		c.Orgs = []string{"o"}
	})
	eventuallySettled(t, f, 5*time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.scopes {
		if s != "/orgs/o" {
			t.Fatalf("runners registered on %v; want all on /orgs/o", f.scopes)
		}
	}
	for _, r := range f.reqs {
		if strings.HasPrefix(r.path, "/repos/o/old/") {
			t.Fatalf("an archived repo was polled: %s", r.path)
		}
	}
}

// A repo listed on its own but whose org is served too gets org runners.
func TestAnOrgWinsOverItsRepoListedAlone(t *testing.T) {
	d := &Dispatcher{orgs: map[string]bool{"acme": true}, repos: map[string]bool{"acme/x": true, "me/y": true}}
	for repo, want := range map[string]string{"acme/x": "/orgs/acme", "ACME/z": "/orgs/ACME", "me/y": "/repos/me/y"} {
		if got := d.scope(repo); got != want {
			t.Errorf("scope(%q) = %q; want %q", repo, got, want)
		}
	}
	if !d.serves("Acme/new") || d.serves("other/x") || !d.serves("ME/Y") {
		t.Error("serves is wrong about an org's new repo, a stranger's or case")
	}
}

func TestAuthConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	key := writeAppKey(t, dir)
	if err := os.WriteFile(tok, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		err  string // "" for none
	}{
		{"nothing to serve", Config{TokenFile: tok}, "repos or orgs"},
		{"repos with a token", Config{Repos: []string{"o/r"}, TokenFile: tok}, ""},
		{"orgs with a token", Config{Orgs: []string{"o"}, TokenFile: tok}, ""},
		{"an org with a slash", Config{Orgs: []string{"o/r"}, TokenFile: tok}, "org's name"},
		{"no auth", Config{Orgs: []string{"o"}}, "token_file, or app_id"},
		{"both", Config{Orgs: []string{"o"}, TokenFile: tok, AppID: 7, AppPrivateKeyFile: key}, "not both"},
		{"app without key", Config{Orgs: []string{"o"}, AppID: 7}, "go together"},
		{"app", Config{Orgs: []string{"o"}, AppID: 7, AppPrivateKeyFile: key}, ""},
		{"app with a token for a key", Config{Orgs: []string{"o"}, AppID: 7, AppPrivateKeyFile: tok}, "PEM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.auth(http.DefaultClient)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("auth() = %v; want an error with %q", err, tc.err)
			}
		})
	}
}

var appKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// writeAppKey writes the App's key as GitHub hands it out, PKCS#1 PEM.
func writeAppKey(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "app.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey())})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// As a GitHub App, fleetci finds its installation on the org, serves the
// jobs with installation tokens and mints a new one as each nears its end.
func TestAGitHubAppServesAnOrg(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	f.app = &appKey().PublicKey
	f.tokenLife = time.Hour
	f.orgRepos = []map[string]any{{"full_name": "o/r"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	key := writeAppKey(t, t.TempDir())
	d, _ := dispatchWith(t, f, srv.URL, &fakeBackend{defaultClass: "2vcpu-1gb"}, func(c *Config) {
		c.Repos, c.TokenFile = nil, ""
		c.Orgs, c.AppID, c.AppPrivateKeyFile = []string{"o"}, 7, key
	})
	eventuallySettled(t, f, 5*time.Second)
	f.mu.Lock()
	mints := f.mints
	f.mu.Unlock()
	if mints != 1 {
		t.Fatalf("%d installation tokens minted for one hour's; want 1", mints)
	}

	// A token near its end is replaced on the next request.
	app := d.gh.tokens.(*appTokens)
	app.mu.Lock()
	t42 := app.cache[42]
	t42.expires = time.Now().Add(renewBefore - time.Second)
	app.cache[42] = t42
	app.mu.Unlock()
	if _, _, err := app.token(context.Background(), "o"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mints != 2 {
		t.Fatalf("%d tokens minted after the first neared its end; want 2", f.mints)
	}
}

// Two owners' tokens count against two limits: one rate-limited
// installation holds up no other.
func TestRateLimitsAreKeptPerToken(t *testing.T) {
	c := newClient("http://unused", staticToken("x"), http.DefaultClient)
	c.until["42"] = time.Now().Add(time.Hour)
	c.tokens = keyed{"o": "42", "p": "43"}
	if err := c.do(context.Background(), http.MethodGet, "/repos/o/r/actions/runs", nil, nil); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("a limited token's request went out: %v", err)
	}
	err := c.do(context.Background(), http.MethodGet, "/repos/p/r/actions/runs", nil, nil)
	if err != nil && strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("another token was held up: %v", err)
	}
}

type keyed map[string]string

func (k keyed) token(_ context.Context, owner string) (string, string, error) {
	return "t", k[owner], nil
}

func TestOwnerOf(t *testing.T) {
	for path, want := range map[string]string{
		"/repos/o/r/actions/runs": "o", "/orgs/acme/actions/runners/3": "acme",
		"/orgs/acme/repos?per_page=1": "acme", "/orgs/acme?x": "acme", "/app/installations/1": "",
	} {
		if got := ownerOf(path); got != want {
			t.Errorf("ownerOf(%q) = %q; want %q", path, got, want)
		}
	}
}
