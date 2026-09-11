package dates

import (
	"fmt"
	"time"

	"github.com/movsar/tt/internal/model"
)

var weekdayShort = map[time.Weekday]string{
	time.Monday:    "mon",
	time.Tuesday:   "tue",
	time.Wednesday: "wed",
	time.Thursday:  "thu",
	time.Friday:    "fri",
	time.Saturday:  "sat",
	time.Sunday:    "sun",
}

var monthShort = [...]string{
	"",
	"jan", "feb", "mar", "apr", "may", "jun",
	"jul", "aug", "sep", "oct", "nov", "dec",
}

func Zone(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}

func Format(t model.Time, allDay bool, tz *time.Location, now time.Time) string {
	if t.IsZero() {
		return "--"
	}

	nowLocal := now.In(time.Local)
	today := civilOf(nowLocal)

	if allDay {
		if tz == nil {
			tz = time.Local
		}
		day := civilOf(t.In(tz))
		if diff := daysBetween(today, day); diff <= -2 {
			return fmt.Sprintf("overdue %dd", -diff)
		}
		return dayBucket(today, day)
	}

	local := t.In(time.Local)
	if local.Before(nowLocal) {
		return "overdue " + overdueSpan(nowLocal.Sub(local))
	}
	return dayBucket(today, civilOf(local)) + fmt.Sprintf(" %02d:%02d", local.Hour(), local.Minute())
}

func FormatNow(t model.Time, allDay bool, tz *time.Location) string {
	return Format(t, allDay, tz, time.Now())
}

func dayBucket(today, day civil) string {
	diff := daysBetween(today, day)
	switch diff {
	case -1:
		return "yst"
	case 0:
		return "today"
	case 1:
		return "tmr"
	}
	if diff >= 2 && diff <= 7 {
		return weekdayShort[day.weekday()]
	}
	if day.y == today.y {
		return fmt.Sprintf("%d%s", day.d, monthShort[day.m])
	}
	return fmt.Sprintf("%d%s%02d", day.d, monthShort[day.m], day.y%100)
}

func overdueSpan(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func daysBetween(a, b civil) int {
	return b.epochDay() - a.epochDay()
}
