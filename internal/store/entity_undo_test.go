package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestEntityGlobalUndoUsesExistingTaskHistory(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p", Name: "P"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Earlier task"})
	if err != nil {
		t.Fatal(err)
	}
	out := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Folder"}`)})
	undo, err := st.LastUndo(ctx)
	if err != nil || undo.Action.EntityRef == nil || *undo.Action.EntityRef != out.Entity.Ref || undo.Action.OperationSeq != out.OperationSeq || undo.Feature != nil {
		t.Fatalf("wrong shared undo: %+v %v", undo, err)
	}
	if _, err := st.ApplyUndo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Entity(ctx, out.Entity.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create not canceled: %v", err)
	}
	previous, err := st.LastUndo(ctx)
	if err != nil || previous.Action.TaskID != task.Id {
		t.Fatalf("earlier task history lost: %+v %v", previous, err)
	}
}

func TestEntityGlobalUndoRestoresLatestBaseAndCancelsDelete(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ref := entityFixture(t, st, "folder", "g", `{"id":"g","name":"Before","future":1}`)
	entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"Local"}`)})
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entityFixture(t, st, "folder", "g", `{"id":"g","name":"Remote now","future":9007199254740993}`)
	if _, err := st.ApplyUndo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	entity, err := st.Entity(ctx, ref)
	if err != nil || entity.Dirty || !strings.Contains(string(entity.Data), "Remote now") || !strings.Contains(string(entity.Data), "9007199254740993") {
		t.Fatalf("latest snapshot lost: %+v %v", entity, err)
	}
	entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "delete", Patch: json.RawMessage(`{}`)})
	undo, err = st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	entity, err = st.Entity(ctx, ref)
	if err != nil || entity.Deleted || entity.Dirty {
		t.Fatalf("delete not canceled: %+v %v", entity, err)
	}
}

func TestEntityGlobalUndoRetainsSentAndConfirmedHistory(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	out := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Folder"}`)})
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := st.ArmEntityOperation(ctx, out.OperationSeq, claimed[0].Item.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := st.FailEntityOperation(ctx, out.OperationSeq, claimed[0].Item.LeaseToken, "lost response", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("uncertain resource reversed: %v", err)
	}
	current, err := st.LastUndo(ctx)
	if err != nil || current.Seq != undo.Seq {
		t.Fatal("uncertain refusal consumed history")
	}
	operations, err := st.EntityOperationSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RetryEntityOperation(ctx, out.OperationSeq, operations[0].Revision); err != nil {
		t.Fatal(err)
	}
	claimed, err = st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("recovery: %+v %v", claimed, err)
	}
	if err := st.ConfirmEntityOperation(ctx, out.OperationSeq, claimed[0].Item.LeaseToken, "g", json.RawMessage(`{"id":"g","name":"Folder"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); !errors.Is(err, ErrEntityUndoUnavailable) {
		t.Fatalf("confirmed resource reversed: %v", err)
	}
	current, err = st.LastUndo(ctx)
	if err != nil || current.Seq != undo.Seq {
		t.Fatal("confirmed refusal consumed history")
	}
}

func TestEntityCancelRemovesOnlyMatchingUndoAndStaleUndoRefuses(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	first := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"First"}`)})
	stale, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Second"}`)})
	if _, err := st.ApplyUndo(ctx, stale); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("stale preview reversed new history: %v", err)
	}
	if err := st.CancelEntityOperation(ctx, first.OperationSeq, first.Entity.Revision); err != nil {
		t.Fatal(err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil || undo.Action.OperationSeq != second.OperationSeq {
		t.Fatalf("direct cancel removed unrelated history: %+v %v", undo, err)
	}
	if _, err := st.ApplyUndo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("canceled resource left stale history: %v", err)
	}
}
