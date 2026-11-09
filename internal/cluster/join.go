package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/nesty92/fleetci/internal/fleet"
)

// Init sets up the first controller: the fleet's CA and this controller's
// certificate.
func Init(cfg *ControllerConfig, out io.Writer) error {
	p := PKI{Dir: cfg.Controller.PKIDir}
	created, err := p.InitCA()
	if err != nil {
		return err
	}
	ca, err := p.CA()
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "ok    %-14s created in %s (hash %s)\n", "ca", p.Dir, CAHash(ca)[:16])
	} else {
		fmt.Fprintf(out, "ok    %-14s %s has one already (hash %s)\n", "ca", p.Dir, CAHash(ca)[:16])
	}
	if p.HasCert() {
		if c, err := p.Cert(); err == nil {
			if id, err := IdentityOf(c); err == nil && id.ID == cfg.Controller.ID && id.Role == RoleController {
				fmt.Fprintf(out, "ok    %-14s %s, valid until %s\n", "certificate", id.ID, c.NotAfter.Format(time.DateOnly))
				return nil
			}
		}
	}
	if err := p.SignSelf(RoleController, cfg.Controller.ID, cfg.Controller.Advertise); err != nil {
		return err
	}
	fmt.Fprintf(out, "ok    %-14s %s, for %s\n", "certificate", cfg.Controller.ID, strings.Join(cfg.Controller.Advertise, ", "))
	return os.MkdirAll(cfg.Controller.StateDir, 0o700)
}

// Admin runs one of fleetci's commands on the controller at addr, with this
// controller's certificate.
func Admin(ctx context.Context, pkiDir, addr string, req adminReq, out any) error {
	p := PKI{Dir: pkiDir}
	pool, err := p.Pool()
	if err != nil {
		return err
	}
	cc, err := dial(addr, clientTLS(p, pool))
	if err != nil {
		return err
	}
	defer cc.Close()
	return call(ctx, cc, mAdmin, "admin", req, out)
}

// AdminRequest is a command for Admin.
func AdminRequest(op, worker, role string, ttl time.Duration) adminReq {
	return adminReq{Op: op, Worker: worker, Role: role, TTL: ttl}
}

// JoinOptions are fleetci join's.
type JoinOptions struct {
	Controller string
	Token      string
	ID         string
	// Config is worker.toml, written by the join.
	Config   string
	PKIDir   string
	StateDir string
	BinDir   string
	Labels   map[string]string
	Classes  []string
	Host     *fleet.HostConfig
	// AsController joins a controller, with a --controller token.
	AsController bool
	Advertise    []string
	Version      string
}

// Join joins this machine to the fleet: preflight, trust, the manifest and
// the install. It prints a line per step and stops at the first failure;
// run again, it picks up where it stopped.
func Join(ctx context.Context, o JoinOptions, out io.Writer) (*WorkerConfig, *Manifest, error) {
	step := func(name, msg string) { fmt.Fprintf(out, "ok    %-14s %s\n", name, msg) }
	tok, err := ParseToken(o.Token)
	if err != nil {
		return nil, nil, err
	}
	if o.ID == "" {
		o.ID, _ = os.Hostname()
	}
	wc := &WorkerConfig{ID: o.ID, Controllers: []string{o.Controller}, PKIDir: o.PKIDir, StateDir: o.StateDir,
		Labels: o.Labels, Classes: o.Classes, Host: o.Host, File: o.Config,
		FleetingdPlugin: filepath.Join(o.BinDir, "fleeting-plugin-fleetingd")}

	// 1. Preflight: what this machine has. Only the controllers decide
	// whether a class fits; here, what no class can do without.
	facts, err := GatherFacts(wc, o.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("preflight: %w", err)
	}
	facts.Hosts = o.Advertise
	if !o.AsController {
		for _, bin := range []string{"ip", "nft"} {
			if _, err := exec.LookPath(bin); err != nil {
				return nil, nil, fmt.Errorf("preflight: %s is not installed (iproute2, nftables)", bin)
			}
		}
		kvm := "works"
		if facts.KVM != "" {
			kvm = facts.KVM
		}
		step("preflight", fmt.Sprintf("%s, budget %d cpus %s, %s free disk, kvm %s", facts.Arch, facts.CPUs, fleet.Size(facts.MemoryMB), fleet.Size(facts.DiskMB), kvm))
	}

	// 2. Trust: the controller's CA, checked against the token's hash; a
	// key made here; a certificate signed with the token.
	p := PKI{Dir: o.PKIDir}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, // checked below
		VerifyConnection: func(cs tls.ConnectionState) error { return pinned(cs, tok.CAHash) }}
	cc, err := dial(o.Controller, cfg)
	if err != nil {
		return nil, nil, err
	}
	defer cc.Close()
	csr, err := p.NewKey()
	if err != nil {
		return nil, nil, err
	}
	var resp JoinResponse
	jctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := call(jctx, cc, mJoin, "join", JoinRequest{Token: tok.ID + "." + tok.Secret, ID: o.ID, CSR: csr, Facts: facts}, &resp); err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	if err := p.Install(resp.Cert, resp.CA); err != nil {
		return nil, nil, err
	}
	if len(resp.CAKey) > 0 {
		if err := p.InstallCAKey(resp.CAKey); err != nil {
			return nil, nil, err
		}
	}
	step("trust", fmt.Sprintf("certificate for %s as %s, from CA %s", o.ID, resp.Role, tok.CAHash[:16]))
	m := resp.Manifest
	if o.AsController {
		return nil, &m, nil
	}

	// 3. The manifest.
	var refused []string
	for name, why := range m.Fit.Refused {
		refused = append(refused, name+" ("+why+")")
	}
	slices.Sort(refused)
	msg := fmt.Sprintf("fleetci %s; classes %s", m.Version, strings.Join(classNames(m.Fit.Classes), ", "))
	if len(refused) > 0 {
		msg += "; not " + strings.Join(refused, ", ")
	}
	step("manifest", msg)

	// 4. Install: the binaries, checked; ip_forward; worker.toml.
	pool := x509.NewCertPool()
	if ca, err := p.CA(); err == nil {
		pool.AddCert(ca)
	}
	authed, err := dial(o.Controller, clientTLS(p, pool))
	if err != nil {
		return nil, nil, err
	}
	defer authed.Close()
	if err := os.MkdirAll(o.BinDir, 0o755); err != nil {
		return nil, nil, err
	}
	for _, a := range m.Artifacts {
		path := filepath.Join(o.BinDir, a.Name)
		if sum, _ := fileSum(path); sum == a.SHA256 {
			step("install", a.Name+" is current")
			continue
		}
		if self, _ := os.Executable(); a.Name == "fleetci" && self == path {
			step("install", "fleetci: this binary differs from the controller's; left as it is")
			continue
		}
		if err := fetchArtifact(ctx, authed, a, path); err != nil {
			return nil, nil, fmt.Errorf("install %s: %w", a.Name, err)
		}
		step("install", fmt.Sprintf("%s into %s (%s)", a.Name, o.BinDir, fleet.Size(a.Size>>20)))
	}
	for _, need := range []string{"cloud-hypervisor", "fleeting-plugin-fleetingd"} {
		if _, err := os.Stat(filepath.Join(o.BinDir, need)); err != nil {
			if _, err := exec.LookPath(need); err != nil {
				return nil, nil, fmt.Errorf("install: %s is neither in the manifest nor installed", need)
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil || strings.TrimSpace(string(b)) != "1" {
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
			return nil, nil, fmt.Errorf("install: net.ipv4.ip_forward: %w", err)
		}
		_ = os.WriteFile("/etc/sysctl.d/90-fleetci.conf", []byte("net.ipv4.ip_forward = 1\n"), 0o644)
		step("install", "net.ipv4.ip_forward = 1")
	}
	// A host that reaches the forges, for the VMs, which go out through it.
	hc := &http.Client{Timeout: 15 * time.Second}
	for _, u := range m.Fit.Reach {
		resp, err := hc.Head(u)
		if err != nil {
			return nil, nil, fmt.Errorf("install: this machine can't reach %s: %w", u, err)
		}
		resp.Body.Close()
	}
	if len(m.Fit.Reach) > 0 {
		step("install", "reaches "+strings.Join(m.Fit.Reach, ", "))
	}
	wc.Controllers = m.Controllers
	if len(wc.Controllers) == 0 {
		wc.Controllers = []string{o.Controller}
	}
	if err := os.MkdirAll(filepath.Dir(o.Config), 0o755); err != nil {
		return nil, nil, err
	}
	if err := wc.Save(); err != nil {
		return nil, nil, err
	}
	step("install", "wrote "+o.Config)
	return wc, &m, nil
}

// pinned checks a controller's certificate against the CA the token
// names: one in the chain it sent whose hash matches.
func pinned(cs tls.ConnectionState, caHash string) error {
	if len(cs.PeerCertificates) < 2 {
		return errors.New("the controller sent no CA")
	}
	var ca *x509.Certificate
	for _, c := range cs.PeerCertificates[1:] {
		if CAHash(c) == caHash {
			ca = c
		}
	}
	if ca == nil {
		return errors.New("the controller's CA is not the token's: wrong controller, or someone in between")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return err
	}
	id, err := IdentityOf(cs.PeerCertificates[0])
	if err != nil {
		return err
	}
	if id.Role != RoleController {
		return fmt.Errorf("%s is not a controller", id.ID)
	}
	return nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchArtifact downloads an artifact from the controller and checks it.
func fetchArtifact(ctx context.Context, cc *grpc.ClientConn, a Artifact, path string) error {
	fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cs, err := cc.NewStream(fctx, &serviceDesc.Streams[1], "/"+serviceName+"/Fetch")
	if err != nil {
		return err
	}
	req, err := newMsg("fetch", a.Name)
	if err != nil {
		return err
	}
	if err := cs.SendMsg(req); err != nil {
		return err
	}
	if err := cs.CloseSend(); err != nil {
		return err
	}
	tmp := path + ".fetch"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	h := sha256.New()
	w := io.MultiWriter(f, h)
	for {
		m := new(Msg)
		if err := cs.RecvMsg(m); err != nil {
			f.Close()
			return err
		}
		if m.Kind == "eof" {
			break
		}
		var data []byte
		if err := m.Decode(&data); err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(data); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != a.SHA256 {
		return fmt.Errorf("checksum %s, the manifest says %s", sum, a.SHA256)
	}
	return os.Rename(tmp, path)
}
