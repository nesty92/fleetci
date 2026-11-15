package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/nesty92/fleetci/internal/fleetci"
	jobusage "github.com/nesty92/fleetci/internal/usage"
)

// usageCmd prints what each job used of its VM, from the recorder's file:
// the jobs whose class is too small first, then those outgrowing theirs,
// then those whose class is too big.
func usageCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	path := configFlag(fs)
	file := fs.String("file", "", "the usage file; default fleetci.toml's usage_file, or usage.jsonl in its state_dir, or "+defaultUsageFile)
	job := fs.String("job", "", "only the jobs whose name holds this")
	runs := fs.Bool("runs", false, "every run of the jobs, not a line per job")
	_ = fs.Parse(args)

	if *file == "" {
		*file = defaultUsageFile
		if c, err := fleetci.Load(*path); err == nil {
			if c.UsageFile != "" {
				*file = c.UsageFile
			} else if c.StateDir != "" {
				*file = filepath.Join(c.StateDir, "usage.jsonl")
			}
		}
	}
	all, err := jobusage.ReadFile(*file)
	if err != nil {
		return err
	}
	var picked []jobusage.Run
	for _, r := range all {
		if strings.Contains(r.Job, *job) {
			picked = append(picked, r)
		}
	}
	if len(picked) == 0 {
		fmt.Fprintln(out, "no jobs measured yet in", *file)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	defer tw.Flush()
	if *runs {
		fmt.Fprintln(tw, "WHEN\tFORGE\tJOB\tCLASS\tTOOK\tCPU PEAK\tMEM PEAK\tCPU WAIT\tMEM WAIT\tIO WAIT\tFIT")
		for _, r := range picked {
			p := r.Report
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.1f/%d\t%.1f/%.0f GB\t%s\t%s\t%s\t%s\n",
				r.Time.Local().Format("01-02 15:04"), r.Forge, r.Job, r.Class, minutes(p.Seconds),
				p.CPUPeak, p.CPUs, float64(p.MemPeakMB)/1024, float64(p.MemoryMB)/1024,
				pct(p.HasPSI, p.CPUSome), pct(p.HasPSI, p.MemSome), pct(p.HasPSI, p.IOSome), advice(r))
		}
		return nil
	}
	fmt.Fprintln(tw, "FORGE\tJOB\tCLASS\tRUNS\tCPU PEAK\tMEM PEAK\tLAST")
	for _, s := range jobusage.Summarize(picked) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%.1f/%d\t%.1f/%.0f GB\t%s\n", s.Forge, s.Job, s.Class, s.Runs,
			s.CPUPeak, s.Last.Report.CPUs, float64(s.MemPeakMB)/1024, float64(s.Last.Report.MemoryMB)/1024, advice(s.Last))
	}
	return nil
}

const defaultUsageFile = "/var/lib/fleetci/usage.jsonl"

func advice(r jobusage.Run) string {
	s := r.Verdict.Advice()
	if len(r.Trend) > 0 {
		s += "; trend: " + strings.Join(r.Trend, ", ")
	}
	return s
}

func pct(ok bool, v float64) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", v)
}

func minutes(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.0fs", s)
	}
	return fmt.Sprintf("%dm%02.0fs", int(s)/60, s-float64(int(s)/60*60))
}
