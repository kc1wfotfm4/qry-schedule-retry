// Package schedule parses five-field UTC cron expressions and computes the
// next matching instant after a given time.
package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression.
type Schedule struct {
	minutes       [64]bool
	hours         [64]bool
	daysOfMonth   [64]bool // index 1..31
	months        [64]bool // index 1..12
	daysOfWeek    [64]bool // index 0 = Sunday
	domRestricted bool
	dowRestricted bool
}

// Parse accepts a standard five-field cron expression: minute hour day-of-month
// month day-of-week. All times are interpreted in UTC.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, errors.New("cron expression must have five fields")
	}

	var s Schedule
	var err error
	if s.minutes, _, err = parseField(fields[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if s.hours, _, err = parseField(fields[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if s.daysOfMonth, s.domRestricted, err = parseField(fields[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if s.months, _, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	dow, _, derr := parseField(fields[4], 0, 7, weekdayNames)
	if derr != nil {
		return nil, derr
	}
	for value, ok := range dow {
		if !ok {
			continue
		}
		if value == 7 {
			value = 0
		}
		s.daysOfWeek[value] = true
	}
	s.dowRestricted = strings.TrimSpace(fields[4]) != "*"
	return &s, nil
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var weekdayNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

func parseField(field string, min, max int, names map[string]int) ([64]bool, bool, error) {
	var bits [64]bool
	for _, token := range strings.Split(field, ",") {
		step := 1
		base := token
		if i := strings.Index(token, "/"); i >= 0 {
			base = token[:i]
			n, err := strconv.Atoi(token[i+1:])
			if err != nil || n <= 0 {
				return bits, false, errors.New("invalid cron step value")
			}
			step = n
		}

		lo, hi, wildcard, err := resolveRange(base, min, max, names)
		if err != nil {
			return bits, false, err
		}
		if wildcard && strings.Contains(token, "/") {
			hi = max
		}
		if lo < min || hi > max || lo > hi {
			return bits, false, errors.New("cron value out of range")
		}
		for v := lo; v <= hi; v += step {
			bits[v] = true
		}
	}
	return bits, strings.TrimSpace(field) != "*", nil
}

func resolveRange(token string, min, max int, names ...map[string]int) (lo, hi int, wildcard bool, err error) {
	var lookup map[string]int
	if len(names) > 0 {
		lookup = names[0]
	}
	resolve := func(part string) (int, error) {
		if n, convErr := strconv.Atoi(part); convErr == nil {
			return n, nil
		}
		if lookup != nil {
			if n, ok := lookup[strings.ToUpper(part)]; ok {
				return n, nil
			}
		}
		return 0, errors.New("invalid cron value")
	}

	switch {
	case token == "*":
		return min, max, true, nil
	case strings.Contains(token, "-"):
		parts := strings.SplitN(token, "-", 2)
		if parts[0] == "" || parts[1] == "" {
			return 0, 0, false, errors.New("invalid cron range")
		}
		lo, err = resolve(parts[0])
		if err != nil {
			return 0, 0, false, err
		}
		hi, err = resolve(parts[1])
		if err != nil {
			return 0, 0, false, err
		}
		return lo, hi, false, nil
	default:
		v, err := resolve(token)
		if err != nil {
			return 0, 0, false, err
		}
		return v, v, false, nil
	}
}

// NextAfter returns the first matching instant strictly after after. It searches
// up to eight years ahead; an expression that never matches yields an error.
func (s *Schedule) NextAfter(after time.Time) (time.Time, error) {
	after = after.UTC()
	candidate := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, time.UTC).Add(time.Minute)
	limit := after.AddDate(8, 0, 0)
	for !candidate.After(limit) {
		if !s.months[int(candidate.Month())] {
			candidate = time.Date(candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.hours[candidate.Hour()] {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if !s.minutes[candidate.Minute()] {
			candidate = candidate.Add(time.Minute)
			continue
		}
		return candidate, nil
	}
	return time.Time{}, errors.New("cron expression has no future match")
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domMatch := !s.domRestricted || s.daysOfMonth[t.Day()]
	dowMatch := !s.dowRestricted || s.daysOfWeek[int(t.Weekday())]
	switch {
	case !s.domRestricted && !s.dowRestricted:
		return true
	case s.domRestricted && s.dowRestricted:
		return domMatch || dowMatch // Vixie cron OR semantics
	default:
		return domMatch && dowMatch
	}
}
