package schedule

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func ParseRepeat(value string) (string, error) {
	switch value {
	case "daily", "weekly", "monthly", "yearly":
		return "RRULE:FREQ=" + map[string]string{"daily": "DAILY", "weekly": "WEEKLY", "monthly": "MONTHLY", "yearly": "YEARLY"}[value] + ";INTERVAL=1", nil
	case "none":
		return "", nil
	}
	if !strings.HasPrefix(value, "RRULE:") || len(value) == len("RRULE:") {
		return "", errors.New("repeat rule must be daily, weekly, monthly, yearly, none, or a nonempty RRULE: value")
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", errors.New("repeat rule must not contain control characters")
	}
	return value, nil
}

func ValidateReminder(value string) error {
	prefix, body, valid := strings.Cut(value, ":")
	if !valid || body == "" || prefix != "TRIGGER" && prefix != "TRIGGER;RELATED=START" && prefix != "TRIGGER;RELATED=END" {
		return errors.New("reminder must be a nonempty TRIGGER: value")
	}
	if strings.ContainsFunc(value, unicode.IsControl) {
		return errors.New("reminder must not contain control characters")
	}
	return nil
}

func ValidateReminders(values []string) error {
	seen := make(map[string]bool, len(values))
	for i, value := range values {
		if err := ValidateReminder(value); err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("duplicate reminder trigger at position %d", i+1)
		}
		seen[value] = true
	}
	return nil
}

func ParseReminder(value string) (string, error) {
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", errors.New("reminder must not contain control characters")
	}
	if value == "at" {
		return "TRIGGER:PT0S", nil
	}
	if strings.HasPrefix(value, "-") {
		d, err := dates.ParseElapsed(value[1:])
		if err != nil {
			return "", err
		}
		minutes := int64(d / time.Minute)
		out := "TRIGGER:-PT"
		if minutes/60 > 0 {
			out += fmt.Sprintf("%dH", minutes/60)
		}
		if minutes%60 > 0 {
			out += fmt.Sprintf("%dM", minutes%60)
		}
		return out, nil
	}
	if err := ValidateReminder(value); err != nil {
		return "", errors.New("reminder needs at, a before-time such as -10min, or a nonempty TRIGGER: value without control characters")
	}
	return value, nil
}

func ParseReminders(values []string) ([]string, error) {
	if len(values) == 1 && values[0] == "none" {
		return []string{}, nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		if value == "none" {
			return nil, errors.New("none must stand alone")
		}
		var err error
		out[i], err = ParseReminder(value)
		if err != nil {
			return nil, err
		}
	}
	return out, ValidateReminders(out)
}

func ReminderInput(value string) string {
	if value == "TRIGGER:PT0S" {
		return "at"
	}
	if strings.HasPrefix(value, "TRIGGER:-PT") {
		short := "-" + strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(value, "TRIGGER:-PT"), "H", "h"), "M", "min")
		if roundTrip, err := ParseReminder(short); err == nil && roundTrip == value {
			return short
		}
	}
	return value
}

func DueEdit(result dates.Result) model.TaskEdit {
	edit := model.TaskEdit{DueDate: model.NewEditTime(result.Time), IsAllDay: model.Ptr(result.AllDay)}
	if result.Clear {
		edit.StartDate = model.NewEditTime(model.Time{})
		edit.IsAllDay = model.Ptr(false)
	} else {
		edit.TimeZone = model.Ptr("")
	}
	return edit
}

func DueUnchanged(task model.Task, result dates.Result) bool {
	if !task.DueDate.Equal(result.Time.Time) {
		return false
	}
	if result.Clear {
		return task.StartDate.IsZero() && !task.IsAllDay
	}
	return task.IsAllDay == result.AllDay && task.TimeZone == ""
}

func ReminderDate(value string, now time.Time) (dates.Result, error) {
	value = strings.TrimSpace(value)
	result, err := dates.Parse(value, now)
	if err != nil && !strings.ContainsAny(value, " \t\r\n") && !strings.HasPrefix(value, "+") {
		result, err = dates.Parse("today "+value, now)
	}
	if err != nil {
		return dates.Result{}, err
	}
	if result.Clear || result.AllDay {
		return dates.Result{}, errors.New("a reminder shortcut needs a date with a time, such as in 2h or tmr 09:00")
	}
	return result, nil
}

type Change struct {
	Interval  *dates.Interval
	Due       *dates.Result
	Repeat    *string
	Reminders *[]string
}

func (change Change) Edit(original model.Task) (model.TaskEdit, error) {
	var edit model.TaskEdit
	if change.Interval != nil {
		if change.Due != nil {
			return edit, errors.New("choose Date or Start/Duration, not both")
		}
		var err error
		edit, err = IntervalEdit(original, *change.Interval)
		if err != nil {
			return edit, err
		}
	}
	if change.Due != nil && !DueUnchanged(original, *change.Due) {
		edit = DueEdit(*change.Due)
	}
	if change.Repeat != nil && *change.Repeat != original.RepeatFlag {
		if *change.Repeat != "" {
			if _, err := ParseRepeat(*change.Repeat); err != nil {
				return edit, err
			}
		}
		edit.RepeatFlag = change.Repeat
	}
	if change.Reminders != nil && !slices.Equal(*change.Reminders, original.Reminders) {
		if err := ValidateReminders(*change.Reminders); err != nil {
			return edit, err
		}
		edit.Reminders = model.NewEditList(slices.Clone(*change.Reminders))
	}
	if change.Interval == nil {
		return ProtectIntervalEnd(original, edit)
	}
	return edit, nil
}
