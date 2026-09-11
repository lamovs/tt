package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestNativeMoveSyncPreservesIDAndReadbackRecovery(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "source", Name: "Source"})
	seedProject(t, st, model.Project{Id: "target", Name: "Target"})
	task := model.Task{Id: "task", ProjectId: "source", Title: "Task"}
	if _, err := st.SyncProject(ctx, "source", []store.ServerTask{{Task: task, Raw: json.RawMessage(`{"id":"task","projectId":"source","title":"Task"}`)}}); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewNativeMove(ctx, task, "target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyNativeMove(ctx, preview); err != nil {
		t.Fatal(err)
	}
	posts := 0
	visible := false
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/move" {
			posts++
			writeJSON(t, w, []map[string]string{{"id": "task", "etag": "etag-1"}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/source/task/task" {
			writeJSON(t, w, map[string]any{"id": "task", "projectId": "source", "title": "Task"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/target/task/task" && visible {
			writeJSON(t, w, map[string]any{"id": "task", "projectId": "target", "title": "Task"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	syncer := testSyncer(t, st, server)
	first, err := syncer.Push(ctx)
	if err != nil || posts != 1 || first.Pushed != 0 {
		t.Fatalf("first: posts=%d %+v %v", posts, first, err)
	}
	queue, err := st.RecoveryQueue(ctx)
	if err != nil || len(queue.Tasks) != 1 {
		t.Fatalf("queue: %+v %v", queue, err)
	}
	move, err := store.DecodeNativeMove(queue.Tasks[0].Item)
	if err != nil || move.Phase != "accepted" || move.Etag != "etag-1" {
		t.Fatalf("accepted proof lost: %+v %v", move, err)
	}
	visible = true
	if err := st.RetryQueueEntry(ctx, queue.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	second, err := syncer.Push(ctx)
	if err != nil || posts != 1 || second.Pushed != 1 {
		t.Fatalf("recovery resent move: posts=%d %+v %v", posts, second, err)
	}
	got, err := st.Task(ctx, "task")
	if err != nil || got.ProjectId != "target" {
		t.Fatalf("identity lost: %+v %v", got, err)
	}
}
