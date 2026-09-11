package dates

import (
	"fmt"
	"testing"
	"time"

	_ "time/tzdata"
)

var dstZones = []string{
	"America/Santiago",
	"America/Havana",
	"America/Sao_Paulo",
	"Asia/Amman",
	"Antarctica/Casey",
	"Australia/Lord_Howe",
	"America/Nuuk",
	"Europe/Berlin",
}

const (
	sweepFirstYear = 2016
	sweepLastYear  = 2028
)

func loadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
	return loc
}

func useLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func TestRoundTripAcrossDST(t *testing.T) {
	for _, name := range dstZones {
		t.Run(name, func(t *testing.T) {
			loc := loadZone(t, name)
			useLocal(t, loc)

			last := civil{y: sweepLastYear, m: time.December, d: 31}
			for day := (civil{y: sweepFirstYear, m: time.January, d: 1}); !last.before(day); day = day.addDays(1) {
				now := day.at(12, 0, loc)
				tomorrow := day.addDays(1)
				want := tomorrow.at(0, 0, loc)

				res := mustParse(t, "tmr", now)
				if !res.Time.Equal(want) {
					t.Fatalf("now=%v: Parse(tmr) = %v, want %v", now, res.Time.Time, want)
				}
				if got := civilOf(res.Time.In(loc)); got != tomorrow {
					t.Fatalf("now=%v: Parse(tmr) landed on %v, want %v", now, got, tomorrow)
				}
				if formatted := Format(res.Time, true, time.Local, now); formatted != "tmr" {
					t.Fatalf("now=%v: Format(tmr's date) = %q, want \"tmr\"", now, formatted)
				}

				today := mustParse(t, "today", now)
				if got := civilOf(today.Time.In(loc)); got != day {
					t.Fatalf("now=%v: Parse(today) landed on %v, want %v", now, got, day)
				}
				if formatted := Format(today.Time, true, time.Local, now); formatted != "today" {
					t.Fatalf("now=%v: Format(today's date) = %q, want \"today\"", now, formatted)
				}
			}
		})
	}
}

func TestAllDayIsFirstMomentOfLocalDay(t *testing.T) {
	for _, name := range dstZones {
		t.Run(name, func(t *testing.T) {
			loc := loadZone(t, name)

			last := civil{y: sweepLastYear, m: time.December, d: 31}
			for day := (civil{y: sweepFirstYear, m: time.January, d: 1}); !last.before(day); day = day.addDays(1) {
				got := day.at(0, 0, loc)
				if back := civilOf(got.In(loc)); back != day {
					t.Fatalf("%v: the all-day instant %v reads back as %v", day, got, back)
				}
				if earlier := got.Add(-time.Nanosecond); civilOf(earlier.In(loc)) == day {
					t.Fatalf("%v: %v is not the first moment of the day, %v is earlier and on it", day, got, earlier)
				}

				_, offStart := got.Zone()
				_, offBefore := got.Add(-zoneSearchWindow).Zone()
				if offStart == offBefore {
					continue
				}
				for back := zoneSearchStep; back <= zoneSearchWindow; back += zoneSearchStep {
					if earlier := got.Add(-back); civilOf(earlier.In(loc)) == day {
						t.Fatalf("%v: %v is on the day too and earlier than %v", day, earlier, got)
					}
				}
			}
		})
	}
}

func TestParseKeepsSkippedTimeInsideTheDay(t *testing.T) {
	loc := loadZone(t, "America/Santiago")
	useLocal(t, loc)

	now := civil{y: 2026, m: time.September, d: 4}.at(12, 0, loc)
	skipped := []string{"6sep 0:30", "6sep 12am", "sun 0:00"}
	for _, in := range skipped {
		r := mustParse(t, in, now)
		got := r.Time.In(loc)
		if y, m, d := got.Date(); y != 2026 || m != time.September || d != 6 {
			t.Errorf("Parse(%q) = %v, want a moment on 2026-09-06", in, got)
			continue
		}
		if got.Hour() != 1 || got.Minute() != 0 {
			t.Errorf("Parse(%q) = %v, want 01:00, the first moment that day has", in, got)
		}
		if r.AllDay {
			t.Errorf("Parse(%q) set AllDay on an expression that named a time", in)
		}
	}

	wantDateTime(t, mustParse(t, "6sep 9:00", now), 2026, 9, 6, 9, 0)
	wantDateTime(t, mustParse(t, "6sep 1:30", now), 2026, 9, 6, 1, 30)
}

func TestSameDayThroughDifferentPaths(t *testing.T) {
	loc := loadZone(t, "America/Santiago")
	useLocal(t, loc)

	now := civil{y: 2026, m: time.September, d: 4}.at(15, 30, loc)
	target := civil{y: 2026, m: time.September, d: 6}
	forms := []string{"eow", "sun", "+2d", "6sep", "sep6", "6.09", "2026-09-06"}

	wantDay := target.at(0, 0, loc)
	if got := civilOf(wantDay.In(loc)); got != target {
		t.Fatalf("the all-day instant for %v reads back as %v", target, got)
	}
	for _, in := range forms {
		if r := mustParse(t, in, now); !r.Time.Equal(wantDay) {
			t.Errorf("Parse(%q) = %v, want %v - every one of %v names %v", in, r.Time.Time, wantDay, forms, target)
		}
	}

	wantEvening := target.at(18, 0, loc)
	for _, in := range forms {
		if r := mustParse(t, in+" 18:00", now); !r.Time.Equal(wantEvening) {
			t.Errorf("Parse(%q) = %v, want %v", in+" 18:00", r.Time.Time, wantEvening)
		}
	}
}

func TestRepeatedReadingTakesTheEarlierInstant(t *testing.T) {
	cases := []struct {
		zone         string
		day          civil
		hour, minute int
		wantOffset   int
	}{

		{"Asia/Amman", civil{y: 2016, m: time.October, d: 28}, 0, 30, 3 * 3600},

		{"Australia/Lord_Howe", civil{y: 2016, m: time.April, d: 3}, 1, 45, 11 * 3600},

		{"Antarctica/Casey", civil{y: 2018, m: time.March, d: 11}, 1, 30, 11 * 3600},
	}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			loc := loadZone(t, c.zone)
			got := c.day.at(c.hour, c.minute, loc)
			if civilOf(got) != c.day || got.Hour() != c.hour || got.Minute() != c.minute {
				t.Fatalf("at(%02d:%02d) = %v, want that reading on %v", c.hour, c.minute, got, c.day)
			}

			var occurrences []time.Time
			for d := -zoneSearchWindow; d <= zoneSearchWindow; d += zoneSearchStep {
				if o := got.Add(d); civilOf(o) == c.day && o.Hour() == c.hour && o.Minute() == c.minute {
					occurrences = append(occurrences, o)
				}
			}
			if len(occurrences) != 2 {
				t.Fatalf("%v %02d:%02d occurs %d times in %s, want twice", c.day, c.hour, c.minute, len(occurrences), c.zone)
			}
			if !got.Equal(occurrences[0]) {
				t.Errorf("at(%02d:%02d) = %v, want %v - the earlier of the two instants the reading names", c.hour, c.minute, got, occurrences[0])
			}
			if _, off := got.Zone(); off != c.wantOffset {
				t.Errorf("at(%02d:%02d) = %v (offset %d), want offset %d", c.hour, c.minute, got, off, c.wantOffset)
			}
		})
	}
}

func TestSkippedTailOfTheDayGoesToTheNextDay(t *testing.T) {
	cases := []struct {
		zone         string
		day          civil
		hour, minute int
	}{

		{"America/Nuuk", civil{y: 2026, m: time.March, d: 28}, 23, 30},
		{"America/Nuuk", civil{y: 2024, m: time.March, d: 30}, 23, 0},
		{"America/Godthab", civil{y: 2027, m: time.March, d: 27}, 23, 59},
		{"America/Scoresbysund", civil{y: 2026, m: time.March, d: 28}, 23, 45},

		{"Asia/Dhaka", civil{y: 2009, m: time.June, d: 19}, 23, 30},

		{"Asia/Pyongyang", civil{y: 2018, m: time.May, d: 4}, 23, 45},
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s/%d-%02d-%02d", c.zone, c.day.y, c.day.m, c.day.d)
		t.Run(name, func(t *testing.T) {
			loc := loadZone(t, c.zone)

			noon := c.day.at(12, 0, loc)
			for d := time.Duration(0); d <= 14*time.Hour; d += zoneSearchStep {
				if o := noon.Add(d); civilOf(o) == c.day && o.Hour() == c.hour && o.Minute() == c.minute {
					t.Fatalf("%v %02d:%02d exists in %s, at %v - the case has to be rewritten", c.day, c.hour, c.minute, c.zone, o)
				}
			}

			got := c.day.at(c.hour, c.minute, loc)
			want := c.day.addDays(1).at(0, 0, loc)
			if !got.Equal(want) {
				t.Fatalf("at(%02d:%02d) on %v = %v, want %v - the first moment of the next day", c.hour, c.minute, c.day, got, want)
			}

			if prev := got.Add(-time.Nanosecond); civilOf(prev) != c.day {
				t.Errorf("the instant before %v is %v, on %v - want it still on %v", got, prev, civilOf(prev), c.day)
			}
		})
	}
}

func TestParseGoesForwardWhenTheDayEndsEarly(t *testing.T) {
	loc := loadZone(t, "America/Nuuk")
	useLocal(t, loc)

	now := civil{y: 2026, m: time.March, d: 27}.at(12, 0, loc)
	want := civil{y: 2026, m: time.March, d: 29}.at(0, 0, loc)
	for _, in := range []string{"28mar 23:30", "2026-03-28 23:00", "tmr 11:30pm", "+1d 23:59"} {
		r := mustParse(t, in, now)
		if !r.Time.Equal(want) {
			t.Errorf("Parse(%q) = %v, want %v", in, r.Time.In(loc), want)
		}
		if r.AllDay {
			t.Errorf("Parse(%q) set AllDay on an expression that named a time", in)
		}
	}

	wantDateTime(t, mustParse(t, "28mar 22:59", now), 2026, 3, 28, 22, 59)
}

func TestMissingDayKeepsTheGuess(t *testing.T) {
	loc := loadZone(t, "Pacific/Apia")
	day := civil{y: 2011, m: time.December, d: 30}

	from := time.Date(2011, 12, 28, 0, 0, 0, 0, loc)
	for d := time.Duration(0); d <= 96*time.Hour; d += zoneSearchStep {
		if o := from.Add(d); civilOf(o) == day {
			t.Fatalf("%v exists in Pacific/Apia, at %v - the case has to be rewritten", day, o)
		}
	}

	for _, c := range []struct {
		hour, minute int
		want         time.Time
	}{
		{0, 0, time.Date(2011, 12, 29, 0, 0, 0, 0, loc)},
		{18, 0, time.Date(2011, 12, 31, 18, 0, 0, 0, loc)},
	} {
		if got := day.at(c.hour, c.minute, loc); !got.Equal(c.want) {
			t.Errorf("at(%02d:%02d) on the missing %v = %v, want %v", c.hour, c.minute, day, got, c.want)
		}
	}
}
