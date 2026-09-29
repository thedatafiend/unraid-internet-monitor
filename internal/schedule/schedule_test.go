package schedule

import (
	"slices"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParse(t *testing.T) {
	s, err := Parse("03:00; mon-fri 23:55/15m ; sat,sun 4:30/1h30m; weekends 12:00/2h; fri-mon 01:00/1m30s")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"daily at 03:00 for 10m",
		"weekdays at 23:55 for 15m",
		"weekends at 04:30 for 1h30m",
		"weekends at 12:00 for 2h",
		"Mon, Fri, Sat, Sun at 01:00 for 1m30s",
	}
	if got := s.Strings(); !slices.Equal(got, want) {
		t.Errorf("Strings() = %q, want %q", got, want)
	}

	for _, empty := range []string{"", "  ", "off", "OFF"} {
		if s, err := Parse(empty); err != nil || len(s) != 0 {
			t.Errorf("Parse(%q) = %v, %v; want empty", empty, s, err)
		}
	}
	for _, bad := range []string{"3am", "25:00", "03:00/0s", "03:00/7h", "03:00/abc", "funday 03:00", "mon 03:00 extra", "03:00x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): want an error", bad)
		}
	}
}

func TestActive(t *testing.T) {
	s, _ := Parse("03:00/10m; mon-fri 23:55/15m")
	// 2026-09-28 is a Monday.
	cases := []struct {
		t          string
		ok         bool
		start, end string
	}{
		{"2026-09-28 02:59:59", false, "", ""},
		{"2026-09-28 03:00:00", true, "2026-09-28 03:00:00", "2026-09-28 03:10:00"},
		{"2026-09-28 03:09:59", true, "2026-09-28 03:00:00", "2026-09-28 03:10:00"},
		{"2026-09-28 03:10:00", false, "", ""},
		{"2026-09-28 23:58:00", true, "2026-09-28 23:55:00", "2026-09-29 00:10:00"},
		{"2026-09-29 00:05:00", true, "2026-09-28 23:55:00", "2026-09-29 00:10:00"}, // crosses midnight
		{"2026-10-03 23:58:00", false, "", ""},                                      // Saturday
		{"2026-10-05 00:05:00", false, "", ""},                                      // started Sunday night
	}
	for _, c := range cases {
		start, end, ok := s.Active(at(c.t))
		if ok != c.ok || (ok && (!start.Equal(at(c.start)) || !end.Equal(at(c.end)))) {
			t.Errorf("Active(%s) = %v, %v, %v; want %v %s-%s", c.t, start, end, ok, c.ok, c.start, c.end)
		}
	}
}

func TestNext(t *testing.T) {
	s, _ := Parse("sun 04:00/20m; wed 03:00")
	start, end, ok := s.Next(at("2026-09-28 12:00:00")) // Monday
	if !ok || !start.Equal(at("2026-09-30 03:00:00")) || !end.Equal(at("2026-09-30 03:10:00")) {
		t.Errorf("Next = %v %v %v", start, end, ok)
	}
	start, _, _ = s.Next(at("2026-09-30 03:00:00")) // exactly at a start: the next one
	if !start.Equal(at("2026-10-04 04:00:00")) {
		t.Errorf("Next after a start = %v", start)
	}
	if _, _, ok := Schedule(nil).Next(at("2026-09-28 12:00:00")); ok {
		t.Error("empty schedule has a next window")
	}
}
