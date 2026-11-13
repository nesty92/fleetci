package fleet

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mirrorScript adds the mirrors to whatever daemon.json the image has,
// leaves an image's own mirrors alone, and always leaves valid JSON.
func TestMirrorScript(t *testing.T) {
	mirrors := []string{"http://10.0.0.5:5000", "https://mirror.example"}
	for _, tc := range []struct {
		name, before string
		want         map[string]any
	}{
		{"no file", "", map[string]any{
			"registry-mirrors":    []any{"http://10.0.0.5:5000", "https://mirror.example"},
			"insecure-registries": []any{"10.0.0.5:5000"},
		}},
		{"empty object", "{}\n", map[string]any{
			"registry-mirrors":    []any{"http://10.0.0.5:5000", "https://mirror.example"},
			"insecure-registries": []any{"10.0.0.5:5000"},
		}},
		{"other settings", "{\n  \"log-driver\": \"local\",\n  \"mtu\": 1400\n}\n", map[string]any{
			"registry-mirrors":    []any{"http://10.0.0.5:5000", "https://mirror.example"},
			"insecure-registries": []any{"10.0.0.5:5000"},
			"log-driver":          "local", "mtu": float64(1400),
		}},
		{"insecure registries of its own", `{"insecure-registries": ["r.local:5000"]}`, map[string]any{
			"registry-mirrors":    []any{"http://10.0.0.5:5000", "https://mirror.example"},
			"insecure-registries": []any{"r.local:5000"},
		}},
		{"mirrors of its own", `{"registry-mirrors": ["https://own.example"]}`, map[string]any{
			"registry-mirrors": []any{"https://own.example"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "docker", "daemon.json")
			if tc.before != "" {
				if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			script := mirrorScript(mirrors)
			script = strings.ReplaceAll(script, "/etc/docker", filepath.Dir(f))
			script = strings.Replace(script, `s=; [ "$(id -u)" = 0 ] || s="sudo -n"`, "s=", 1)
			script = strings.Replace(script, `$s systemctl reload docker 2>/dev/null || $s kill -HUP "$(pidof dockerd)"`, "touch "+dir+"/reloaded", 1)
			if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, b)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			_, err = os.Stat(filepath.Join(dir, "reloaded"))
			if own := strings.Contains(tc.before, "registry-mirrors"); own != os.IsNotExist(err) {
				t.Fatalf("reloaded Docker: %v, with mirrors of its own: %v", err == nil, own)
			}
		})
	}
}

func TestRegistryMirrorsValidated(t *testing.T) {
	for _, m := range []string{"10.0.0.5:5000", "ftp://x", "http://", "http://x/'; rm -rf /"} {
		c := Class{Plugin: "p", MaxInstances: 1, RegistryMirrors: []string{m}}
		if err := c.validate("c"); err == nil || !strings.Contains(err.Error(), "registry_mirrors") {
			t.Errorf("%q: %v", m, err)
		}
	}
	c := Class{Plugin: "p", MaxInstances: 1, RegistryMirrors: []string{"http://10.0.0.5:5000"}}
	if err := c.validate("c"); err != nil {
		t.Error(err)
	}
}
