// Package dates holds the follow-up ladder's calendar arithmetic.
package dates

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const FollowupBusinessDays = 5

// AddBusinessDays walks forward n weekdays from start, skipping Sat/Sun.
func AddBusinessDays(start time.Time, n int) time.Time {
	d := start
	for n > 0 {
		d = d.AddDate(0, 0, 1)
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n--
		}
	}
	return d
}

// ParseSince turns a lookback like "7d", "48h" or an absolute "2026-08-01"
// into a cutoff instant. Relative forms are what anyone types at a prompt;
// the absolute form is what you reach for when chasing one specific day.
func ParseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if unit := value[len(value)-1]; unit == 'd' || unit == 'h' || unit == 'w' {
		n, err := strconv.Atoi(value[:len(value)-1])
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("bad duration %q: want a number then d, h or w", value)
		}
		switch unit {
		case 'h':
			return now.Add(-time.Duration(n) * time.Hour), nil
		case 'd':
			return now.AddDate(0, 0, -n), nil
		default:
			return now.AddDate(0, 0, -7*n), nil
		}
	}
	if d := ParseDay(value); !d.IsZero() {
		return d, nil
	}
	return time.Time{}, fmt.Errorf("bad --since %q: want 7d, 48h, 2w or 2026-08-01", value)
}

// dayLayouts mirrors what datetime.fromisoformat accepts for the values this
// app actually stores: date-only, and the two timestamp shapes db.Now writes.
var dayLayouts = []string{
	"2006-01-02",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	time.RFC3339,
}

// ParseDay returns the date part of an ISO value, or zero time when the value
// is empty or unparseable. Callers check IsZero rather than handling an error:
// a malformed stored date is not worth failing a whole run over.
func ParseDay(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range dayLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		}
	}
	return time.Time{}
}
