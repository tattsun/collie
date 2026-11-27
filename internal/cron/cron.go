// Package cron parses five-field cron expressions (minute hour
// day-of-month month day-of-week) and finds the times they match.
//
// The syntax is the usual one: * (or ?), numbers, ranges (1-5), steps
// (*/15, 8-18/2, 5/10), lists (1,15), month and weekday names (JAN, MON),
// 7 for Sunday, and the shorthands @hourly, @daily (@midnight), @weekly,
// @monthly and @yearly (@annually). As in Vixie cron, when both the day of
// the month and the day of the week are restricted, a day matching either
// one matches.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Expr is a parsed expression.
type Expr struct {
	minute, hour, dom, month, dow uint64 // bit i set: value i matches
	domAny, dowAny                bool   // the field started with * or ?
}

type field struct {
	name     string
	min, max int
	names    []string // names[i] stands for min+i
}

var (
	fMinute = field{name: "minute", min: 0, max: 59}
	fHour   = field{name: "hour", min: 0, max: 23}
	fDom    = field{name: "day of month", min: 1, max: 31}
	fMonth  = field{name: "month", min: 1, max: 12,
		names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}}
	// 7 is accepted for Sunday and folded onto 0.
	fDow = field{name: "day of week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse parses an expression. Its errors start with "invalid cron
// expression".
func Parse(s string) (*Expr, error) {
	s = strings.TrimSpace(s)
	if m, ok := macros[strings.ToLower(s)]; ok {
		s = m
	}
	parts := strings.Fields(s)
	if len(parts) != 5 {
		return nil, errors.New("invalid cron expression: it needs 5 fields (minute hour day-of-month month day-of-week)")
	}
	var e Expr
	var err error
	for i, f := range []struct {
		dst *uint64
		def field
	}{{&e.minute, fMinute}, {&e.hour, fHour}, {&e.dom, fDom}, {&e.month, fMonth}, {&e.dow, fDow}} {
		if *f.dst, err = parseField(parts[i], f.def); err != nil {
			return nil, err
		}
	}
	if e.dow&(1<<7) != 0 {
		e.dow = e.dow&^(1<<7) | 1
	}
	e.domAny = strings.HasPrefix(parts[2], "*") || strings.HasPrefix(parts[2], "?")
	e.dowAny = strings.HasPrefix(parts[4], "*") || strings.HasPrefix(parts[4], "?")
	return &e, nil
}

func parseField(s string, f field) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		bad := func(why string) error {
			return fmt.Errorf("invalid cron expression: %s field %q: %s", f.name, part, why)
		}
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, bad("the step must be a positive number")
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*" || rng == "?":
		case rng == "":
			return 0, bad("empty")
		default:
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, bad(err.Error())
			}
			switch {
			case isRange:
				if hi, err = f.value(b); err != nil {
					return 0, bad(err.Error())
				}
				if hi < lo {
					return 0, bad("the range runs backwards")
				}
			case !hasStep:
				hi = lo // a single value; "5/10" runs from 5 to the end
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func (f field) value(s string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("not a number")
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("must be between %d and %d", f.min, f.max)
	}
	return v, nil
}

func has(bits uint64, v int) bool { return bits&(1<<v) != 0 }

func (e *Expr) dayMatches(t time.Time) bool {
	dom, dow := has(e.dom, t.Day()), has(e.dow, int(t.Weekday()))
	switch {
	case e.domAny && e.dowAny:
		return true
	case e.domAny:
		return dow
	case e.dowAny:
		return dom
	}
	return dom || dow
}

// horizon bounds the search: every valid expression matches within it
// (February 29 can be eight years away across a century).
const horizon = 10 * 366 * 24 * time.Hour

// Next returns the first matching minute strictly after t, in t's
// location; the zero time if there is none (e.g. February 30th).
//
// Wall-clock times that a daylight-saving change skips do not match; those
// it repeats match once.
func (e *Expr) Next(t time.Time) time.Time {
	loc := t.Location()
	end := t.Add(horizon)
	t = t.Truncate(time.Minute).Add(time.Minute)
	for t.Before(end) {
		l := t.In(loc)
		y, mo, d := l.Date()
		var nt time.Time
		switch {
		case !has(e.month, int(mo)):
			nt = time.Date(y, mo+1, 1, 0, 0, 0, 0, loc)
		case !e.dayMatches(l):
			nt = time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
		case !has(e.hour, l.Hour()):
			nt = time.Date(y, mo, d, l.Hour()+1, 0, 0, 0, loc)
		case !has(e.minute, l.Minute()) || repeated(l):
			nt = t.Add(time.Minute)
		default:
			return t
		}
		// time.Date can land before t around a daylight-saving change.
		if !nt.After(t) {
			nt = t.Add(time.Minute)
		}
		t = nt
	}
	return time.Time{}
}

// repeated reports whether t's wall-clock time already occurred earlier:
// the hour a daylight-saving change turns the clock back over.
func repeated(t time.Time) bool {
	_, off := t.Zone()
	_, before := t.Add(-3 * time.Hour).Zone()
	if before <= off {
		return false
	}
	e := t.Add(-time.Duration(before-off) * time.Second)
	_, eoff := e.Zone()
	return eoff == before && e.Day() == t.Day() && e.Hour() == t.Hour() && e.Minute() == t.Minute()
}
