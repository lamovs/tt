package store

import (
	"context"
	"errors"
	"github.com/movsar/tt/internal/schedule"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func intervalTask(t *testing.T, title, project, expression string) model.Task {
	t.Helper()
	i, err := dates.ParseInterval(expression, "UTC", time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return model.Task{Title: title, ProjectId: project, StartDate: i.Start, DueDate: i.End, TimeZone: i.Zone}
}

func TestIntervalOverlapGeometryContextAndUndo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "a", Name: "A"}, model.Project{Id: "b", Name: "B"}, model.Project{Id: "closed", Name: "Closed", Closed: true})
	target, err := s.CreateTask(ctx, intervalTask(t, "target", "a", "14:00 + 90min"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		title, expr          string
		allDay, repeat, done bool
		project              string
	}{
		{"touch left", "13:00 + 1h", false, false, false, "b"},
		{"touch right", "15:30 + 1h", false, false, false, "b"},
		{"inside", "14:15 + 10min", false, false, false, "b"},
		{"recurring", "14:30 + 1h", false, true, false, "b"},
		{"all-day", "14:00 + 1h", true, false, false, "b"},
		{"done", "14:00 + 1h", false, false, true, "b"},
		{"unavailable", "14:00 + 1h", false, false, false, "closed"},
	} {
		v := intervalTask(t, tc.title, tc.project, tc.expr)
		v.IsAllDay = tc.allDay
		if tc.repeat {
			v.RepeatFlag = "RRULE:FREQ=DAILY"
		}
		if tc.done {
			v.Status = model.TaskDone
		}
		if _, err := s.CreateTask(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	fraction := intervalTask(t, "fraction", "b", "15:29 + 1min")
	fraction.StartDate = model.NewTime(fraction.StartDate.Add(59*time.Second + 500*time.Millisecond))
	if _, err := s.CreateTask(ctx, fraction); err != nil {
		t.Fatal(err)
	}
	partial := intervalTask(t, "partial", "b", "14:00 + 1h")
	partial.StartDate = model.Time{}
	if _, err := s.CreateTask(ctx, partial); err != nil {
		t.Fatal(err)
	}
	target, err = s.Task(ctx, target.Id)
	if err != nil {
		t.Fatal(err)
	}
	i := dates.Interval{Start: target.StartDate, End: target.DueDate, Zone: target.TimeZone}
	edit, err := schedule.IntervalEdit(target, i)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PrepareInterval(ctx, &target, edit, model.Task{})
	if err != nil || len(p.Overlaps) != 3 {
		t.Fatalf("overlaps=%+v %v", p.Overlaps, err)
	}
	if p.Overlaps[2].End.Sub(p.Overlaps[2].Start.Time) != 500*time.Millisecond {
		t.Fatal("fraction rounded away")
	}
	text := strings.Join(p.Lines(), "\n")
	for _, want := range []string{"All-day context", "Future repeats unchecked", "Point/partial", "Excluded list context", "500ms", "Cache last sync: unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s", want)
		}
	}
	out, err := s.ApplyInterval(ctx, p, true)
	if err != nil || out.Changed {
		t.Fatalf("noop=%+v %v", out, err)
	}
	i.End = model.NewTime(i.End.Add(time.Hour))
	edit, err = schedule.IntervalEdit(target, i)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.PrepareInterval(ctx, &target, edit, model.Task{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyInterval(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	u, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyUndo(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, target.Id)
	if err != nil || !got.StartDate.Equal(target.StartDate.Time) || !got.DueDate.Equal(target.DueDate.Time) || got.TimeZone != target.TimeZone {
		t.Fatalf("undo=%+v %v", got, err)
	}
}

func TestIntervalConcurrentPreviewOneCommit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "a", Name: "A"})
	input := intervalTask(t, "same", "a", "14:00 + 90min")
	p, err := s.PrepareInterval(ctx, nil, model.TaskEdit{}, input)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := s.ApplyInterval(ctx, p, true); results <- err }()
	}
	success, stale := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrIntervalChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
}

func TestIntervalContainingAcrossListsAndPhantomGuard(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "a", Name: "A"}, model.Project{Id: "b", Name: "B"})
	input := intervalTask(t, "new", "a", "14:00 + 90min")
	p, err := s.PrepareInterval(ctx, nil, model.TaskEdit{}, input)
	if err != nil || len(p.Overlaps) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	other, err := s.CreateTask(ctx, intervalTask(t, "covers", "b", "13:00 + 4h"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyInterval(ctx, p, true); !errors.Is(err, ErrIntervalChanged) {
		t.Fatalf("phantom not rejected: %v", err)
	}
	p, err = s.PrepareInterval(ctx, nil, model.TaskEdit{}, input)
	if err != nil || len(p.Overlaps) != 1 || p.Overlaps[0].Task.Id != other.Id || p.Overlaps[0].End.Sub(p.Overlaps[0].Start.Time) != 90*time.Minute {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := s.ApplyInterval(ctx, p, false); !errors.Is(err, ErrIntervalOverlap) {
		t.Fatalf("missing consent: %v", err)
	}
	out, err := s.ApplyInterval(ctx, p, true)
	if err != nil || !out.Changed {
		t.Fatalf("save: %+v %v", out, err)
	}
	entry, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyUndo(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, out.Task.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undo did not remove create: %v", err)
	}
}
