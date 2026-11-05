package github

import "testing"

func TestRunningJob(t *testing.T) {
	for _, c := range []struct {
		line, job string
		ok        bool
	}{
		{"2026-11-05 00:49:12Z: Running job: with-service", "with-service", true},
		{"2026-11-05 00:49:12Z: Running job: busy (1)", "busy (1)", true},
		{"√ Connected to GitHub", "", false},
		{"Running job: ", "", false},
	} {
		job, ok := runningJob(c.line)
		if job != c.job || ok != c.ok {
			t.Errorf("runningJob(%q) = %q %v, want %q %v", c.line, job, ok, c.job, c.ok)
		}
	}
}
