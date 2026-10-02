package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func fixedNext(fixed time.Time) NextFunc {
	return func(time.Time) (time.Time, error) { return fixed, nil }
}

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st := openTestStore(t)
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestCreateJobInitialState(t *testing.T) {
	st := openTestStore(t)
	created, err := st.CreateJob("backup", "0 * * * *", 3, fixedNext(mustParse("2026-10-03T11:00:00Z")))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Paused || created.FailureRetries != 0 {
		t.Fatalf("unexpected initial state: %+v", created)
	}
	if got := created.NextRunAt.Format(time.RFC3339); got != "2026-10-03T11:00:00Z" {
		t.Fatalf("next run = %s", got)
	}

	if _, err := st.CreateJob("backup", "0 * * * *", 3, fixedNext(time.Now())); !errors.Is(err, ErrJobExists) {
		t.Fatalf("duplicate error = %v, want ErrJobExists", err)
	}
}

func TestGetJobMissing(t *testing.T) {
	st := openTestStore(t)
	if _, _, err := st.GetJob("ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestRecordSuccessResetsRetriesAndAdvances(t *testing.T) {
	st := openTestStore(t)
	base := mustParse("2026-10-03T10:00:00Z")
	if _, err := st.CreateJob("job", "0 * * * *", 3, fixedNext(base)); err != nil {
		t.Fatal(err)
	}

	for seq := 1; seq <= 2; seq++ {
		if _, err := st.RecordAttempt("job", Attempt{Sequence: seq, Outcome: "failure", AttemptedAt: base},
			func(string) (NextFunc, error) { return fixedNext(base.Add(time.Hour)), nil }); err != nil {
			t.Fatal(err)
		}
	}
	job, attempts, err := st.GetJob("job")
	if err != nil {
		t.Fatal(err)
	}
	if job.FailureRetries != 2 {
		t.Fatalf("retries = %d, want 2", job.FailureRetries)
	}
	wantBackoff := base.Add(4 * time.Minute)
	if !job.NextRunAt.Equal(wantBackoff) {
		t.Fatalf("next = %s, want %s", job.NextRunAt, wantBackoff)
	}

	successAt := mustParse("2026-10-03T12:00:00Z")
	if _, err := st.RecordAttempt("job", Attempt{Sequence: 3, Outcome: "success", AttemptedAt: successAt},
		func(string) (NextFunc, error) { return fixedNext(mustParse("2026-10-03T13:00:00Z")), nil }); err != nil {
		t.Fatal(err)
	}
	job, attempts, err = st.GetJob("job")
	if err != nil {
		t.Fatal(err)
	}
	if job.FailureRetries != 0 {
		t.Fatalf("retries = %d, want 0 after success", job.FailureRetries)
	}
	if got := job.NextRunAt.Format(time.RFC3339); got != "2026-10-03T13:00:00Z" {
		t.Fatalf("next after success = %s", got)
	}
	if len(attempts) != 3 || attempts[0].Sequence != 1 || attempts[2].Outcome != "success" {
		t.Fatalf("attempts = %+v", attempts)
	}
}

func TestFailureExhaustionPausesWithoutAdvancing(t *testing.T) {
	st := openTestStore(t)
	original := mustParse("2026-10-03T11:00:00Z")
	if _, err := st.CreateJob("job", "0 * * * *", 1, fixedNext(original)); err != nil {
		t.Fatal(err)
	}
	at1 := mustParse("2026-10-03T10:00:00Z")
	if _, err := st.RecordAttempt("job", Attempt{Sequence: 1, Outcome: "failure", AttemptedAt: at1},
		func(string) (NextFunc, error) { return fixedNext(original), nil }); err != nil {
		t.Fatal(err)
	}
	at2 := mustParse("2026-10-03T10:30:00Z")
	updated, err := st.RecordAttempt("job", Attempt{Sequence: 2, Outcome: "failure", AttemptedAt: at2},
		func(string) (NextFunc, error) { return fixedNext(original), nil })
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Paused {
		t.Fatal("job should be paused after exhaustion")
	}
	if updated.FailureRetries != 1 {
		t.Fatalf("retries = %d, want 1", updated.FailureRetries)
	}
	if !updated.NextRunAt.Equal(at1.Add(2 * time.Minute)) {
		t.Fatalf("next = %s, want backoff from first failure, unchanged by exhaustion", updated.NextRunAt)
	}

	if _, err := st.RecordAttempt("job", Attempt{Sequence: 3, Outcome: "success", AttemptedAt: at2},
		func(string) (NextFunc, error) { return fixedNext(original), nil }); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("paused attempt error = %v, want ErrJobPaused", err)
	}
	_, attempts, err := st.GetJob("job")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts while paused must not be recorded, got %d", len(attempts))
	}
}

func TestDuplicateSequenceConflicts(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateJob("job", "0 * * * *", 3, fixedNext(mustParse("2026-10-03T11:00:00Z"))); err != nil {
		t.Fatal(err)
	}
	at := mustParse("2026-10-03T10:00:00Z")
	prepare := func(string) (NextFunc, error) { return fixedNext(mustParse("2026-10-03T11:00:00Z")), nil }
	if _, err := st.RecordAttempt("job", Attempt{Sequence: 7, Outcome: "success", AttemptedAt: at}, prepare); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordAttempt("job", Attempt{Sequence: 7, Outcome: "success", AttemptedAt: at}, prepare); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("err = %v, want ErrSequenceConflict", err)
	}
}

func TestConcurrentDuplicateSequenceOnlyOneCommits(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateJob("job", "0 * * * *", 3, fixedNext(mustParse("2026-10-03T11:00:00Z"))); err != nil {
		t.Fatal(err)
	}
	at := mustParse("2026-10-03T10:00:00Z")
	prepare := func(string) (NextFunc, error) { return fixedNext(mustParse("2026-10-03T11:00:00Z")), nil }

	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, results[i] = st.RecordAttempt("job", Attempt{Sequence: 1, Outcome: "failure", AttemptedAt: at}, prepare)
		}(i)
	}
	wg.Wait()

	var ok, conflict int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrSequenceConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("ok = %d, conflict = %d, want 1 and %d", ok, conflict, n-1)
	}
	_, attempts, err := st.GetJob("job")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("stored attempts = %d, want 1", len(attempts))
	}
}

func TestSetPausedLeavesRetriesAndAttempts(t *testing.T) {
	st := openTestStore(t)
	original := mustParse("2026-10-03T11:00:00Z")
	if _, err := st.CreateJob("job", "0 * * * *", 3, fixedNext(original)); err != nil {
		t.Fatal(err)
	}
	at := mustParse("2026-10-03T10:00:00Z")
	if _, err := st.RecordAttempt("job", Attempt{Sequence: 1, Outcome: "failure", AttemptedAt: at},
		func(string) (NextFunc, error) { return fixedNext(original), nil }); err != nil {
		t.Fatal(err)
	}
	paused, err := st.SetPaused("job", true)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || paused.FailureRetries != 1 || !paused.NextRunAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("pause changed unrelated fields: %+v", paused)
	}
	resumed, err := st.SetPaused("job", false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Paused || resumed.FailureRetries != 1 {
		t.Fatalf("resume changed counters: %+v", resumed)
	}
	if _, err := st.SetPaused("ghost", true); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestRetryBackoffPowersOfTwo(t *testing.T) {
	cases := map[int]time.Duration{
		1: 2 * time.Minute,
		2: 4 * time.Minute,
		3: 8 * time.Minute,
		5: 32 * time.Minute,
	}
	for retries, want := range cases {
		if got := retryBackoff(retries); got != want {
			t.Errorf("retryBackoff(%d) = %s, want %s", retries, got, want)
		}
	}
}

func TestRecordAttemptMissingJob(t *testing.T) {
	st := openTestStore(t)
	_, err := st.RecordAttempt("ghost", Attempt{Sequence: 1, Outcome: "success", AttemptedAt: time.Now()},
		func(string) (NextFunc, error) { return fixedNext(time.Now()), nil })
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}
