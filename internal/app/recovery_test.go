package app

import (
	"context"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestRecoveryUsesNoClientAndMoveNeverStillRefuses(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	created, err := a.Create(ctx, Draft{ProjectID: "work", Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE outbox SET state='failed',op=?,payload='{}'`, store.OpTaskComplete); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.FocusUpload.Enabled = true
	cfg.Sync.MoveByRecreate = config.MoveNever
	a = NewActions(st, cfg, func() (*api.Client, error) { t.Fatal("recovery requested a network client"); return nil, nil })
	q, err := a.RecoveryQueue(ctx)
	if err != nil || len(q.Tasks) != 1 {
		t.Fatal(q, err)
	}
	if err := a.RetryQueueEntry(ctx, q.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PreviewTaskOperation(ctx, created.Task, "home"); err == nil {
		t.Fatal("never move setting bypassed")
	}
	current, err := st.Task(ctx, created.Task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PreviewTaskOperation(ctx, current, ""); err != nil {
		t.Fatal("delete incorrectly depends on move setting", err)
	}
}

func TestActionsNeverFallbackToRecreateWithoutNativePreview(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	task := model.Task{Id: "remote-task", ProjectId: "work", Title: "Keep identity"}
	if _, err := st.SyncProject(ctx, "work", []store.ServerTask{{Task: task}}); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.PreviewTaskOperation(ctx, task, "home")
	if err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	if _, err := a.ApplyTaskOperation(ctx, legacy); err == nil {
		t.Fatal("missing native contract fell into recreation")
	}
	if after := dumpCache(t, st); after != before {
		t.Fatal("refused fallback changed cache or queue")
	}
	preview, err := a.PreviewTaskOperation(ctx, task, "home")
	if err != nil || preview.Native == nil {
		t.Fatalf("native preview unavailable: %+v %v", preview, err)
	}
	out, err := a.ApplyTaskOperation(ctx, preview)
	if err != nil || out.Task.Id != task.Id {
		t.Fatalf("native identity not retained: %+v %v", out, err)
	}
}
