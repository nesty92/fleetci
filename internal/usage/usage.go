// Package usage measures what a job asked of its VM, and says whether the
// VM's class fits it: too small (the job waited for CPU or memory), too big
// (most of it sat unused), or right. It keeps each job's runs, so a job that
// needs more over time shows before it gets slow or fails.
//
// It reads the VM's own kernel: pressure stall information (PSI,
// /proc/pressure), the time tasks were stalled waiting for CPU, memory or
// I/O, and the CPU and memory in use. Start records the counters as the job
// starts and leaves a sampler running for the peaks; Collect reads them once
// the job ends, before the VM goes. A VM runs one job, so the whole VM is the
// job's.
package usage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

var validKey = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func dir(key string) (string, error) {
	if !validKey.MatchString(key) {
		return "", fmt.Errorf("usage key %q: only letters, digits, '.', '_' and '-'", key)
	}
	return `"$HOME/.fleetci/usage/` + key + `"`, nil
}

// snap prints the counters a Report is the difference of.
const snap = `snap() {
  echo "uptime $(cut -d' ' -f1 /proc/uptime)"
  for r in cpu memory io; do
    if [ -r /proc/pressure/$r ]; then sed "s/^/$r /" /proc/pressure/$r; fi
  done
  head -n 1 /proc/stat
  grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
  echo "cpus $(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc)"
}
`

// sampler keeps the peaks: memory in use (MemTotal - MemAvailable) every
// second, and the CPUs busy over each 5 s, in hundredths of a CPU. It
// writes them to peak, renamed into place, and ends with the VM.
const sampler = `d=$1
read -r _ u n s i w q sq st _ < /proc/stat
pb=$((u + n + s + q + sq + st)); pt=$((pb + i + w))
cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc)
mem=0; cpu=0; k=0; n=0
# The VM ends it; a VM kept for more jobs, or a test, when its directory
# goes, and after 12 hours in any case.
while [ -d "$d" ] && [ $n -lt 43200 ]; do
  n=$((n + 1))
  while read -r f v _; do
    case $f in MemTotal:) t=$v ;; MemAvailable:) a=$v ;; esac
  done < /proc/meminfo
  [ $((t - a)) -gt $mem ] && mem=$((t - a))
  k=$((k + 1))
  if [ $k -ge 5 ]; then
    k=0
    read -r _ u n s i w q sq st _ < /proc/stat
    b=$((u + n + s + q + sq + st)); tt=$((b + i + w))
    if [ $((tt - pt)) -gt 0 ]; then
      c=$(( (b - pb) * cpus * 100 / (tt - pt) ))
      [ $c -gt $cpu ] && cpu=$c
    fi
    pb=$b; pt=$tt
  fi
  { echo "peak_mem_kb $mem"; echo "peak_cpu_x100 $cpu"; } > "$d/peak.new" 2>/dev/null && mv "$d/peak.new" "$d/peak" 2>/dev/null
  sleep 1
done
`

// Start records the VM's counters under key and starts the sampler, in a
// session of its own so it outlives the SSH session.
func Start(ctx context.Context, client *ssh.Client, key string) error {
	d, err := dir(key)
	if err != nil {
		return err
	}
	script := snap + `d=` + d + `
rm -rf "$d" && mkdir -p "$d"
snap > "$d/start.new" && mv "$d/start.new" "$d/start"
cat > "$d/sampler"
if command -v setsid >/dev/null 2>&1; then set -- setsid; else set -- nohup; fi
"$@" sh "$d/sampler" "$d" < /dev/null > /dev/null 2>&1 &
`
	// The sampler's script is the session's stdin: it holds no secret, but
	// keeps the command line short.
	return run(ctx, client, script, strings.NewReader(sampler), nil)
}

// Collect reads the counters Start recorded and the VM's now, and returns
// what the job used, ending the sampler. A VM without PSI (a kernel built without it, or
// booted with psi=0) gives a Report with HasPSI false.
func Collect(ctx context.Context, client *ssh.Client, key string) (Report, error) {
	d, err := dir(key)
	if err != nil {
		return Report{}, err
	}
	var out strings.Builder
	script := snap + `d=` + d + `
[ -r "$d/start" ] || { echo "no usage recorded" >&2; exit 3; }
cat "$d/start"; echo ---; snap; echo ---; cat "$d/peak" 2>/dev/null
# Its sampler stops with it, on a VM kept for another job.
rm -rf "$d"
`
	if err := run(ctx, client, script, nil, &out); err != nil {
		return Report{}, fmt.Errorf("reading the vm's usage: %w", err)
	}
	return parse(out.String())
}

// Report is what a job used of its VM.
type Report struct {
	Seconds float64 `json:"seconds"`
	// CPUs and MemoryMB are the VM's size, as its kernel sees it.
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memory_mb"`
	// HasPSI says whether the pressure figures were read.
	HasPSI bool `json:"psi"`
	// Pressure: the share of the job's time, in percent, that some task
	// (Some) or all of them (Full) were stalled waiting for the resource.
	CPUSome float64 `json:"cpu_some"`
	MemSome float64 `json:"mem_some"`
	MemFull float64 `json:"mem_full"`
	IOSome  float64 `json:"io_some"`
	IOFull  float64 `json:"io_full"`
	// CPUAvg is the CPUs busy on average; CPUPeak the most busy over 5 s.
	CPUAvg  float64 `json:"cpu_avg"`
	CPUPeak float64 `json:"cpu_peak"`
	// MemPeakMB is the most memory in use: what the kernel couldn't free
	// at once, page cache it can drop left out.
	MemPeakMB int `json:"mem_peak_mb"`
}

type counters struct {
	uptime   float64
	stall    map[string]float64 // "cpu some" → total µs
	busy     float64            // /proc/stat jiffies
	total    float64
	memTotal int // kB
	memAvail int
	cpus     int
}

func parse(s string) (Report, error) {
	parts := strings.Split(s, "---\n")
	if len(parts) < 2 {
		return Report{}, errors.New("usage: unexpected output from the vm")
	}
	a, b := read(parts[0]), read(parts[1])
	r := Report{Seconds: round1(b.uptime - a.uptime), CPUs: b.cpus, MemoryMB: b.memTotal / 1024}
	if r.Seconds <= 0 {
		return Report{}, fmt.Errorf("usage: the vm's uptime went from %v to %v; restarted?", a.uptime, b.uptime)
	}
	share := func(k string) float64 {
		return round1(100 * (b.stall[k] - a.stall[k]) / 1e6 / r.Seconds)
	}
	if _, ok := b.stall["cpu some"]; ok {
		r.HasPSI = true
		r.CPUSome, r.MemSome, r.MemFull = share("cpu some"), share("memory some"), share("memory full")
		r.IOSome, r.IOFull = share("io some"), share("io full")
	}
	if dt := b.total - a.total; dt > 0 {
		r.CPUAvg = round1(float64(b.cpus) * (b.busy - a.busy) / dt)
	}
	if len(parts) > 2 {
		for l := range strings.SplitSeq(parts[2], "\n") {
			f := strings.Fields(l)
			if len(f) != 2 {
				continue
			}
			v, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "peak_mem_kb":
				r.MemPeakMB = v / 1024
			case "peak_cpu_x100":
				r.CPUPeak = round1(float64(v) / 100)
			}
		}
	}
	// A job shorter than the sampler's first rounds: the ends are all
	// there is.
	r.CPUPeak = max(r.CPUPeak, r.CPUAvg)
	r.MemPeakMB = max(r.MemPeakMB, (a.memTotal-a.memAvail)/1024, (b.memTotal-b.memAvail)/1024)
	return r, nil
}

func read(s string) counters {
	c := counters{stall: map[string]float64{}}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "uptime":
			c.uptime, _ = strconv.ParseFloat(f[1], 64)
		case "cpu", "memory", "io":
			// "cpu some avg10=0.00 avg60=0.00 avg300=0.00 total=123"
			for _, kv := range f[2:] {
				if v, ok := strings.CutPrefix(kv, "total="); ok {
					c.stall[f[0]+" "+f[1]], _ = strconv.ParseFloat(v, 64)
				}
			}
			if f[0] == "cpu" && len(f) > 8 && !strings.Contains(f[1], "=") && f[1] != "some" && f[1] != "full" {
				c.jiffies(f)
			}
		case "MemTotal:":
			c.memTotal, _ = strconv.Atoi(f[1])
		case "MemAvailable:":
			c.memAvail, _ = strconv.Atoi(f[1])
		case "cpus":
			c.cpus, _ = strconv.Atoi(f[1])
		}
	}
	return c
}

// jiffies reads /proc/stat's "cpu user nice system idle iowait irq softirq
// steal ...": busy is all but idle and iowait.
func (c *counters) jiffies(f []string) {
	var v [8]float64
	for i := range v {
		v[i], _ = strconv.ParseFloat(f[i+1], 64)
	}
	c.busy = v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
	c.total = c.busy + v[3] + v[4]
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// run runs cmd in a session of its own, closed when ctx ends.
func run(ctx context.Context, client *ssh.Client, cmd string, stdin *strings.Reader, out *strings.Builder) error {
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if stdin != nil {
		s.Stdin = stdin
	}
	var stderr strings.Builder
	s.Stderr = &stderr
	if out != nil {
		s.Stdout = out
	}
	err = s.Run(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && stderr.Len() > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return err
}

// startTimeout bounds Start, which runs before the job, and collectTimeout
// Collect, which runs as the VM is being let go.
const (
	startTimeout   = 5 * time.Second
	collectTimeout = 5 * time.Second
)
