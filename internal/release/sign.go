// Package release is fleetci's signed releases: versions.json, the
// manifest of the binaries a release is made of, and its signature, which
// a machine checks before it runs what it downloads.
//
// Signatures are minisign's (https://jedisct1.github.io/minisign/), so a
// release can be checked by hand with the stock tool too:
//
//	minisign -Vm versions.json -P <PublicKey>
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// PublicKey is the key fleetci's releases are signed with.
const PublicKey = "RWTgbngGzFGS8xCuzg+JIhe87MYOqwyUNqye94EFX+ZJ2sWPDfWFGgYS"

var (
	algPrehashed = [2]byte{'E', 'D'} // Ed25519 over BLAKE2b-512 of the file
	algLegacy    = [2]byte{'E', 'd'} // Ed25519 over the file; keys say this
	kdfNone      = [2]byte{0, 0}
	cksumBlake2b = [2]byte{'B', '2'}
)

// Key is a public key, as minisign prints it.
type Key struct {
	ID  [8]byte
	Pub ed25519.PublicKey
}

// ParseKey reads a public key: its base64 line, or a .pub file.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(lastLine(s))
	if err != nil || len(b) != 2+8+ed25519.PublicKeySize || [2]byte(b[:2]) != algLegacy {
		return Key{}, errors.New("not a minisign public key")
	}
	var k Key
	copy(k.ID[:], b[2:10])
	k.Pub = ed25519.PublicKey(bytes.Clone(b[10:]))
	return k, nil
}

func (k Key) String() string {
	return base64.StdEncoding.EncodeToString(append(append(algLegacy[:], k.ID[:]...), k.Pub...))
}

// IDString is the key's id as minisign shows it.
func (k Key) IDString() string { return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(k.ID[:])) }

// SecretKey is a key to sign with. Its file is minisign's, unencrypted
// (minisign -G -W): a CI secret holds it, and the CI's store encrypts it.
type SecretKey struct {
	ID   [8]byte
	Priv ed25519.PrivateKey
}

// GenerateKey makes a key pair.
func GenerateKey() (SecretKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SecretKey{}, err
	}
	var sk SecretKey
	if _, err := rand.Read(sk.ID[:]); err != nil {
		return SecretKey{}, err
	}
	sk.Priv = priv
	return sk, nil
}

// Public is its public key.
func (sk SecretKey) Public() Key {
	return Key{ID: sk.ID, Pub: sk.Priv.Public().(ed25519.PublicKey)}
}

// keynum is what the checksum covers: the algorithm, id and key.
func (sk SecretKey) checksum() [32]byte {
	return blake2b.Sum256(append(append(algLegacy[:], sk.ID[:]...), sk.Priv...))
}

// Marshal writes it as a minisign secret key file.
func (sk SecretKey) Marshal() []byte {
	var b bytes.Buffer
	b.Write(algLegacy[:])
	b.Write(kdfNone[:])
	b.Write(cksumBlake2b[:])
	b.Write(make([]byte, 32+8+8)) // salt, opslimit, memlimit: no KDF
	b.Write(sk.ID[:])
	b.Write(sk.Priv)
	sum := sk.checksum()
	b.Write(sum[:])
	return fmt.Appendf(nil, "untrusted comment: fleetci release signing key\n%s\n", base64.StdEncoding.EncodeToString(b.Bytes()))
}

// ParseSecretKey reads an unencrypted minisign secret key file.
func ParseSecretKey(s string) (SecretKey, error) {
	b, err := base64.StdEncoding.DecodeString(lastLine(s))
	if err != nil || len(b) != 2+2+2+32+8+8+8+64+32 {
		return SecretKey{}, errors.New("not a minisign secret key")
	}
	if [2]byte(b[:2]) != algLegacy || [2]byte(b[4:6]) != cksumBlake2b {
		return SecretKey{}, errors.New("not an Ed25519 minisign secret key")
	}
	if [2]byte(b[2:4]) != kdfNone {
		return SecretKey{}, errors.New("the secret key is encrypted: make one with minisign -G -W, or fleetci's hack/release keygen")
	}
	var sk SecretKey
	off := 2 + 2 + 2 + 32 + 8 + 8
	copy(sk.ID[:], b[off:off+8])
	sk.Priv = ed25519.PrivateKey(bytes.Clone(b[off+8 : off+8+64]))
	// minisign leaves an unencrypted key's checksum zero; it checks a
	// password with it. The key's own halves must agree either way.
	sum, chk := sk.checksum(), b[off+72:]
	if !bytes.Equal(chk, make([]byte, 32)) && subtle.ConstantTimeCompare(sum[:], chk) != 1 {
		return SecretKey{}, errors.New("the secret key's checksum is wrong")
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(sk.Priv.Seed()), sk.Priv) {
		return SecretKey{}, errors.New("the secret key is damaged")
	}
	return sk, nil
}

// Sign signs data; trusted goes in the signature, under it.
func Sign(sk SecretKey, data []byte, trusted string) []byte {
	if strings.ContainsAny(trusted, "\r\n") {
		panic("a trusted comment is one line")
	}
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(sk.Priv, h[:])
	global := ed25519.Sign(sk.Priv, append(bytes.Clone(sig), trusted...))
	line := append(append(algPrehashed[:], sk.ID[:]...), sig...)
	return fmt.Appendf(nil, "untrusted comment: signature from fleetci's release key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(line), trusted, base64.StdEncoding.EncodeToString(global))
}

// Verify checks a minisign signature of data by the key, and returns its
// trusted comment.
func Verify(k Key, data, sigFile []byte) (string, error) {
	lines := strings.Split(strings.TrimRight(string(sigFile), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return "", errors.New("not a minisign signature")
	}
	sl, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sl) != 2+8+ed25519.SignatureSize {
		return "", errors.New("not a minisign signature")
	}
	if [8]byte(sl[2:10]) != k.ID {
		return "", fmt.Errorf("signed with key %016X, not %s", binary.LittleEndian.Uint64(sl[2:10]), k.IDString())
	}
	msg := data
	switch [2]byte(sl[:2]) {
	case algPrehashed:
		h := blake2b.Sum512(data)
		msg = h[:]
	case algLegacy:
	default:
		return "", errors.New("an unknown signature algorithm")
	}
	sig := sl[10:]
	if !ed25519.Verify(k.Pub, msg, sig) {
		return "", errors.New("the signature does not match")
	}
	trusted := strings.TrimPrefix(lines[2], "trusted comment: ")
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || !ed25519.Verify(k.Pub, append(bytes.Clone(sig), trusted...), global) {
		return "", errors.New("the trusted comment's signature does not match")
	}
	return trusted, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
