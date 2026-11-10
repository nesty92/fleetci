// Package cluster runs fleetci on several machines: controllers, one of
// them the leader, hold the forge credentials, find the jobs and place each
// on a worker; workers dial out to the controllers, from behind any NAT,
// run the jobs' VMs and never register with GitHub or GitLab. See
// docs/design/ha.md.
package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The roles a certificate's organization names.
const (
	RoleController = "fleetci:controller"
	RoleWorker     = "fleetci:worker"
)

// certLifetime is how long a worker's or controller's certificate lasts;
// it is renewed after two thirds of it.
const certLifetime = 30 * 24 * time.Hour

// caLifetime is the fleet CA's.
const caLifetime = 10 * 365 * 24 * time.Hour

// PKI is the fleet's certificates in a directory: the CA (its key only on
// controllers) and this machine's own certificate and key.
type PKI struct {
	Dir string
}

func (p PKI) path(name string) string { return filepath.Join(p.Dir, name) }

// CAFile, CAKeyFile, CertFile and KeyFile are the PKI's files.
func (p PKI) CAFile() string    { return p.path("ca.crt") }
func (p PKI) CAKeyFile() string { return p.path("ca.key") }
func (p PKI) CertFile() string  { return p.path("node.crt") }
func (p PKI) KeyFile() string   { return p.path("node.key") }

// InitCA makes a new CA, unless one is there already.
func (p PKI) InitCA() (created bool, err error) {
	if _, err := os.Stat(p.CAFile()); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return false, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "fleetci fleet CA", Organization: []string{"fleetci"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		return false, err
	}
	if err := writeKey(p.CAKeyFile(), key); err != nil {
		return false, err
	}
	return true, writePEM(p.CAFile(), "CERTIFICATE", der, 0o644)
}

// CA reads the CA's certificate.
func (p PKI) CA() (*x509.Certificate, error) { return readCert(p.CAFile()) }

// CAKey reads the CA's key, which only controllers have.
func (p PKI) CAKey() (ed25519.PrivateKey, error) { return readKey(p.CAKeyFile()) }

// HasCert says whether this machine has a certificate yet.
func (p PKI) HasCert() bool {
	_, err := os.Stat(p.CertFile())
	return err == nil
}

// Cert reads this machine's certificate.
func (p PKI) Cert() (*x509.Certificate, error) { return readCert(p.CertFile()) }

// TLSCert is this machine's certificate and key for TLS.
func (p PKI) TLSCert() (tls.Certificate, error) {
	return tls.LoadX509KeyPair(p.CertFile(), p.KeyFile())
}

// Pool is a pool holding the CA.
func (p PKI) Pool() (*x509.CertPool, error) {
	ca, err := p.CA()
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, nil
}

// NewKey makes this machine's key, replacing any, and returns a CSR for it.
// The key never leaves the machine; the CSR's subject is ignored by the
// signer, which decides the identity.
func (p PKI) NewKey() (csr []byte, err error) {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	_ = pub
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "fleetci"}}, key)
	if err != nil {
		return nil, err
	}
	if err := writeKey(p.KeyFile()+".new", key); err != nil {
		return nil, err
	}
	return der, nil
}

// Install puts a signed certificate in place with the key NewKey made.
func (p PKI) Install(cert, ca []byte) error {
	if len(ca) > 0 {
		if err := writePEM(p.CAFile(), "CERTIFICATE", ca, 0o644); err != nil {
			return err
		}
	}
	if err := writePEM(p.CertFile()+".new", "CERTIFICATE", cert, 0o644); err != nil {
		return err
	}
	if err := os.Rename(p.KeyFile()+".new", p.KeyFile()); err != nil {
		return err
	}
	return os.Rename(p.CertFile()+".new", p.CertFile())
}

// InstallCAKey writes the CA's key, for a controller that joined.
func (p PKI) InstallCAKey(der []byte) error {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return errors.New("the CA key is not ed25519")
	}
	return writeKey(p.CAKeyFile(), key)
}

// CAKeyDER is the CA key, for a controller that joins.
func (p PKI) CAKeyDER() ([]byte, error) {
	key, err := p.CAKey()
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKCS8PrivateKey(key)
}

// Sign signs a CSR as role and id, with the hosts (names or IPs) as SANs.
func (p PKI) Sign(csrDER []byte, role, id string, hosts []string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr: %w", err)
	}
	ca, err := p.CA()
	if err != nil {
		return nil, err
	}
	key, err := p.CAKey()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: id, Organization: []string{role}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		h, _, _ = strings.Cut(h, ":")
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca, csr.PublicKey, key)
}

// SignSelf gives this controller a certificate of its own from its CA.
func (p PKI) SignSelf(role, id string, hosts []string) error {
	csr, err := p.NewKey()
	if err != nil {
		return err
	}
	der, err := p.Sign(csr, role, id, hosts)
	if err != nil {
		return err
	}
	return p.Install(der, nil)
}

// Identity is who a certificate says its holder is.
type Identity struct {
	ID     string
	Role   string
	Serial string
}

// IdentityOf reads a peer's identity from its verified certificate.
func IdentityOf(c *x509.Certificate) (Identity, error) {
	if c == nil || len(c.Subject.Organization) != 1 || c.Subject.CommonName == "" {
		return Identity{}, errors.New("certificate names no fleetci identity")
	}
	role := c.Subject.Organization[0]
	if role != RoleController && role != RoleWorker {
		return Identity{}, fmt.Errorf("certificate has no fleetci role (%q)", role)
	}
	return Identity{ID: c.Subject.CommonName, Role: role, Serial: c.SerialNumber.Text(16)}, nil
}

// CAHash is the hash a join token pins: SHA-256 of the CA's public key.
func CAHash(ca *x509.Certificate) string {
	sum := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// Pin is this machine's public key as curl's --pinnedpubkey takes it:
// sha256// and the base64 of its SubjectPublicKeyInfo's hash.
func (p PKI) Pin() (string, error) {
	c, err := p.Cert()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// Token is a join token: fci1.<CA hash>.<id>.<secret>. The id finds the
// token on the controller; the secret proves it.
type Token struct {
	CAHash string
	ID     string
	Secret string
}

func (t Token) String() string {
	return "fci1." + t.CAHash + "." + t.ID + "." + t.Secret
}

// ParseToken reads a join token.
func ParseToken(s string) (Token, error) {
	f := strings.Split(strings.TrimSpace(s), ".")
	if len(f) != 4 || f[0] != "fci1" || len(f[1]) != 64 || f[2] == "" || f[3] == "" {
		return Token{}, errors.New("not a fleetci join token (fci1.<CA hash>.<id>.<secret>)")
	}
	return Token{CAHash: f[1], ID: f[2], Secret: f[3]}, nil
}

// NewToken makes a token for the CA.
func NewToken(caHash string) Token {
	return Token{CAHash: caHash, ID: randomID(4), Secret: randomID(16)}
}

// SecretMatches compares a token's secret with a stored hash of it.
func SecretMatches(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(hash)) == 1
}

// HashSecret is what a controller keeps of a token's secret.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToLower(b32.EncodeToString(b))
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func writeKey(path string, key ed25519.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "PRIVATE KEY", der, 0o600)
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return key, nil
}
