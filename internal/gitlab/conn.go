// sharedClient below is ported from fleeting's SSH connector
// (gitlab.com/gitlab-org/fleeting/fleeting, connector/ssh.go and
// connector/runconn.go; MIT, Copyright (c) 2015-present GitLab B.V., see
// LICENSE.fleeting): Run, DialRun and Dial as fleeting's sshClient has them,
// over a VM's shared connection instead of one the job dials and closes.

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"

	"github.com/nesty92/fleetci/internal/proxy"
)

// sharedClient is a job's connector.Client on a VM. It opens sessions and
// channels on the VM's one connection, which fleetci's reclaim watch and
// the VM's other jobs share, so Close leaves that connection open.
type sharedClient struct {
	conn *proxy.Conn
}

var (
	_ connector.Client = sharedClient{}
	_ net.Conn         = (*rwConn)(nil)
)

func (c sharedClient) Run(ctx context.Context, opts connector.RunOptions) error {
	client, lost, err := c.conn.Client(ctx)
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		return lostErr(lost, err)
	}
	defer sess.Close()

	var stderr *stderrTail
	if opts.Stderr == nil {
		stderr = newStderrTail()
		opts.Stderr = stderr
	}
	sess.Stdin = opts.Stdin
	sess.Stdout = opts.Stdout
	sess.Stderr = opts.Stderr

	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(opts.Command) }()

	select {
	case <-ctx.Done():
		// The job's session goes; the VM's connection stays.
		return lostErr(lost, errors.Join(ctx.Err(), sess.Close()))
	case err := <-errCh:
		if err == nil {
			return nil
		}
		if stderr != nil {
			err = errors.Join(err, stderr.Error())
		}
		var exit *ssh.ExitError
		if !errors.As(err, &exit) {
			// No exit status: the session may have gone with the
			// connection, which the keepalive tells in a moment.
			select {
			case <-lost:
			case <-time.After(time.Second):
			}
		}
		return lostErr(lost, newExitError(err))
	}
}

// exitError is a command's failure with its exit code, 0 when the command
// gave none, as fleeting's connector.ExitError has it.
type exitError struct {
	err  error
	code int
}

func (e *exitError) Error() string {
	return fmt.Sprintf("run exit (exit code: %d): %v", e.code, e.err)
}
func (e *exitError) Unwrap() error { return e.err }
func (e *exitError) ExitCode() int { return e.code }

func newExitError(err error) error {
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return &exitError{err: err, code: exit.ExitStatus()}
	}
	return &exitError{err: err}
}

// lostErr says so when err came from a VM whose connection is gone, as
// fleeting's wrapKeepaliveErr does.
func lostErr(lost <-chan struct{}, err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-lost:
		return fmt.Errorf("lost the vm: its ssh connection is gone: %w", err)
	default:
		return err
	}
}

func (c sharedClient) DialRun(ctx context.Context, command string) (net.Conn, error) {
	connReader, w := io.Pipe()
	r, connWriter := io.Pipe()

	go func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		defer r.Close()
		defer w.Close()

		stderr := newStderrTail()
		err := c.Run(ctx, connector.RunOptions{Command: command, Stdin: r, Stdout: w, Stderr: stderr})
		if err != nil {
			err = fmt.Errorf("ssh tunnel: %w", errors.Join(err, stderr.Error()))
			r.CloseWithError(err)
			w.CloseWithError(err)
		}
	}()

	return &rwConn{connWriter, connReader}, nil
}

func (c sharedClient) Dial(network, address string) (net.Conn, error) {
	client, lost, err := c.conn.Client(context.Background())
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial(network, address)
	return conn, lostErr(lost, err)
}

// Close leaves the connection: it is the VM's, not the job's.
func (sharedClient) Close() error { return nil }

// stderrTail keeps the last 32 KiB a command wrote to stderr, for its error.
type stderrTail struct {
	buf        []byte
	start, end int
	n          int64
}

func newStderrTail() *stderrTail { return &stderrTail{buf: make([]byte, 32*1024)} }

func (r *stderrTail) Write(p []byte) (int, error) {
	r.n += int64(len(p))
	for _, b := range p {
		r.buf[r.end] = b
		r.end = (r.end + 1) % cap(r.buf)
		if r.end == r.start {
			r.start = (r.start + 1) % cap(r.buf)
		}
	}
	return len(p), nil
}

func (r *stderrTail) bytes() []byte {
	if r.start == r.end {
		return nil
	}
	if r.end < r.start {
		return append(append([]byte{}, r.buf[r.start:]...), r.buf[:r.end]...)
	}
	return r.buf[r.start:r.end]
}

func (r *stderrTail) Error() error {
	length := int64(r.end - r.start)
	if r.end < r.start {
		length = int64(cap(r.buf) - (r.start - r.end))
	}
	if r.n > length {
		return fmt.Errorf("omitted %d... %s", r.n-length, string(r.bytes()))
	}
	if str := string(r.bytes()); str != "" {
		return errors.New(str)
	}
	return nil
}

// rwConn is DialRun's stream: the command's stdin and stdout.
type rwConn struct {
	io.WriteCloser
	io.ReadCloser
}

func (*rwConn) LocalAddr() net.Addr                { return addr{} }
func (*rwConn) RemoteAddr() net.Addr               { return addr{} }
func (*rwConn) SetDeadline(t time.Time) error      { return fmt.Errorf("unsupported") }
func (*rwConn) SetReadDeadline(t time.Time) error  { return fmt.Errorf("unsupported") }
func (*rwConn) SetWriteDeadline(t time.Time) error { return fmt.Errorf("unsupported") }

type addr struct{}

func (addr) Network() string { return "gocat.Conn" }
func (addr) String() string  { return "gocat.Conn" }

func (c *rwConn) CloseWrite() error { return silenceAlreadyClosed(c.WriteCloser.Close()) }
func (c *rwConn) CloseRead() error  { return silenceAlreadyClosed(c.ReadCloser.Close()) }
func (c *rwConn) Close() error {
	return errors.Join(silenceAlreadyClosed(c.ReadCloser.Close()), silenceAlreadyClosed(c.WriteCloser.Close()))
}

func silenceAlreadyClosed(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
