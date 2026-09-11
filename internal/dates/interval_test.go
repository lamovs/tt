package dates

import (
	"strings"
	"testing"
	"time"
)

func TestIntervalStrictClocksAndElapsedEnd(t *testing.T) {
	now := time.Date(2026, 9, 10, 17, 0, 0, 0, time.UTC)
	for _, test := range []struct{ expr, zone, start, end, failure string }{
		{"14:00 + 1h30min", "UTC", "2026-09-10T14:00:00Z", "2026-09-10T15:30:00Z", ""},
		{"23:30 + 2h", "UTC", "2026-09-10T23:30:00Z", "2026-09-11T01:30:00Z", ""},
		{"+1m 14:00 + 30min", "UTC", "2026-10-10T14:00:00Z", "2026-10-10T14:30:00Z", ""},
		{"2026-03-29 01:30 + 2h", "Europe/Berlin", "2026-03-29T00:30:00Z", "2026-03-29T02:30:00Z", ""},
		{"2026-03-29 02:30 + 1h", "Europe/Berlin", "", "", "does not exist"},
		{"2026-10-25 02:30 + 1h", "Europe/Berlin", "", "", "occurs twice"},
		{"2026-10-25T02:30:00+02:00 + 1h", "Europe/Berlin", "2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z", ""},
		{"2026-10-25T02:30:00+01:00 + 1h", "Europe/Berlin", "2026-10-25T01:30:00Z", "2026-10-25T02:30:00Z", ""},
		{"2026-10-25T02:30:00+03:00 + 1h", "Europe/Berlin", "", "", "offset"},
		{"2011-12-30 12:00 + 1h", "Pacific/Apia", "", "", "does not exist"},
		{"14:00 + 1m", "UTC", "", "", "hours/minutes"},
		{"14:00 + 0min", "UTC", "", "", "positive"},
		{"14:00 + 1h", "Local", "", "", "IANA"},
		{"2099-12-31 23:30 + 1h", "UTC", "", "", "2000-2099"},
	} {
		t.Run(test.expr+test.zone, func(t *testing.T) {
			got, err := ParseInterval(test.expr, test.zone, now)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("got %+v, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Start.UTC().Format(time.RFC3339) != test.start || got.End.UTC().Format(time.RFC3339) != test.end {
				t.Fatalf("unexpected %+v", got)
			}
		})
	}
}
