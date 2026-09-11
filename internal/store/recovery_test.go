package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func recoveryState(t *testing.T, st *Store) []string {
	t.Helper()
	var out []string
	for _, table := range []string{"tasks", "items", "outbox", "item_identities", "events", "undo_log", "focus_sessions", "focus_uploads", "timer_state", "listing"} {
		v, err := rowVersion(context.Background(), st.db, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestRecoveryInspectionDoesNotReconcileOrWrite(t *testing.T) {
	st, task := keyedChecklist(t)
	ctx := context.Background()
	if _, err := st.StartTimer(ctx, TimerStartOptions{TaskID: task.Id, FocusType: 0, Planned: time.Second}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := recoveryState(t, st)
	q, err := st.RecoveryQueue(ctx)
	if err != nil || len(q.Tasks) != 1 || len(q.Focus) != 0 {
		t.Fatalf("queue %+v %v", q, err)
	}
	if !reflect.DeepEqual(before, recoveryState(t, st)) {
		t.Fatal("inspection reconciled timer or wrote data")
	}
}

func TestRecoveryExactEntryRefusesConcurrentQueueAndPreservesFocus(t *testing.T) {
	ctx := context.Background()
	st, task := keyedChecklist(t)
	seq, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, TaskID: task.Id, ProjectID: task.ProjectId, Op: OpTaskComplete})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE outbox SET state='failed' WHERE seq=?`, seq); err != nil {
		t.Fatal(err)
	}
	q, err := st.RecoveryQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := q.Tasks[len(q.Tasks)-1]
	if expected.Refusal != "" {
		t.Fatal(expected.Refusal)
	}
	if _, err := st.db.Exec(`UPDATE outbox SET last_error='concurrent failure' WHERE seq=?`, seq); err != nil {
		t.Fatal(err)
	}
	before := recoveryState(t, st)
	if err := st.RetryQueueEntry(ctx, expected); !errors.Is(err, ErrConfirmationChanged) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, recoveryState(t, st)) {
		t.Fatal("stale recovery wrote")
	}
	q, err = st.RecoveryQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected = q.Tasks[len(q.Tasks)-1]
	other, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, TaskID: "other", ProjectID: "p1", Op: OpTaskDelete})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE outbox SET state='failed' WHERE seq=?`, other); err != nil {
		t.Fatal(err)
	}
	focusBefore, err := rowVersion(ctx, st.db, `SELECT * FROM focus_uploads`)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RetryQueueEntry(ctx, expected); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := st.db.QueryRow(`SELECT state FROM outbox WHERE seq=?`, other).Scan(&state); err != nil || state != "failed" {
		t.Fatal("new queue row joined confirmation", state, err)
	}
	focusAfter, _ := rowVersion(ctx, st.db, `SELECT * FROM focus_uploads`)
	if focusBefore != focusAfter {
		t.Fatal("task recovery changed focus uploads")
	}
	before = recoveryState(t, st)
	if err := st.RetryQueueEntry(ctx, expected); !errors.Is(err, ErrConfirmationChanged) {
		t.Fatal("duplicate recovery", err)
	}
	if !reflect.DeepEqual(before, recoveryState(t, st)) {
		t.Fatal("duplicate recovery wrote")
	}
}

func TestRecoveryAllocatingPhasesAndLegacyRefusal(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "rejected"}[rejected], func(t *testing.T) {
			ctx := context.Background()
			st, _ := keyedChecklist(t)
			items, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(items) != 1 {
				t.Fatal(items, err)
			}
			send, present, post, err := st.PrepareFeatureSend(ctx, items[0])
			if err != nil || !present || !post {
				t.Fatal(send, err)
			}
			if rejected {
				if err := st.RejectFeature(ctx, items[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "lost or rejected"); err != nil {
				t.Fatal(err)
			}
			q, err := st.RecoveryQueue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := q.Tasks[0]
			before := recoveryState(t, st)
			err = st.RetryQueueEntry(ctx, e)
			if !rejected {
				if e.Refusal == "" || err == nil || !reflect.DeepEqual(before, recoveryState(t, st)) {
					t.Fatal("uncertain allocation was rearmed", e, err)
				}
			} else {
				if e.Refusal != "" || err != nil {
					t.Fatal(e.Refusal, err)
				}
				q, err = st.RecoveryQueue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if string(q.Tasks[0].Item.Payload) != string(e.Item.Payload) {
					t.Fatal("retry changed frozen request")
				}
			}
		})
	}
	e := QueueEntry{Item: OutboxItem{OutboxEntry: OutboxEntry{Target: TargetOpenAPI, Op: OpTaskCreate, TaskID: "local-legacy", Payload: []byte(`{"title":"old"}`)}, State: OutboxFailed, Attempts: 1}}
	if _, refusal := queueRecovery(e); refusal == "" {
		t.Fatal("legacy create retry allowed")
	}
	e.Item.Op = OpTaskUpdate
	e.Item.Payload = []byte(`{"items":[{"title":"allocating"}]}`)
	if _, refusal := queueRecovery(e); refusal == "" {
		t.Fatal("legacy allocating checklist retry allowed")
	}
}

func TestTaskOperationSnapshotRejectsRawQueueAndDestinationChanges(t *testing.T) {
	for _, change := range []string{"raw", "queue", "item", "destination", "ABA"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			st, task := keyedChecklist(t)
			seedProjects(t, st, model.Project{Id: "p2", Name: "Other"})
			p, err := st.PreviewTaskOperation(ctx, task, "p2")
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "raw":
				_, err = st.db.Exec(`UPDATE tasks SET raw='{"unseen":true}' WHERE id=?`, task.Id)
			case "queue":
				_, err = st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, TaskID: task.Id, ProjectID: task.ProjectId, Op: OpTaskComplete})
			case "item":
				_, err = st.RenameTaskItem(ctx, task.Id, 1, "concurrent")
			case "destination":
				_, err = st.db.Exec(`UPDATE projects SET name='Changed' WHERE id='p2'`)
			case "ABA":
				_, err = st.RenameTaskItem(ctx, task.Id, 1, "temporary")
				if err == nil {
					var entry UndoEntry
					entry, err = st.LastUndo(ctx)
					if err == nil {
						_, err = st.ApplyUndo(ctx, entry)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, st)
			out, err := st.ApplyTaskOperation(ctx, p)
			if !errors.Is(err, ErrConfirmationChanged) || out.Changed {
				t.Fatalf("stale operation %+v %v", out, err)
			}
			if !reflect.DeepEqual(before, recoveryState(t, st)) {
				t.Fatal("stale operation wrote")
			}
		})
	}
}

func TestTaskOperationDeleteUndoAndMoveShareStoreSemantics(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "move"}[move], func(t *testing.T) {
			ctx := context.Background()
			st, task := keyedChecklist(t)
			seedProjects(t, st, model.Project{Id: "p2", Name: "Other"})
			destination := ""
			if move {
				destination = "p2"
			}
			p, err := st.PreviewTaskOperation(ctx, task, destination)
			if err != nil {
				t.Fatal(err)
			}
			if p.RemoteDelete || len(p.Queue) != 1 || p.RetainedCreates != 0 {
				t.Fatalf("incorrect local consequences %+v", p)
			}
			out, err := st.ApplyTaskOperation(ctx, p)
			if err != nil || !out.Changed {
				t.Fatal(out, err)
			}
			if _, err := st.Task(ctx, task.Id); !errors.Is(err, ErrNotFound) {
				t.Fatal("old task remains", err)
			}
			if move {
				if out.Task.Id == task.Id || out.Task.ProjectId != "p2" || out.Task.Content != task.Content || out.Task.Items[0].Key == task.Items[0].Key {
					t.Fatal("move lost bytes or reused identity", out)
				}
				id, err := st.CurrentTaskID(ctx, task.Id)
				if err != nil || id != out.Task.Id {
					t.Fatal("move reconciliation", id, err)
				}
				entry, err := st.LastUndo(ctx)
				if err != nil || entry.Action.Op != OpTaskMoveRecreate {
					t.Fatal("move did not fence undo", entry, err)
				}
			} else {
				entry, err := st.LastUndo(ctx)
				if err != nil {
					t.Fatal(err)
				}
				restored, err := st.ApplyUndo(ctx, entry)
				if err != nil || restored.Content != task.Content || len(restored.Items) != 2 {
					t.Fatal("delete undo", restored, err)
				}
			}
		})
	}
}

func TestMovePromotionChainUsesOnlyCommittedIdentityEvents(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "One"}, model.Project{Id: "p2", Name: "Two"})
	first, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "duplicate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceLocalID(ctx, first.Id, "server-original"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM outbox`); err != nil {
		t.Fatal(err)
	}
	second, err := st.MoveTask(ctx, "server-original", "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceLocalID(ctx, second.Id, "server-copy"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.Id, "server-original", second.Id, "server-copy"} {
		got, err := st.CurrentTaskID(ctx, id)
		if err != nil || got != "server-copy" {
			t.Fatal(id, got, err)
		}
	}
	got, err := st.CurrentTaskID(ctx, "missing")
	if err != nil || got != "missing" {
		t.Fatal("missing task guessed by title", got, err)
	}
}

func TestCanceledRecoveryAndTaskOperationDoNotWrite(t *testing.T) {
	st, task := keyedChecklist(t)
	ctx, cancel := context.WithCancel(context.Background())
	preview, err := st.PreviewTaskOperation(ctx, task, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE outbox SET state='failed',op=?,payload='{}'`, OpTaskComplete); err != nil {
		t.Fatal(err)
	}
	queue, err := st.RecoveryQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	before := recoveryState(t, st)
	if err := st.RetryQueueEntry(ctx, queue.Tasks[0]); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := st.ApplyTaskOperation(ctx, preview); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, recoveryState(t, st)) {
		t.Fatal("canceled operation wrote")
	}
}
