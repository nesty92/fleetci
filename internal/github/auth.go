package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tokens hands out the token for requests about an owner's repos or org.
// The key names the rate limit the token counts against.
type tokens interface {
	token(ctx context.Context, owner string) (tok, key string, err error)
}

// staticToken is one token for every owner: a personal access token.
type staticToken string

func (t staticToken) token(context.Context, string) (string, string, error) {
	return string(t), "", nil
}

// appTokens are a GitHub App's installation tokens, one per owner the App
// is installed on, minted with a JWT signed by the App's key and renewed
// before they expire.
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation
type appTokens struct {
	base  string
	appID int64
	key   *rsa.PrivateKey
	http  *http.Client
	now   func() time.Time

	mint  sync.Mutex // one token minted at a time, so requests share it
	mu    sync.Mutex
	inst  map[string]int64          // owner → installation id
	cache map[int64]installationTok // installation id → token
}

type installationTok struct {
	tok     string
	expires time.Time
}

// renewBefore is how long before it expires an installation token is
// replaced. They last an hour.
const renewBefore = 5 * time.Minute

func newAppTokens(base string, appID int64, keyPEM []byte, hc *http.Client) (*appTokens, error) {
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &appTokens{base: base, appID: appID, key: key, http: hc, now: time.Now,
		inst: map[string]int64{}, cache: map[int64]installationTok{}}, nil
}

// parseRSAKey reads the PEM private key GitHub generates for an App
// (PKCS#1), or the same key converted to PKCS#8.
func parseRSAKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block in the private key file")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not an RSA key")
	}
	return rk, nil
}

// jwt signs the App's own token, good for ten minutes less a minute of
// clock skew, as GitHub allows.
func (a *appTokens) jwt() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.appID, 10),
	})
	if err != nil {
		return "", err
	}
	signed := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func (a *appTokens) token(ctx context.Context, owner string) (string, string, error) {
	owner = strings.ToLower(owner)
	if t, id, ok := a.cached(owner); ok {
		return t, id, nil
	}
	a.mint.Lock()
	defer a.mint.Unlock()
	if t, id, ok := a.cached(owner); ok {
		return t, id, nil // minted while this request waited
	}
	a.mu.Lock()
	id, ok := a.inst[owner]
	a.mu.Unlock()
	if !ok {
		var err error
		if id, err = a.installation(ctx, owner); err != nil {
			return "", "", err
		}
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), &out); err != nil {
		return "", "", fmt.Errorf("github app: installation token for %s: %w", owner, err)
	}
	a.mu.Lock()
	a.inst[owner] = id
	a.cache[id] = installationTok{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, strconv.FormatInt(id, 10), nil
}

// cached is the owner's token while it has more than renewBefore left.
func (a *appTokens) cached(owner string) (tok, key string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, found := a.inst[owner]
	t := a.cache[id]
	if found && a.now().Before(t.expires.Add(-renewBefore)) {
		return t.tok, strconv.FormatInt(id, 10), true
	}
	return "", "", false
}

// installation finds the App's installation on an org, or on a user's
// account.
func (a *appTokens) installation(ctx context.Context, owner string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := a.do(ctx, http.MethodGet, "/orgs/"+owner+"/installation", &out)
	if errors.Is(err, errNotFound) {
		err = a.do(ctx, http.MethodGet, "/users/"+owner+"/installation", &out)
	}
	if errors.Is(err, errNotFound) {
		return 0, fmt.Errorf("github app %d is not installed on %s", a.appID, owner)
	}
	if err != nil {
		return 0, fmt.Errorf("github app: finding its installation on %s: %w", owner, err)
	}
	return out.ID, nil
}

// do is a request as the App itself.
func (a *appTokens) do(ctx context.Context, method, path string, out any) error {
	jwt, err := a.jwt()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return statusError{method, path, resp.StatusCode, strings.TrimSpace(string(raw))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readSecret reads a file holding one secret, such as a token.
func readSecret(what, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("github: %s is required", what)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("github: reading %s: %w", what, err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("github: %s is empty", what)
	}
	return s, nil
}
