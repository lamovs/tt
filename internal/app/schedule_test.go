package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func TestScheduleAtomicFieldsRawUndoAndConflicts(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Schedule", Content: "keep body", Items: []model.Item{{Title: "keep item"}}, StartDate: model.NewTime(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)), TimeZone: "Europe/Berlin", IsAllDay: true})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"unknown":{"literal":"keep  spaces"},"repeatFrom":"vendor","reminders":["unsupported","unsupported"]}`
	if _, err := st.DB().Exec("UPDATE tasks SET raw=? WHERE id=?", raw, created.Id); err != nil {
		t.Fatal(err)
	}
	original := readTask(t, st, created.Id)
	before := dumpCache(t, st)
	if _, err := a.Schedule(ctx, original, schedule.Change{Reminders: model.Ptr([]string{"TRIGGER:PT0S", "TRIGGER:PT0S"})}); err == nil || dumpCache(t, st) != before {
		t.Fatal("invalid list mutated cache")
	}
	date, err := dates.Parse("+2h", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rule := "RRULE:FREQ=WEEKLY;INTERVAL=1"
	reminders := []string{"TRIGGER:-PT10M", "TRIGGER:PT0S"}
	out, err := a.Schedule(ctx, original, schedule.Change{Due: &date, Repeat: &rule, Reminders: &reminders})
	if err != nil || !out.Changed {
		t.Fatalf("save: %+v %v", out, err)
	}
	want := original
	want.DueDate, want.TimeZone, want.IsAllDay, want.RepeatFlag, want.Reminders, want.ModifiedTime = date.Time, "", false, rule, reminders, out.Task.ModifiedTime
	if !reflect.DeepEqual(out.Task, want) {
		t.Fatalf("untouched fields changed: %+v\n%+v", out.Task, want)
	}
	var gotRaw string
	if err := st.DB().QueryRow("SELECT raw FROM tasks WHERE id=?", original.Id).Scan(&gotRaw); err != nil || gotRaw != raw {
		t.Fatalf("raw: %q %v", gotRaw, err)
	}
	current := readTask(t, st, original.Id)
	before = dumpCache(t, st)
	if out, err := a.Schedule(ctx, current, schedule.Change{Due: &date, Repeat: &rule, Reminders: &reminders}); err != nil || out.Changed || dumpCache(t, st) != before {
		t.Fatalf("no-op: %+v %v", out, err)
	}
	if _, err := a.Schedule(ctx, original, schedule.Change{Repeat: model.Ptr("")}); !errors.Is(err, store.ErrTaskChanged) || dumpCache(t, st) != before {
		t.Fatalf("conflict: %v", err)
	}
	preview, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Undo(ctx, preview); err != nil {
		t.Fatal(err)
	}
	current = readTask(t, st, original.Id)
	if !current.DueDate.Equal(original.DueDate.Time) || current.RepeatFlag != original.RepeatFlag || !reflect.DeepEqual(current.Items, original.Items) {
		t.Fatalf("undo did not restore schedule: %+v", current)
	}
	clear := dates.Result{Clear: true}
	out, err = a.Schedule(ctx, current, schedule.Change{Due: &clear})
	if err != nil || !out.Task.StartDate.IsZero() || !out.Task.DueDate.IsZero() || out.Task.IsAllDay || out.Task.TimeZone != original.TimeZone {
		t.Fatalf("clear: %+v %v", out, err)
	}
}
