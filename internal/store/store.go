// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
)

// ErrJobExists is returned when creating a job whose name is already taken.
var ErrJobExists = errors.New("job already exists")

// ErrJobNotFound is returned when no job carries the requested name.
var ErrJobNotFound = errors.New("job not found")

// ErrJobPaused is returned when an attempt is recorded against a paused job.
var ErrJobPaused = errors.New("job is paused")

// ErrSequenceConflict is returned when an attempt reuses a stored sequence.
var ErrSequenceConflict = errors.New("attempt sequence already exists")

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Job is the complete persisted state of one job.
type Job struct {
	Job        string    `json:"job"`
	Schedule   string    `json:"schedule"`
	MaxRetries int       `json:"maxRetries"`
	Retries    int       `json:"retries"`
	Paused     bool      `json:"paused"`
	NextRunAt  time.Time `json:"nextRunAt"`
	Attempts   []Attempt `json:"attempts"`
}

// Attempt is one recorded execution attempt.
type Attempt struct {
	Sequence    int       `json:"sequence"`
	Outcome     string    `json:"outcome"`
	AttemptedAt time.Time `json:"attemptedAt"`
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite permits one writer per database. A single pooled connection turns
	// write transactions into a serial queue so duplicate sequences cannot race.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// CreateJob inserts a new, unpaused job. NextRunAt is the first cron match
// strictly after the transaction's current time. A duplicate name returns
// ErrJobExists.
func (s *Store) CreateJob(ctx context.Context, job string, cronExpr string, maxRetries int) (*Job, error) {
	cron, err := schedule.Parse(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("parse schedule: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM jobs WHERE job = ?`, job).Scan(&existing); err != nil {
		return nil, fmt.Errorf("check job: %w", err)
	}
	if existing > 0 {
		return nil, ErrJobExists
	}

	now := currentTime(ctx, tx)
	nextRun, err := cron.NextAfter(now)
	if err != nil {
		return nil, fmt.Errorf("compute next run: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (job, schedule, max_retries, retries, paused, next_run_at)
		 VALUES (?, ?, ?, 0, 0, ?)`,
		job, cronExpr, maxRetries, formatTime(nextRun)); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrJobExists
		}
		return nil, fmt.Errorf("insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrJobExists
		}
		return nil, fmt.Errorf("commit job: %w", err)
	}
	return s.GetJob(ctx, job)
}

// GetJob returns the complete state, including attempts in sequence order.
func (s *Store) GetJob(ctx context.Context, job string) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	return loadJob(ctx, tx, job)
}

// RecordAttempt stores one attempt and updates retry/backoff/paused state
// atomically. success clears the retry count and advances the cron schedule
// from attemptedAt; failure increments retries and sets the exponential
// backoff, and the failure that reaches the limit pauses the job without
// advancing the schedule.
func (s *Store) RecordAttempt(ctx context.Context, job string, sequence int, outcome string, attemptedAt time.Time) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	j, err := loadJob(ctx, tx, job)
	if err != nil {
		return nil, err
	}
	if j.Paused {
		return nil, ErrJobPaused
	}
	for _, attempt := range j.Attempts {
		if attempt.Sequence == sequence {
			return nil, ErrSequenceConflict
		}
	}

	attemptedAt = attemptedAt.UTC()
	var nextRun time.Time
	paused := false
	retries := j.Retries

	switch outcome {
	case "success":
		retries = 0
		nextRun, err = mustNext(j.Schedule, attemptedAt)
		if err != nil {
			return nil, err
		}
	default: // "failure", guaranteed by API validation
		if j.Retries >= j.MaxRetries {
			paused = true
			nextRun = j.NextRunAt
			retries = j.Retries
		} else {
			retries = j.Retries + 1
			nextRun = attemptedAt.Add(backoff(retries))
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts (job, sequence, outcome, attempted_at) VALUES (?, ?, ?, ?)`,
		job, sequence, outcome, formatTime(attemptedAt)); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSequenceConflict
		}
		return nil, fmt.Errorf("insert attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET retries = ?, paused = ?, next_run_at = ? WHERE job = ?`,
		retries, boolToInt(paused), formatTime(nextRun), job); err != nil {
		return nil, fmt.Errorf("update job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSequenceConflict
		}
		return nil, fmt.Errorf("commit attempt: %w", err)
	}
	return s.GetJob(ctx, job)
}

// SetPaused replaces the paused flag without touching attempts or retry state.
func (s *Store) SetPaused(ctx context.Context, job string, paused bool) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := loadJob(ctx, tx, job); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET paused = ? WHERE job = ?`, boolToInt(paused), job); err != nil {
		return nil, fmt.Errorf("update paused: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit paused: %w", err)
	}
	return s.GetJob(ctx, job)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadJob(ctx context.Context, q rowQuerier, job string) (*Job, error) {
	j := &Job{Job: job, Attempts: []Attempt{}}
	var nextRunText string
	err := q.QueryRowContext(ctx,
		`SELECT schedule, max_retries, retries, paused, next_run_at FROM jobs WHERE job = ?`, job).
		Scan(&j.Schedule, &j.MaxRetries, &j.Retries, &j.Paused, &nextRunText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load job: %w", err)
	}
	if j.NextRunAt, err = parseTime(nextRunText); err != nil {
		return nil, err
	}

	rows, err := q.(queryer).QueryContext(ctx,
		`SELECT sequence, outcome, attempted_at FROM attempts WHERE job = ? ORDER BY sequence ASC`, job)
	if err != nil {
		return nil, fmt.Errorf("load attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attempt Attempt
		var attemptedText string
		if err := rows.Scan(&attempt.Sequence, &attempt.Outcome, &attemptedText); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		if attempt.AttemptedAt, err = parseTime(attemptedText); err != nil {
			return nil, err
		}
		j.Attempts = append(j.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attempts: %w", err)
	}
	return j, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func mustNext(cronExpr string, after time.Time) (time.Time, error) {
	cron, err := schedule.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse schedule: %w", err)
	}
	nextRun, err := cron.NextAfter(after.UTC())
	if err != nil {
		return time.Time{}, fmt.Errorf("compute next run: %w", err)
	}
	return nextRun, nil
}

// backoff returns 2^retries minutes, guarding against time.Duration overflow.
func backoff(retries int) time.Duration {
	minutes := int64(1)
	if retries < 63 {
		minutes = int64(1) << uint(retries)
	}
	nanos := minutes * int64(time.Minute)
	if retries >= 63 || nanos/minutes != int64(time.Minute) || nanos < 0 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(nanos)
}

func currentTime(ctx context.Context, tx *sql.Tx) time.Time {
	var text string
	if err := tx.QueryRowContext(ctx, `SELECT strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`).Scan(&text); err != nil {
		return time.Now().UTC()
	}
	parsed, err := parseTime(text)
	if err != nil {
		return time.Now().UTC()
	}
	return parsed
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time: %w", err)
	}
	return parsed.UTC(), nil
}

func isUniqueViolation(err error) bool {
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case 19, // SQLITE_CONSTRAINT
			2067, // SQLITE_CONSTRAINT_UNIQUE
			1555: // SQLITE_CONSTRAINT_PRIMARYKEY
			return true
		}
	}
	return false
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	job          TEXT PRIMARY KEY,
	schedule     TEXT NOT NULL,
	max_retries  INTEGER NOT NULL,
	retries      INTEGER NOT NULL DEFAULT 0,
	paused       INTEGER NOT NULL DEFAULT 0,
	next_run_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attempts (
	job           TEXT NOT NULL,
	sequence      INTEGER NOT NULL,
	outcome       TEXT NOT NULL,
	attempted_at  TEXT NOT NULL,
	PRIMARY KEY (job, sequence),
	FOREIGN KEY (job) REFERENCES jobs(job) ON DELETE CASCADE
);
`
