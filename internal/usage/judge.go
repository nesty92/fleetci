package usage

import (
	"fmt"
	"math"
	"strings"
)

// Fit is how a job's class fits it.
type Fit string

const (
	// Undersized: the job waited for CPU or memory; a bigger class makes
	// it faster, or keeps it from running out.
	Undersized Fit = "undersized"
	// Oversized: most of the VM sat unused; a smaller class runs it as
	// well and leaves the host room for more jobs.
	Oversized Fit = "oversized"
	// IOBound: the job waited on its disk, which a bigger class doesn't
	// change unless memory was short too.
	IOBound Fit = "io-bound"
	// Fits: neither.
	Fits Fit = "fits"
	// Short: the job ran too briefly to say.
	Short Fit = "short"
)

// The thresholds, in percent of the job's time stalled (PSI) or of the VM.
// A CPU-bound build on too few CPUs shows cpu some near 100%; a job on
// enough, under 5%. Memory some of a few percent already means the kernel
// reclaimed page cache the job needed again, or swapped; full means every
// task stood still.
const (
	minSeconds     = 30
	cpuSomeHigh    = 20.0
	cpuSomeLow     = 5.0
	memSomeHigh    = 5.0
	memFullHigh    = 1.0
	memSomeLow     = 1.0
	memPeakHigh    = 0.90 // of the VM's memory
	memPeakLow     = 0.50
	cpuPeakLow     = 0.50 // of the VM's CPUs
	ioSomeIOBound  = 30.0
	ioSomeHigh     = 10.0 // with memory near full: memory, not the disk
	memPeakIO      = 0.80
	headroomCPU    = 1.25
	headroomMemory = 1.5
)

// Verdict is Judge's answer.
type Verdict struct {
	Fit Fit `json:"fit"`
	// Reasons say what the figures showed, one per finding.
	Reasons []string `json:"reasons,omitempty"`
	// CPUs and MemoryMB are the size the job looks to need, 0 where the
	// VM's own fits.
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
}

// Advice is the verdict as a line for a log.
func (v Verdict) Advice() string {
	var size []string
	if v.CPUs > 0 {
		size = append(size, fmt.Sprintf("%d vCPU", v.CPUs))
	}
	if v.MemoryMB > 0 {
		size = append(size, gb(v.MemoryMB))
	}
	s := string(v.Fit)
	if len(v.Reasons) > 0 {
		s += ": " + strings.Join(v.Reasons, "; ")
	}
	if len(size) > 0 {
		s += "; try " + strings.Join(size, ", ")
	}
	return s
}

// Judge says how the VM's size fit the job that ran on it.
func Judge(r Report) Verdict {
	if r.Seconds < minSeconds {
		return Verdict{Fit: Short}
	}
	var v Verdict
	under, over := false, false

	// CPU.
	switch {
	case r.HasPSI && r.CPUSome >= cpuSomeHigh:
		under = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("tasks waited for a CPU %.0f%% of the time", r.CPUSome))
		// Waiting near all of the time: twice the CPUs; less, half again.
		f := 1.5
		if r.CPUSome >= 50 {
			f = 2
		}
		v.CPUs = int(math.Ceil(float64(r.CPUs) * f))
	case r.CPUs > 1 && r.CPUPeak <= cpuPeakLow*float64(r.CPUs) && (!r.HasPSI || r.CPUSome < cpuSomeLow):
		over = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("at most %.1f of %d CPUs busy", r.CPUPeak, r.CPUs))
		v.CPUs = max(1, int(math.Ceil(r.CPUPeak*headroomCPU)))
	}

	// Memory.
	memShort := r.HasPSI && (r.MemSome >= memSomeHigh || r.MemFull >= memFullHigh)
	switch {
	case memShort || float64(r.MemPeakMB) >= memPeakHigh*float64(r.MemoryMB):
		under = true
		if memShort {
			v.Reasons = append(v.Reasons, fmt.Sprintf("stalled on memory %.1f%% of the time (all tasks %.1f%%)", r.MemSome, r.MemFull))
		} else {
			v.Reasons = append(v.Reasons, fmt.Sprintf("peaked at %s of %s memory", gb(r.MemPeakMB), gb(r.MemoryMB)))
		}
		v.MemoryMB = roundGB(max(float64(r.MemoryMB)*headroomMemory, float64(r.MemPeakMB)*headroomMemory))
	case r.MemoryMB > 1024 && float64(r.MemPeakMB) <= memPeakLow*float64(r.MemoryMB) && (!r.HasPSI || r.MemSome < memSomeLow):
		over = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("used at most %s of %s memory", gb(r.MemPeakMB), gb(r.MemoryMB)))
		v.MemoryMB = max(1024, roundGB(float64(r.MemPeakMB)*headroomMemory))
	}

	// I/O: what is left after CPU and memory.
	io := r.HasPSI && r.IOSome >= ioSomeIOBound
	if r.HasPSI && r.IOSome >= ioSomeHigh && !under && float64(r.MemPeakMB) >= memPeakIO*float64(r.MemoryMB) {
		// A VM near full of memory that waits on its disk is most often
		// reading again the files the kernel had to drop from its cache:
		// memory pressure that PSI counts as I/O. Seen with 3.3 GB held on
		// 4 GB: memory some 0.5%, io some 21%.
		under, io = true, false
		v.Reasons = append(v.Reasons, fmt.Sprintf("waited on the disk %.0f%% of the time with memory %.0f%% in use: files read again once dropped from the cache",
			r.IOSome, 100*float64(r.MemPeakMB)/float64(r.MemoryMB)))
		v.MemoryMB = roundGB(float64(r.MemoryMB) * headroomMemory)
	}
	if io {
		v.Reasons = append(v.Reasons, fmt.Sprintf("waited on the disk %.0f%% of the time", r.IOSome))
	}

	switch {
	case under:
		v.Fit = Undersized
		// A dimension that was too big stays as it is while the other is
		// too small: one change at a time.
		if !(r.HasPSI && r.CPUSome >= cpuSomeHigh) {
			v.CPUs = 0
		}
		if v.MemoryMB > 0 && v.MemoryMB < r.MemoryMB {
			v.MemoryMB = 0
		}
		v.Reasons = keep(v.Reasons, func(s string) bool {
			return !strings.HasPrefix(s, "at most") && !strings.HasPrefix(s, "used at most")
		})
	case io:
		v.Fit = IOBound
		v.CPUs, v.MemoryMB = 0, 0
		v.Reasons = keep(v.Reasons, func(s string) bool { return strings.HasPrefix(s, "waited on the disk") })
	case over:
		v.Fit = Oversized
	default:
		v.Fit = Fits
	}
	return v
}

func keep(s []string, f func(string) bool) []string {
	var out []string
	for _, x := range s {
		if f(x) {
			out = append(out, x)
		}
	}
	return out
}

// roundGB rounds MB up to a whole GB.
func roundGB(mb float64) int { return int(math.Ceil(mb/1024)) * 1024 }

func gb(mb int) string {
	if mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(mb)/1024)
}
