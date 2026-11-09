package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
)

// DefaultPort is the controllers' port.
const DefaultPort = "9481"

// The fleet's messages are JSON over gRPC: one hand-written service, no
// generated code. Every call carries a Msg.
type jsonCodec struct{}

func (jsonCodec) Name() string                       { return "json" }
func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func init() { encoding.RegisterCodec(jsonCodec{}) }

// Msg is one message on the fleet's calls. On a stream, a request has an
// ID and its reply the same ID with Reply set; a message with no ID needs
// no answer.
type Msg struct {
	ID    uint64          `json:"id,omitempty"`
	Reply bool            `json:"reply,omitempty"`
	Kind  string          `json:"kind,omitempty"`
	Err   string          `json:"err,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

func newMsg(kind string, body any) (*Msg, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &Msg{Kind: kind, Body: b}, nil
}

// Decode reads the message's body into v.
func (m *Msg) Decode(v any) error {
	if len(m.Body) == 0 {
		return nil
	}
	return json.Unmarshal(m.Body, v)
}

// ErrNoRoom is a worker's answer when it has no room for a class.
var ErrNoRoom = errors.New("no room")

func remoteErr(s string) error {
	if s == ErrNoRoom.Error() {
		return ErrNoRoom
	}
	return errors.New(s)
}

// service is the fleet's gRPC service.
type service interface {
	unary(ctx context.Context, method string, in *Msg) (*Msg, error)
	connect(stream grpc.ServerStream) error
	fetch(in *Msg, stream grpc.ServerStream) error
}

const serviceName = "fleetci.Fleet"

func unaryHandler(method string) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		in := new(Msg)
		if err := dec(in); err != nil {
			return nil, err
		}
		return srv.(service).unary(ctx, method, in)
	}
}

// The unary methods.
const (
	mJoin  = "Join"  // a new machine's CSR, with a token
	mPing  = "Ping"  // a controller's heartbeat to another
	mAdmin = "Admin" // fleetci's commands on a controller
)

var serviceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*service)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: mJoin, Handler: unaryHandler(mJoin)},
		{MethodName: mPing, Handler: unaryHandler(mPing)},
		{MethodName: mAdmin, Handler: unaryHandler(mAdmin)},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "Connect", ServerStreams: true, ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(service).connect(stream)
		}},
		{StreamName: "Fetch", ServerStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			in := new(Msg)
			if err := stream.RecvMsg(in); err != nil {
				return err
			}
			return srv.(service).fetch(in, stream)
		}},
	},
}

// call makes a unary call.
func call(ctx context.Context, cc *grpc.ClientConn, method, kind string, body, out any) error {
	in, err := newMsg(kind, body)
	if err != nil {
		return err
	}
	reply := new(Msg)
	if err := cc.Invoke(ctx, "/"+serviceName+"/"+method, in, reply, grpc.CallContentSubtype("json")); err != nil {
		return err
	}
	if reply.Err != "" {
		return remoteErr(reply.Err)
	}
	if out != nil {
		return reply.Decode(out)
	}
	return nil
}

func reply(body any, err error) (*Msg, error) {
	if err != nil {
		return &Msg{Err: err.Error()}, nil
	}
	m, merr := newMsg("", body)
	if merr != nil {
		return nil, merr
	}
	return m, nil
}

// peerIdentity is the verified identity of a call's caller, if it showed
// a certificate.
func peerIdentity(ctx context.Context) (Identity, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Identity{}, false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return Identity{}, false
	}
	id, err := IdentityOf(info.State.VerifiedChains[0][0])
	return id, err == nil
}

// serverTLS is a controller's: its own certificate, and callers' checked
// against the CA when they show one (Join's callers have none yet).
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

// dial opens a connection to a controller.
func dial(addr string, cfg *tls.Config) (*grpc.ClientConn, error) {
	return grpc.NewClient(withPort(addr),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype("json"), grpc.MaxCallRecvMsgSize(64<<20)),
	)
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

func newServer(cfg *tls.Config, s service) *grpc.Server {
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.MaxRecvMsgSize(64<<20),
	)
	srv.RegisterService(&serviceDesc, s)
	return srv
}

// stream is one end of a Connect stream: requests either way, with their
// replies matched by ID, and messages that need none.
type stream struct {
	s interface {
		SendMsg(any) error
		RecvMsg(any) error
	}
	sendMu sync.Mutex
	next   atomic.Uint64

	mu      sync.Mutex
	waiting map[uint64]chan *Msg
	closed  error
}

func newStream(s interface {
	SendMsg(any) error
	RecvMsg(any) error
}) *stream {
	return &stream{s: s, waiting: map[uint64]chan *Msg{}}
}

func (s *stream) send(m *Msg) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.s.SendMsg(m)
}

// tell sends a message that needs no answer.
func (s *stream) tell(kind string, body any) error {
	m, err := newMsg(kind, body)
	if err != nil {
		return err
	}
	return s.send(m)
}

// ask sends a request and waits for its reply.
func (s *stream) ask(ctx context.Context, kind string, body, out any) error {
	m, err := newMsg(kind, body)
	if err != nil {
		return err
	}
	m.ID = s.next.Add(1)
	ch := make(chan *Msg, 1)
	s.mu.Lock()
	if s.closed != nil {
		s.mu.Unlock()
		return s.closed
	}
	s.waiting[m.ID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiting, m.ID)
		s.mu.Unlock()
	}()
	if err := s.send(m); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r == nil {
			return s.err()
		}
		if r.Err != "" {
			return remoteErr(r.Err)
		}
		if out != nil {
			return r.Decode(out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *stream) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// answer replies to a request.
func (s *stream) answer(req *Msg, body any, err error) error {
	r, merr := reply(body, err)
	if merr != nil {
		return merr
	}
	r.ID, r.Reply, r.Kind = req.ID, true, req.Kind
	return s.send(r)
}

// serve reads the stream until it ends, handing requests and messages to
// handle, each in its own goroutine for requests.
func (s *stream) serve(handle func(m *Msg)) error {
	for {
		m := new(Msg)
		if err := s.s.RecvMsg(m); err != nil {
			s.mu.Lock()
			s.closed = fmt.Errorf("stream closed: %w", err)
			for id, ch := range s.waiting {
				close(ch)
				delete(s.waiting, id)
			}
			s.mu.Unlock()
			return err
		}
		if m.Reply {
			s.mu.Lock()
			ch := s.waiting[m.ID]
			s.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		if m.ID != 0 {
			go handle(m)
		} else {
			handle(m)
		}
	}
}
