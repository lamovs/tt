package schedule

import (
	"time"

	"github.com/movsar/tt/internal/dates"
)

type TriggerDaysPolicy int

const (
	TriggerDaysUnverified TriggerDaysPolicy = iota
	TriggerDaysWallClock
	TriggerDaysElapsed
)

type TriggerMonthsPolicy int

const (
	TriggerMonthsUnverified TriggerMonthsPolicy = iota
	TriggerMonthsClamp
	TriggerMonthsNormalize
	TriggerMonthsExact
)

type TriggerAllDayPolicy int

const (
	TriggerAllDayUnverified TriggerAllDayPolicy = iota
	TriggerAllDayReferenceMidnight
)

type TriggerPolicy struct {
	DefaultRelation TriggerRelation
	Days            TriggerDaysPolicy
	Months          TriggerMonthsPolicy
	AllDay          TriggerAllDayPolicy
}

type TriggerDates struct {
	Start, End time.Time
	AllDay     bool
	TimeZone   string
}

type TriggerResult struct {
	At        time.Time
	Supported bool
	Reason    string
}

// EvaluateTrigger is interpretation only. The zero policy leaves unverified
// provider semantics unsupported; ReminderTime remains the delivery allowlist.
// Explicit calendar policies apply years/months, then weeks/days, then clock
// units. Calendar and all-day interpretation requires an explicit IANA zone.
func EvaluateTrigger(raw string, reference TriggerDates, policy TriggerPolicy) TriggerResult {
	trigger, err := ParseTrigger(raw)
	if err != nil {
		return triggerUnsupported("unsupported_trigger")
	}
	relation := trigger.Relation
	if relation == TriggerRelationUnspecified {
		relation = policy.DefaultRelation
	}
	var at time.Time
	switch relation {
	case TriggerRelatedStart:
		at = reference.Start
		if at.IsZero() {
			return triggerUnsupported("missing_start")
		}
	case TriggerRelatedEnd:
		at = reference.End
		if at.IsZero() {
			return triggerUnsupported("missing_end")
		}
	case TriggerRelationUnspecified:
		return triggerUnsupported("default_reference_unverified")
	default:
		return triggerUnsupported("invalid_reference_policy")
	}
	if reference.AllDay && policy.AllDay != TriggerAllDayReferenceMidnight {
		return triggerUnsupported("all_day_unverified")
	}
	if trigger.hasMonths && policy.Months != TriggerMonthsClamp && policy.Months != TriggerMonthsNormalize && policy.Months != TriggerMonthsExact {
		return triggerUnsupported("calendar_months_unverified")
	}
	if trigger.hasDays && policy.Days != TriggerDaysWallClock && policy.Days != TriggerDaysElapsed {
		return triggerUnsupported("calendar_days_unverified")
	}
	if reference.TimeZone != "" || reference.AllDay || trigger.hasMonths || trigger.hasDays {
		loc, err := dates.IntervalZone(reference.TimeZone)
		if err != nil {
			return triggerUnsupported("invalid_timezone")
		}
		at = at.In(loc)
	}
	if !triggerYearSupported(at) {
		return triggerUnsupported("date_out_of_range")
	}
	if reference.AllDay {
		resolved := triggerWallTime(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		if !resolved.Supported {
			return resolved
		}
		at = resolved.At
	}
	sign := int64(1)
	if trigger.Negative {
		sign = -1
	}
	if trigger.hasMonths {
		if trigger.Years > 9999 || trigger.Months > 9999*12 {
			return triggerUnsupported("date_out_of_range")
		}
		month := int64(at.Year()-1)*12 + int64(at.Month()-1) + sign*(trigger.Years*12+trigger.Months)
		if month < 0 || month >= 9999*12 {
			return triggerUnsupported("date_out_of_range")
		}
		year, targetMonth, day := int(month/12)+1, time.Month(month%12)+1, at.Day()
		lastDay := time.Date(year, targetMonth+1, 0, 0, 0, 0, 0, time.UTC).Day()
		if day > lastDay {
			switch policy.Months {
			case TriggerMonthsClamp:
				day = lastDay
			case TriggerMonthsExact:
				return triggerUnsupported("calendar_date_missing")
			}
		}
		civil := time.Date(year, targetMonth, day, 0, 0, 0, 0, time.UTC)
		resolved := triggerWallTime(civil.Year(), civil.Month(), civil.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), at.Location())
		if !resolved.Supported {
			return resolved
		}
		at = resolved.At
	}
	if trigger.hasDays {
		const maximumDays = int64(366 * 9999)
		if trigger.Weeks > maximumDays/7 || trigger.Days > maximumDays-trigger.Weeks*7 {
			return triggerUnsupported("date_out_of_range")
		}
		days := sign * (trigger.Weeks*7 + trigger.Days)
		if policy.Days == TriggerDaysElapsed {
			const maximumElapsedDays = int64((1<<63 - 1) / int64(24*time.Hour))
			if days < -maximumElapsedDays || days > maximumElapsedDays {
				return triggerUnsupported("duration_out_of_range")
			}
			at = at.Add(time.Duration(days) * 24 * time.Hour)
		} else {
			civil := time.Date(at.Year(), at.Month(), at.Day()+int(days), 0, 0, 0, 0, time.UTC)
			resolved := triggerWallTime(civil.Year(), civil.Month(), civil.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), at.Location())
			if !resolved.Supported {
				return resolved
			}
			at = resolved.At
		}
	}
	duration, _ := trigger.clockDuration()
	at = at.Add(duration)
	if !triggerYearSupported(at) {
		return triggerUnsupported("date_out_of_range")
	}
	return TriggerResult{At: at, Supported: true}
}

func triggerUnsupported(reason string) TriggerResult { return TriggerResult{Reason: reason} }

func triggerYearSupported(at time.Time) bool { return at.Year() >= 1 && at.Year() <= 9999 }

func triggerWallTime(year int, month time.Month, day, hour, minute, second, nano int, loc *time.Location) TriggerResult {
	wall := time.Date(year, month, day, hour, minute, second, nano, time.UTC)
	if !triggerYearSupported(wall) {
		return triggerUnsupported("date_out_of_range")
	}
	var matches []time.Time
	seen := make(map[int]bool)
	end := wall.Add(48 * time.Hour)
	for probe := wall.Add(-48 * time.Hour); !probe.After(end); {
		zoned := probe.In(loc)
		_, offset := zoned.Zone()
		if !seen[offset] {
			seen[offset] = true
			candidate := wall.Add(-time.Duration(offset) * time.Second).In(loc)
			if candidate.Year() == year && candidate.Month() == month && candidate.Day() == day && candidate.Hour() == hour && candidate.Minute() == minute && candidate.Second() == second && candidate.Nanosecond() == nano {
				matches = append(matches, candidate)
			}
		}
		_, next := zoned.ZoneBounds()
		if next.IsZero() || !next.After(probe) {
			break
		}
		probe = next
	}
	if len(matches) == 0 {
		return triggerUnsupported("nonexistent_local_time")
	}
	if len(matches) != 1 {
		return triggerUnsupported("ambiguous_local_time")
	}
	return TriggerResult{At: matches[0], Supported: true}
}
