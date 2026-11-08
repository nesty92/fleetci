// Package vmjob runs a job's process on a VM apart from the SSH session
// that starts it, so the job keeps running when fleetci stops or loses the
// connection, and any later connection, from this process or the next,
// follows it to its end.
//
// A job lives in ~/.fleetci/jobs/<key> on the VM: run (the script), log
// (its output), pid (its process group) and, once it ends, exit (its
// status).
package vmjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// ErrNoJob is Follow's error when the VM has no job of that key: it was
// never started, or the VM is another one.
var ErrNoJob = errors.New("no such job on the vm")

// ErrLost is Follow's error when the job's process is gone without an exit
// status: killed, or the VM restarted.
var ErrLost = errors.New("job ended without an exit status")

var validKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func dir(key string) (string, error) {
	if !validKey.MatchString(key) {
		return "", fmt.Errorf("job key %q: only letters, digits, '.', '_' and '-'", key)
	}
	return `"$HOME/.fleetci/jobs/` + key + `"`, nil
}

// Start writes script and its stdin into the job's directory and starts
// the script there in a session of its own (setsid), its output to the
// job's log. stdin is a file only until the script opens it, so a secret
// in it doesn't stay on disk.
func Start(ctx context.Context, client *ssh.Client, key, script string, stdin []byte) error {
	d, err := dir(key)
	if err != nil {
		return err
	}
	// A key is used once; what an earlier job of it left goes. sshd gives
	// a command whose client went an end of input, not a signal, so the
	// script is put in place only once all of it came.
	write := fmt.Sprintf(`umask 077 && rm -rf %[1]s && mkdir -p %[1]s && cat > %[1]s/run.new &&
[ $(($(wc -c < %[1]s/run.new))) -eq %[2]d ] && mv %[1]s/run.new %[1]s/run`, d, len(script))
	if err := run(ctx, client, write, strings.NewReader(script), nil); err != nil {
		return fmt.Errorf("writing the job's script: %w", err)
	}
	// The job's shell writes its own pid, which is its process group's id,
	// and its status once the script ends: exit is renamed into place, so
	// a reader sees all of it or nothing. A job whose stdin was cut short
	// doesn't start.
	launch := `set -e
umask 077
d=` + d + `
cat > "$d/stdin"
if [ $(($(wc -c < "$d/stdin"))) -ne ` + strconv.Itoa(len(stdin)) + ` ]; then rm -f "$d/stdin"; echo "stdin cut short" >&2; exit 1; fi
if command -v setsid >/dev/null 2>&1; then set -- setsid; else set -- nohup; fi
"$@" sh -c 'd=$1; echo $$ > "$d/pid"; { rm -f "$d/stdin"; sh "$d/run"; } < "$d/stdin" > "$d/log" 2>&1; echo $? > "$d/exit.new"; mv "$d/exit.new" "$d/exit"' sh "$d" < /dev/null > /dev/null 2>&1 &
`
	if err := run(ctx, client, launch, bytes.NewReader(stdin), nil); err != nil {
		return fmt.Errorf("starting the job: %w", err)
	}
	return nil
}

// followScript prints the log from line n on as it grows (n 0: from its
// end as it is now), and once the
// job has ended, the rest of it and a status line on stderr. The status is
// read before the log on each round, so every line written before the job
// ended is printed.
const followScript = `d=%s
n=%d
if [ ! -d "$d" ]; then echo "fleetci-job: none" >&2; exit 0; fi
if [ "$n" -lt 1 ]; then
  n=1
  if [ -e "$d/log" ]; then n=$(($(wc -l < "$d/log") + 1)); fi
fi
echo "fleetci-job: from $n" >&2
while :; do
  ended=
  if [ -e "$d/exit" ]; then ended=1
  elif [ -s "$d/pid" ] && ! kill -0 "$(cat "$d/pid")" 2>/dev/null && [ ! -e "$d/exit" ]; then ended=lost
  elif [ ! -e "$d/pid" ] && [ ! -e "$d/stdin" ] && [ ! -e "$d/log" ]; then ended=lost
  fi
  if [ -e "$d/log" ]; then
    if [ -n "$ended" ]; then sed -n "${n},\$p" "$d/log"; break; fi
    total=$(wc -l < "$d/log")
    if [ "$total" -ge "$n" ]; then sed -n "${n},${total}p" "$d/log"; n=$((total + 1)); fi
  elif [ -n "$ended" ]; then break
  fi
  sleep 0.5 2>/dev/null || sleep 1
done
if [ "$ended" = lost ]; then echo "fleetci-job: lost" >&2; else echo "fleetci-job: exit $(cat "$d/exit")" >&2; fi
`

// Follow calls line with each line of the job's output from line from on
// (1 for all of it, 0 for the lines written from now on) until the job
// ends, and returns its exit status and the line to go on from, so a
// follow cut short by a lost session goes on where it stopped.
func Follow(ctx context.Context, client *ssh.Client, key string, from int, line func(string)) (status, next int, err error) {
	d, err := dir(key)
	if err != nil {
		return 0, from, err
	}
	var stderr syncBuffer
	read := 0
	out := &lineWriter{line: func(s string) {
		read++
		line(s)
	}}
	err = run(ctx, client, fmt.Sprintf(followScript, d, from), nil, &stdio{out, &stderr})
	out.flush()
	statuses := statusLines(stderr.String())
	next = from
	for _, s := range statuses {
		if v, ok := strings.CutPrefix(s, "from "); ok {
			if n, err := strconv.Atoi(v); err == nil {
				next = n
			}
		}
	}
	next += read
	if err != nil {
		return 0, next, err
	}
	s := ""
	if len(statuses) > 0 {
		s = statuses[len(statuses)-1]
	}
	switch {
	case s == "none":
		return 0, next, ErrNoJob
	case s == "lost":
		return 0, next, ErrLost
	case strings.HasPrefix(s, "exit "):
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "exit ")))
		if err != nil {
			return 0, next, fmt.Errorf("job status %q: %w", s, err)
		}
		return n, next, nil
	default:
		return 0, next, fmt.Errorf("following the job: no status (stderr %q)", strings.TrimSpace(stderr.String()))
	}
}

// statusLines are the script's own lines on stderr.
func statusLines(stderr string) []string {
	var out []string
	for l := range strings.SplitSeq(stderr, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "fleetci-job: "); ok {
			out = append(out, v)
		}
	}
	return out
}

// syncBuffer is a buffer the session's stderr copier writes while Follow
// may read it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stdio struct{ out, err io.Writer }

// run runs cmd in a session of its own, closed when ctx ends.
func run(ctx context.Context, client *ssh.Client, cmd string, stdin io.Reader, out *stdio) error {
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	s.Stdin = stdin
	if out != nil {
		s.Stdout, s.Stderr = out.out, out.err
	}
	err = s.Run(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// lineWriter calls line with each whole line written to it.
type lineWriter struct {
	line func(string)
	buf  []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// flush passes on a last line with no newline.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}
