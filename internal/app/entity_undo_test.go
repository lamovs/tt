package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/movsar/tt/internal/store"
)

func TestActionsPreviewGlobalEntityUndo(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	preview, err := st.PreviewEntityMutation(ctx, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Folder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyEntityMutation(ctx, preview); err != nil {
		t.Fatal(err)
	}
	undo, err := a.PreviewUndo(ctx)
	if err != nil || undo.Title != "Folder" || undo.Entry.Action.EntityRef == nil {
		t.Fatalf("entity preview unavailable: %+v %v", undo, err)
	}
	out, err := a.Undo(ctx, undo)
	if err != nil || !out.Changed || out.Task.Id != "" {
		t.Fatalf("entity undo returned a fake task: %+v %v", out, err)
	}
}
