package dates

import (
	"testing"
	"time"
)

func TestCivilMatchesUTC(t *testing.T) {
	c := civil{y: 1800, m: time.January, d: 1}
	u := time.Date(1800, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 365*500; i++ {
		if got := civilOf(u); got != c {
			t.Fatalf("day %d: civil %v, time.Time says %v", i, c, got)
		}
		if c.weekday() != u.Weekday() {
			t.Fatalf("%v: weekday = %v, want %v", c, c.weekday(), u.Weekday())
		}
		if round := civilFromEpochDay(c.epochDay()); round != c {
			t.Fatalf("%v: epochDay round trip gave %v", c, round)
		}
		if got, want := int64(c.epochDay())*86400, u.Unix(); got != want {
			t.Fatalf("%v: epochDay = %d days, %d seconds, want %d", c, c.epochDay(), got, want)
		}
		if !c.valid() {
			t.Fatalf("%v: valid = false", c)
		}
		c = c.addDays(1)
		u = u.AddDate(0, 0, 1)
	}
}

func TestCivilRejectsDaysThatDoNotExist(t *testing.T) {
	bad := []civil{
		{y: 2026, m: time.February, d: 29},
		{y: 2100, m: time.February, d: 29},
		{y: 2026, m: time.April, d: 31},
		{y: 2026, m: time.September, d: 0},
		{y: 2026, m: time.Month(13), d: 1},
		{y: 2026, m: time.Month(0), d: 1},
	}
	for _, c := range bad {
		if c.valid() {
			t.Errorf("%v: valid = true, want false", c)
		}
	}
	for _, c := range []civil{
		{y: 2028, m: time.February, d: 29},
		{y: 2000, m: time.February, d: 29},
		{y: 2026, m: time.April, d: 30},
		{y: 2026, m: time.December, d: 31},
	} {
		if !c.valid() {
			t.Errorf("%v: valid = false, want true", c)
		}
	}
}

func TestCivilBefore(t *testing.T) {
	sep4 := civil{y: 2026, m: time.September, d: 4}
	cases := []struct {
		a, b civil
		want bool
	}{
		{sep4, sep4, false},
		{sep4, civil{y: 2026, m: time.September, d: 5}, true},
		{civil{y: 2026, m: time.September, d: 5}, sep4, false},
		{civil{y: 2026, m: time.August, d: 31}, sep4, true},
		{civil{y: 2025, m: time.December, d: 31}, sep4, true},
		{civil{y: 2027, m: time.January, d: 1}, sep4, false},
	}
	for _, c := range cases {
		if got := c.a.before(c.b); got != c.want {
			t.Errorf("%v.before(%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
