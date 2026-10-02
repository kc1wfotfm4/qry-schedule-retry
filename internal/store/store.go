// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Job is the complete persisted state of one scheduled job.
type Job struct {
	Job            string
	Schedule       string
	MaxRetries     int
	FailureRetries int
	Paused         bool
	NextRunAt      time.Time
}

// Attempt is one recorded execution attempt.
type Attempt struct {
	Sequence    int
	Outcome     string
	AttemptedAt time.Time
}

var (
	// ErrJobExists is returned when creating a job whose name is already stored.
	ErrJobExists = errors.New("job already exists")
	// ErrJobNotFound is returned when no job carries the requested name.
	ErrJobNotFound = errors.New("job not found")
	// ErrSequenceConflict is returned when an attempt reuses a stored sequence.
	ErrSequenceConflict = errors.New("attempt sequence already exists")
	// ErrJobPaused is returned when an attempt is posted to a paused job.
	ErrJobPaused = errors.New("job is paused")
)

// PrepareNext builds the next-trigger function for a stored schedule expression.
type PrepareNext func(schedule string) (NextFunc, error)

// NextFunc computes the next trigger instant strictly after now for a schedule.
type NextFunc func(now time.Time) (time.Time, error)

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Serializing access on one connection makes every transaction a faithful
	// single-writer critical section without lock-upgrade deadlocks.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
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

// CreateJob stores a new job atomically. nextRunAt is computed from the current
// time observed inside the transaction.
func (s *Store) CreateJob(job, schedule string, maxRetries int, nextFor NextFunc) (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE job = ?`, job).Scan(&existing); err != nil {
		return nil, fmt.Errorf("check job: %w", err)
	}
	if existing > 0 {
		return nil, ErrJobExists
	}

	now := time.Now().UTC()
	nextRunAt, err := nextFor(now)
	if err != nil {
		return nil, err
	}

	created := &Job{
		Job:            job,
		Schedule:       schedule,
		MaxRetries:     maxRetries,
		FailureRetries: 0,
		Paused:         false,
		NextRunAt:      nextRunAt,
	}
	if _, err := tx.Exec(
		`INSERT INTO jobs (job, schedule, max_retries, failure_retries, paused, next_run_at)
		 VALUES (?, ?, ?, 0, 0, ?)`,
		created.Job, created.Schedule, created.MaxRetries, formatTime(created.NextRunAt),
	); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrJobExists
		}
		return nil, fmt.Errorf("insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrJobExists
		}
		return nil, fmt.Errorf("commit: %w", err)
	}
	return created, nil
}

// GetJob loads a job and its attempts ordered by ascending sequence.
func (s *Store) GetJob(name string) (*Job, []Attempt, error) {
	job := &Job{}
	var nextRunAt string
	err := s.db.QueryRow(
		`SELECT job, schedule, max_retries, failure_retries, paused, next_run_at FROM jobs WHERE job = ?`,
		name,
	).Scan(&job.Job, &job.Schedule, &job.MaxRetries, &job.FailureRetries, &job.Paused, &nextRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrJobNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load job: %w", err)
	}
	job.NextRunAt, err = parseStoredTime(nextRunAt)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.Query(
		`SELECT sequence, outcome, attempted_at FROM attempts WHERE job = ? ORDER BY sequence ASC`,
		name,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("load attempts: %w", err)
	}
	defer rows.Close()

	attempts := []Attempt{}
	for rows.Next() {
		var attempt Attempt
		var attemptedAt string
		if err := rows.Scan(&attempt.Sequence, &attempt.Outcome, &attemptedAt); err != nil {
			return nil, nil, fmt.Errorf("scan attempt: %w", err)
		}
		attempt.AttemptedAt, err = parseStoredTime(attemptedAt)
		if err != nil {
			return nil, nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate attempts: %w", err)
	}
	return job, attempts, nil
}

// RecordAttempt applies one attempt and all of its schedule/retry side effects in
// a single transaction. Pause and sequence-conflict checks participate in that
// transaction so concurrent callers serialize into one committed outcome. The
// returned job reflects the committed state.
func (s *Store) RecordAttempt(name string, attempt Attempt, prepareNext PrepareNext) (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	job, err := loadJobTx(tx, name)
	if err != nil {
		return nil, err
	}
	if job.Paused {
		return nil, ErrJobPaused
	}

	var used int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM attempts WHERE job = ? AND sequence = ?`,
		name, attempt.Sequence,
	).Scan(&used); err != nil {
		return nil, fmt.Errorf("check sequence: %w", err)
	}
	if used > 0 {
		return nil, ErrSequenceConflict
	}

	if _, err := tx.Exec(
		`INSERT INTO attempts (job, sequence, outcome, attempted_at) VALUES (?, ?, ?, ?)`,
		name, attempt.Sequence, attempt.Outcome, formatTime(attempt.AttemptedAt),
	); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSequenceConflict
		}
		return nil, fmt.Errorf("insert attempt: %w", err)
	}

	switch attempt.Outcome {
	case "success":
		nextFor, err := prepareNext(job.Schedule)
		if err != nil {
			return nil, err
		}
		job.FailureRetries = 0
		job.NextRunAt, err = nextFor(attempt.AttemptedAt)
		if err != nil {
			return nil, err
		}
	case "failure":
		if job.FailureRetries >= job.MaxRetries {
			job.Paused = true
		} else {
			job.FailureRetries++
			job.NextRunAt = attempt.AttemptedAt.Add(retryBackoff(job.FailureRetries))
		}
	}

	if _, err := tx.Exec(
		`UPDATE jobs
		 SET failure_retries = ?, paused = ?, next_run_at = ?
		 WHERE job = ?`,
		job.FailureRetries, boolToInt(job.Paused), formatTime(job.NextRunAt), name,
	); err != nil {
		return nil, fmt.Errorf("update job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSequenceConflict
		}
		return nil, fmt.Errorf("commit: %w", err)
	}
	return job, nil
}

// SetPaused replaces the pause flag and returns the untouched job state.
func (s *Store) SetPaused(name string, paused bool) (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	job, err := loadJobTx(tx, name)
	if err != nil {
		return nil, err
	}
	job.Paused = paused
	if _, err := tx.Exec(`UPDATE jobs SET paused = ? WHERE job = ?`, boolToInt(job.Paused), name); err != nil {
		return nil, fmt.Errorf("update paused: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return job, nil
}

func loadJobTx(tx *sql.Tx, name string) (*Job, error) {
	job := &Job{}
	var nextRunAt string
	err := tx.QueryRow(
		`SELECT job, schedule, max_retries, failure_retries, paused, next_run_at FROM jobs WHERE job = ?`,
		name,
	).Scan(&job.Job, &job.Schedule, &job.MaxRetries, &job.FailureRetries, &job.Paused, &nextRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load job: %w", err)
	}
	job.NextRunAt, err = parseStoredTime(nextRunAt)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// retryBackoff returns 2^retries minutes using arbitrary precision so absurd retry
// counts cannot overflow.
func retryBackoff(retries int) time.Duration {
	base := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(retries)), nil)
	if !base.IsInt64() {
		return time.Duration(math.MaxInt64)
	}
	minutes := base.Int64()
	if minutes > (1<<63-1)/int64(time.Minute) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(minutes) * time.Minute
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseStoredTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time: %w", err)
	}
	return parsed.UTC(), nil
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	job             TEXT PRIMARY KEY,
	schedule        TEXT NOT NULL,
	max_retries     INTEGER NOT NULL,
	failure_retries INTEGER NOT NULL,
	paused          INTEGER NOT NULL,
	next_run_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS attempts (
	job          TEXT NOT NULL,
	sequence     INTEGER NOT NULL,
	outcome      TEXT NOT NULL,
	attempted_at TEXT NOT NULL,
	PRIMARY KEY (job, sequence),
	FOREIGN KEY (job) REFERENCES jobs(job)
) WITHOUT ROWID;
`
