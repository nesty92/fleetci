// Package jobtoken is the token fleetci gives each job it runs: what fleetci
// knows of the job (the forge, the project, the ref and whether it is
// protected), signed with fleetci's ed25519 key and valid until the job's
// timeout. A service such as fleetci's Go cache checks it with the public
// key alone, with no call to the forge.
package jobtoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// prefix starts every token, so one is recognised and the format can
// change.
const prefix = "fjt1."

// Claims is what a token says about its job.
type Claims struct {
	// Forge is "gitlab" or "github".
	Forge string `json:"forge"`
	// Instance is the forge's host, such as gitlab.com.
	Instance string `json:"instance"`
	// Project is the project's ID on the forge.
	Project string `json:"project"`
	// Ref is the branch or tag the job runs for.
	Ref string `json:"ref"`
	// Protected says whether Ref is protected: only people allowed to push
	// to it ran the job's code.
	Protected bool `json:"protected"`
	// Job is the job's ID.
	Job string `json:"job"`
	// Expires is when the token stops being valid, in Unix seconds.
	Expires int64 `json:"exp"`
}

// Sign returns a token for c.
func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := prefix + base64.RawURLEncoding.EncodeToString(b)
	sig := ed25519.Sign(key, []byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ErrInvalid is a token that is malformed, not signed by the key or
// expired.
var ErrInvalid = errors.New("invalid job token")

// Verify returns the claims of a token signed by one of keys and not
// expired at now.
func Verify(keys []ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	if !strings.HasPrefix(token, prefix) {
		return Claims{}, ErrInvalid
	}
	i := strings.LastIndexByte(token, '.')
	payload, sig := token[:i], token[i+1:]
	s, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	ok := false
	for _, k := range keys {
		if ed25519.Verify(k, []byte(payload), s) {
			ok = true
			break
		}
	}
	if !ok {
		return Claims{}, ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(payload[len(prefix):])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(b, &c); err != nil {
		return Claims{}, ErrInvalid
	}
	if now.Unix() >= c.Expires {
		return Claims{}, fmt.Errorf("%w: expired", ErrInvalid)
	}
	return c, nil
}

// GenerateKey writes a new private key to privPath (0600) and its public
// key to pubPath. Each file holds the key in base64.
func GenerateKey(privPath, pubPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	enc := base64.StdEncoding
	if err := os.WriteFile(privPath, []byte(enc.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(pubPath, []byte(enc.EncodeToString(pub)+"\n"), 0o644)
}

// LoadPrivateKey reads a private key written by GenerateKey.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := readKey(path, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// LoadPublicKey reads a public key written by GenerateKey.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := readKey(path, ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(b), nil
}

func readKey(path string, size int) ([]byte, error) {
	s, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(s)))
	if err != nil || len(b) != size {
		return nil, fmt.Errorf("%s: not a key written by fleetci-gocache keygen", path)
	}
	return b, nil
}
