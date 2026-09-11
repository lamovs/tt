package sync

import (
	"fmt"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
)

func confirmScheduleEdit(got api.Task, edit model.TaskEdit) error {
	for _, field := range []struct {
		name string
		want *model.EditTime
		got  string
	}{
		{"startDate", edit.StartDate, got.StartDate},
		{"dueDate", edit.DueDate, got.DueDate},
	} {
		if field.want == nil {
			continue
		}
		value, err := model.ParseTime(field.got)
		if err != nil || !value.Equal(field.want.Time.Time) {
			return fmt.Errorf("server did not confirm the requested %s", field.name)
		}
	}
	if (edit.StartDate != nil || edit.DueDate != nil) && edit.IsAllDay != nil && got.IsAllDay != *edit.IsAllDay {
		return fmt.Errorf("server did not confirm the requested isAllDay")
	}
	return nil
}
