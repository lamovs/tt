package dates

import (
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func at(y int, m time.Month, d, hh, mm int) model.Time {
	return model.NewTime(civil{y: y, m: m, d: d}.at(hh, mm, time.Local))
}

func allDay(y int, m time.Month, d int) model.Time {
	return at(y, m, d, 0, 0)
}

func TestFormatNoDate(t *testing.T) {
	var zero model.Time
	if got := Format(zero, true, time.Local, fixedNow); got != "--" {
		t.Errorf("Format(zero) = %q, want \"--\"", got)
	}
}

func TestFormatNearWords(t *testing.T) {

	cases := []struct {
		name string
		t    model.Time
		day  bool
		want string
	}{
		{"today all-day", allDay(2026, 9, 4), true, "today"},
		{"tmr all-day", allDay(2026, 9, 5), true, "tmr"},
		{"yst all-day", allDay(2026, 9, 3), true, "yst"},
		{"today, time still ahead", at(2026, 9, 4, 18, 0), false, "today 18:00"},
		{"tmr, with time", at(2026, 9, 5, 9, 0), false, "tmr 09:00"},
	}
	for _, c := range cases {
		if got := Format(c.t, c.day, time.Local, fixedNow); got != c.want {
			t.Errorf("%s: Format = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatWeekAhead(t *testing.T) {

	if got := Format(allDay(2026, 9, 11), true, time.Local, fixedNow); got != "fri" {
		t.Errorf("Format(sep11) = %q, want \"fri\"", got)
	}
	if got := Format(allDay(2026, 9, 10), true, time.Local, fixedNow); got != "thu" {
		t.Errorf("Format(sep10) = %q, want \"thu\"", got)
	}
	if got := Format(at(2026, 9, 11, 18, 0), false, time.Local, fixedNow); got != "fri 18:00" {
		t.Errorf("Format(sep11 18:00) = %q, want \"fri 18:00\"", got)
	}
}

func TestFormatBeyondWeek(t *testing.T) {
	if got := Format(allDay(2026, 9, 25), true, time.Local, fixedNow); got != "25sep" {
		t.Errorf("Format(sep25 same year) = %q, want \"25sep\"", got)
	}
	if got := Format(allDay(2027, 9, 25), true, time.Local, fixedNow); got != "25sep27" {
		t.Errorf("Format(sep25 next year) = %q, want \"25sep27\"", got)
	}
}

func TestFormatOverdueAllDay(t *testing.T) {

	if got := Format(allDay(2026, 9, 2), true, time.Local, fixedNow); got != "overdue 2d" {
		t.Errorf("Format(sep2) = %q, want \"overdue 2d\"", got)
	}
	if got := Format(allDay(2026, 9, 1), true, time.Local, fixedNow); got != "overdue 3d" {
		t.Errorf("Format(sep1) = %q, want \"overdue 3d\"", got)
	}
}

func TestFormatOverdueTimed(t *testing.T) {

	cases := []struct {
		name string
		t    model.Time
		want string
	}{
		{"30 minutes ago", at(2026, 9, 4, 15, 0), "overdue 30m"},
		{"6.5 hours ago, same day", at(2026, 9, 4, 9, 0), "overdue 6h"},
		{"yesterday morning", at(2026, 9, 3, 10, 0), "overdue 1d"},
		{"three days ago", at(2026, 9, 1, 8, 0), "overdue 3d"},
	}
	for _, c := range cases {
		if got := Format(c.t, false, time.Local, fixedNow); got != c.want {
			t.Errorf("%s: Format = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatTimedNeverSaysYst(t *testing.T) {

	got := Format(at(2026, 9, 3, 20, 0), false, time.Local, fixedNow)
	if got == "yst" || got == "yst 20:00" {
		t.Errorf("timed entry due yesterday formatted as %q, want an overdue form", got)
	}
}

func TestFormatAllDayReadsTheTaskZone(t *testing.T) {
	moscow := loadZone(t, "Europe/Moscow")
	newYork := loadZone(t, "America/New_York")
	useLocal(t, newYork)

	due := model.NewTime(civil{y: 2026, m: time.September, d: 25}.at(0, 0, moscow))
	now := civil{y: 2026, m: time.September, d: 4}.at(15, 30, newYork)

	if got := Format(due, true, moscow, now); got != "25sep" {
		t.Errorf("Format(2026-09-25 all day, Europe/Moscow) = %q, want \"25sep\"", got)
	}

	if got := Format(due, true, newYork, now); got != "24sep" {
		t.Errorf("Format(the same instant, America/New_York) = %q, want \"24sep\"", got)
	}
}

func TestFormatAllDayBucketAgainstLocalToday(t *testing.T) {
	moscow := loadZone(t, "Europe/Moscow")
	newYork := loadZone(t, "America/New_York")
	useLocal(t, newYork)

	due := model.NewTime(civil{y: 2026, m: time.September, d: 25}.at(0, 0, moscow))

	now := civil{y: 2026, m: time.September, d: 25}.at(1, 0, moscow)

	if got := Format(due, true, moscow, now); got != "tmr" {
		t.Errorf("Format = %q, want \"tmr\" - the 25th, seen from a machine still on the 24th", got)
	}
}

func TestFormatTimedStaysInTheDisplayZone(t *testing.T) {
	moscow := loadZone(t, "Europe/Moscow")
	newYork := loadZone(t, "America/New_York")
	useLocal(t, newYork)

	due := model.NewTime(civil{y: 2026, m: time.September, d: 25}.at(18, 0, moscow))
	now := civil{y: 2026, m: time.September, d: 4}.at(15, 30, newYork)

	for _, tz := range []*time.Location{moscow, newYork, nil} {
		if got := Format(due, false, tz, now); got != "25sep 11:00" {
			t.Errorf("Format(timed, tz=%v) = %q, want \"25sep 11:00\"", tz, got)
		}
	}
}

func TestFormatNilZoneMeansLocal(t *testing.T) {
	newYork := loadZone(t, "America/New_York")
	useLocal(t, newYork)

	due := model.NewTime(civil{y: 2026, m: time.September, d: 25}.at(0, 0, newYork))
	now := civil{y: 2026, m: time.September, d: 4}.at(15, 30, newYork)
	if got, want := Format(due, true, nil, now), Format(due, true, time.Local, now); got != want {
		t.Errorf("Format with a nil zone = %q, with time.Local = %q", got, want)
	}
}

func mustTime(t *testing.T, s string) model.Time {
	t.Helper()
	v, err := model.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFormatLiveAllDayFixtures(t *testing.T) {
	moscow := loadZone(t, "Europe/Moscow")
	auckland := loadZone(t, "Pacific/Auckland")
	losAngeles := loadZone(t, "America/Los_Angeles")
	useLocal(t, moscow)

	now := time.Date(2026, 9, 5, 22, 40, 0, 0, moscow)

	cases := []struct {
		name string
		raw  string
		zone *time.Location

		want, wantNilZone string
	}{

		{"account task", "2026-09-04T21:00:00.000+0000", moscow, "today", "today"},

		{"auckland midnight", "2026-09-09T12:00:00.000+0000", auckland, "thu", "wed"},

		{"los angeles midnight", "2026-09-10T07:00:00.000+0000", losAngeles, "thu", "thu"},

		{"moscow midnight", "2026-09-09T21:00:00.000+0000", moscow, "thu", "thu"},

		{"auckland non-midnight", "2026-09-10T03:30:00.000+0000", auckland, "thu", "thu"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			due := mustTime(t, c.raw)
			if got := Format(due, true, c.zone, now); got != c.want {
				t.Errorf("Format(tz=%v) = %q, want %q", c.zone, got, c.want)
			}
			if got := Format(due, true, nil, now); got != c.wantNilZone {
				t.Errorf("Format(tz=nil) = %q, want %q", got, c.wantNilZone)
			}
		})
	}
}

func TestZone(t *testing.T) {
	moscow := loadZone(t, "Europe/Moscow")
	if got := Zone("Europe/Moscow"); got.String() != moscow.String() {
		t.Errorf("Zone(\"Europe/Moscow\") = %v, want %v", got, moscow)
	}

	for _, name := range []string{"", "Nowhere/Atlantis", "../etc/passwd"} {
		if got := Zone(name); got != time.Local {
			t.Errorf("Zone(%q) = %v, want time.Local", name, got)
		}
	}
}
