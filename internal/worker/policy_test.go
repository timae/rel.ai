package worker

import (
	"testing"
	"time"
)

// date returns a time on the given weekday at hh:mm (June 2026: the 1st is a Monday).
func date(t *testing.T, day time.Weekday, hh, mm int) time.Time {
	t.Helper()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local) // Monday
	for base.Weekday() != day {
		base = base.AddDate(0, 0, 1)
	}
	return time.Date(base.Year(), base.Month(), base.Day(), hh, mm, 0, 0, time.Local)
}

func TestInWindow(t *testing.T) {
	cases := []struct {
		windows []string
		day     time.Weekday
		hh, mm  int
		want    bool
	}{
		{nil, time.Monday, 12, 0, true}, // no windows = always
		{[]string{"Sat 00:00-24:00"}, time.Saturday, 13, 0, true},
		{[]string{"Sat 00:00-24:00"}, time.Friday, 13, 0, false},
		{[]string{"Sat 00:00-24:00", "Sun 00:00-24:00"}, time.Sunday, 23, 59, true},
		{[]string{"Mon-Fri 19:00-07:00"}, time.Tuesday, 20, 0, true},  // evening of listed day
		{[]string{"Mon-Fri 19:00-07:00"}, time.Tuesday, 6, 30, true},  // morning after Monday
		{[]string{"Mon-Fri 19:00-07:00"}, time.Tuesday, 12, 0, false}, // working hours
		{[]string{"Mon-Fri 19:00-07:00"}, time.Saturday, 6, 0, true},  // morning after Friday
		{[]string{"Mon-Fri 19:00-07:00"}, time.Sunday, 20, 0, false},  // Sunday evening not listed
		{[]string{"bogus"}, time.Monday, 12, 0, false},                // malformed = no match
	}
	for _, c := range cases {
		p := Policy{Windows: c.windows}
		got := p.InWindow(date(t, c.day, c.hh, c.mm))
		if got != c.want {
			t.Errorf("InWindow(%v, %s %02d:%02d) = %v, want %v", c.windows, c.day, c.hh, c.mm, got, c.want)
		}
	}
}
