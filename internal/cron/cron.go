// Package cron reads the standard five-field cron expression (minute, hour,
// day of month, month, day of week) and finds its next fire on the wall
// clock of a time zone (#292). Nothing else: no seconds field, no @macros.
package cron

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // a server image without zoneinfo still knows every zone
)

// Spec is a parsed expression: one bit per allowed value of each field.
type Spec struct {
	minute, hour, dom, month, dow uint64
	// domAny and dowAny are true when the field was "*": with both day
	// fields restricted, a day matches when either does (as cron does).
	domAny, dowAny bool
}

type field struct {
	name     string
	min, max int
	names    []string // names[i] is value min+i
}

var fields = [5]field{
	{"minute", 0, 59, nil},
	{"hour", 0, 23, nil},
	{"day of month", 1, 31, nil},
	{"month", 1, 12, []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	{"day of week", 0, 7, []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// Parse reads a five-field expression. Each field is *, a value, a range
// a-b, a list of those, and an optional step /n (on * or a range; a/n runs
// from a to the end). Months and days of week take three-letter English
// names; day of week 7 is Sunday.
func Parse(expr string) (Spec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Spec{}, errors.New("cron needs five fields: minute hour day-of-month month day-of-week")
	}
	var bits [5]uint64
	for i, p := range parts {
		b, err := parseField(p, fields[i])
		if err != nil {
			return Spec{}, err
		}
		bits[i] = b
	}
	if bits[4]&(1<<7) != 0 {
		bits[4] = bits[4]&^(1<<7) | 1
	}
	return Spec{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domAny: parts[2] == "*", dowAny: parts[4] == "*"}, nil
}

func parseField(s string, f field) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(s, ",") {
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron %s: bad step in %q", f.name, item)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, f); err != nil {
				return 0, err
			}
			if hi, err = value(b, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("cron %s: range %q runs backwards", f.name, rng)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, f field) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("cron %s: %q is not in %d-%d", f.name, s, f.min, f.max)
	}
	return v, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

func (s Spec) dayMatches(t time.Time) bool {
	d, w := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	if !s.domAny && !s.dowAny {
		return d || w
	}
	return d && w
}

// Next is the first fire strictly after the minute of after, on the wall
// clock of loc. A wall time that does not exist (a day-light saving gap) is
// skipped; a repeated hour fires once. Zero when there is none in five years
// (30 February).
func (s Spec) Next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc)
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, loc)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		var next time.Time
		switch {
		case !has(s.month, int(t.Month())):
			next = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			next = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !has(s.hour, t.Hour()):
			next = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !has(s.minute, t.Minute()):
			next = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, loc)
		default:
			return t
		}
		// A wall time in a gap can normalise backwards; always move on.
		if !next.After(t) {
			next = t.Truncate(time.Minute).Add(time.Minute)
		}
		t = next
	}
	return time.Time{}
}

// Zone loads a zone recorded with a schedule: an IANA name, or a fixed
// offset ±hh:mm when the machine had no name for its zone.
func Zone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("time zone required")
	}
	if name[0] == '+' || name[0] == '-' {
		t, err := time.Parse("-07:00", name)
		if err != nil {
			return nil, fmt.Errorf("time zone %q: want an IANA name or ±hh:mm", name)
		}
		_, off := t.Zone()
		return time.FixedZone(name, off), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %v", name, err)
	}
	return loc, nil
}

// LocalZone names this machine's zone for Zone: $TZ, else the target of
// /etc/localtime under a zoneinfo directory, else the current fixed offset
// (Windows; day-light saving changes are then not followed).
func LocalZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := Zone(tz); err == nil {
			return tz
		}
	}
	if n := time.Local.String(); n != "Local" && n != "" {
		if _, err := Zone(n); err == nil {
			return n
		}
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(filepath.ToSlash(target), "zoneinfo/"); ok {
			if _, err := Zone(name); err == nil {
				return name
			}
		}
	}
	return time.Now().Format("-07:00")
}
