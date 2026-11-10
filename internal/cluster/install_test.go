package cluster

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// get is a new machine's request: no certificate, and the controller's
// not checked, as curl -k does.
func (f *testFleet) get(t *testing.T, path, auth string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://"+f.addr+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestInstallScript(t *testing.T) {
	f := newTestFleet(t)
	f.c.artifacts = []Artifact{{Name: "fleetci", SHA256: "abc123"}}
	pin, err := f.c.pki.Pin()
	if err != nil {
		t.Fatal(err)
	}
	code, b := f.get(t, "/v1/install.sh", "")
	if code != http.StatusOK {
		t.Fatalf("install.sh: %d", code)
	}
	s := string(b)
	for _, want := range []string{"SUM='abc123'", "PIN='" + pin + "'", "PORT='" + DefaultPort + "'", "exec $BIN join -service"} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh has no %q", want)
		}
	}
	if strings.Contains(s, "{{") {
		t.Error("install.sh has a placeholder left")
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		if out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v: %s", err, out)
		}
	}
}

func TestInstallBinary(t *testing.T) {
	f := newTestFleet(t)
	get := func(auth string) (int, []byte) { t.Helper(); return f.get(t, "/v1/install/fleetci", auth) }
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := get("Bearer fci1." + strings.Repeat("0", 64) + ".nope.nope"); code != http.StatusForbidden {
		t.Errorf("an unknown token: %d", code)
	}
	ctlTok, _ := f.c.CreateToken(RoleController, time.Hour)
	if code, _ := get("Bearer " + ctlTok); code != http.StatusForbidden {
		t.Errorf("a controller's token: %d", code)
	}
	tok, err := f.c.CreateToken(RoleWorker, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, b := get("Bearer " + tok)
	if code != http.StatusOK {
		t.Fatalf("a worker's token: %d", code)
	}
	self, _ := os.Executable()
	want, _ := os.ReadFile(self)
	if sha256.Sum256(b) != sha256.Sum256(want) {
		t.Error("the download is not the controller's binary")
	}
	// The download uses nothing up: the join still can.
	if code, _ := get("Bearer " + tok); code != http.StatusOK {
		t.Errorf("a second download: %d", code)
	}
	p, _ := ParseToken(tok)
	f.c.mu.Lock()
	r := f.c.tokens[p.ID]
	r.Used = "w9"
	f.c.tokens[p.ID] = r
	f.c.mu.Unlock()
	if code, _ := get("Bearer " + tok); code != http.StatusForbidden {
		t.Errorf("a used token: %d", code)
	}
}

// TestInstallPin checks the line token create prints: curl takes the
// controller by its pinned key, and refuses another.
func TestInstallPin(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	f := newTestFleet(t)
	pin, err := f.c.pki.Pin()
	if err != nil {
		t.Fatal(err)
	}
	line := InstallLine(f.addr, pin, "fci1.x")
	if !strings.HasPrefix(line, "curl -fsSk --pinnedpubkey "+pin+" https://"+f.addr+"/v1/install.sh | sh -s -- "+f.addr+" fci1.x") {
		t.Errorf("the line: %s", line)
	}
	run := func(pin string) error {
		return exec.Command(curl, "-fsSk", "--pinnedpubkey", pin, "-o", os.DevNull, "https://"+f.addr+"/v1/install.sh").Run()
	}
	if err := run(pin); err != nil {
		t.Errorf("the controller's own pin: %v", err)
	}
	other := sha256.Sum256([]byte("another key"))
	if err := run("sha256//" + base64.StdEncoding.EncodeToString(other[:])); err == nil {
		t.Error("curl took a controller with another key")
	}
}
