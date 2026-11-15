package usage

import (
	"cmp"
	"slices"
)

// Summary is a job's runs at a glance.
type Summary struct {
	Forge string
	Job   string
	// Class is the class of its last run, and Runs how many there were.
	Class string
	Runs  int
	// Last is its last run that ran long enough to judge, or the last.
	Last Run
	// Fits counts its judged runs by verdict.
	Fits map[Fit]int
	// CPUPeak and MemPeakMB are the medians of its runs.
	CPUPeak   float64
	MemPeakMB int
}

// Summarize groups runs by job, the jobs that need looking at first:
// undersized, then growing, then oversized, then the rest, by name.
func Summarize(runs []Run) []Summary {
	by := map[string]*Summary{}
	var order []string
	var cpu = map[string][]float64{}
	var mem = map[string][]float64{}
	for _, r := range runs {
		k := key(r.Forge, r.Job)
		s := by[k]
		if s == nil {
			s = &Summary{Forge: r.Forge, Job: r.Job, Fits: map[Fit]int{}}
			by[k] = s
			order = append(order, k)
		}
		s.Runs++
		s.Class = r.Class
		if r.Verdict.Fit != Short || s.Last.Job == "" || s.Last.Verdict.Fit == Short {
			s.Last = r
		}
		if r.Verdict.Fit != Short {
			s.Fits[r.Verdict.Fit]++
			cpu[k] = append(cpu[k], r.Report.CPUPeak)
			mem[k] = append(mem[k], float64(r.Report.MemPeakMB))
		}
	}
	out := make([]Summary, 0, len(order))
	for _, k := range order {
		s := by[k]
		s.CPUPeak = median(cpu[k])
		s.MemPeakMB = int(median(mem[k]))
		out = append(out, *s)
	}
	rank := func(s Summary) int {
		switch {
		case s.Last.Verdict.Fit == Undersized:
			return 0
		case len(s.Last.Trend) > 0:
			return 1
		case s.Last.Verdict.Fit == Oversized:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(out, func(a, b Summary) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Forge, b.Forge), cmp.Compare(a.Job, b.Job))
	})
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	v = slices.Sorted(slices.Values(v))
	if n := len(v); n%2 == 0 {
		return (v[n/2-1] + v[n/2]) / 2
	}
	return v[len(v)/2]
}
