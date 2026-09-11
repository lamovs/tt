package dates

import (
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

var roundTripZones = append([]string{""}, dstZones...)

func eachRoundTripZone(t *testing.T, fn func(t *testing.T, loc *time.Location, now time.Time)) {
	for _, name := range roundTripZones {
		label := name
		if label == "" {
			label = "local"
		}
		t.Run(label, func(t *testing.T) {
			loc := time.Local
			if name != "" {
				loc = loadZone(t, name)
			}
			useLocal(t, loc)
			fn(t, loc, civil{y: 2026, m: time.September, d: 4}.at(15, 30, loc))
		})
	}
}

func TestRoundTripAllDay(t *testing.T) {
	eachRoundTripZone(t, func(t *testing.T, loc *time.Location, now time.Time) {
		today := civilOf(now)
		for diff := -1; diff <= 400; diff++ {
			day := today.addDays(diff)
			want := day.at(0, 0, loc)
			mt := model.NewTime(want)
			formatted := Format(mt, true, loc, now)
			res, err := Parse(formatted, now)
			if err != nil {
				t.Fatalf("diff=%d: Format gave %q, Parse failed: %v", diff, formatted, err)
			}
			if !res.AllDay {
				t.Errorf("diff=%d: round trip of %q lost AllDay", diff, formatted)
			}
			if !res.Time.Equal(want) {
				t.Errorf("diff=%d: round trip of %q gave %v, want %v", diff, formatted, res.Time.Time, want)
			}
		}
	})
}

func TestRoundTripTimed(t *testing.T) {
	clocks := []struct{ h, m int }{{0, 0}, {9, 0}, {18, 0}, {23, 59}}
	eachRoundTripZone(t, func(t *testing.T, loc *time.Location, now time.Time) {
		today := civilOf(now)
		for diff := 0; diff <= 400; diff++ {
			day := today.addDays(diff)
			for _, c := range clocks {
				moment := day.at(c.h, c.m, loc)
				if moment.Before(now) {
					continue
				}
				mt := model.NewTime(moment)
				formatted := Format(mt, false, loc, now)
				res, err := Parse(formatted, now)
				if err != nil {
					t.Fatalf("diff=%d %02d:%02d: Format gave %q, Parse failed: %v", diff, c.h, c.m, formatted, err)
				}
				if res.AllDay {
					t.Errorf("diff=%d %02d:%02d: round trip of %q lost the time of day", diff, c.h, c.m, formatted)
				}
				if !res.Time.Equal(moment) {
					t.Errorf("diff=%d %02d:%02d: round trip of %q gave %v, want %v", diff, c.h, c.m, formatted, res.Time.Time, moment)
				}
			}
		}
	})
}

func TestRoundTripLeapDay(t *testing.T) {
	now := time.Date(2027, 12, 1, 8, 0, 0, 0, time.Local)
	day := civil{y: 2028, m: time.February, d: 29}.at(0, 0, time.Local)
	mt := model.NewTime(day)
	formatted := Format(mt, true, time.Local, now)
	res, err := Parse(formatted, now)
	if err != nil {
		t.Fatalf("Format gave %q, Parse failed: %v", formatted, err)
	}
	if !res.Time.Equal(day) {
		t.Errorf("round trip of %q gave %v, want %v", formatted, res.Time.Time, day)
	}
}

func TestRoundTripYearBoundary(t *testing.T) {
	now := time.Date(2026, 12, 30, 8, 0, 0, 0, time.Local)
	day := civil{y: 2027, m: time.January, d: 2}.at(0, 0, time.Local)
	mt := model.NewTime(day)
	formatted := Format(mt, true, time.Local, now)
	if formatted != "sat" {
		t.Fatalf("Format = %q, want \"sat\"", formatted)
	}
	res, err := Parse(formatted, now)
	if err != nil {
		t.Fatalf("Parse(%q): %v", formatted, err)
	}
	if !res.Time.Equal(day) {
		t.Errorf("round trip of %q gave %v, want %v", formatted, res.Time.Time, day)
	}
}
