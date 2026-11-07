package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client is the little of GitHub's REST API fleetci needs.
type client struct {
	base  string
	token string
	http  *http.Client

	mu    sync.Mutex
	until time.Time // rate limited: no request before this
}

type job struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

type runner struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

var errNotFound = errors.New("not found")

// statusError is an answer GitHub gave that is not a success.
type statusError struct {
	method, path string
	status       int
	msg          string
}

func (e statusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.method, e.path, e.status, http.StatusText(e.status), e.msg)
}

// refused says whether GitHub answered no to the request itself, so the
// same request will fail again: a 4xx that is no rate limit.
func refused(err error) bool {
	var s statusError
	return errors.As(err, &s) && s.status >= 400 && s.status < 500 && !errors.As(err, new(rateLimited))
}

// rateLimited is GitHub saying to send nothing until a time.
type rateLimited struct{ until time.Time }

func (e rateLimited) Error() string {
	return "rate limited by github until " + e.until.Format(time.TimeOnly)
}

// maxWait caps how long a rate limit holds every request off, so a
// nonsense header cannot stop fleetci for good. GitHub's limits reset
// within an hour.
const maxWait = time.Hour

// rateLimit reads when GitHub lets the next request through, after an
// answer that says the limit is hit: the primary limit spent, or a
// secondary limit, which comes with Retry-After or, failing that, means at
// least a minute.
// https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#handle-rate-limit-errors-appropriately
func rateLimit(resp *http.Response, msg string, now time.Time) (time.Time, bool) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return time.Time{}, false
	}
	h := resp.Header
	var until time.Time
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		until = now.Add(time.Duration(s) * time.Second)
	} else if h.Get("X-RateLimit-Remaining") == "0" {
		until = now.Add(time.Minute)
		if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			until = time.Unix(r, 0)
		}
	} else if resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(msg), "rate limit") {
		until = now.Add(time.Minute)
	} else {
		return time.Time{}, false // forbidden, not limited
	}
	// A clock behind GitHub's would make the reset look past.
	if until.Before(now.Add(time.Second)) {
		until = now.Add(time.Second)
	}
	if until.After(now.Add(maxWait)) {
		until = now.Add(maxWait)
	}
	return until, true
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.mu.Lock()
	until := c.until
	c.mu.Unlock()
	if time.Now().Before(until) {
		return fmt.Errorf("%s %s: %w", method, path, rateLimited{until})
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(raw))
		err := statusError{method, path, resp.StatusCode, msg}
		if until, ok := rateLimit(resp, msg, time.Now()); ok {
			c.mu.Lock()
			if until.After(c.until) {
				c.until = until
			}
			c.mu.Unlock()
			return fmt.Errorf("%w: %w", err, rateLimited{until})
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// queuedJobs lists the repo's jobs waiting for a runner. A job can wait in
// a queued run or in one that has already started other jobs.
//
// A run whose jobs cannot be listed does not hold up the others: the jobs
// that could be listed come back with the error.
func (c *client) queuedJobs(ctx context.Context, repo string) ([]job, error) {
	var jobs []job
	var errs []error
	for _, status := range []string{"queued", "in_progress"} {
		var runs struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		q := url.Values{"status": {status}, "per_page": {"100"}}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs?"+q.Encode(), nil, &runs); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range runs.Runs {
			var page struct {
				Jobs []job `json:"jobs"`
			}
			path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, r.ID)
			if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
				errs = append(errs, err)
				continue
			}
			for _, j := range page.Jobs {
				if j.Status == "queued" {
					jobs = append(jobs, j)
				}
			}
		}
	}
	return jobs, errors.Join(errs...)
}

// jitConfig registers a runner that takes one job and is then removed by
// GitHub. It returns the runner's id and the config for run.sh --jitconfig.
func (c *client) jitConfig(ctx context.Context, repo, name string, groupID int64, labels []string) (int64, string, error) {
	var out struct {
		Runner  runner `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	body := map[string]any{"name": name, "runner_group_id": groupID, "labels": labels, "work_folder": "_work"}
	if err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/actions/runners/generate-jitconfig", body, &out); err != nil {
		return 0, "", err
	}
	return out.Runner.ID, out.Encoded, nil
}

func (c *client) runner(ctx context.Context, repo string, id int64) (runner, error) {
	var r runner
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id), nil, &r)
	return r, err
}

// runnersNamed lists the ids of the repo's runners called name.
func (c *client) runnersNamed(ctx context.Context, repo, name string) ([]int64, error) {
	var out struct {
		Runners []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runners"`
	}
	q := url.Values{"name": {name}, "per_page": {"100"}}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runners?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range out.Runners {
		if r.Name == name {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func (c *client) deleteRunner(ctx context.Context, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}
