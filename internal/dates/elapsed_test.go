package dates

import (
	"testing"
	"time"
)

func TestElapsedDatesKeepElapsedTimeAcrossDST(t *testing.T) {
	for _, zone := range []string{"UTC", "Europe/Berlin", "Australia/Lord_Howe"} {
		t.Run(zone, func(t *testing.T) {
			loc := loadZone(t, zone)
			useLocal(t, loc)
			for _, now := range []time.Time{
				time.Date(2026, 3, 29, 1, 30, 23, 456000000, loc),
				time.Date(2026, 10, 25, 1, 30, 23, 456000000, loc),
				time.Date(2026, 4, 5, 1, 30, 23, 456000000, loc),
			} {
				for input, elapsed := range map[string]time.Duration{"in 2h": 2 * time.Hour, "+2h": 2 * time.Hour, "in 30min": 30 * time.Minute, "+30min": 30 * time.Minute, "in 1h30min": 90 * time.Minute, "+1h30min": 90 * time.Minute} {
					got, err := Parse(input, now)
					if err != nil || got.Clear || got.AllDay || got.Time.Sub(now) != elapsed {
						t.Fatalf("%s at %s: %+v %v", input, now, got, err)
					}
				}
			}
		})
	}
}

func TestElapsedGrammarPreservesMonthsAndRejectsInvalidValues(t *testing.T) {
	useLocal(t, time.UTC)
	now := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	month, err := Parse("+1m", now)
	if err != nil || !month.AllDay || month.Time.Format("2006-01-02") != "2026-02-28" {
		t.Fatalf("month changed: %+v %v", month, err)
	}
	for _, input := range []string{"in 0h", "+0min", "in -2h", "in 2h 18:00", "+1.5h", "in 1m", "in 9999999999999999999999999h", "+999999999h", "in 2hours", "in 1min2h"} {
		if _, err := Parse(input, now); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	if _, err := Parse("+2h", time.Date(2099, 12, 31, 23, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted out-of-range date")
	}
}
