package jobtoken

import (
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	dir := t.TempDir()
	priv, pub := filepath.Join(dir, "key"), filepath.Join(dir, "key.pub")
	if err := GenerateKey(priv, pub); err != nil {
		t.Fatal(err)
	}
	sk, err := LoadPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := LoadPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(nil)

	now := time.Unix(1_800_000_000, 0)
	c := Claims{Forge: "gitlab", Instance: "gitlab.com", Project: "42", Ref: "main", Protected: true, Job: "7", Expires: now.Add(time.Hour).Unix()}
	tok, err := Sign(sk, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify([]ed25519.PublicKey{other, pk}, tok, now)
	if err != nil || got != c {
		t.Fatalf("Verify = %+v, %v; want %+v", got, err, c)
	}

	for name, tc := range map[string]struct {
		keys []ed25519.PublicKey
		tok  string
		now  time.Time
	}{
		"expired":   {[]ed25519.PublicKey{pk}, tok, now.Add(time.Hour)},
		"other key": {[]ed25519.PublicKey{other}, tok, now},
		"tampered":  {[]ed25519.PublicKey{pk}, strings.Replace(tok, tok[10:12], "AA", 1), now},
		"no prefix": {[]ed25519.PublicKey{pk}, strings.TrimPrefix(tok, prefix), now},
		"garbage":   {[]ed25519.PublicKey{pk}, prefix + "x", now},
	} {
		if _, err := Verify(tc.keys, tc.tok, tc.now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Verify err = %v, want ErrInvalid", name, err)
		}
	}
}
