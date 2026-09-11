package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func actionStore(t *testing.T) (*Actions, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ReplaceProjects(context.Background(), []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "Home"}}); err != nil {
		t.Fatal(err)
	}
	return NewActions(st, config.Default(), nil), st
}
func draftOf(task model.Task) Draft {
	return Draft{ProjectID: task.ProjectId, Title: task.Title, Body: task.Content, Priority: task.Priority}
}
func readTask(t *testing.T, st *store.Store, id string) model.Task {
	t.Helper()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestEverydayMutationsAreAtomicAndPreserveUntouchedFields(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	empty := dumpCache(t, st)
	for _, draft := range []Draft{{ProjectID: "work"}, {ProjectID: "work", Title: "x", Priority: 2}, {ProjectID: "work", Title: "x\nother"}} {
		if _, err := a.Create(ctx, draft); err == nil {
			t.Fatal("invalid create accepted")
		}
		if dumpCache(t, st) != empty {
			t.Fatal("invalid create wrote data")
		}
	}
	created, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Target", Body: "  body\nnext\n", Priority: model.PriorityLow})
	if err != nil || !created.Changed {
		t.Fatalf("create: %+v %v", created, err)
	}
	var outbox, undo, events int
	for table, dst := range map[string]*int{"outbox": &outbox, "undo_log": &undo, "events": &events} {
		if err := st.DB().QueryRow("SELECT count(*) FROM " + table).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if outbox != 1 || undo != 1 || events != 1 {
		t.Fatalf("create was not one atomic operation: %d %d %d", outbox, undo, events)
	}
	task := created.Task
	_, err = st.UpdateTaskOutcome(ctx, task.Id, model.TaskEdit{RepeatFlag: model.Ptr("RRULE:FREQ=WEEKLY"), Reminders: model.NewEditList([]string{"TRIGGER:PT0S", "TRIGGER:-PT5M"}), Tags: model.NewEditList([]string{"tag"})})
	if err != nil {
		t.Fatal(err)
	}
	task = readTask(t, st, task.Id)
	if err := st.SetListing(ctx, []string{task.Id}); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	same, err := a.Edit(ctx, task, draftOf(task))
	if err != nil || same.Changed || dumpCache(t, st) != before {
		t.Fatalf("unchanged edit wrote: %+v %v", same, err)
	}
	draft := draftOf(task)
	draft.Title = ""
	if _, err = a.Edit(ctx, task, draft); err == nil || dumpCache(t, st) != before {
		t.Fatal("invalid edit changed data")
	}
	draft.Title = "Changed"
	draft.Body = ""
	draft.Priority = model.PriorityHigh
	changed, err := a.Edit(ctx, task, draft)
	if err != nil || !changed.Changed {
		t.Fatalf("edit: %+v %v", changed, err)
	}
	want := task
	want.Title = draft.Title
	want.Content = ""
	want.Priority = draft.Priority
	want.ModifiedTime = changed.Task.ModifiedTime
	if !reflect.DeepEqual(changed.Task, want) {
		t.Fatalf("untouched fields changed: got %+v want %+v", changed.Task, want)
	}
	refs, err := st.ResolveRefs(ctx, []string{"1"})
	if err != nil || len(refs) != 1 || refs[0] != task.Id {
		t.Fatalf("listing changed: %v %v", refs, err)
	}
	state, err := a.State(ctx, []string{task.Id})
	if err != nil || state.Queue.Pending != 3 {
		t.Fatalf("queue status: %+v %v", state, err)
	}
}

func TestEverydayConcurrentEditsAndCompletionAreRefused(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Checklist", Items: []model.Item{{Title: "one"}, {Title: "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	task = readTask(t, st, task.Id)
	_, err = st.UpdateTask(ctx, task.Id, model.TaskEdit{Title: model.Ptr("CLI edit")})
	if err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	draft := draftOf(task)
	draft.Body = "TUI edit"
	if _, err := a.Edit(ctx, task, draft); !errors.Is(err, store.ErrTaskChanged) {
		t.Fatalf("lost edit: %v", err)
	}
	if _, err := a.Complete(ctx, task, false); !errors.Is(err, store.ErrTaskChanged) {
		t.Fatalf("lost completion: %v", err)
	}
	if before != dumpCache(t, st) {
		t.Fatal("conflicts wrote data")
	}
	task = readTask(t, st, task.Id)
	completed, err := a.Complete(ctx, task, true)
	if err != nil || !completed.Task.Status.Done() || completed.Task.Items[0].Status.Done() {
		t.Fatalf("keep policy: %+v %v", completed, err)
	}
	before = dumpCache(t, st)
	same, err := a.Complete(ctx, readTask(t, st, completed.Task.Id), false)
	if err != nil || same.Changed || before != dumpCache(t, st) {
		t.Fatalf("already completed task changed: changed=%v err=%v", same.Changed, err)
	}
	preview, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Undo(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	task = readTask(t, st, task.Id)
	completed, err = a.Complete(ctx, task, false)
	if err != nil || !completed.Task.Status.Done() {
		t.Fatalf("default completion: %v", err)
	}
	for i, item := range completed.Task.Items {
		if !item.Status.Done() || item.Key != task.Items[i].Key {
			t.Fatal("completion lost item policy or key")
		}
	}
	preview, err = a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Undo(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	restored := readTask(t, st, task.Id)
	if restored.Status.Done() || !reflect.DeepEqual(restored.Items, task.Items) {
		t.Fatal("undo failed to restore checklist")
	}
}

func TestGlobalUndoRefusesChangedHistoryAndTracksExactPromotion(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	first, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Same"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Create(ctx, Draft{ProjectID: "home", Title: "Same"})
	if err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	if _, err = a.Undo(ctx, preview); !errors.Is(err, store.ErrUndoConflict) || before != dumpCache(t, st) {
		t.Fatalf("undo consumed different top: %v", err)
	}
	preview, err = a.PreviewUndo(ctx)
	if err != nil || preview.Entry.Action.TaskID != second.Task.Id {
		t.Fatalf("wrong global target: %+v %v", preview, err)
	}
	if _, err = a.Undo(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Task(ctx, first.Task.Id); err != nil {
		t.Fatal("undo removed wrong same-title task")
	}
	if err = st.ReplaceLocalID(ctx, first.Task.Id, "remote-first"); err != nil {
		t.Fatal(err)
	}
	before = dumpCache(t, st)
	state, err := a.State(ctx, []string{first.Task.Id, second.Task.Id, "remote-first"})
	if err != nil || state.Replacements[first.Task.Id] != "remote-first" || len(state.Replacements) != 1 {
		t.Fatalf("promotion: %+v %v", state, err)
	}
	if before != dumpCache(t, st) {
		t.Fatal("status read wrote data")
	}
}
