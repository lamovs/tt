package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestTaskColumnActionsShareLocalPreviewAndMutation(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	actions := NewActions(st, config.Default(), func() (*api.Client, error) { t.Fatal("column preview/apply requested the network"); return nil, nil })
	if err := st.MergeEntities(ctx, "column", "work", []store.ResourceEntity{{ServerID: "column", Data: json.RawMessage(`{"id":"column","projectId":"work","name":"Doing"}`)}}, false); err != nil {
		t.Fatal(err)
	}
	task := model.Task{Id: "task", ProjectId: "work", Title: "Task", Status: model.TaskOpen, SortOrder: 42}
	if _, err := st.SyncProject(ctx, "work", []store.ServerTask{{Task: task, Raw: json.RawMessage(`{"id":"task","projectId":"work","title":"Task","status":0}`)}}); err != nil {
		t.Fatal(err)
	}
	task = readTask(t, st, task.Id)
	preview, err := actions.PreviewTaskColumn(ctx, task, "column")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := actions.ApplyTaskColumn(ctx, preview)
	if err != nil || !outcome.Changed || outcome.Task.ColumnId != "column" || outcome.Task.Status != task.Status || outcome.Task.SortOrder != 42 {
		t.Fatalf("column action: %+v %v", outcome, err)
	}
	undo, err := actions.PreviewUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = actions.Undo(ctx, undo)
	if err != nil || outcome.Task.ColumnId != "" {
		t.Fatalf("local undo: %+v %v", outcome, err)
	}
}
