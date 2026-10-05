// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
)

var (
	// ErrJobExists is returned when a job with the same id is already stored.
	ErrJobExists = errors.New("job already exists")
	// ErrJobNotFound is returned when no job carries the requested id.
	ErrJobNotFound = errors.New("job not found")
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Job is the stored view of a registered job.
type Job struct {
	ID            string
	Schedule      string
	MaxRetries    int
	NextTriggerAt time.Time
	Paused        bool
}

// RegisterJobInput carries validated registration data. BaseTime is optional;
// when nil the reference time is taken once inside the registration
// transaction.
type RegisterJobInput struct {
	ID         string
	Schedule   *schedule.Schedule
	MaxRetries int
	BaseTime   *time.Time
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
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

// RegisterJob checks for a duplicate, computes next_trigger_at and inserts the
// job in a single transaction: any failure rolls back and leaves no record.
func (s *Store) RegisterJob(in RegisterJobInput) (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing string
	switch err := tx.QueryRow(`SELECT id FROM jobs WHERE id = ?`, in.ID).Scan(&existing); {
	case err == nil:
		return nil, ErrJobExists
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	reference := time.Now()
	if in.BaseTime != nil {
		reference = *in.BaseTime
	}
	nextTriggerAt := in.Schedule.NextAfter(reference).UTC()

	if _, err := tx.Exec(
		`INSERT INTO jobs (id, schedule, max_retries, next_trigger_at) VALUES (?, ?, ?, ?)`,
		in.ID, in.Schedule.String(), in.MaxRetries, nextTriggerAt.Format(time.RFC3339),
	); err != nil {
		// Concurrent registrations can race past the existence check and hit
		// the primary key constraint; classify that as a duplicate as well.
		if check := tx.QueryRow(`SELECT id FROM jobs WHERE id = ?`, in.ID).Scan(&existing); check == nil {
			return nil, ErrJobExists
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Job{
		ID:            in.ID,
		Schedule:      in.Schedule.String(),
		MaxRetries:    in.MaxRetries,
		NextTriggerAt: nextTriggerAt,
		Paused:        false,
	}, nil
}

// GetJob returns the stored job without recomputing any of its fields.
func (s *Store) GetJob(id string) (*Job, error) {
	var job Job
	var nextTriggerAt string
	err := s.db.QueryRow(
		`SELECT id, schedule, max_retries, next_trigger_at FROM jobs WHERE id = ?`,
		id,
	).Scan(&job.ID, &job.Schedule, &job.MaxRetries, &nextTriggerAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	job.NextTriggerAt, err = time.Parse(time.RFC3339, nextTriggerAt)
	if err != nil {
		return nil, err
	}
	job.Paused = false
	return &job, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	schedule        TEXT NOT NULL,
	max_retries     INTEGER NOT NULL,
	next_trigger_at TEXT NOT NULL
);
`
