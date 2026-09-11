package schedule

import (
	"regexp"
	"strconv"
	"time"
)

var relativeReminderPattern = regexp.MustCompile(`^TRIGGER:-PT(?:(\d+)H)?(?:(\d+)M)?$`)

func ReminderTime(trigger string, due time.Time) (time.Time, bool) {
	if trigger == "TRIGGER:PT0S" {
		return due, true
	}
	parts := relativeReminderPattern.FindStringSubmatch(trigger)
	if parts == nil || parts[1] == "" && parts[2] == "" {
		return time.Time{}, false
	}
	hours, err := strconv.ParseInt(zeroIfEmpty(parts[1]), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	minutes, err := strconv.ParseInt(zeroIfEmpty(parts[2]), 10, 64)
	if err != nil || minutes >= 60 {
		return time.Time{}, false
	}
	if hours > int64((24*time.Hour*365*200)/time.Hour) {
		return time.Time{}, false
	}
	offset := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	if offset <= 0 {
		return time.Time{}, false
	}
	return due.Add(-offset), true
}

func zeroIfEmpty(value string) string {
	if value == "" {
		return "0"
	}
	return value
}
