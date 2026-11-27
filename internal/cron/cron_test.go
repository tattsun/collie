package cron

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNext(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, tokyo)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		expr, from string
		want       []string
	}{
		// every day at 9:00
		{"0 9 * * *", "2026-11-27 08:59", []string{"2026-11-27 09:00", "2026-11-28 09:00"}},
		{"0 9 * * *", "2026-11-27 09:00", []string{"2026-11-28 09:00"}},
		// Fridays at 18:00 (2026-11-27 is a Friday)
		{"0 18 * * 5", "2026-11-27 10:00", []string{"2026-11-27 18:00", "2026-12-04 18:00"}},
		{"0 18 * * FRI", "2026-11-27 18:30", []string{"2026-12-04 18:00"}},
		// weekdays, a list of minutes, a step
		{"15,45 9 * * 1-5", "2026-11-27 09:20", []string{"2026-11-27 09:45", "2026-11-30 09:15"}},
		{"*/20 * * * *", "2026-11-27 10:05", []string{"2026-11-27 10:20", "2026-11-27 10:40", "2026-11-27 11:00"}},
		{"5/30 * * * *", "2026-11-27 10:00", []string{"2026-11-27 10:05", "2026-11-27 10:35"}},
		{"0 8-18/5 * * *", "2026-11-27 09:00", []string{"2026-11-27 13:00", "2026-11-27 18:00", "2026-11-28 08:00"}},
		// Sunday as 7, and the month rolling over
		{"0 0 * * 7", "2026-11-27 00:00", []string{"2026-11-29 00:00"}},
		{"30 23 31 * *", "2026-11-27 00:00", []string{"2026-12-31 23:30", "2027-01-31 23:30", "2027-03-31 23:30"}},
		// day of month OR day of week when both are restricted
		{"0 12 1 * MON", "2026-11-27 00:00", []string{"2026-11-30 12:00", "2026-12-01 12:00", "2026-12-07 12:00"}},
		// a leap day
		{"0 0 29 2 *", "2026-11-27 00:00", []string{"2028-02-29 00:00", "2032-02-29 00:00"}},
		{"@weekly", "2026-11-27 00:00", []string{"2026-11-29 00:00"}},
		{"@hourly", "2026-11-27 10:59", []string{"2026-11-27 11:00"}},
		{"0 0 1 jan *", "2026-11-27 00:00", []string{"2027-01-01 00:00"}},
	}
	for _, c := range cases {
		e, err := Parse(c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		cur := at(c.from)
		for _, w := range c.want {
			cur = e.Next(cur)
			if got := cur.In(tokyo).Format("2006-01-02 15:04"); got != w {
				t.Errorf("%s after %s: got %s, want %s", c.expr, c.from, got, w)
				break
			}
		}
	}
}

func TestNextNever(t *testing.T) {
	e, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if n := e.Next(time.Now()); !n.IsZero() {
		t.Errorf("February 30th matched %v", n)
	}
}

func TestNextDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-03-08 02:00 EST jumps to 03:00 EDT: 02:30 does not exist that day.
	e, _ := Parse("30 2 * * *")
	got := e.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny))
	if s := got.In(ny).Format("2006-01-02 15:04 MST"); s != "2026-03-09 02:30 EDT" {
		t.Errorf("skipped time: got %s", s)
	}
	// 2026-11-01 02:00 EDT falls back to 01:00 EST: 01:30 happens twice but runs once.
	e, _ = Parse("30 1 * * *")
	first := e.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny))
	if s := first.In(ny).Format("2006-01-02 15:04 MST"); s != "2026-11-01 01:30 EDT" {
		t.Errorf("repeated time, first: got %s", s)
	}
	second := e.Next(first)
	if s := second.In(ny).Format("2006-01-02 15:04 MST"); s != "2026-11-02 01:30 EST" {
		t.Errorf("repeated time, second: got %s", s)
	}
	// Hourly runs keep going through the change (once per real hour).
	e, _ = Parse("0 * * * *")
	cur := time.Date(2026, 11, 1, 0, 30, 0, 0, ny)
	var seen []string
	for range 4 {
		cur = e.Next(cur)
		seen = append(seen, cur.In(ny).Format("15:04 MST"))
	}
	if s := strings.Join(seen, ","); s != "01:00 EDT,02:00 EST,03:00 EST,04:00 EST" {
		t.Errorf("hourly across fall-back: %s", s)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "5-1 * * * *", "*/0 * * * *", "a * * * *", "1,,2 * * * *", "@sometimes",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q parsed", s)
		} else if !strings.HasPrefix(err.Error(), "invalid cron expression") {
			t.Errorf("%q: error %q lacks the prefix", s, err)
		}
	}
}
