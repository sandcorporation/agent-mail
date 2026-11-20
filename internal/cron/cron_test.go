package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Spec {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseRefusesWhatIsNotFiveStandardFields(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "@daily", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *", "? * * * *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted", expr)
		}
	}
	for _, expr := range []string{"* * * * *", "*/15 9-17 * * 1-5", "0 0 1,15 * *", "30 6 * JAN-mar sun,7", "5/10 * * * *", "0 12 * * 0-6/2"} {
		mustParse(t, expr)
	}
}

func TestNext(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, seoul)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ expr, after, want string }{
		{"* * * * *", "2026-11-20 09:00", "2026-11-20 09:01"},
		{"*/15 * * * *", "2026-11-20 09:07", "2026-11-20 09:15"},
		{"0 9 * * *", "2026-11-20 09:00", "2026-11-21 09:00"},
		{"0 9 * * 1-5", "2026-11-20 10:00", "2026-11-23 09:00"}, // Friday → Monday
		{"0 0 1 * *", "2026-12-15 00:00", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		// Day of month and day of week both restricted: either matches.
		{"0 8 13 * 5", "2026-11-01 00:00", "2026-11-06 08:00"},
		{"30 6 * jan sun", "2026-11-20 00:00", "2027-01-03 06:30"},
		{"0 12 * * 7", "2026-11-20 00:00", "2026-11-22 12:00"},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).Next(at(c.after), seoul)
		if !got.Equal(at(c.want)) {
			t.Errorf("%q after %s = %s, want %s", c.expr, c.after, got.In(seoul).Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
	// Seconds are dropped: a fire is strictly after the given time.
	if got := mustParse(t, "* * * * *").Next(at("2026-11-20 09:00").Add(30*time.Second), seoul); !got.Equal(at("2026-11-20 09:01")) {
		t.Errorf("mid-minute: %s", got)
	}
	if got := mustParse(t, "0 0 30 2 *").Next(at("2026-11-20 09:00"), seoul); !got.IsZero() {
		t.Errorf("30 Feb fired at %s", got)
	}
}

// The cron runs on the wall clock of its zone: a day-light saving gap is
// skipped and the repeated hour fires once.
func TestNextAcrossDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-08 02:00 does not exist in New York; 2026-11-01 01:00 happens twice.
	s := mustParse(t, "30 2 * * *")
	got := s.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), ny)
	if want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny); !got.Equal(want) {
		t.Errorf("gap: %s want %s", got, want)
	}
	every := mustParse(t, "*/30 * * * *")
	start := time.Date(2026, 11, 1, 0, 50, 0, 0, ny)
	var fires []time.Time
	for at := start; len(fires) < 4; {
		at = every.Next(at, ny)
		fires = append(fires, at)
	}
	for i := 1; i < len(fires); i++ {
		if !fires[i].After(fires[i-1]) {
			t.Fatalf("not increasing: %v", fires)
		}
	}
	if d := fires[3].Sub(fires[0]); d != 90*time.Minute+time.Hour {
		// 01:00, 01:30 (first), [01:00, 01:30 second skipped], 02:00, 02:30
		t.Errorf("fall back fires %v (span %s)", fires, d)
	}
}

func TestZone(t *testing.T) {
	for name, offset := range map[string]int{"Asia/Seoul": 9 * 3600, "UTC": 0, "+09:00": 9 * 3600, "-03:30": -(3*3600 + 1800)} {
		loc, err := Zone(name)
		if err != nil {
			t.Fatalf("Zone(%q): %v", name, err)
		}
		if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone(); off != offset {
			t.Errorf("Zone(%q) offset %d", name, off)
		}
	}
	for _, name := range []string{"", "Local", "Mars/Base", "+25:00", "9"} {
		if _, err := Zone(name); err == nil {
			t.Errorf("Zone(%q) accepted", name)
		}
	}
	if _, err := Zone(LocalZone()); err != nil {
		t.Errorf("LocalZone %q: %v", LocalZone(), err)
	}
}
