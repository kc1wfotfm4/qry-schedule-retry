package schedule

import (
	"testing"
	"time"
)

func TestParseRejectsBadExpressions(t *testing.T) {
	bad := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"*/x * * * *",
		"5-2 * * * *",
		"a * * * *",
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", expr)
		}
	}
}

func TestNextAfterBasic(t *testing.T) {
	cron := mustParse(t, "5 0 * * *")
	after := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
	got := cron.NextAfterOrFail(t, after)
	want := time.Date(2026, 1, 2, 0, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextAfterIsStrictlyAfter(t *testing.T) {
	cron := mustParse(t, "30 10 1 1 *")
	at := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
	got := cron.NextAfterOrFail(t, at)
	want := time.Date(2027, 1, 1, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextAfterStepsAndLists(t *testing.T) {
	cron := mustParse(t, "*/15 9-17 * * MON-FRI")
	after := time.Date(2026, 10, 2, 10, 7, 0, 0, time.UTC) // Friday
	got := cron.NextAfterOrFail(t, after)
	want := time.Date(2026, 10, 2, 10, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextAfterDomOrDowVixieSemantics(t *testing.T) {
	// Restricted dom and dow: match if either matches.
	cron := mustParse(t, "0 12 13 * FRI")
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got := cron.NextAfterOrFail(t, after)
	// Oct 2 2026 is a Friday and comes before Oct 13.
	want := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
}

func TestNextAfterMonthNames(t *testing.T) {
	cron := mustParse(t, "0 0 29 FEB *")
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := cron.NextAfterOrFail(t, after)
	if got.Year() != 2028 || got.Month() != time.February || got.Day() != 29 {
		t.Fatalf("next leap day = %s", got)
	}
}

func TestImpossibleScheduleHasNoMatch(t *testing.T) {
	cron := mustParse(t, "0 0 30 2 *")
	if _, err := cron.NextAfter(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("expected no match for Feb 30")
	}
}

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	cron, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return cron
}

func (s *Schedule) NextAfterOrFail(t *testing.T, after time.Time) time.Time {
	t.Helper()
	next, err := s.NextAfter(after)
	if err != nil {
		t.Fatalf("NextAfter: %v", err)
	}
	return next
}
