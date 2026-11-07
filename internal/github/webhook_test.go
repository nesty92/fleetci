package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesty92/fleetci/internal/fleet"
)

const testSecret = "s3cret"

// hooked runs a dispatcher serving org o from webhooks, polling every
// poll, and returns where deliveries go.
func hooked(t *testing.T, f *forge, backend *fakeBackend, poll time.Duration) (d *Dispatcher, addr string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, _ = dispatchWith(t, f, srv.URL, backend, func(c *Config) {
		c.Repos = nil
		c.Orgs = []string{"o"}
		c.PollInterval = fleet.Duration(poll)
		c.Webhook = &WebhookConfig{Listen: "127.0.0.1:0", Path: "/hook", SecretFile: secret}
	})
	select {
	case a := <-d.listening:
		return d, "http://" + a + "/hook"
	case <-time.After(3 * time.Second):
		t.Fatal("the webhook never listened")
	}
	return nil, ""
}

func event(action string, j job, repo string) []byte {
	b, _ := json.Marshal(map[string]any{"action": action, "workflow_job": j, "repository": map[string]string{"full_name": repo}})
	return b
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func deliver(t *testing.T, addr, kind, sig string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, addr, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", kind)
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func created(b *fakeBackend) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.created)
}

// A queued job's delivery gets it a runner, on its org, with no repo
// polled: before it, nothing started.
func TestAWebhookServesAQueuedJob(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
	_, addr := hooked(t, f, backend, time.Hour)
	time.Sleep(200 * time.Millisecond)
	if n := created(backend); n != 0 {
		t.Fatalf("%d vms before any delivery", n)
	}
	body := event("queued", queuedJob(1, 7), "o/r")
	if code := deliver(t, addr, "workflow_job", sign(testSecret, body), body); code != http.StatusAccepted {
		t.Fatalf("delivery answered %d", code)
	}
	eventuallySettled(t, f, 5*time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.path == "/repos/o/r/actions/runs" {
			t.Fatal("a repo was polled though its job came by webhook")
		}
	}
	if len(f.scopes) != 1 || f.scopes[0] != "/orgs/o" {
		t.Fatalf("runners registered on %v; want one on /orgs/o", f.scopes)
	}
}

// Only signed workflow_job events for served repos start anything.
func TestAWebhookTakesOnlyWhatIsSignedAndServed(t *testing.T) {
	f := newForge(t, queuedJob(1, 7))
	backend := &fakeBackend{defaultClass: "2vcpu-1gb"}
	_, addr := hooked(t, f, backend, time.Hour)
	body := event("queued", queuedJob(1, 7), "o/r")
	stranger := event("queued", queuedJob(1, 7), "someone/else")
	for _, tc := range []struct {
		name, kind, sig string
		body            []byte
		want            int
	}{
		{"no signature", "workflow_job", "", body, http.StatusUnauthorized},
		{"wrong secret", "workflow_job", sign("guess", body), body, http.StatusUnauthorized},
		{"signature of another body", "workflow_job", sign(testSecret, stranger), body, http.StatusUnauthorized},
		{"not hex", "workflow_job", "sha256=zz", body, http.StatusUnauthorized},
		{"ping", "ping", sign(testSecret, []byte("{}")), []byte("{}"), http.StatusOK},
		{"another event", "push", sign(testSecret, body), body, http.StatusNoContent},
		{"not a job", "workflow_job", sign(testSecret, []byte(`{"action":"queued"}`)), []byte(`{"action":"queued"}`), http.StatusBadRequest},
		{"a repo not served", "workflow_job", sign(testSecret, stranger), stranger, http.StatusAccepted},
	} {
		if code := deliver(t, addr, tc.kind, tc.sig, tc.body); code != tc.want {
			t.Errorf("%s: answered %d; want %d", tc.name, code, tc.want)
		}
	}
	resp, err := http.Get(addr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)
	if n := created(backend); n != 0 {
		t.Fatalf("%d vms started by deliveries that should start none", n)
	}
}

// GitHub hands a runner any queued job with its labels. The job whose
// runner took another sends no new event, yet still gets a runner.
func TestAWebhookJobWhoseRunnerTookAnotherIsLookedAtAgain(t *testing.T) {
	f := newForge(t, queuedJob(1, 7), queuedJob(2, 7))
	_, addr := hooked(t, f, &fakeBackend{defaultClass: "2vcpu-1gb"}, time.Hour)
	body := event("queued", queuedJob(2, 7), "o/r") // job 1's is lost
	deliver(t, addr, "workflow_job", sign(testSecret, body), body)
	eventuallySettled(t, f, 5*time.Second)
}

// A repo a webhook spoke of is polled too, so a job whose delivery was
// lost is served all the same.
func TestALostDeliveryIsServedByTheReconcile(t *testing.T) {
	f := newForge(t, queuedJob(3, 9))
	_, addr := hooked(t, f, &fakeBackend{defaultClass: "2vcpu-1gb"}, 50*time.Millisecond)
	body := event("completed", job{ID: 99, RunID: 1, Status: "completed", Labels: []string{"fleetci"}}, "o/r")
	deliver(t, addr, "workflow_job", sign(testSecret, body), body)
	eventuallySettled(t, f, 5*time.Second)
}
