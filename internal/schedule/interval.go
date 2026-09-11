package schedule

import (
	"errors"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func TimedInterval(t model.Task) bool {
	return !t.IsAllDay && !t.StartDate.IsZero() && !t.DueDate.IsZero() && t.DueDate.After(t.StartDate.Time)
}

func EditableInterval(t model.Task) bool {
	return TimedInterval(t) && t.RepeatFlag == "" && !t.Status.Done()
}

func ReviewsInterval(original model.Task, edit model.TaskEdit) bool {
	return EditableInterval(original) && edit.DueDate != nil && !edit.DueDate.Time.IsZero()
}

func IntervalEdit(original model.Task, interval dates.Interval) (model.TaskEdit, error) {
	if original.IsAllDay || original.RepeatFlag != "" || original.Status.Done() {
		return model.TaskEdit{}, errors.New("interval editing needs an open timed task without repeats; all-day and recurring conversion is not supported")
	}
	if err := interval.Validate(); err != nil {
		return model.TaskEdit{}, err
	}
	return model.TaskEdit{StartDate: model.NewEditTime(interval.Start), DueDate: model.NewEditTime(interval.End), IsAllDay: model.Ptr(false), TimeZone: model.Ptr(interval.Zone)}, nil
}

func ProtectIntervalEnd(original model.Task, edit model.TaskEdit) (model.TaskEdit, error) {
	if !EditableInterval(original) || edit.DueDate == nil || edit.DueDate.Time.IsZero() {
		return edit, nil
	}
	if edit.IsAllDay != nil && *edit.IsAllDay {
		return edit, errors.New("a date-only end would convert this interval to all-day; clear its dates explicitly first")
	}
	interval := dates.Interval{Start: original.StartDate, End: edit.DueDate.Time, Zone: original.TimeZone}
	if err := interval.Validate(); err != nil {
		return edit, err
	}
	edit.StartDate = model.NewEditTime(interval.Start)
	edit.IsAllDay = model.Ptr(false)
	edit.TimeZone = model.Ptr(interval.Zone)
	return edit, nil
}
