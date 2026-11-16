package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

func aRun(job string, i int) Run {
	return Run{Time: time.Unix(int64(i), 0).UTC(), Forge: "gitlab", Job: job, Class: "c",
		Report: Report{Seconds: float64(i), CPUs: 2, MemoryMB: 4096, MemPeakMB: 1000 + i}}
}

func writeLines(t *testing.T, file string, runs ...Run) {
	t.Helper()
	var b []byte
	for _, r := range runs {
		j, _ := json.Marshal(r)
		b = append(append(b, j...), '\n')
	}
	b = append(b, "not json\n"...)
	if err := os.WriteFile(file, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheOldFileIsImported: a usage.jsonl beside the database, as fleetci
// kept before, is read in once and set aside.
func TestTheOldFileIsImported(t *testing.T) {
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "usage.jsonl"), aRun("a", 1), aRun("a", 2), aRun("b", 3))

	rec := NewRecorder(filepath.Join(dir, "usage.db"), hclog.NewNullLogger())
	if n := len(rec.Runs("gitlab", "a")); n != 2 {
		t.Fatalf("a: %d runs, want 2", n)
	}
	rec.Close()
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl.imported")); err != nil {
		t.Fatal(err)
	}
	// Opening again imports nothing twice.
	rec = NewRecorder(filepath.Join(dir, "usage.db"), hclog.NewNullLogger())
	rec.Close()
	runs, err := ReadFile(filepath.Join(dir, "usage.db"))
	if err != nil || len(runs) != 3 {
		t.Fatalf("%d runs, %v; want 3", len(runs), err)
	}
}

// TestAUsageFileOfJSONLinesBecomesTheDatabase: usage_file set to a file of
// JSON lines keeps its name, now a database holding its runs.
func TestAUsageFileOfJSONLinesBecomesTheDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "jobs.log")
	writeLines(t, file, aRun("a", 1), aRun("a", 2))
	if _, err := ReadFile(file); err == nil {
		t.Fatal("read JSON lines as a database")
	}
	rec := NewRecorder(file, hclog.NewNullLogger())
	rec.Record(aRun("a", 3))
	rec.Close()
	runs, err := ReadFile(file)
	if err != nil || len(runs) != 3 {
		t.Fatalf("%d runs, %v; want 3", len(runs), err)
	}
	if _, err := os.Stat(file + ".jsonl.imported"); err != nil {
		t.Fatal(err)
	}
}

// TestEachJobKeepsItsLastRuns: older runs go as new ones come, per job, and
// the recorder starts with each job's last keepRuns.
func TestEachJobKeepsItsLastRuns(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.db")
	s, err := openStore(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := range keepStored + 5 {
		if err := s.add(aRun("busy", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.add(aRun("quiet", 0)); err != nil {
		t.Fatal(err)
	}
	all, err := s.last(0)
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	if len(all) != keepStored+1 {
		t.Fatalf("%d runs kept, want %d", len(all), keepStored+1)
	}
	if all[0].Report.Seconds != 5 {
		t.Fatalf("oldest kept is run %v, want 5", all[0].Report.Seconds)
	}

	rec := NewRecorder(file, hclog.NewNullLogger())
	defer rec.Close()
	busy := rec.Runs("gitlab", "busy")
	if len(busy) != keepRuns || busy[len(busy)-1].Report.Seconds != keepStored+4 {
		t.Fatalf("busy: %d runs ending at %v", len(busy), busy[len(busy)-1].Report.Seconds)
	}
	if n := len(rec.Runs("gitlab", "quiet")); n != 1 {
		t.Fatalf("quiet: %d runs", n)
	}
}

// TestTwoProcessesShareTheDatabase: GitLab's plugin processes sharing a
// state_dir write to one database at once, and none of their runs is lost.
func TestTwoProcessesShareTheDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.db")
	a := NewRecorder(file, hclog.NewNullLogger())
	b := NewRecorder(file, hclog.NewNullLogger())
	var wg sync.WaitGroup
	for i, rec := range []*Recorder{a, b} {
		wg.Go(func() {
			for j := range 50 {
				rec.Record(aRun(fmt.Sprint("job", i), j))
			}
		})
	}
	// Read while they write.
	if _, err := ReadFile(file); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	a.Close()
	b.Close()
	runs, err := ReadFile(file)
	if err != nil || len(runs) != 100 {
		t.Fatalf("%d runs, %v; want 100", len(runs), err)
	}
}

func TestReadingNoDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.db")
	if _, err := ReadFile(file); !os.IsNotExist(err) {
		t.Fatalf("err %v, want not exist", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("reading made the database")
	}
}
