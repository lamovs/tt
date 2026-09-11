package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestTimerReadDoesNotReconcileAndGuardIgnoresObservation(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	start, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ReadTimer(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	before := timerTables(t, st)
	later, err := st.ReadTimer(ctx, now.Add(time.Hour))
	if err != nil || later.State == nil || later.State.SessionID != start.State.SessionID {
		t.Fatalf("read %+v %v", later, err)
	}
	if !reflect.DeepEqual(before, timerTables(t, st)) || snap.Guard != later.Guard {
		t.Fatal("read reconciled or changed version")
	}
	if _, err = st.TimerStatus(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := st.ControlTimer(ctx, "pause", TimerStartOptions{}, &snap.Guard, now.Add(2*time.Second))
	if err != nil || result.State == nil || result.State.PausedAt == nil {
		t.Fatalf("observation invalidated preview %+v %v", result, err)
	}
}

func TestTimerGuardRejectsABAReplacementAndTaskEdits(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	seedProjects(t, st, model.Project{Id: "work", Name: "Work"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Frozen title"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ReadTimer(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := st.GuardTimerTask(ctx, snapshot.Guard, task.Id)
	if err != nil || guard.TaskTitle != "Frozen title" {
		t.Fatalf("guard %+v %v", guard, err)
	}
	if _, err = st.UpdateTask(ctx, task.Id, model.TaskEdit{Title: model.Ptr("CLI title")}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "start", TimerStartOptions{FocusType: 1, TaskID: task.Id}, &guard, now); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("task conflict %v", err)
	}
	first, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ = st.ReadTimer(ctx, now)

	if _, err = st.PauseTimer(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ResumeTimer(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "stop", TimerStartOptions{}, &snapshot.Guard, now); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("ABA accepted %v", err)
	}
	snapshot, _ = st.ReadTimer(ctx, now)
	if _, err = st.StopTimer(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "cancel", TimerStartOptions{}, &snapshot.Guard, now.Add(3*time.Second)); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("replacement accepted %v", err)
	}
	active, err := st.ReadTimer(ctx, now.Add(3*time.Second))
	if err != nil || active.State.SessionID != second.State.SessionID || active.State.SessionID == first.State.SessionID {
		t.Fatalf("new session changed %+v %v", active, err)
	}
}

func TestTimerGuardTaskPromotionRequiresFreshPreview(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	seedProjects(t, st, model.Project{Id: "p", Name: "P"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.StartTimer(ctx, TimerStartOptions{FocusType: 1, TaskID: task.Id}, now); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := st.ReadTimer(ctx, now)
	if err = st.ReplaceLocalID(ctx, task.Id, "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "stop", TimerStartOptions{}, &snapshot.Guard, now.Add(time.Second)); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("promoted task silently retargeted %v", err)
	}
}

func TestTimerStartGuardCannotAuthorizeAnotherTask(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	seedProjects(t, st, model.Project{Id: "p", Name: "P"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ReadTimer(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "start", TimerStartOptions{FocusType: 1, TaskID: task.Id}, &snapshot.Guard, now); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("unbound target authorized: %v", err)
	}
	bound, err := st.GuardTimerTask(ctx, snapshot.Guard, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ControlTimer(ctx, "start", TimerStartOptions{FocusType: 1}, &bound, now); !errors.Is(err, ErrTimerChanged) {
		t.Fatalf("silently anonymous: %v", err)
	}
}

func TestTimerCancelAfterDeadlinePreservesCompletedOutcome(t *testing.T) {
	ctx, st, now := context.Background(), testStore(t), timerTestNow()
	if _, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ReadTimer(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := st.ControlTimer(ctx, "cancel", TimerStartOptions{}, &snapshot.Guard, now.Add(2*time.Second))
	if err != nil || result.Completed == nil || result.Completed.Outcome != "done" || result.Completed.ActiveDuration != time.Second {
		t.Fatalf("late cancel %+v %v", result, err)
	}
}
