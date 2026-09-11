package schedule

import (
	"time"

	"github.com/movsar/tt/internal/model"
)

type ReminderInterpretation struct {
	Raw        string    `json:"raw"`
	Parsed     *Trigger  `json:"parsed,omitempty"`
	At         time.Time `json:"at,omitzero"`
	Computable bool      `json:"computable"`
	Delivery   string    `json:"delivery"`
	Reason     string    `json:"reason,omitempty"`
}

func InterpretReminders(task model.Task) []ReminderInterpretation {
	result := make([]ReminderInterpretation, 0, len(task.Reminders))
	for _, raw := range task.Reminders {
		item := ReminderInterpretation{Raw: raw, Delivery: "provider_only"}
		trigger, err := ParseTrigger(raw)
		if err != nil {
			item.Reason = "unsupported_trigger"
			result = append(result, item)
			continue
		}
		item.Parsed = &trigger
		if at, ok := ReminderTime(raw, task.DueDate.Time); ok && !task.DueDate.IsZero() && !task.IsAllDay {
			item.At, item.Computable, item.Delivery, item.Reason = at, true, "legacy_allowlist", "server-confirmed tasks remain provider-owned"
			result = append(result, item)
			continue
		}
		value := EvaluateTrigger(raw, TriggerDates{Start: task.StartDate.Time, End: task.DueDate.Time, AllDay: task.IsAllDay, TimeZone: task.TimeZone}, TriggerPolicy{})
		item.At, item.Computable, item.Reason = value.At, value.Supported, value.Reason
		if !item.Computable {
			item.Delivery = "provider_only"
		} else {
			item.Reason = "local_delivery_not_verified"
		}
		result = append(result, item)
	}
	return result
}
