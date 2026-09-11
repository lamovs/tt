package schedule

import (
	"testing"
	"time"
)

func TestReminderTime(t *testing.T) {
	due := time.Date(2026, 9, 11, 12, 30, 0, 0, time.FixedZone("test", 3*60*60))
	tests := []struct {
		trigger string
		want    time.Time
		ok      bool
	}{
		{"TRIGGER:PT0S", due, true},
		{"TRIGGER:-PT10M", due.Add(-10 * time.Minute), true},
		{"TRIGGER:-PT1H", due.Add(-time.Hour), true},
		{"TRIGGER:-PT1H30M", due.Add(-90 * time.Minute), true},
		{"TRIGGER:-PT60M", time.Time{}, false},
		{"TRIGGER:-PT0M", time.Time{}, false},
		{"TRIGGER:vendor-extension", time.Time{}, false},
		{"TRIGGER:-P1D", time.Time{}, false},
	}
	for _, test := range tests {
		got, ok := ReminderTime(test.trigger, due)
		if ok != test.ok || ok && !got.Equal(test.want) {
			t.Errorf("ReminderTime(%q) = %v, %v; want %v, %v", test.trigger, got, ok, test.want, test.ok)
		}
	}
}
