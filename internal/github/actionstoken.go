package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// A job's ACTIONS_RUNTIME_TOKEN says which caches the job may read and
// write, as GitHub's cache service reads it: the "ac" claim lists
// {"Scope": ref, "Permission": bits}, 1 to read and 2 to write, and
// "repository_id" is the repo. The node checks the token GitHub signed
// before it lists or signs anything for the job, so a job can't name
// another ref's or repo's caches. How, after falcondev-oss's
// github-actions-cache-server (src/scope.rs, MIT): the issuer's keys
// through OIDC discovery, kept 10 minutes, fetched again at most every
// 30 s for a key they lack.

// DefaultTokenIssuer issues github.com's job tokens.
const DefaultTokenIssuer = "https://token.actions.githubusercontent.com"

// cacheGrant is what a verified token lets the job do.
type cacheGrant struct {
	Repo  string   // repository_id
	Write string   // the scope a save goes to, "" for none
	Reads []string // the scopes a restore searches, in order
}

type scopeGrant struct {
	Scope      string `json:"Scope"`
	Permission *int   `json:"Permission"`
}

// grantOf is the grant of the token's claims.
func grantOf(repo, ac string) (cacheGrant, error) {
	if repo == "" {
		return cacheGrant{}, errors.New("the token has no repository id")
	}
	var gs []scopeGrant
	if err := json.Unmarshal([]byte(ac), &gs); err != nil {
		return cacheGrant{}, errors.New("the token's cache scopes aren't JSON")
	}
	if len(gs) == 0 {
		return cacheGrant{}, errors.New("the token has no cache scopes")
	}
	for _, g := range gs {
		if g.Scope == "" || g.Permission == nil || *g.Permission < 0 {
			return cacheGrant{}, errors.New("the token's cache scopes are malformed")
		}
	}
	gr := cacheGrant{Repo: repo}
	for _, g := range gs {
		if *g.Permission&2 != 0 {
			gr.Write = g.Scope
			break
		}
	}
	sorted := append([]scopeGrant(nil), gs...)
	sort.SliceStable(sorted, func(i, j int) bool { return *sorted[i].Permission > *sorted[j].Permission })
	for _, g := range sorted {
		if *g.Permission&1 != 0 {
			gr.Reads = append(gr.Reads, g.Scope)
		}
	}
	return gr, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	AC     string `json:"ac"`
	RepoID string `json:"repository_id"`
}

// tokenVerifier checks job tokens against the issuer's keys.
type tokenVerifier struct {
	issuer string
	http   *http.Client

	mu      sync.Mutex
	keys    *jose.JSONWebKeySet
	fetched time.Time
	tried   time.Time
}

func newTokenVerifier(issuer string) *tokenVerifier {
	if issuer == "" {
		issuer = DefaultTokenIssuer
	}
	return &tokenVerifier{issuer: strings.TrimSuffix(issuer, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

const (
	keysKeep  = 10 * time.Minute
	keysRetry = 30 * time.Second
)

// verify is the grant of a request's "Authorization: Bearer" token.
func (v *tokenVerifier) verify(ctx context.Context, auth string) (cacheGrant, error) {
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		return cacheGrant{}, errors.New("no bearer token")
	}
	var c tokenClaims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "ES256", "ES384", "EdDSA"}),
	)
	if err != nil {
		return cacheGrant{}, fmt.Errorf("invalid token: %w", err)
	}
	return grantOf(c.RepoID, c.AC)
}

// key is the issuer's public key kid, or every key for a token without
// one, as jose does: the first that verifies.
func (v *tokenVerifier) key(ctx context.Context, kid string) (any, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	stale := v.keys == nil || now.Sub(v.fetched) > keysKeep
	if !stale && kid != "" && len(v.keys.Key(kid)) == 0 && now.Sub(v.tried) > keysRetry {
		stale = true
	}
	if stale && now.Sub(v.tried) > time.Second {
		v.tried = now
		ks, err := v.fetch(ctx)
		if err != nil && v.keys == nil {
			return nil, err
		}
		if err == nil {
			v.keys, v.fetched = ks, now
		}
	}
	if v.keys == nil {
		return nil, errors.New("no keys from the token issuer")
	}
	var found []jose.JSONWebKey
	if kid != "" {
		found = v.keys.Key(kid)
	} else {
		found = v.keys.Keys
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("the token issuer has no key %q", kid)
	}
	set := jwt.VerificationKeySet{}
	for _, k := range found {
		if k.Use == "" || k.Use == "sig" {
			set.Keys = append(set.Keys, k.Key)
		}
	}
	return set, nil
}

// fetch is the issuer's keys, from its discovery document's jwks_uri,
// else from {issuer}/.well-known/jwks.
func (v *tokenVerifier) fetch(ctx context.Context) (*jose.JSONWebKeySet, error) {
	var disc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	uri := v.issuer + "/.well-known/jwks"
	if err := v.getJSON(ctx, v.issuer+"/.well-known/openid-configuration", &disc); err == nil && disc.JWKSURI != "" {
		uri = disc.JWKSURI
	}
	var ks jose.JSONWebKeySet
	if err := v.getJSON(ctx, uri, &ks); err != nil {
		return nil, fmt.Errorf("the token issuer's keys: %w", err)
	}
	return &ks, nil
}

func (v *tokenVerifier) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
}
