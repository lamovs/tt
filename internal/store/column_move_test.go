package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func columnMoveFixture(t *testing.T, st *Store, source string) model.Task {
	t.Helper()
	ctx := context.Background()
	seedProjects(t, st, model.Project{Id: "p", Name: "Project", ViewMode: "kanban", Permission: "write"})
	if err := st.MergeEntities(ctx, "column", "p", []ResourceEntity{
		{ServerID: "a", Data: json.RawMessage(`{"id":"a","projectId":"p","name":"Ready","sortOrder":0}`)},
		{ServerID: "b", Data: json.RawMessage(`{"id":"b","projectId":"p","name":"Doing","sortOrder":1}`)},
	}, false); err != nil {
		t.Fatal(err)
	}
	name := ""
	if source == "a" {
		name = "Ready"
	}
	task := model.Task{Id: "task", ProjectId: "p", Title: "Task", Content: "Keep this body", Status: model.TaskOpen, ColumnId: source, ColumnName: name, SortOrder: 9007199254740993, Tags: []string{"tag"}, Reminders: []string{"TRIGGER:-PT5M"}, Items: []model.Item{{Id: "item", Title: "Keep item", SortOrder: 17}}}
	raw, err := json.Marshal(map[string]any{"id": task.Id, "projectId": task.ProjectId, "title": task.Title, "columnId": source, "columnName": name, "status": 0, "sortOrder": task.SortOrder, "content": task.Content, "tags": task.Tags, "reminders": task.Reminders, "items": []map[string]any{{"id": "item", "title": "Keep item", "status": 0, "sortOrder": 17}}, "unknown": map[string]any{"large": int64(9007199254740993)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncProject(ctx, "p", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	task, err = st.Task(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestTaskColumnPreviewQueueAndUnsentUndo(t *testing.T) {
	for _, source := range []string{"a", ""} {
		t.Run("source_"+source, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			task := columnMoveFixture(t, st, source)
			var beforeRaw string
			if err := st.DB().QueryRow(`SELECT raw FROM tasks WHERE id='task'`).Scan(&beforeRaw); err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewTaskColumn(ctx, task, "b")
			if err != nil || preview.DestinationName != "Doing" || preview.Version == "" || preview.DestinationVersion == "" {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			if counts, err := st.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
				t.Fatalf("preview queued changes: %+v %v", counts, err)
			}
			out, err := st.ApplyTaskColumn(ctx, preview)
			if err != nil || !out.Changed || out.Task.ColumnId != "b" || out.Task.ColumnName != "Doing" {
				t.Fatalf("apply: %+v %v", out, err)
			}
			want := task
			want.ColumnId, want.ColumnName, want.ModifiedTime = "b", "Doing", out.Task.ModifiedTime
			if !reflect.DeepEqual(out.Task, want) {
				t.Fatalf("column move changed unrelated fields: %+v", out.Task)
			}
			queue, err := st.RecoveryQueue(ctx)
			if err != nil || len(queue.Tasks) != 1 {
				t.Fatalf("queue: %+v %v", queue, err)
			}
			item := queue.Tasks[0].Item
			edit, metadata, err := DecodeTaskEditPayload(item.Payload)
			if err != nil || !columnOnlyEdit(edit) || *edit.ColumnId != "b" || metadata == nil || metadata.Version != 4 || metadata.Phase != FeaturePrepared || *metadata.ExtensionBaseline.ColumnId != source {
				t.Fatalf("new protocol was not minimal v4: %+v %+v %v", edit, metadata, err)
			}
			if id, check, err := TaskExtensionColumn(item); err != nil || !check || id != "b" {
				t.Fatalf("preflight target: %q %t %v", id, check, err)
			}
			if _, err := st.ApplyTaskColumn(ctx, preview); err == nil {
				t.Fatal("stale acceptance reused")
			}
			undo, err := st.LastUndo(ctx)
			if err != nil || undo.Action.OperationSeq != item.Seq || undo.Feature == nil || undo.Feature.Version != 4 {
				t.Fatalf("undo identity: %+v %v", undo, err)
			}
			restored, err := st.ApplyUndo(ctx, undo)
			if err != nil || restored.ColumnId != source || restored.ColumnName != task.ColumnName || restored.Status != task.Status || restored.SortOrder != task.SortOrder || !reflect.DeepEqual(restored.Items, task.Items) {
				t.Fatalf("undo: %+v %v", restored, err)
			}
			if counts, err := st.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
				t.Fatalf("undo queued an inverse: %+v %v", counts, err)
			}
			var afterRaw string
			if err := st.DB().QueryRow(`SELECT raw FROM tasks WHERE id='task'`).Scan(&afterRaw); err != nil || afterRaw != beforeRaw {
				t.Fatalf("raw was changed: %v", err)
			}
		})
	}
}

func TestTaskColumnPreviewJSONRoundTrip(t *testing.T) {
	for _, destination := range []string{"a", "b"} {
		t.Run(destination, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			task := columnMoveFixture(t, st, "a")
			preview, err := st.PreviewTaskColumn(ctx, task, destination)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(preview)
			if err != nil {
				t.Fatal(err)
			}
			var accepted TaskColumnPreview
			if err := json.Unmarshal(raw, &accepted); err != nil {
				t.Fatal(err)
			}
			if task.Items[0].Key == "" || accepted.Task.Items[0].Key != "" {
				t.Fatal("fixture did not exercise private checklist keys")
			}
			out, err := st.ApplyTaskColumn(ctx, accepted)
			if err != nil || out.Changed != (destination != "a") || out.Task.ColumnId != destination || !reflect.DeepEqual(out.Task.Items, task.Items) || out.Task.SortOrder != task.SortOrder {
				t.Fatalf("JSON preview lost private or exact task data: %+v %v", out, err)
			}
		})
	}
}

func TestTaskColumnPreviewJSONTamperingRefused(t *testing.T) {
	for _, field := range []string{"title", "item", "sort", "version"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			task := columnMoveFixture(t, st, "a")
			preview, err := st.PreviewTaskColumn(ctx, task, "b")
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "title":
				preview.Task.Title = "Changed"
			case "item":
				preview.Task.Items[0].Title = "Changed"
			case "sort":
				preview.Task.SortOrder--
			case "version":
				preview.Version = "Changed"
			}
			if _, err := st.ApplyTaskColumn(ctx, preview); !errors.Is(err, ErrConfirmationChanged) {
				t.Fatalf("tampered preview accepted: %v", err)
			}
		})
	}
}

func TestTaskColumnPreviewRejectsUnsafeTargets(t *testing.T) {
	for _, tc := range []struct{ name, mutation, destination string }{
		{"missing", "", "missing"}, {"empty", "", ""},
		{"closed task", `UPDATE tasks SET status=2 WHERE id='task'`, "b"},
		{"parent task", `UPDATE tasks SET child_ids='["child"]' WHERE id='task'`, "b"},
		{"child task", `UPDATE tasks SET parent_id='parent' WHERE id='task'`, "b"},
		{"dirty task", `UPDATE tasks SET dirty=7 WHERE id='task'`, "b"},
		{"missing raw", `UPDATE tasks SET raw='{}' WHERE id='task'`, "b"},
		{"other project", `UPDATE resource_entities SET project_key='other' WHERE server_id='b'`, "b"},
		{"missing column project", `UPDATE resource_entities SET data='{"id":"b","name":"Doing"}',base='{"id":"b","name":"Doing"}' WHERE server_id='b'`, "b"},
		{"dirty column", `UPDATE resource_entities SET dirty=1 WHERE server_id='b'`, "b"},
		{"deleted column", `UPDATE resource_entities SET deleted=1 WHERE server_id='b'`, "b"},
		{"closed project", `UPDATE projects SET closed=1 WHERE id='p'`, "b"},
		{"read project", `UPDATE projects SET permission='read' WHERE id='p'`, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			task := columnMoveFixture(t, st, "a")
			if tc.mutation != "" {
				if _, err := st.DB().Exec(tc.mutation); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.PreviewTaskColumn(context.Background(), task, tc.destination); err == nil {
				t.Fatal("unsafe column target accepted")
			}
		})
	}
}

func TestTaskColumnSameDestinationIsNoop(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := columnMoveFixture(t, st, "a")
	preview, err := st.PreviewTaskColumn(ctx, task, "a")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.ApplyTaskColumn(ctx, preview)
	if err != nil || outcome.Changed || !reflect.DeepEqual(outcome.Task, task) {
		t.Fatalf("same-column mutation was not a no-op: %+v %v", outcome, err)
	}
	if counts, err := st.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("no-op queued work: %+v %v", counts, err)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("no-op added undo: %v", err)
	}
}

func TestTaskColumnAcceptFencesTaskAndDestination(t *testing.T) {
	for _, mutation := range []string{`UPDATE tasks SET title='changed' WHERE id='task'`, `UPDATE resource_entities SET revision=revision+1 WHERE server_id='b'`} {
		st := testStore(t)
		task := columnMoveFixture(t, st, "a")
		preview, err := st.PreviewTaskColumn(context.Background(), task, "b")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(mutation); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyTaskColumn(context.Background(), preview); !errors.Is(err, ErrConfirmationChanged) {
			t.Fatalf("stale preview accepted: %v", err)
		}
	}
}

func TestTaskColumnUncertainAndConfirmedUndoRefused(t *testing.T) {
	for _, phase := range []string{"inflight", "armed", "rejected", "confirmed", "prepared sent marker"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			task := columnMoveFixture(t, st, "a")
			preview, err := st.PreviewTaskColumn(ctx, task, "b")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyTaskColumn(ctx, preview); err != nil {
				t.Fatal(err)
			}
			undo, err := st.LastUndo(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claimed, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			item := claimed[0]
			if phase == "prepared sent marker" {
				if _, err := st.DB().Exec(`UPDATE outbox SET state='failed',sent_at=1 WHERE seq=?`, item.Seq); err != nil {
					t.Fatal(err)
				}
			} else if phase != "inflight" {
				send, present, post, err := st.PrepareFeatureSend(ctx, item)
				if err != nil || !present || !post {
					t.Fatalf("arm: %t %t %v", present, post, err)
				}
				if _, check, err := TaskExtensionColumn(OutboxItem{OutboxEntry: OutboxEntry{Op: OpTaskUpdate, Payload: mustColumnPayload(t, send)}}); err != nil || check {
					t.Fatalf("armed operation requested preflight write: %t %v", check, err)
				}
				if phase == "rejected" {
					if err := st.RejectFeature(ctx, item); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "confirmed" {
					if _, err := st.DB().Exec(`DELETE FROM outbox WHERE seq=?`, item.Seq); err != nil {
						t.Fatal(err)
					}
				} else if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "fixture response"); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "armed" || phase == "rejected" {
				var dirty int64
				var raw string
				if err := st.DB().QueryRow(`SELECT dirty,raw FROM tasks WHERE id=?`, task.Id).Scan(&dirty, &raw); err != nil {
					t.Fatal(err)
				}
				if dirty != item.Rev {
					t.Fatalf("parking discarded column revision: %d != %d", dirty, item.Rev)
				}
				if _, err := st.SyncProject(ctx, task.ProjectId, []ServerTask{{Task: task, Raw: json.RawMessage(raw)}}); err != nil {
					t.Fatal(err)
				}
				if current, err := st.Task(ctx, task.Id); err != nil || current.ColumnId != "b" {
					t.Fatalf("pull overwrote parked column intent: %+v %v", current, err)
				}
			}
			_, err = st.ApplyUndo(ctx, undo)
			if phase == "rejected" {
				if err != nil {
					t.Fatalf("confirmed rejection cannot cancel: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("uncertain or confirmed inverse was accepted")
				}
				if _, err := st.LastUndo(ctx); err != nil {
					t.Fatalf("refused undo lost history: %v", err)
				}
				if phase == "armed" || phase == "prepared sent marker" {
					if dropped, err := st.DropParked(ctx); err != nil || len(dropped) != 0 {
						t.Fatalf("parked column operation was discarded: %+v %v", dropped, err)
					}
					if counts, err := st.OutboxCounts(ctx); err != nil || counts.Failed != 1 {
						t.Fatalf("parked uncertainty disappeared: %+v %v", counts, err)
					}
				}
			}
		})
	}
}

func mustColumnPayload(t *testing.T, send FeatureSend) []byte {
	t.Helper()
	raw, err := EncodeTaskEditPayload(*send.Edit, send.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTaskColumnCreationMixedEditsAndPendingUndoRefused(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := columnMoveFixture(t, st, "a")
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "New", ColumnId: "b"}); err == nil {
		t.Fatal("unverified creation column was accepted")
	}
	if _, err := st.UpdateTask(ctx, task.Id, model.TaskEdit{ColumnId: model.Ptr("b"), Status: model.Ptr(model.TaskDone)}); err == nil {
		t.Fatal("column move changed completion status")
	}
	preview, err := st.PreviewTaskColumn(ctx, task, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyTaskColumn(ctx, preview); err != nil {
		t.Fatal(err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: task.Id, ProjectID: task.ProjectId, Payload: json.RawMessage(`{"Title":"Later"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("cancellation discarded a later operation: %v", err)
	}
}
