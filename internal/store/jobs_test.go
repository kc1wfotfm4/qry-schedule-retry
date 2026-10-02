package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCreateAndGetJob(t *testing.T) {
	st := openStore(t)
	job, err := st.CreateJob(context.Background(), "nightly", "30 2 * * *", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Paused || job.Retries != 0 {
		t.Fatalf("initial state wrong: %+v", job)
	}
	if job.NextRunAt.UTC().Format("15:04") != "02:30" {
		t.Fatalf("next run = %s", job.NextRunAt)
	}
	if _, err := st.CreateJob(context.Background(), "nightly", "0 * * * *", 0); !errors.Is(err, ErrJobExists) {
		t.Fatalf("duplicate err = %v, want ErrJobExists", err)
	}
	if _, err := st.GetJob(context.Background(), "missing"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestFailureAtomicUpdateAndDuplicateSequence(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateJob(ctx, "j", "0 * * * *", 1); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	job, err := st.RecordAttempt(ctx, "j", 1, "failure", at)
	if err != nil {
		t.Fatal(err)
	}
	if job.Retries != 1 || !job.NextRunAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("after first failure: %+v", job)
	}
	if _, err := st.RecordAttempt(ctx, "j", 1, "success", at); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("dup sequence err = %v", err)
	}
}

func TestExhaustionPausesWithoutAdvancing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateJob(ctx, "j", "0 0 * * *", 1); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.RecordAttempt(ctx, "j", 1, "failure", at); err != nil {
		t.Fatal(err)
	}
	job, err := st.RecordAttempt(ctx, "j", 2, "failure", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !job.Paused || job.Retries != 1 {
		t.Fatalf("state = %+v", job)
	}
	want := at.Add(2 * time.Minute)
	if !job.NextRunAt.Equal(want) {
		t.Fatalf("nextRunAt = %s, want unchanged %s", job.NextRunAt, want)
	}
	if len(job.Attempts) != 2 {
		t.Fatalf("attempts len = %d", len(job.Attempts))
	}
	if _, err := st.RecordAttempt(ctx, "j", 3, "success", at.Add(2*time.Hour)); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("paused err = %v", err)
	}
}

func TestSetPausedKeepsScheduleState(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateJob(ctx, "j", "0 * * * *", 2); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.RecordAttempt(ctx, "j", 1, "failure", at); err != nil {
		t.Fatal(err)
	}
	paused, err := st.SetPaused(ctx, "j", true)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || paused.Retries != 1 || !paused.NextRunAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("paused state = %+v", paused)
	}
	if _, err := st.SetPaused(ctx, "missing", true); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{
		1:  2 * time.Minute,
		2:  4 * time.Minute,
		10: 1024 * time.Minute,
	}
	for retries, want := range cases {
		if got := backoff(retries); got != want {
			t.Errorf("backoff(%d) = %s, want %s", retries, got, want)
		}
	}
	if got := backoff(63); got != time.Duration(1<<63-1) {
		t.Errorf("backoff(63) = %d, want max duration", got)
	}
}

func TestCreateRejectsBadCron(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateJob(context.Background(), "j", "not cron", 1); err == nil {
		t.Fatal("expected error for bad cron")
	}
}
