package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestDueClearRemovesWholeScheduleAndUndoRestoresIt(t *testing.T) {
	isolate(t)
	start := model.NewTime(time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC))
	due := model.NewTime(start.Add(2 * time.Hour))
	id := seedFeatureCommandTask(t, model.Task{Title: "Schedule", StartDate: start, DueDate: due, IsAllDay: true, TimeZone: "Europe/Moscow"})
	before := readFeatureTask(t, id)
	if code, _, err := runFeatureCommand(t, "due", id, "none"); code != exitOK {
		t.Fatalf("clear: %d %s", code, err)
	}
	got := readFeatureTask(t, id)
	if !got.StartDate.IsZero() || !got.DueDate.IsZero() || got.IsAllDay {
		t.Fatalf("schedule remains: %+v", got)
	}
	state := featureCommandState(t, id)
	if code, _, err := runFeatureCommand(t, "due", id, "none"); code != exitOK {
		t.Fatalf("repeat clear: %d %s", code, err)
	}
	if !reflect.DeepEqual(state, featureCommandState(t, id)) {
		t.Fatal("repeated clear changed state")
	}
	if code, _, err := runFeatureCommand(t, "undo"); code != exitOK {
		t.Fatalf("undo: %d %s", code, err)
	}
	got = readFeatureTask(t, id)
	if !got.StartDate.Equal(before.StartDate.Time) || !got.DueDate.Equal(before.DueDate.Time) || got.IsAllDay != before.IsAllDay || got.TimeZone != before.TimeZone {
		t.Fatalf("undo lost schedule: %+v", got)
	}
}
