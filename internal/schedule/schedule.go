// Package schedule parses and evaluates recurring time windows, such as a
// router that reboots itself every night at 03:00.
package schedule

import (
	"fmt"
	"strings"
	"time"
)

// MaxLength is the longest window accepted.
const MaxLength = 6 * time.Hour

// DefaultLength is used when an entry gives no length.
const DefaultLength = 10 * time.Minute

// Window is one recurring window: at Hour:Minute local time on the enabled
// days, lasting Length.
type Window struct {
	Days   [7]bool // indexed by time.Weekday
	Hour   int
	Minute int
	Length time.Duration
}

// Schedule is a set of windows. The zero value never matches.
type Schedule []Window

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Parse reads entries separated by ";". Each entry is
//
//	[days] HH:MM[/length]
//
// where days is omitted (every day), "daily", "weekdays", "weekends", or a
// comma-separated list of day names and ranges such as "mon-fri" or
// "sun,wed". The length is a Go duration and defaults to 10m. An empty
// string or "off" gives an empty schedule.
func Parse(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "off") {
		return nil, nil
	}
	var out Schedule
	for _, entry := range strings.Split(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		w, err := parseWindow(entry)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", entry, err)
		}
		out = append(out, w)
	}
	return out, nil
}

func parseWindow(entry string) (Window, error) {
	fields := strings.Fields(strings.ToLower(entry))
	var days, at string
	switch len(fields) {
	case 1:
		at = fields[0]
	case 2:
		days, at = fields[0], fields[1]
	default:
		return Window{}, fmt.Errorf("want [days] HH:MM[/length]")
	}

	w := Window{Length: DefaultLength}
	if clock, length, ok := strings.Cut(at, "/"); ok {
		d, err := time.ParseDuration(length)
		if err != nil || d < time.Minute || d > MaxLength {
			return Window{}, fmt.Errorf("length %q: want a duration from 1m to %s", length, MaxLength)
		}
		w.Length, at = d, clock
	}
	clock, err := time.Parse("15:04", at)
	if err != nil {
		return Window{}, fmt.Errorf("time %q: want HH:MM (24-hour)", at)
	}
	w.Hour, w.Minute = clock.Hour(), clock.Minute()
	if err := parseDays(days, &w.Days); err != nil {
		return Window{}, err
	}
	return w, nil
}

func parseDays(s string, days *[7]bool) error {
	switch s {
	case "", "daily", "everyday":
		*days = [7]bool{true, true, true, true, true, true, true}
		return nil
	case "weekdays":
		s = "mon-fri"
	case "weekends":
		s = "sat,sun"
	}
	for _, part := range strings.Split(s, ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, ok1 := dayNames[from]
		b, ok2 := dayNames[to]
		if !isRange {
			b, ok2 = a, ok1
		}
		if !ok1 || !ok2 {
			return fmt.Errorf("days %q: want names like mon, mon-fri, sat,sun, weekdays or weekends", s)
		}
		for d := a; ; d = (d + 1) % 7 {
			days[d] = true
			if d == b {
				break
			}
		}
	}
	return nil
}

// Active returns the window containing t (start inclusive, end exclusive).
// When windows overlap, the one ending last is returned.
func (s Schedule) Active(t time.Time) (start, end time.Time, ok bool) {
	for _, w := range s {
		// A window can start on the previous day and run past midnight.
		for off := -1; off <= 0; off++ {
			ws := w.startOn(t, off)
			we := ws.Add(w.Length)
			if !w.Days[ws.Weekday()] || t.Before(ws) || !t.Before(we) {
				continue
			}
			if !ok || we.After(end) {
				start, end, ok = ws, we, true
			}
		}
	}
	return start, end, ok
}

// Next returns the first window starting after t, looking up to a week ahead.
func (s Schedule) Next(t time.Time) (start, end time.Time, ok bool) {
	for _, w := range s {
		for off := 0; off <= 7; off++ {
			ws := w.startOn(t, off)
			if !w.Days[ws.Weekday()] || !ws.After(t) {
				continue
			}
			if !ok || ws.Before(start) {
				start, end, ok = ws, ws.Add(w.Length), true
			}
			break
		}
	}
	return start, end, ok
}

func (w Window) startOn(t time.Time, dayOffset int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+dayOffset, w.Hour, w.Minute, 0, 0, t.Location())
}

// String describes the window, e.g. "daily at 03:00 for 10m".
func (w Window) String() string {
	return fmt.Sprintf("%s at %02d:%02d for %s", w.daysString(), w.Hour, w.Minute, fmtLength(w.Length))
}

func (w Window) daysString() string {
	all, weekdays, weekends := true, true, true
	for d, on := range w.Days {
		all = all && on
		isWeekend := d == int(time.Saturday) || d == int(time.Sunday)
		if isWeekend {
			weekends = weekends && on
			weekdays = weekdays && !on
		} else {
			weekdays = weekdays && on
			weekends = weekends && !on
		}
	}
	switch {
	case all:
		return "daily"
	case weekdays:
		return "weekdays"
	case weekends:
		return "weekends"
	}
	var names []string
	for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday} {
		if w.Days[d] {
			names = append(names, d.String()[:3])
		}
	}
	return strings.Join(names, ", ")
}

func fmtLength(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
		if strings.HasSuffix(s, "h0m") {
			s = strings.TrimSuffix(s, "0m")
		}
	}
	return s
}

// Strings describes every window.
func (s Schedule) Strings() []string {
	out := make([]string, 0, len(s))
	for _, w := range s {
		out = append(out, w.String())
	}
	return out
}
