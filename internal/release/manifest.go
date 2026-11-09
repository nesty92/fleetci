package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ManifestName is the manifest's asset name; its signature is
// ManifestName + ".minisig".
const ManifestName = "versions.json"

// Manifest is a release: fleetci's version and protocol, and per platform
// each binary a machine installs, with the version it was tested with.
// One update moves them all together.
type Manifest struct {
	Version     string `json:"version"`
	Protocol    int    `json:"protocol"`
	MinProtocol int    `json:"min_protocol"`
	// Platforms: "linux/amd64" → binary name → the binary.
	Platforms map[string]map[string]Binary `json:"platforms"`
}

// Binary is one of a release's binaries.
type Binary struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size,omitempty"`
}

// Trusted is the manifest's trusted comment, signed with it: a manifest
// can't be passed off as another version's.
func Trusted(version string) string { return "fleetci " + version + " " + ManifestName }

// SignManifest signs a manifest's bytes.
func SignManifest(sk SecretKey, m Manifest) (data, sig []byte, err error) {
	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	data = append(data, '\n')
	return data, Sign(sk, data, Trusted(m.Version)), nil
}

// ParseManifest checks a manifest's signature by the key and reads it.
func ParseManifest(k Key, data, sig []byte) (Manifest, error) {
	trusted, err := Verify(k, data, sig)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestName, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if trusted != Trusted(m.Version) {
		return Manifest{}, fmt.Errorf("%s: signed for %q, and says %s", ManifestName, trusted, m.Version)
	}
	return m, nil
}

// FetchManifest downloads a release's manifest from base, the directory
// its assets are in, and checks it is signed by the key.
func FetchManifest(ctx context.Context, hc *http.Client, k Key, base string) (Manifest, error) {
	base = strings.TrimSuffix(base, "/") + "/"
	data, err := get(ctx, hc, base+ManifestName, 1<<20)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := get(ctx, hc, base+ManifestName+".minisig", 4<<10)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(k, data, sig)
}

func get(ctx context.Context, hc *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return b, err
}

// Download fetches the binary to path, through a temporary file next to
// it, and checks its checksum before it is there.
func Download(ctx context.Context, hc *http.Client, b Binary, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", b.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	limit := int64(1 << 30)
	if b.Size > 0 {
		limit = b.Size
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if b.Size > 0 && n != b.Size {
		return fmt.Errorf("%s: %d bytes, not %d", b.URL, n, b.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != b.SHA256 {
		return fmt.Errorf("%s: sha256 %s, not %s", b.URL, sum, b.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SHA256File is a file's checksum and size.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ErrNoPlatform is a release with nothing for this machine.
var ErrNoPlatform = errors.New("the release has no binaries for this platform")
