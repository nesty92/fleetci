package cluster

import (
	"strings"
	"testing"
)

func TestFitClasses(t *testing.T) {
	classes := map[string]*ClassSpec{
		"small": {CPUs: 1, Memory: 2048, Arch: "amd64"},
		"big":   {CPUs: 4, Memory: 8192, Arch: "amd64"},
		"arm":   {CPUs: 1, Memory: 2048, Arch: "arm64"},
		"gpu":   {CPUs: 1, Memory: 2048, Arch: "amd64", On: map[string]string{"gpu": "yes"}},
	}
	f := Facts{Arch: "amd64", CPUs: 2, MemoryMB: 4096, DiskMB: 100 << 10}
	fit := FitClasses(classes, f)
	if got := strings.Join(classNames(fit.Classes), ","); got != "small" {
		t.Fatalf("classes %s, want small", got)
	}
	for _, name := range []string{"big", "arm", "gpu"} {
		if fit.Refused[name] == "" {
			t.Errorf("%s not refused", name)
		}
	}
	f.KVM = "no /dev/kvm"
	if fit := FitClasses(classes, f); len(fit.Classes) != 0 {
		t.Errorf("without KVM it runs %v", classNames(fit.Classes))
	}
}

func TestJoinPKI(t *testing.T) {
	ctl := PKI{Dir: t.TempDir()}
	if _, err := ctl.InitCA(); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SignSelf(RoleController, "ctl-1", []string{"ctl-1"}); err != nil {
		t.Fatal(err)
	}
	ca, _ := ctl.CA()
	tok := NewToken(CAHash(ca))
	parsed, err := ParseToken(tok.String())
	if err != nil || parsed != tok {
		t.Fatalf("token round trip: %v %+v", err, parsed)
	}
	if !SecretMatches(tok.Secret, HashSecret(tok.Secret)) || SecretMatches("x", HashSecret(tok.Secret)) {
		t.Fatal("secret check")
	}

	w := PKI{Dir: t.TempDir()}
	csr, err := w.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ctl.Sign(csr, RoleWorker, "w1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Install(cert, ca.Raw); err != nil {
		t.Fatal(err)
	}
	c, err := w.Cert()
	if err != nil {
		t.Fatal(err)
	}
	id, err := IdentityOf(c)
	if err != nil || id.ID != "w1" || id.Role != RoleWorker {
		t.Fatalf("identity %+v %v", id, err)
	}
}

func TestGroupName(t *testing.T) {
	a, b := groupName("w1", "1vcpu-2gb"), groupName("w1", "4vcpu-8gb")
	if a == b || len(a) > 10 {
		t.Fatalf("%s %s", a, b)
	}
}
