package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kc1wfotfm4/qry-schedule-retry/internal/schedule"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func mustSchedule(t *testing.T, expr string) *schedule.Schedule {
	t.Helper()
	s, err := schedule.Parse(expr)
	if err != nil {
		t.Fatalf("parse schedule: %v", err)
	}
	return s
}

func TestRegisterAndGetJob(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	baseTime := time.Date(2026, time.October, 5, 9, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	job, err := st.RegisterJob(RegisterJobInput{
		ID:         "job-1",
		Schedule:   mustSchedule(t, "  30\t2 *  * *"),
		MaxRetries: 3,
		BaseTime:   &baseTime,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if job.Schedule != "30 2 * * *" {
		t.Fatalf("schedule = %q, want canonical form", job.Schedule)
	}
	want := time.Date(2026, time.October, 5, 2, 30, 0, 0, time.UTC)
	if !job.NextTriggerAt.Equal(want) || job.Paused {
		t.Fatalf("job = %+v, want next %s, paused false", job, want.Format(time.RFC3339))
	}

	got, err := st.GetJob("job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != job.ID || got.Schedule != job.Schedule || got.MaxRetries != job.MaxRetries ||
		!got.NextTriggerAt.Equal(job.NextTriggerAt) || got.Paused != job.Paused {
		t.Fatalf("stored job = %+v, want %+v", got, job)
	}
}

func TestRegisterRejectsDuplicateWithoutChangingRecord(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	baseTime := time.Date(2026, time.October, 5, 9, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	first, err := st.RegisterJob(RegisterJobInput{
		ID:         "dup",
		Schedule:   mustSchedule(t, "30 2 * * *"),
		MaxRetries: 1,
		BaseTime:   &baseTime,
	})
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	later := baseTime.Add(24 * time.Hour)
	if _, err := st.RegisterJob(RegisterJobInput{
		ID:         "dup",
		Schedule:   mustSchedule(t, "0 0 * * *"),
		MaxRetries: 99,
		BaseTime:   &later,
	}); !errors.Is(err, ErrJobExists) {
		t.Fatalf("register duplicate err = %v, want ErrJobExists", err)
	}
	got, err := st.GetJob("dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Schedule != first.Schedule || got.MaxRetries != first.MaxRetries ||
		!got.NextTriggerAt.Equal(first.NextTriggerAt) {
		t.Fatalf("record changed after duplicate: %+v, want %+v", got, first)
	}
}

func TestGetMissingJob(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.GetJob("ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestJobsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	baseTime := time.Date(2026, time.October, 5, 9, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.RegisterJob(RegisterJobInput{
		ID:         "persist",
		Schedule:   mustSchedule(t, "30 2 * * *"),
		MaxRetries: 0,
		BaseTime:   &baseTime,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetJob("persist")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got, want := got.NextTriggerAt.Format(time.RFC3339), "2026-10-05T02:30:00Z"; got != want {
		t.Fatalf("next_trigger_at = %s, want %s", got, want)
	}
}
