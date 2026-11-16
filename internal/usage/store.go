package usage

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, so fleetci builds without cgo
)

// keepStored is how many runs of each job the database keeps; older ones
// are deleted as new ones come.
const keepStored = 1000

// A store keeps runs in a SQLite database. Several processes may write to
// one: GitLab's plugin runs one per runner, sharing a state_dir.
type store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id    INTEGER PRIMARY KEY,
	time  TEXT NOT NULL,
	forge TEXT NOT NULL,
	job   TEXT NOT NULL,
	run   TEXT NOT NULL -- the Run, as JSON
);
CREATE INDEX IF NOT EXISTS runs_by_job ON runs (forge, job, id);
`

// dsn opens file waiting up to 5s on another process's write; WAL lets
// readers go on while one writes.
func dsn(file string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		q.Add("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	return "file:" + (&url.URL{Path: file}).EscapedPath() + "?" + q.Encode()
}

// openStore opens or creates the database at file. A file of JSON lines
// there, or usage.jsonl beside it (the format and name before), is read
// into it once and renamed with ".imported" on the end.
func openStore(file string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	var legacy []string
	if isJSONLines(file) {
		old := file + ".jsonl"
		if err := os.Rename(file, old); err != nil {
			return nil, err
		}
		legacy = append(legacy, old)
	}
	if old := filepath.Join(filepath.Dir(file), "usage.jsonl"); old != file {
		if _, err := os.Stat(old); err == nil {
			legacy = append(legacy, old)
		}
	}
	db, err := sql.Open("sqlite", dsn(file, false))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	s := &store{db: db}
	for _, old := range legacy {
		if err := s.importLines(old); err != nil {
			db.Close()
			return nil, fmt.Errorf("importing %s: %w", old, err)
		}
		if err := os.Rename(old, old+".imported"); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// isJSONLines says whether file holds something other than a SQLite
// database: the JSON lines fleetci kept runs in before.
func isJSONLines(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return n > 0 && !bytes.Equal(head[:n], []byte("SQLite format 3\x00")[:n])
}

func (s *store) importLines(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var run Run
		if json.Unmarshal(sc.Bytes(), &run) != nil || run.Job == "" {
			continue
		}
		if err := insert(tx, run); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insert(e execer, run Run) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = e.Exec(`INSERT INTO runs (time, forge, job, run) VALUES (?, ?, ?, ?)`,
		run.Time.UTC().Format(time.RFC3339Nano), run.Forge, run.Job, string(b))
	return err
}

// add keeps run, and deletes the job's runs older than its last
// keepStored.
func (s *store) add(run Run) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insert(tx, run); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM runs WHERE forge = ?1 AND job = ?2 AND id <= (
		SELECT id FROM runs WHERE forge = ?1 AND job = ?2 ORDER BY id DESC LIMIT 1 OFFSET ?3)`,
		run.Forge, run.Job, keepStored); err != nil {
		return err
	}
	return tx.Commit()
}

// last reads each job's last n runs, oldest first; n < 1 is all of them.
func (s *store) last(n int) ([]Run, error) {
	q, args := `SELECT run FROM runs ORDER BY id`, []any(nil)
	if n > 0 {
		q, args = `SELECT run FROM (
			SELECT run, id, row_number() OVER (PARTITION BY forge, job ORDER BY id DESC) AS nth FROM runs
		) WHERE nth <= ? ORDER BY id`, []any{n}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var run Run
		if json.Unmarshal(b, &run) == nil {
			out = append(out, run)
		}
	}
	return out, rows.Err()
}

func (s *store) close() error { return s.db.Close() }

// ReadFile reads every run kept in the database at file, oldest first,
// without changing it.
func ReadFile(file string) ([]Run, error) {
	if _, err := os.Stat(file); err != nil {
		return nil, err
	}
	if isJSONLines(file) {
		return nil, errors.New(file + " is the older JSON lines file; fleetci imports it into its database when it next starts")
	}
	db, err := sql.Open("sqlite", dsn(file, true))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return (&store{db: db}).last(0)
}
