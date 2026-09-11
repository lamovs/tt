package schedule

import (
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestReminderInterpretationSeparatesComputationFromDelivery(t *testing.T) {
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	task := model.Task{StartDate: model.NewTime(start), DueDate: model.NewTime(start.Add(time.Hour)), TimeZone: "UTC", Reminders: []string{
		"TRIGGER:PT0S", "TRIGGER;RELATED=START:-PT30S", "TRIGGER;RELATED=END:-P1Y2M3W4D", "opaque",
	}}
	rows := InterpretReminders(task)
	if len(rows) != 4 {
		t.Fatal(rows)
	}
	if !rows[0].Computable || rows[0].Delivery != "legacy_allowlist" || !rows[0].At.Equal(task.DueDate.Time) {
		t.Fatal(rows[0])
	}
	if !rows[1].Computable || rows[1].Delivery != "provider_only" || !rows[1].At.Equal(start.Add(-30*time.Second)) {
		t.Fatal(rows[1])
	}
	if rows[2].Computable || rows[2].Parsed == nil || rows[2].Parsed.Years != 1 || rows[2].Reason != "calendar_months_unverified" {
		t.Fatal(rows[2])
	}
	if rows[3].Parsed != nil || rows[3].Raw != "opaque" {
		t.Fatal(rows[3])
	}
}
