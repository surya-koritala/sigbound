package cell

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo); registers "sqlite"
)

// Store is the coordination state: a small SQLite database for durability and
// observability of runs, branches, changes, the landing queue, and conflicts.
//
// It is deliberately OFF THE HOT PATH. The OCC partitioner runs entirely in
// memory (inverted index + union-find) and is never consulted here; the store
// only persists results after the fact. This keeps the engine's measured time
// about git and partitioning, not the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS run (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    base_sha    TEXT NOT NULL,
    strategy    TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    final_sha   TEXT,
    duration_ns INTEGER,
    groups      INTEGER,
    max_batch   INTEGER
);
CREATE TABLE IF NOT EXISTS branch (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id   INTEGER NOT NULL REFERENCES run(id),
    name     TEXT NOT NULL,
    head_sha TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS change (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    branch_id INTEGER NOT NULL REFERENCES branch(id),
    path      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS queue (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id   INTEGER NOT NULL REFERENCES run(id),
    group_no INTEGER NOT NULL,
    branch   TEXT NOT NULL,
    state    TEXT NOT NULL   -- landed | flagged
);
CREATE TABLE IF NOT EXISTS conflict (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES run(id),
    branch TEXT NOT NULL,
    paths  TEXT NOT NULL     -- newline-joined conflicting paths
);
`

// OpenStore opens (creating if needed) the SQLite database at path and applies
// the schema. Use ":memory:" for tests. A pragma turns on WAL + normal sync for
// reasonable durability without dominating write latency.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single writer avoids "database is locked" churn; we're off the hot path.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// StartRun records a new integration run and returns its id.
func (s *Store) StartRun(ctx context.Context, baseSHA, strategy string) (int64, error) {
	r, err := s.db.ExecContext(ctx,
		`INSERT INTO run (base_sha, strategy, started_at) VALUES (?, ?, ?)`,
		baseSHA, strategy, time.Now().UnixNano())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// RecordBranch persists a branch and its write-set (changed paths) for a run.
func (s *Store) RecordBranch(ctx context.Context, runID int64, name, headSHA string, paths []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO branch (run_id, name, head_sha) VALUES (?, ?, ?)`, runID, name, headSHA)
	if err != nil {
		return err
	}
	bid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO change (branch_id, path) VALUES (?, ?)`, bid, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FinishRun completes a run with the integration result and persists per-branch
// queue outcomes and any flagged conflicts.
func (s *Store) FinishRun(ctx context.Context, runID int64, res IntegrationResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE run SET final_sha=?, duration_ns=?, groups=?, max_batch=? WHERE id=?`,
		res.FinalSHA, res.Duration.Nanoseconds(), res.Groups, res.MaxBatch, runID); err != nil {
		return err
	}
	for _, b := range res.Landed {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO queue (run_id, group_no, branch, state) VALUES (?, 0, ?, 'landed')`,
			runID, b); err != nil {
			return err
		}
	}
	for _, f := range res.Flagged {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO queue (run_id, group_no, branch, state) VALUES (?, 0, ?, 'flagged')`,
			runID, f.Branch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conflict (run_id, branch, paths) VALUES (?, ?, ?)`,
			runID, f.Branch, strings.Join(f.Conflicts, "\n")); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Counts returns row counts per table, for observability / test assertions.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, tbl := range []string{"run", "branch", "change", "queue", "conflict"} {
		var n int
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n); err != nil {
			return nil, err
		}
		out[tbl] = n
	}
	return out, nil
}
