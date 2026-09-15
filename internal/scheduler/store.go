package scheduler

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zdaniels/fathom/internal/security"
	_ "modernc.org/sqlite"
)

// Job is one persisted scheduled task.
type Job struct {
	ID         string
	Cron       string
	Prompt     string
	Skill      string // optional: pin the job to a single skill (e.g. "gmail")
	CreatedAt  time.Time
	LastRunAt  *time.Time
	LastResult string
	NextRunAt  time.Time
}

// Store persists scheduled jobs in SQLite under ~/.fantazm/schedule.db.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore creates / opens the SQLite store. WAL mode for low-write
// contention; idempotent schema creation.
func OpenStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, err
	}
	schema := `
		CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			cron TEXT NOT NULL,
			prompt TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_run_at TEXT,
			last_result TEXT,
			next_run_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_jobs_next_run ON jobs(next_run_at);
		CREATE TABLE IF NOT EXISTS routines (
			name TEXT PRIMARY KEY,
			prompt TEXT NOT NULL,
			skill TEXT,
			created_at TEXT NOT NULL
		);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema create: %w", err)
	}
	// Migration: add jobs.skill to stores created before skill targeting
	// landed. CREATE TABLE IF NOT EXISTS can't alter an existing table, so
	// add the column explicitly and tolerate it already being there.
	if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN skill TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, fmt.Errorf("schema migrate: %w", err)
	}
	return &Store{db: db, path: dbPath}, nil
}

// Close releases the underlying DB.
func (s *Store) Close() error { return s.db.Close() }

// Add inserts a new job with the given cron + prompt (no skill pin). Computes
// the initial next_run_at off `now`.
func (s *Store) Add(cron, prompt string, now time.Time) (Job, error) {
	return s.AddWithSkill(cron, prompt, "", now)
}

// AddWithSkill inserts a new job, optionally pinned to a single skill so the
// scheduled run uses just that skill. An empty skill behaves like Add.
func (s *Store) AddWithSkill(cron, prompt, skill string, now time.Time) (Job, error) {
	parsed, err := ParseCron(cron)
	if err != nil {
		return Job{}, err
	}
	next, err := NextRun(parsed, now)
	if err != nil {
		return Job{}, fmt.Errorf("cron expression %q has no future occurrences", cron)
	}
	job := Job{
		ID:        security.GenerateID(),
		Cron:      parsed.Expression,
		Prompt:    prompt,
		Skill:     skill,
		CreatedAt: now.UTC(),
		NextRunAt: next.UTC(),
	}
	_, err = s.db.Exec(
		`INSERT INTO jobs (id, cron, prompt, skill, created_at, next_run_at) VALUES (?, ?, ?, ?, ?, ?)`,
		job.ID, job.Cron, job.Prompt, job.Skill, job.CreatedAt.Format(time.RFC3339Nano), job.NextRunAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

// List returns all jobs ordered by next_run_at.
func (s *Store) List() ([]Job, error) {
	rows, err := s.db.Query(
		`SELECT id, cron, prompt, skill, created_at, last_run_at, last_result, next_run_at FROM jobs ORDER BY next_run_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get returns a single job by ID, or sql.ErrNoRows.
func (s *Store) Get(id string) (Job, error) {
	row := s.db.QueryRow(
		`SELECT id, cron, prompt, skill, created_at, last_run_at, last_result, next_run_at FROM jobs WHERE id = ?`,
		id,
	)
	return scanJob(row)
}

// Delete removes a job. Returns true if it existed.
func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkRun records a completed run and advances next_run_at. Anchored at
// time.Now(), NOT the captured runAt — for invokers slower than the cron
// interval, anchoring at runAt would write a next_run_at already in the
// past and produce a back-to-back catch-up loop. If the cron has no future
// occurrence (impossible spec), parks 1 year out instead of looping forever.
func (s *Store) MarkRun(id string, runAt time.Time, result string) error {
	job, err := s.Get(id)
	if err != nil {
		return err
	}
	parsed, err := ParseCron(job.Cron)
	if err != nil {
		return err
	}
	next, err := NextRun(parsed, time.Now().UTC())
	var nextStr string
	if err != nil {
		next = time.Now().UTC().Add(365 * 24 * time.Hour)
	}
	nextStr = next.Format(time.RFC3339Nano)

	_, err = s.db.Exec(
		`UPDATE jobs SET last_run_at = ?, last_result = ?, next_run_at = ? WHERE id = ?`,
		runAt.UTC().Format(time.RFC3339Nano), result, nextStr, id,
	)
	return err
}

// Due returns jobs whose next_run_at is at or before now.
func (s *Store) Due(now time.Time) ([]Job, error) {
	rows, err := s.db.Query(
		`SELECT id, cron, prompt, skill, created_at, last_run_at, last_result, next_run_at FROM jobs WHERE next_run_at <= ?`,
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanJob(row scannable) (Job, error) {
	var j Job
	var lastRunStr sql.NullString
	var lastResultStr sql.NullString
	var skillStr sql.NullString
	var createdStr, nextStr string
	if err := row.Scan(&j.ID, &j.Cron, &j.Prompt, &skillStr, &createdStr, &lastRunStr, &lastResultStr, &nextStr); err != nil {
		return Job{}, err
	}
	j.Skill = skillStr.String
	t, err := time.Parse(time.RFC3339Nano, createdStr)
	if err != nil {
		return Job{}, fmt.Errorf("parse createdAt: %w", err)
	}
	j.CreatedAt = t
	t2, err := time.Parse(time.RFC3339Nano, nextStr)
	if err != nil {
		return Job{}, fmt.Errorf("parse nextRunAt: %w", err)
	}
	j.NextRunAt = t2
	if lastRunStr.Valid {
		t3, err := time.Parse(time.RFC3339Nano, lastRunStr.String)
		if err == nil {
			j.LastRunAt = &t3
		}
	}
	if lastResultStr.Valid {
		j.LastResult = lastResultStr.String
	}
	return j, nil
}
