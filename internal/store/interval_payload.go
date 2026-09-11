package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func intervalRoot(task *model.Task, edit *model.TaskEdit) (dates.Interval, error) {
	var interval dates.Interval
	if task != nil {
		if task.IsAllDay || task.RepeatFlag != "" {
			return interval, errors.New("not a non-recurring timed interval")
		}
		interval = dates.Interval{Start: task.StartDate, End: task.DueDate, Zone: task.TimeZone}
	} else if edit != nil && edit.StartDate != nil && edit.DueDate != nil && edit.IsAllDay != nil && !*edit.IsAllDay && edit.TimeZone != nil {
		if edit.RepeatFlag != nil && *edit.RepeatFlag != "" {
			return interval, errors.New("recurring interval writes are unsupported")
		}
		interval = dates.Interval{Start: edit.StartDate.Time, End: edit.DueDate.Time, Zone: *edit.TimeZone}
	} else {
		return interval, errors.New("interval write requires both dates, false all-day and timezone")
	}
	return interval, interval.Validate()
}

func confirmIntervalRoot(root map[string]json.RawMessage, interval dates.Interval) error {
	for _, value := range []struct {
		name string
		want model.Time
	}{{"startDate", interval.Start}, {"dueDate", interval.End}} {
		var text string
		raw, ok := root[value.name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &text) != nil {
			return fmt.Errorf("interval confirmation has no explicit %s", value.name)
		}
		got, err := model.ParseTime(text)
		if err != nil || !got.Equal(value.want.Time) {
			return fmt.Errorf("server did not confirm interval %s", value.name)
		}
	}
	var allDay bool
	raw, ok := root["isAllDay"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &allDay) != nil || allDay {
		return errors.New("server did not explicitly confirm timed interval")
	}
	return exactRawString(root, "timeZone", interval.Zone)
}
