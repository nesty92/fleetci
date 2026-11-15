package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// testIssuer serves an OIDC discovery document and its keys, and signs
// job tokens with them.
type testIssuer struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	kid   string
	fetch int
	disco bool
}

func newTestIssuer(t *testing.T, discovery bool) *testIssuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &testIssuer{key: k, kid: "k1", disco: discovery}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !is.disco {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": is.srv.URL + "/keys"})
	})
	keys := func(w http.ResponseWriter, r *http.Request) {
		is.fetch++
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &is.key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig"}}})
	}
	mux.HandleFunc("/keys", keys)
	mux.HandleFunc("/.well-known/jwks", keys)
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

// token is a job token with the claims changed: a nil value drops one.
func (is *testIssuer) token(t *testing.T, changes map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":           is.srv.URL,
		"exp":           time.Now().Add(10 * time.Minute).Unix(),
		"repository_id": "1",
		"ac":            `[{"Scope":"refs/heads/main","Permission":3}]`,
	}
	for k, v := range changes {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = is.kid
	s, err := tok.SignedString(is.key)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + s
}

// The cases are falcondev-oss/github-actions-cache-server's
// verified_tokens_need_exp_and_well_formed_scopes, and more.
func TestTokenVerify(t *testing.T) {
	is := newTestIssuer(t, true)
	v := newTokenVerifier(is.srv.URL)
	ctx := context.Background()

	g, err := v.verify(ctx, is.token(t, nil))
	if err != nil || !reflect.DeepEqual(g, cacheGrant{Repo: "1", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}}) {
		t.Fatalf("a valid token: %+v %v", g, err)
	}
	if _, err := v.verify(ctx, is.token(t, map[string]any{"aud": "anything"})); err != nil {
		t.Errorf("the audience isn't checked: %v", err)
	}
	for name, c := range map[string]map[string]any{
		"no exp, never expiring":     {"exp": nil},
		"expired":                    {"exp": time.Now().Add(-time.Minute).Unix()},
		"another issuer":             {"iss": "https://example.com"},
		"no repository id":           {"repository_id": nil},
		"no scopes":                  {"ac": nil},
		"empty scopes":               {"ac": "[]"},
		"a negative permission":      {"ac": `[{"Scope":"refs/heads/main","Permission":-1}]`},
		"an empty scope":             {"ac": `[{"Scope":"","Permission":1}]`},
		"a scope with no permission": {"ac": `[{"Scope":"refs/heads/main"}]`},
	} {
		if _, err := v.verify(ctx, is.token(t, c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Signed by another key with the same id.
	other := newTestIssuer(t, true)
	forged := other.token(t, map[string]any{"iss": is.srv.URL})
	if _, err := v.verify(ctx, forged); err == nil {
		t.Error("a token signed by another key: accepted")
	}
	if _, err := v.verify(ctx, "Bearer x.y.z"); err == nil {
		t.Error("garbage: accepted")
	}
	if _, err := v.verify(ctx, ""); err == nil {
		t.Error("no token: accepted")
	}
	if is.fetch != 1 {
		t.Errorf("the keys were fetched %d times, want once", is.fetch)
	}
}

// Without a discovery document the keys are at {issuer}/.well-known/jwks.
func TestTokenVerifyWithoutDiscovery(t *testing.T) {
	is := newTestIssuer(t, false)
	v := newTokenVerifier(is.srv.URL + "/")
	if _, err := v.verify(context.Background(), is.token(t, nil)); err != nil {
		t.Fatal(err)
	}
}

// falcondev's permissions_are_bit_flags.
func TestGrantPermissionsAreBitFlags(t *testing.T) {
	g, err := grantOf("1", `[{"Scope":"read","Permission":1},{"Scope":"write","Permission":2},{"Scope":"both","Permission":3}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.Reads, []string{"both", "read"}) || g.Write != "write" {
		t.Fatalf("reads %v, write %q", g.Reads, g.Write)
	}
}
