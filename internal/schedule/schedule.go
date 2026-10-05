// Package schedule parses the fixed five-field schedule expressions the service
// accepts and computes trigger times in UTC.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression. Only minute and hour may carry concrete
// values; day-of-month, month and day-of-week are always wildcards.
type Schedule struct {
	minute     int // -1 when the field is a wildcard
	hour       int // -1 when the field is a wildcard
	normalized string
}

// Parse validates expr and returns the parsed schedule. The accepted input is
// five whitespace-separated fields: minute and hour are "*" or a decimal
// integer in range, and the remaining three fields must be "*".
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("schedule must have exactly five fields")
	}
	minute, err := parseField(fields[0], 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("minute field: %w", err)
	}
	hour, err := parseField(fields[1], 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("hour field: %w", err)
	}
	for _, field := range fields[2:] {
		if field != "*" {
			return Schedule{}, fmt.Errorf("field %q must be *", field)
		}
	}
	return Schedule{
		minute:     minute,
		hour:       hour,
		normalized: strings.Join(fields, " "),
	}, nil
}

// parseField returns the field value, or -1 for a wildcard. Only "*" or a
// run of ASCII digits within [0, max] is accepted.
func parseField(field string, max int) (int, error) {
	if field == "*" {
		return -1, nil
	}
	for _, r := range field {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not * or an integer", field)
		}
	}
	value, err := strconv.Atoi(field)
	if err != nil || value > max {
		return 0, fmt.Errorf("%q is out of range 0-%d", field, max)
	}
	return value, nil
}

// String returns the normalized expression: the fields joined by single
// spaces.
func (s Schedule) String() string { return s.normalized }

// Next returns the first matching minute strictly after ref, computed in UTC
// with zero seconds.
func (s Schedule) Next(ref time.Time) time.Time {
	candidate := ref.UTC().Truncate(time.Minute).Add(time.Minute)
	// Minute and hour are the only constrained fields, so a match always
	// occurs within 24 hours; the bound only guards against future changes.
	for i := 0; i < 2*24*60; i++ {
		if s.matches(candidate.Hour(), candidate.Minute()) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return candidate
}

func (s Schedule) matches(hour, minute int) bool {
	if s.hour >= 0 && s.hour != hour {
		return false
	}
	if s.minute >= 0 && s.minute != minute {
		return false
	}
	return true
}
