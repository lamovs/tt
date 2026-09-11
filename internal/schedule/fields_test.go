package schedule

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func TestFriendlyRemindersKeepOrderRawAndRejectDuplicates(t *testing.T) {
	got, err := ParseReminders([]string{"-10min", "at", "-1h30min", "TRIGGER:vendor-extension "})
	want := []string{"TRIGGER:-PT10M", "TRIGGER:PT0S", "TRIGGER:-PT1H30M", "TRIGGER:vendor-extension "}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%q %v", got, err)
	}
	for _, values := range [][]string{{"at", "TRIGGER:PT0S"}, {"-1h", "-60min"}, {"at", "at"}, {"none", "at"}, {"-0min"}, {"TRIGGER:"}, {"TRIGGER:x\n"}, {"-10min\n"}} {
		if _, err := ParseReminders(values); err == nil {
			t.Errorf("accepted %q", values)
		}
	}
	got, err = ParseReminders([]string{"none"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("clear: %q %v", got, err)
	}
}

func TestScheduleMasksKeepUnchangedUnsupportedValues(t *testing.T) {
	original := model.Task{RepeatFlag: "unknown repeat", Reminders: []string{"unknown", "unknown"}, TimeZone: "Pacific/Kiritimati", IsAllDay: true}
	change := Change{Repeat: &original.RepeatFlag, Reminders: &original.Reminders}
	edit, err := change.Edit(original)
	if err != nil || !reflect.DeepEqual(edit, model.TaskEdit{}) {
		t.Fatalf("untouched unsupported: %+v %v", edit, err)
	}
	clear := dates.Result{Clear: true}
	change.Due = &clear
	edit, err = change.Edit(original)
	if err != nil || edit.StartDate == nil || edit.DueDate == nil || edit.IsAllDay == nil || *edit.IsAllDay || edit.TimeZone != nil || edit.RepeatFlag != nil || edit.Reminders != nil {
		t.Fatalf("clear mask: %+v %v", edit, err)
	}
	if _, err := ReminderDate("tmr", time.Now()); err == nil {
		t.Fatal("all-day shortcut accepted")
	}
	if _, err := ReminderDate("none", time.Now()); err == nil {
		t.Fatal("clear shortcut accepted")
	}
	if got, err := ReminderDate("18:00", time.Now()); err != nil || got.AllDay {
		t.Fatalf("bare clock: %+v %v", got, err)
	}
}

func TestFriendlyDisplayOnlyShortensLosslessValues(t *testing.T) {
	for _, value := range []string{"TRIGGER:PT0S", "TRIGGER:-PT1H30M", "TRIGGER:-PT10M", "TRIGGER:-PT60M", "TRIGGER:PT00S", "TRIGGER:-PT1M ", "unknown"} {
		input := ReminderInput(value)
		if input != value {
			parsed, err := ParseReminder(input)
			if err != nil || parsed != value {
				t.Fatalf("lossy display %q -> %q -> %q", value, input, parsed)
			}
		}
	}
}
