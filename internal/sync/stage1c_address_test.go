package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/movsar/tt/internal/store"
)

func TestStage1CCompletedRecoveryRequiresExactAddress(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong_task", func(task map[string]any) { task["id"] = "other-task" }},
		{"missing_task", func(task map[string]any) { delete(task, "id") }},
		{"wrong_project", func(task map[string]any) { task["projectId"] = "other-project" }},
		{"missing_project", func(task map[string]any) { delete(task, "projectId") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			task := stage1CHolderTask(stage1CTaskID, "completed recovery", []map[string]any{
				stage1CHolderItem("remote-a", "alpha", 1),
			})
			task["status"] = 2
			task["completedTime"] = "2026-09-09T12:00:00.000+0000"
			st, fake, sy := stage1CHolderSetup(t, task)
			stage1CAmbiguousDiscard(t, st, sy, fake)
			addressedPath := "/open/v1/project/" + stage1CProjectID + "/task/" + stage1CTaskID
			fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
				if request.Method == http.MethodGet && request.Path == stage1CProjectDataPath {
					return stage1CWireJSONResponse(http.StatusOK, map[string]any{
						"project": map[string]any{"id": stage1CProjectID, "name": "Stage 1C"},
						"tasks":   []any{}, "columns": []any{},
					})
				}
				if request.Method == http.MethodGet && request.Path == addressedPath {
					var body map[string]any
					if err := json.Unmarshal(response.Body, &body); err != nil {
						t.Fatal(err)
					}
					tc.mutate(body)
					return stage1CWireJSONResponse(http.StatusOK, body)
				}
				return response
			})
			before := stage1CHolderProtectedState(t, st)
			result, err := sy.Pull(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) == 0 {
				t.Fatalf("wrong-address recovery result=%+v, want a reported failed read", result)
			}
			if after := stage1CHolderProtectedState(t, st); after != before {
				t.Fatalf("wrong-address recovery changed protected state\nbefore=%s\nafter=%s", before, after)
			}
			if control := stage1CControl(t, st, stage1CTaskID); control != string(store.RecoveryPending) {
				t.Fatalf("wrong-address recovery consumed control %q", control)
			}

			fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
				if request.Method == http.MethodGet && request.Path == stage1CProjectDataPath {
					return stage1CWireJSONResponse(http.StatusOK, map[string]any{
						"project": map[string]any{"id": stage1CProjectID, "name": "Stage 1C"},
						"tasks":   []any{}, "columns": []any{},
					})
				}
				return response
			})
			if _, err := sy.Pull(ctx); err != nil {
				t.Fatalf("correctly addressed recovery: %v", err)
			}
			if control := stage1CControl(t, st, stage1CTaskID); control != string(store.RecoveryIdle) {
				t.Fatalf("correctly addressed recovery left control %q", control)
			}
		})
	}
}
