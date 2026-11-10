package release

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	sk, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sk2, err := ParseSecretKey(string(sk.Marshal()))
	if err != nil || sk2.Public().String() != sk.Public().String() {
		t.Fatalf("secret key round trip: %v", err)
	}
	k, err := ParseKey("untrusted comment: x\n" + sk.Public().String() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(sk, []byte("data"), "fleetci v1 versions.json")
	if tc, err := Verify(k, []byte("data"), sig); err != nil || tc != "fleetci v1 versions.json" {
		t.Fatalf("verify: %q %v", tc, err)
	}
	if _, err := Verify(k, []byte("other"), sig); err == nil {
		t.Fatal("other data verified")
	}
	forged := strings.Replace(string(sig), "fleetci v1", "fleetci v2", 1)
	if _, err := Verify(k, []byte("data"), []byte(forged)); err == nil {
		t.Fatal("a changed trusted comment verified")
	}
	other, _ := GenerateKey()
	if _, err := Verify(other.Public(), []byte("data"), sig); err == nil {
		t.Fatal("another key verified")
	}
	if _, err := ParseKey(PublicKey); err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
}

// TestMinisign checks against the stock tool, with FLEETCI_MINISIGN_DIR
// holding minisign's own key (key.pub, key.sec unencrypted), a file it
// signed (data, data.minisig); it writes ours and ours.minisig, signed
// here with key.sec, for minisign -V to check.
func TestMinisign(t *testing.T) {
	dir := os.Getenv("FLEETCI_MINISIGN_DIR")
	if dir == "" {
		t.Skip("FLEETCI_MINISIGN_DIR not set")
	}
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	k, err := ParseKey(string(read("key.pub")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(k, read("data"), read("data.minisig")); err != nil {
		t.Fatalf("minisign's signature: %v", err)
	}
	sk, err := ParseSecretKey(string(read("key.sec")))
	if err != nil {
		t.Fatalf("minisign's secret key: %v", err)
	}
	if sk.Public().String() != k.String() {
		t.Fatal("the secret key is not key.pub's")
	}
	// For minisign -V to check after: a file signed here.
	ours := []byte("signed by fleetci\n")
	if err := os.WriteFile(filepath.Join(dir, "ours"), ours, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ours.minisig"), Sign(sk, ours, "fleetci test"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchManifest(t *testing.T) {
	sk, _ := GenerateKey()
	bin := []byte("#!/bin/sh\necho fleetci\n")
	srv := httptest.NewServer(nil)
	defer srv.Close()
	sum := "" // filled below
	{
		f := filepath.Join(t.TempDir(), "b")
		_ = os.WriteFile(f, bin, 0o644)
		sum, _, _ = SHA256File(f)
	}
	m := Manifest{Version: "v1.2.3", Protocol: 1, MinProtocol: 1, Platforms: map[string]map[string]Binary{
		"linux/amd64": {"fleetci": {Version: "v1.2.3", URL: srv.URL + "/fleetci", SHA256: sum, Size: int64(len(bin))}}}}
	data, sig, err := SignManifest(sk, m)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"/versions.json": data, "/versions.json.minisig": sig, "/fleetci": bin}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	got, err := FetchManifest(t.Context(), srv.Client(), sk.Public(), srv.URL)
	if err != nil || got.Version != "v1.2.3" {
		t.Fatalf("fetch: %+v %v", got, err)
	}
	dst := filepath.Join(t.TempDir(), "fleetci")
	if err := Download(t.Context(), srv.Client(), got.Platforms["linux/amd64"]["fleetci"], dst); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("downloaded: %v %v", st, err)
	}

	// A manifest passed off as another version's, and a binary changed.
	other := m
	other.Version = "v9.9.9"
	data2, _, _ := SignManifest(sk, other)
	files["/versions.json"] = data2
	if _, err := FetchManifest(t.Context(), srv.Client(), sk.Public(), srv.URL); err == nil {
		t.Fatal("a manifest with another's signature")
	}
	files["/fleetci"] = []byte("#!/bin/sh\necho evilbin\n")
	if err := Download(t.Context(), srv.Client(), got.Platforms["linux/amd64"]["fleetci"], dst+"2"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a changed binary: %v", err)
	}
	if _, err := os.Stat(dst + "2"); err == nil {
		t.Fatal("the changed binary was left in place")
	}
}
