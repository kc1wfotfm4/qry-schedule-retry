// Package schedule parses standard five-field cron expressions in UTC and computes
// the first instant strictly after a given time that matches the expression.
package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression (minute, hour, day of month,
// month, day of week).
type Schedule struct {
	minutes uint64
	hours   uint32
	dom     uint32
	months  uint16
	dow     uint8

	domRestricted bool
	dowRestricted bool
}

// ErrInvalid reports that an expression is not a valid five-field cron expression.
var ErrInvalid = errors.New("invalid cron expression")

var (
	monthNames = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	dowNames = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
)

// Parse validates and parses a five-field cron expression. Fields are split on
// whitespace and must be exactly five.
func Parse(expr string) (*Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, ErrInvalid
	}

	minutes, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, err
	}
	hours, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, err
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, err
	}
	months, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, err
	}
	dow, err := parseField(fields[4], 0, 6, dowNames)
	if err != nil {
		return nil, err
	}

	s := &Schedule{
		minutes:       minutes,
		hours:         uint32(hours),
		dom:           uint32(dom),
		months:        uint16(months),
		dow:           uint8(dow),
		domRestricted: fields[2] != "*",
		dowRestricted: fields[4] != "*",
	}
	return s, nil
}

// NextAfter returns the earliest instant strictly after t (interpreted in UTC)
// that matches the schedule. Cron is defined over years 1970-2099; an expression
// with no match before year 2100 returns ErrInvalid.
func (s *Schedule) NextAfter(t time.Time) (time.Time, error) {
	t = t.UTC()
	minute := t.Minute()
	hour := t.Hour()
	day := t.Day()
	month := int(t.Month())
	year := t.Year()

	if nextMinute := firstSet(s.minutes, minute+1, 59); nextMinute >= 0 {
		if cand, ok := s.checkedDay(year, month, day, hour, nextMinute); ok {
			return cand, nil
		}
	}
	if nextHour := firstSet(uint64(s.hours), hour+1, 23); nextHour >= 0 {
		nextMinute := firstSet(s.minutes, 0, 59)
		if nextMinute >= 0 {
			if cand, ok := s.checkedDay(year, month, day, nextHour, nextMinute); ok {
				return cand, nil
			}
		}
	}

	// Same-day options are exhausted; start the scan at the next calendar day.
	first := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	y, m, d := first.Date()
	year, month, day = y, int(m), d

	const maxYear = 2099
	for year <= maxYear {
		mi := month
		if s.months&(1<<uint(mi)) == 0 {
			mi = firstSet(uint64(s.months), month, 12)
			day = 1
		}
		if mi < 0 {
			year++
			month = 1
			day = 1
			continue
		}

		di := day
		if !s.dayMatches(year, mi, di) {
			di = s.nextMatchingDay(year, mi, di)
		}
		if di < 1 || di > daysIn(year, mi) {
			month = mi + 1
			day = 1
			continue
		}

		nextHour := firstSet(uint64(s.hours), 0, 23)
		if nextHour < 0 {
			return time.Time{}, ErrInvalid
		}
		nextMinute := firstSet(s.minutes, 0, 59)
		if nextMinute < 0 {
			return time.Time{}, ErrInvalid
		}
		return time.Date(year, time.Month(mi), di, nextHour, nextMinute, 0, 0, time.UTC), nil
	}
	return time.Time{}, ErrInvalid
}

func (s *Schedule) checkedDay(year, month, day, hour, minute int) (time.Time, bool) {
	if !s.dayMatches(year, month, day) {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC), true
}

// dayMatches implements the cron union rule: if either the day-of-month or the
// day-of-week field is unrestricted ("*"), matching the other is enough; when
// both are restricted, either one matching is sufficient.
func (s *Schedule) dayMatches(year, month, day int) bool {
	dow := int(time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Weekday())
	domMatch := s.dom&(1<<uint(day)) != 0
	dowMatch := s.dow&(1<<uint(dow)) != 0
	switch {
	case !s.domRestricted && !s.dowRestricted:
		return true
	case !s.domRestricted:
		return dowMatch
	case !s.dowRestricted:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

func (s *Schedule) nextMatchingDay(year, month, from int) int {
	for day := from; day <= daysIn(year, month); day++ {
		if s.dayMatches(year, month, day) {
			return day
		}
	}
	return -1
}

func firstSet(bits uint64, from, to int) int {
	if from > to || from > 63 {
		return -1
	}
	if from < 0 {
		from = 0
	}
	mask := ^uint64(0) << uint(from)
	if to < 63 {
		mask &= ^uint64(0) >> uint(63-to)
	}
	found := bits & mask
	if found == 0 {
		return -1
	}
	return trailingZeros(found)
}

func trailingZeros(v uint64) int {
	n := 0
	for v&1 == 0 {
		v >>= 1
		n++
	}
	return n
}

func daysIn(year, month int) int {
	if month == 2 {
		if isLeap(year) {
			return 29
		}
		return 28
	}
	if month == 4 || month == 6 || month == 9 || month == 11 {
		return 30
	}
	return 31
}

func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// parseField parses one cron field into a bitmask where valid value v maps to
// bit v. A field is a comma-separated list of terms; each term is "*", a number
// (or month/weekday name), a "lo-hi" range, or any of those followed by "/step".
func parseField(field string, min, max int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, term := range strings.Split(field, ",") {
		if term == "" {
			return 0, ErrInvalid
		}
		rangePart := term
		step := 1
		if slash := strings.IndexByte(term, '/'); slash >= 0 {
			rangePart = term[:slash]
			stepPart := term[slash+1:]
			if stepPart == "" || strings.ContainsAny(stepPart, "/-") {
				return 0, ErrInvalid
			}
			n, err := strconv.Atoi(stepPart)
			if err != nil || n <= 0 {
				return 0, ErrInvalid
			}
			step = n
		}

		lo, hi := min, max
		switch {
		case rangePart == "*":
		case strings.ContainsRune(rangePart, '-'):
			ends := strings.Split(rangePart, "-")
			if len(ends) != 2 || ends[0] == "" || ends[1] == "" {
				return 0, ErrInvalid
			}
			a, err := parseValue(ends[0], min, max, names)
			if err != nil {
				return 0, err
			}
			b, err := parseValue(ends[1], min, max, names)
			if err != nil {
				return 0, err
			}
			if a > b {
				return 0, ErrInvalid
			}
			lo, hi = a, b
		default:
			v, err := parseValue(rangePart, min, max, names)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
		}

		if lo < min || hi > max {
			return 0, ErrInvalid
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	if bits == 0 {
		return 0, ErrInvalid
	}
	return bits, nil
}

func parseValue(token string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToLower(token)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(token)
	if err != nil || v < min || v > max {
		return 0, ErrInvalid
	}
	return v, nil
}
