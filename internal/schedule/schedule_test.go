package schedule

import (
	"testing"
	"time"
)

func TestParseNormalizesWhitespace(t *testing.T) {
	s, err := Parse("  30\t2  *   *  *  ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := s.String(); got != "30 2 * * *" {
		t.Fatalf("canonical = %q, want %q", got, "30 2 * * *")
	}
}

func TestNextAfterSpecExample(t *testing.T) {
	s, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ref, err := time.Parse(time.RFC3339, "2026-10-05T09:30:00+08:00")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	got := s.NextAfter(ref)
	want := time.Date(2026, time.October, 5, 2, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Second() != 0 || got.Location() != time.UTC {
		t.Fatalf("next = %v, want zero seconds in UTC", got)
	}
}

func TestNextAfterIsStrictlyAfter(t *testing.T) {
	cases := []struct {
		expr string
		ref  string
		want string
	}{
		// Reference exactly at the daily match -> next day.
		{"0 10 * * *", "2026-10-05T10:00:00Z", "2026-10-06T10:00:00Z"},
		// Wildcard minute: next minute inside the matching hour.
		{"* 10 * * *", "2026-10-05T10:20:30Z", "2026-10-05T10:21:00Z"},
		// Wildcard minute past the matching hour: next day.
		{"* 10 * * *", "2026-10-05T10:59:30Z", "2026-10-06T10:00:00Z"},
		// Wildcard hour: first matching minute in the next hour.
		{"15 * * * *", "2026-10-05T10:40:00Z", "2026-10-05T11:15:00Z"},
		// Both wildcards: next minute.
		{"* * * * *", "2026-10-05T10:59:00Z", "2026-10-05T11:00:00Z"},
		// Same minute, later hour.
		{"20 10 * * *", "2026-10-05T09:00:00Z", "2026-10-05T10:20:00Z"},
		// Offset reference converts to UTC first.
		{"0 0 * * *", "2026-10-05T07:30:00+08:00", "2026-10-05T00:00:00Z"},
	}
	for _, tc := range cases {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		ref, err := time.Parse(time.RFC3339, tc.ref)
		if err != nil {
			t.Fatalf("parse ref %q: %v", tc.ref, err)
		}
		if got := s.NextAfter(ref).Format(time.RFC3339); got != tc.want {
			t.Errorf("NextAfter(%q, %q) = %q, want %q", tc.expr, tc.ref, got, tc.want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"05-10 * * * *",
		"*/5 * * * *",
		"0 0 1 * *",
		"0 0 * 1 *",
		"0 0 * * 1",
		"x * * * *",
		" 0  0  *  *  * extra",
	}
	for _, expr := range cases {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", expr)
		}
	}
}
