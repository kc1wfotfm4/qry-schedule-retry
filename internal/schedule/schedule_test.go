package schedule

import (
	"testing"
	"time"
)

func TestParseAcceptsWildcardsAndIntegers(t *testing.T) {
	cases := []struct {
		expr       string
		normalized string
	}{
		{"* * * * *", "* * * * *"},
		{"30 2 * * *", "30 2 * * *"},
		{"0 0 * * *", "0 0 * * *"},
		{"59 23 * * *", "59 23 * * *"},
		{"  30   2\t* * *  ", "30 2 * * *"},
		{"05 03 * * *", "05 03 * * *"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if got.String() != tc.normalized {
			t.Fatalf("Parse(%q).String() = %q, want %q", tc.expr, got.String(), tc.normalized)
		}
	}
}

func TestParseRejectsBadExpressions(t *testing.T) {
	cases := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"-1 * * * *",
		"+1 * * * *",
		"1.5 * * * *",
		"*/5 * * * *",
		"1,2 * * * *",
		"1-5 * * * *",
		"a * * * *",
		"* * 1 * *",
		"* * * 1 *",
		"* * * * 1",
		"３０ * * * *", // full-width digits are not ASCII
	}
	for _, expr := range cases {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestNextMatchesPublishedExample(t *testing.T) {
	parsed, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref, err := time.Parse(time.RFC3339, "2026-10-05T09:30:00+08:00")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	got := parsed.Next(ref)
	want := time.Date(2026, 10, 5, 2, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Second() != 0 || got.Location() != time.UTC {
		t.Fatalf("Next = %s, want UTC with zero seconds", got.Format(time.RFC3339))
	}
}

func TestNextIsStrictlyAfterReference(t *testing.T) {
	parsed, err := Parse("* * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref := time.Date(2026, 10, 5, 2, 30, 0, 0, time.UTC)
	got := parsed.Next(ref)
	want := time.Date(2026, 10, 5, 2, 31, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	ref = time.Date(2026, 10, 5, 2, 30, 45, 0, time.UTC)
	got = parsed.Next(ref)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNextRollsAcrossDays(t *testing.T) {
	parsed, err := Parse("0 0 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	got := parsed.Next(ref)
	want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
