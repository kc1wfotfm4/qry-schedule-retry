// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
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

// Job is one registered schedule entry.
type Job struct {
	ID            string
	Schedule      string
	MaxRetries    int
	NextTriggerAt time.Time
	Paused        bool
}

var (
	// ErrJobExists reports a registration that collides with a stored id.
	ErrJobExists = errors.New("job already exists")
	// ErrJobNotFound reports a lookup for an id that was never registered.
	ErrJobNotFound = errors.New("job not found")
)

// RegisterJob inserts a job inside a single transaction so a failure anywhere
// leaves no record behind. When baseTime is nil the transaction takes the
// current time once and uses it as the reference for nextRun.
func (s *Store) RegisterJob(id, schedule string, maxRetries int, baseTime *time.Time, nextRun func(time.Time) time.Time) (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, fmt.Errorf("begin register job: %w", err)
	}
	defer tx.Rollback()

	reference := time.Now().UTC()
	if baseTime != nil {
		reference = baseTime.UTC()
	}
	next := nextRun(reference).UTC().Truncate(time.Minute)

	var existing int
	if err := tx.QueryRow("SELECT COUNT(1) FROM jobs WHERE id = ?", id).Scan(&existing); err != nil {
		return Job{}, fmt.Errorf("check job id: %w", err)
	}
	if existing > 0 {
		return Job{}, ErrJobExists
	}
	if _, err := tx.Exec(
		"INSERT INTO jobs (id, schedule, max_retries, next_trigger_at, paused) VALUES (?, ?, ?, ?, 0)",
		id, schedule, maxRetries, next.Format(time.RFC3339),
	); err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit job: %w", err)
	}
	return Job{ID: id, Schedule: schedule, MaxRetries: maxRetries, NextTriggerAt: next, Paused: false}, nil
}

// GetJob reads a job exactly as stored; nothing is recomputed.
func (s *Store) GetJob(id string) (Job, error) {
	var (
		job       Job
		nextRaw   string
		pausedRaw int
	)
	err := s.db.QueryRow(
		"SELECT id, schedule, max_retries, next_trigger_at, paused FROM jobs WHERE id = ?", id,
	).Scan(&job.ID, &job.Schedule, &job.MaxRetries, &nextRaw, &pausedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read job: %w", err)
	}
	next, err := time.Parse(time.RFC3339, nextRaw)
	if err != nil {
		return Job{}, fmt.Errorf("parse stored trigger time: %w", err)
	}
	job.NextTriggerAt = next
	job.Paused = pausedRaw != 0
	return job, nil
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
	next_trigger_at TEXT NOT NULL,
	paused          INTEGER NOT NULL DEFAULT 0
);
`
