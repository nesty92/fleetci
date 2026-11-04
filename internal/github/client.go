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
	"strings"
	"time"
)

// client is the little of GitHub's REST API fleetci needs.
type client struct {
	base  string
	token string
	http  *http.Client
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

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// queuedJobs lists the repo's jobs waiting for a runner. A job can wait in
// a queued run or in one that has already started other jobs.
func (c *client) queuedJobs(ctx context.Context, repo string) ([]job, error) {
	var jobs []job
	for _, status := range []string{"queued", "in_progress"} {
		var runs struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		q := url.Values{"status": {status}, "per_page": {"100"}}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs?"+q.Encode(), nil, &runs); err != nil {
			return nil, err
		}
		for _, r := range runs.Runs {
			var page struct {
				Jobs []job `json:"jobs"`
			}
			path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, r.ID)
			if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
				return nil, err
			}
			for _, j := range page.Jobs {
				if j.Status == "queued" {
					jobs = append(jobs, j)
				}
			}
		}
	}
	return jobs, nil
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
