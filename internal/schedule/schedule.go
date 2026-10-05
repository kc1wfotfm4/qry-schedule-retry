// Package schedule parses the five-field cron subset accepted at job
// registration and computes the first matching minute after a reference time.
package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned when an expression falls outside the supported subset.
var ErrInvalid = errors.New("invalid schedule expression")

// Schedule is a parsed five-field expression. The day-of-month, month and
// day-of-week fields are always "*", so only the minute and hour fields
// constrain matches.
type Schedule struct {
	fields    [5]string
	minute    int
	minuteAny bool
	hour      int
	hourAny   bool
}

// Parse accepts five whitespace-separated fields. Leading and trailing
// whitespace is ignored and any run of whitespace separates fields. The minute
// and hour fields must be "*" or ASCII-digit integers in the ranges 0-59 and
// 0-23 respectively; the remaining three fields must be "*".
func Parse(expr string) (*Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, ErrInvalid
	}
	minute, minuteAny, err := parseNumberOrStar(parts[0], 0, 59)
	if err != nil {
		return nil, err
	}
	hour, hourAny, err := parseNumberOrStar(parts[1], 0, 23)
	if err != nil {
		return nil, err
	}
	for i := 2; i < 5; i++ {
		if parts[i] != "*" {
			return nil, ErrInvalid
		}
	}
	var fields [5]string
	copy(fields[:], parts)
	return &Schedule{
		fields:    fields,
		minute:    minute,
		minuteAny: minuteAny,
		hour:      hour,
		hourAny:   hourAny,
	}, nil
}

func parseNumberOrStar(value string, min, max int) (int, bool, error) {
	if value == "*" {
		return 0, true, nil
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false, ErrInvalid
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < min || number > max {
		return 0, false, ErrInvalid
	}
	return number, false, nil
}

// String returns the stored canonical form: the accepted fields joined with
// single spaces.
func (s *Schedule) String() string { return strings.Join(s.fields[:], " ") }

// NextAfter returns, in UTC, the first matching minute strictly after ref,
// with zero seconds.
func (s *Schedule) NextAfter(ref time.Time) time.Time {
	t := ref.UTC()
	candidate := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC).
		Add(time.Minute)
	for !s.matches(candidate) {
		candidate = candidate.Add(time.Minute)
	}
	return candidate
}

func (s *Schedule) matches(t time.Time) bool {
	if !s.hourAny && t.Hour() != s.hour {
		return false
	}
	if !s.minuteAny && t.Minute() != s.minute {
		return false
	}
	return true
}
