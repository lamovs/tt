package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func nativeMoveFixture(t *testing.T, st *Store) model.Task {
	t.Helper()
	ctx := context.Background()
	seedProjects(t, st, model.Project{Id: "source", Name: "Source"}, model.Project{Id: "target", Name: "Target"})
	task := model.Task{Id: "task", ProjectId: "source", Title: "Task"}
	if _, err := st.SyncProject(ctx, "source", []ServerTask{{Task: task, Raw: json.RawMessage(`{"id":"task","projectId":"source","title":"Task","unknown":{"retained":true}}`)}}); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestNativeMoveStableIdentityAndUnsentUndo(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := nativeMoveFixture(t, st)
	preview, err := st.PreviewNativeMove(ctx, task, "target")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.ApplyNativeMove(ctx, preview)
	if err != nil || outcome.Task.Id != task.Id || outcome.Task.ProjectId != "target" {
		t.Fatalf("move: %+v %v", outcome, err)
	}
	if _, err := st.UpdateTask(ctx, task.Id, model.TaskEdit{Title: model.Ptr("blocked")}); err == nil {
		t.Fatal("pending move allowed later edit")
	}
	if err := st.DeleteTask(ctx, task.Id); err == nil {
		t.Fatal("pending move allowed deletion")
	}
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := st.ApplyUndo(ctx, undo)
	if err != nil || restored.Id != task.Id || restored.ProjectId != "source" {
		t.Fatalf("undo: %+v %v", restored, err)
	}
	counts, err := st.OutboxCounts(ctx)
	if err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("undo queue: %+v %v", counts, err)
	}
}

func TestNativeMoveUncertainRetainsDirtyAndCannotDrop(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := nativeMoveFixture(t, st)
	preview, err := st.PreviewNativeMove(ctx, task, "target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyNativeMove(ctx, preview); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	item := claimed[0]
	if err := st.RecordNativeMovePhase(ctx, item, "armed", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "lost response"); err != nil {
		t.Fatal(err)
	}
	var dirty int
	if err := st.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id='task'`).Scan(&dirty); err != nil || dirty == 0 {
		t.Fatalf("uncertain overlay cleared: %d %v", dirty, err)
	}
	if dropped, err := st.DropParked(ctx); err != nil || len(dropped) != 0 {
		t.Fatalf("uncertain move dropped: %+v %v", dropped, err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("uncertain move undone: %v", err)
	}
	queue, err := st.RecoveryQueue(ctx)
	if err != nil || len(queue.Tasks) != 1 || queue.Tasks[0].Refusal != "" {
		t.Fatalf("no recovery: %+v %v", queue, err)
	}
	if err := st.RetryQueueEntry(ctx, queue.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	claimed, _, err = st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("recovery claim: %+v %v", claimed, err)
	}
	if err := st.RecordNativeMovePhase(ctx, claimed[0], "armed", ""); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("uncertain move rearmed: %v", err)
	}
	if err := st.ConfirmNativeMove(ctx, claimed[0], json.RawMessage(`{"id":"task","projectId":"target"}`)); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id='task'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || string(fields["unknown"]) != `{"retained":true}` {
		t.Fatalf("native confirmation lost unknown snapshot: %s", raw)
	}
	undo, err = st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := st.ApplyUndo(ctx, undo)
	if err != nil || reverse.Id != "task" || reverse.ProjectId != "source" {
		t.Fatalf("reverse move: %+v %v", reverse, err)
	}
}

func TestNativeMoveStaleArmParksUntilExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := nativeMoveFixture(t, st)
	preview, err := st.PreviewNativeMove(ctx, task, "target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyNativeMove(ctx, preview); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := st.RecordNativeMovePhase(ctx, claimed[0], "armed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET inflight_at=0 WHERE seq=?`, claimed[0].Seq); err != nil {
		t.Fatal(err)
	}
	claimed, parked, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 0 || parked != 1 {
		t.Fatalf("stale arm automatically reclaimed: %+v %d %v", claimed, parked, err)
	}
}

func TestNativeMoveRejectsUnverifiedRelations(t *testing.T) {
	for _, field := range []string{"parent_id", "child_ids", "column_id"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			task := nativeMoveFixture(t, st)
			value := "relation"
			if field == "child_ids" {
				value = `["child"]`
			}
			if _, err := st.DB().ExecContext(ctx, `UPDATE tasks SET `+field+`=? WHERE id='task'`, value); err != nil {
				t.Fatal(err)
			}
			task, err := st.Task(ctx, task.Id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.PreviewNativeMove(ctx, task, "target"); err == nil {
				t.Fatal("unverified relation accepted")
			}
		})
	}
}
