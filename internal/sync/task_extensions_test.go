package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestTaskExtensionSyncConflictClearAndReadback(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "clear", true: "conflict"}[conflict], func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p", Name: "P"})
			task := model.Task{Id: "t", ProjectId: "p", Title: "Task", EstimatedDuration: 90, EstimatedPomo: 2}
			if _, err := st.SyncProject(ctx, "p", []store.ServerTask{{Task: task, Raw: json.RawMessage(`{"id":"t","projectId":"p","focusSummaries":[{"estimatedDuration":90,"estimatedPomo":2,"pomoCount":4}]}`)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpdateTask(ctx, "t", model.TaskEdit{EstimatedDuration: model.Ptr(int64(0)), EstimatedPomo: model.Ptr(0)}); err != nil {
				t.Fatal(err)
			}
			summary := map[string]any{"estimatedDuration": 90, "estimatedPomo": 2, "pomoCount": 4}
			remote := map[string]any{"id": "t", "projectId": "p", "title": "Task", "focusSummaries": []any{summary}}
			if conflict {
				summary["estimatedDuration"] = 100
			}
			posts := 0
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					var patch map[string]any
					if err := json.Unmarshal(readBody(t, r), &patch); err != nil {
						t.Fatal(err)
					}
					if _, ok := patch["estimatedDuration"]; ok {
						t.Fatal("top-level estimate sent")
					}
					summaries := patch["focusSummaries"].([]any)
					for k, v := range summaries[0].(map[string]any) {
						if k != "estimatedDuration" && k != "estimatedPomo" {
							t.Fatalf("read-only summary field sent: %s", k)
						}
						summary[k] = v
					}
					w.WriteHeader(http.StatusOK)
					return
				}
				writeJSON(t, w, remote)
			})
			result, err := testSyncer(t, st, server).Push(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				if posts != 0 || result.Pushed != 0 {
					t.Fatalf("conflict sent: posts=%d %+v", posts, result)
				}
				return
			}
			if posts != 1 || result.Pushed != 1 || result.ErrorCount() != 0 {
				t.Fatalf("clear sync: posts=%d %+v", posts, result)
			}
			got, err := st.Task(ctx, "t")
			if err != nil || got.EstimatedDuration != 0 || got.EstimatedPomo != 0 {
				t.Fatalf("clear lost: %+v %v", got, err)
			}
		})
	}
}

func TestTaskExtensionGuardRefusalRetainsDefiniteUnsentState(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p", Name: "P"})
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Task", EstimatedDuration: 90}); err != nil {
		t.Fatal(err)
	}
	client := api.NewClient("token", "test", api.WithRequestGuard(func(context.Context) (func(), error) { return nil, errors.New("logged out") }))
	_, err := New(st, client, Options{}).Push(ctx)
	if err == nil {
		t.Fatal("guard failure not reported")
	}
	queue, err := st.RecoveryQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tasks) != 1 {
		t.Fatalf("queue lost: %+v", queue)
	}
	_, metadata, err := store.DecodeTaskPayload(queue.Tasks[0].Item.Payload)
	if err != nil || metadata.Phase != store.FeatureRejected {
		t.Fatalf("unsent write marked uncertain: %+v %v", metadata, err)
	}
}

func TestTaskParentPreflightRejectsRemoteCycle(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p", Name: "P"})
	if _, err := st.SyncProject(ctx, "p", []store.ServerTask{{Task: model.Task{Id: "child", ProjectId: "p", Title: "Child"}}, {Task: model.Task{Id: "parent", ProjectId: "p", Title: "Parent"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr("parent")}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			t.Error("cyclic relationship sent")
		}
		writeJSON(t, w, map[string]any{"id": "parent", "projectId": "p", "parentId": "child", "status": 0})
	})
	result, err := testSyncer(t, st, server).Push(ctx)
	if err != nil || posts != 0 || result.Pushed != 0 || result.ErrorCount() != 1 {
		t.Fatalf("remote cycle ignored: %+v posts=%d %v", result, posts, err)
	}
}
