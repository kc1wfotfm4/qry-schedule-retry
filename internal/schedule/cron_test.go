package schedule

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseRejects(t *testing.T) {
	invalid := []string{
		"", "* * * *", "* * * * * *",
		"60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 7",
		"5-3 * * * *", "*/0 * * * *", "*/x * * * *",
		"1,,2 * * * *", "1- * * * *", "a * * * *",
		"*/* * * * *", "*/2/3 * * * *",
	}
	for _, expr := range invalid {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", expr)
		}
	}
}

func TestNextAfter(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
	}{
		{"* * * * *", "2026-10-03T10:30:15Z", "2026-10-03T10:31:00Z"},
		{"0 * * * *", "2026-10-03T10:30:00Z", "2026-10-03T11:00:00Z"},
		{"0 0 1 * *", "2026-01-15T08:00:00Z", "2026-02-01T00:00:00Z"},
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"0 0 1 1 *", "2026-06-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"*/15 * * * *", "2026-10-03T10:30:01Z", "2026-10-03T10:45:00Z"},
		{"0 */6 * * *", "2026-10-03T10:30:00Z", "2026-10-03T12:00:00Z"},
		{"30 9 * * 1-5", "2026-10-03T12:00:00Z", "2026-10-05T09:30:00Z"},
		{"0 0 * * 0", "2026-10-01T00:00:00Z", "2026-10-04T00:00:00Z"},
		{"0 0 13 * 5", "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z"},
		{"0 0 31 * *", "2026-10-01T00:00:00Z", "2026-10-31T00:00:00Z"},
		{"0 0 1 JAN *", "2026-06-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"0 12 * * MON-FRI", "2026-10-03T12:00:00Z", "2026-10-05T12:00:00Z"},
		{"0 0 31 12 *", "2026-01-01T00:00:00Z", "2026-12-31T00:00:00Z"},
	}
	for _, tc := range cases {
		sched, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		got, err := sched.NextAfter(at(tc.from))
		if err != nil {
			t.Fatalf("NextAfter(%q, %s): %v", tc.expr, tc.from, err)
		}
		if got.Format(time.RFC3339) != tc.want {
			t.Errorf("NextAfter(%q, %s) = %s, want %s", tc.expr, tc.from, got.Format(time.RFC3339), tc.want)
		}
	}
}

func TestNextAfterIsStrictlyAfterAtExactMatch(t *testing.T) {
	sched, err := Parse("30 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sched.NextAfter(at("2026-10-05T09:30:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-06T09:30:00Z"; got.Format(time.RFC3339) != want {
		t.Fatalf("got %s, want %s", got.Format(time.RFC3339), want)
	}
}

func TestDayOfMonthDayOfWeekUnion(t *testing.T) {
	// 2026-10-04 is a Sunday and 2026-10-05 is a Monday. With both fields
	// restricted the union (OR) rule means the Sunday comes first.
	sched, err := Parse("0 0 5 * 0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sched.NextAfter(at("2026-10-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Format(time.RFC3339) != "2026-10-04T00:00:00Z" {
		t.Fatalf("got %s", got.Format(time.RFC3339))
	}

	// From after that Sunday, the matching DOM day 5 is the next hit.
	got, err = sched.NextAfter(at("2026-10-04T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Format(time.RFC3339) != "2026-10-05T00:00:00Z" {
		t.Fatalf("got %s", got.Format(time.RFC3339))
	}
}
