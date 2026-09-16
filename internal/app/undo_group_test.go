package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/movsar/tt/internal/store"
)

func TestActionsPreviewAndUndoAGroupTogether(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	base, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Sample base"})
	if err != nil {
		t.Fatal(err)
	}
	ungrouped, err := a.PreviewUndo(ctx)
	if err != nil || ungrouped.Group != nil || ungrouped.Title != "Sample base" {
		t.Fatalf("ungrouped preview = %+v, %v; want it without a group", ungrouped, err)
	}

	grouped := store.WithUndoGroup(ctx, "group-app")
	added, err := a.Create(grouped, Draft{ProjectID: "home", Title: "Sample added"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Complete(grouped, readTask(t, st, base.Task.Id), false); err != nil {
		t.Fatal(err)
	}

	preview, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Group) != 2 || preview.Entry.Seq != preview.Group[0].Entry.Seq || preview.Title != "Sample base" {
		t.Fatalf("group preview = %+v, want the completion on top of a group of two", preview)
	}
	if got := preview.Group[1]; got.Title != "Sample added" || got.Entry.Action.TaskID != added.Task.Id || got.Group != nil {
		t.Fatalf("second group item = %+v, want the add", got)
	}

	out, err := a.Undo(ctx, preview)
	if err != nil || !out.Changed || out.Task.Id != base.Task.Id {
		t.Fatalf("group undo = %+v, %v", out, err)
	}
	if restored := readTask(t, st, base.Task.Id); restored.Status.Done() {
		t.Fatal("the completion in the group was not reversed")
	}
	if _, err := st.Task(ctx, added.Task.Id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the add in the group was not reversed: %v", err)
	}
	next, err := a.PreviewUndo(ctx)
	if err != nil || next.Group != nil || next.Entry.Action.TaskID != base.Task.Id || next.Entry.Action.Op != store.OpTaskCreate {
		t.Fatalf("preview after the group = %+v, %v; want the ungrouped add before it", next, err)
	}
}

func TestActionsGroupPreviewDoesNotRefuseWhatTheGroupItselfClears(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	grouped := store.WithUndoGroup(ctx, "group-entity")
	create, err := st.PreviewEntityMutation(grouped, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Sample folder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.ApplyEntityMutation(grouped, create)
	if err != nil {
		t.Fatal(err)
	}
	rename, err := st.PreviewEntityMutation(grouped, store.EntityMutation{Ref: created.Entity.Ref, Action: "update", Patch: json.RawMessage(`{"name":"Sample folder renamed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyEntityMutation(grouped, rename); err != nil {
		t.Fatal(err)
	}
	if err := st.CancelEntityOperation(ctx, created.OperationSeq, created.Entity.Revision); !errors.Is(err, store.ErrEntityDependency) {
		t.Fatalf("cancelling the create on its own = %v, want ErrEntityDependency while the rename is queued", err)
	}

	preview, err := a.PreviewUndo(ctx)
	if err != nil {
		t.Fatalf("group preview refused a record the newer one in the group depends on: %v", err)
	}
	if len(preview.Group) != 2 || preview.Group[0].Title != "Sample folder renamed" || preview.Group[1].Title != "Sample folder renamed" {
		t.Fatalf("group preview = %+v", preview)
	}
	for _, item := range preview.Group {
		if item.Entry.Action.EntityRef == nil || *item.Entry.Action.EntityRef != created.Entity.Ref {
			t.Fatalf("group item = %+v, want the folder", item)
		}
	}
	if _, err := a.Undo(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Entity(ctx, created.Entity.Ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the folder is still cached: %v", err)
	}
	if _, err := a.PreviewUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Fatalf("stack after the group = %v, want it empty", err)
	}
}

func TestActionsGroupUndoRefusesAPreviewTheStackOutgrew(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	grouped := store.WithUndoGroup(ctx, "group-stale")
	if _, err := a.Create(grouped, Draft{ProjectID: "work", Title: "Sample one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(grouped, Draft{ProjectID: "work", Title: "Sample two"}); err != nil {
		t.Fatal(err)
	}
	preview, err := a.PreviewUndo(ctx)
	if err != nil || len(preview.Group) != 2 {
		t.Fatalf("group preview = %+v, %v", preview, err)
	}
	if _, err := a.Create(grouped, Draft{ProjectID: "work", Title: "Sample three"}); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	if out, err := a.Undo(ctx, preview); !errors.Is(err, store.ErrUndoConflict) || out.Changed {
		t.Fatalf("stale group undo = %+v, %v; want ErrUndoConflict", out, err)
	}
	if dumpCache(t, st) != before {
		t.Fatal("a refused group undo wrote data")
	}
	withGroup := UndoPreview{Entry: preview.Entry, Title: preview.Title}
	if _, err := a.Undo(ctx, withGroup); !errors.Is(err, store.ErrUndoConflict) {
		t.Fatalf("single undo under a grown group = %v, want ErrUndoConflict", err)
	}
	fresh, err := a.PreviewUndo(ctx)
	if err != nil || len(fresh.Group) != 3 {
		t.Fatalf("fresh preview = %+v, %v", fresh, err)
	}
	if _, err := a.Undo(ctx, UndoPreview{Entry: fresh.Entry, Title: fresh.Title}); !errors.Is(err, store.ErrUndoGrouped) {
		t.Fatalf("undo of one record of a group = %v, want ErrUndoGrouped", err)
	}
	if dumpCache(t, st) != before {
		t.Fatal("a refused undo wrote data")
	}
	if _, err := a.Undo(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PreviewUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Fatalf("stack after the group = %v, want it empty", err)
	}
}
