package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The fleet's API is HTTPS and JSON, as Kubernetes's is: versioned paths
// under /v1, fields only ever added, mutual TLS with the fleet's CA.
// Workers and controllers dial controllers; nothing dials a worker, so a
// worker may sit behind NAT. The leader writes assignments; a worker
// watches its own with a long poll, makes them so, and reports their
// status with its own every 5 s.
//
//	POST /v1/join                         a new machine's CSR, with a token (no certificate yet)
//	POST /v1/ping                         a controller's heartbeat to another
//	GET  /v1/status                       the fleet, for fleetci fleet status
//	POST /v1/tokens                       a join token
//	POST /v1/workers/{id}/drain           and undrain, revoke
//	PUT  /v1/workers/{id}/status          a worker's facts and report: its heartbeat
//	GET  /v1/workers/{id}/assignments     its assignments; ?rev= waits for a newer revision
//	POST /v1/workers/{id}/certificate     a worker's certificate renewal
//	GET  /v1/artifacts/{name}             a binary a worker installs
const apiVersion = "v1"

// DefaultPort is the controllers' port.
const DefaultPort = "9481"

// ErrNoRoom is a worker's answer when it has no room for a class.
var ErrNoRoom = errors.New("no room")

// watchTimeout is how long a watch waits for a change before it answers
// with the list as it is.
const watchTimeout = 30 * time.Second

func workerPath(id, rest string) string {
	return "/" + apiVersion + "/workers/" + url.PathEscape(id) + rest
}

// apiError is the body of every error answer.
type apiError struct {
	Error string `json:"error"`
}

// StatusError is an error a controller answered with.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// serverTLS is a controller's: its own certificate, and callers' checked
// against the CA when they show one (a joining machine has none yet).
func serverTLS(p PKI) (*tls.Config, error) {
	pool, err := p.Pool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := p.TLSCert() // read each time: renewals take effect at once
			if err != nil {
				return nil, err
			}
			// The CA goes with it, so a joining machine can check it
			// against its token's hash.
			if ca, err := p.CA(); err == nil {
				c.Certificate = append(c.Certificate, ca.Raw)
			}
			return &c, nil
		},
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
	}, nil
}

// clientTLS dials a controller: this machine's certificate if it has one,
// and the controller's checked against the CA and its role, not its name,
// so a controller is reached by any address it has, NAT's included.
func clientTLS(p PKI, pool *x509.CertPool) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the controller showed no certificate")
			}
			opts := x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
				return fmt.Errorf("the controller's certificate: %w", err)
			}
			id, err := IdentityOf(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if id.Role != RoleController {
				return fmt.Errorf("%s is not a controller", id.ID)
			}
			return nil
		},
	}
	if p.HasCert() {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := p.TLSCert()
			return &c, err
		}
	}
	return cfg
}

func withPort(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		switch addr[i] {
		case ':':
			return addr
		case ']', '.':
			return addr + ":" + DefaultPort
		}
	}
	return addr + ":" + DefaultPort
}

// client is a connection to one controller, over HTTP/2.
type client struct {
	addr string
	base string
	hc   *http.Client
}

func newClient(addr string, cfg *tls.Config) *client {
	tr := &http.Transport{
		TLSClientConfig:     cfg,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     2 * time.Minute,
		// A connection that stops answering pings is dropped, so a
		// controller that went away is noticed in seconds, not minutes.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 10 * time.Second, PingTimeout: 5 * time.Second},
	}
	return &client{addr: addr, base: "https://" + withPort(addr), hc: &http.Client{Transport: tr}}
}

func (c *client) close() { c.hc.CloseIdleConnections() }

// do sends in, as JSON, and reads the answer into out.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.send(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// send makes a request and checks its status; the caller reads and closes
// the body.
func (c *client) send(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e apiError
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, &StatusError{Code: resp.StatusCode, Msg: e.Error}
		}
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return nil, &StatusError{Code: resp.StatusCode, Msg: msg}
	}
	return resp, nil
}

// The server's side.

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, apiError{Error: err.Error()})
}

// readJSON reads a request's body, up to 16 MB.
func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}

// peerIdentity is the verified identity of a request's caller, if it
// showed a certificate.
func peerIdentity(r *http.Request) (Identity, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return Identity{}, false
	}
	id, err := IdentityOf(r.TLS.VerifiedChains[0][0])
	return id, err == nil
}
