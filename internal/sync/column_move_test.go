package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func columnMoveFixture(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	seedProject(t, st, model.Project{Id: "p", Name: "Board"})
	task := model.Task{Id: "t", ProjectId: "p", Title: "Keep title", Content: "Keep content", ColumnId: "a", ColumnName: "A", SortOrder: 123}
	raw := json.RawMessage(`{"id":"t","projectId":"p","title":"Keep title","content":"Keep content","columnId":"a","columnName":"A","sortOrder":123,"status":0,"future":{"keep":true}}`)
	if _, err := st.SyncProject(ctx, "p", []store.ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	var columns []store.ResourceEntity
	for _, id := range []string{"a", "b"} {
		body, _ := json.Marshal(map[string]any{"id": id, "projectId": "p", "name": id})
		columns = append(columns, store.ResourceEntity{Ref: store.EntityRef{Kind: "column", Key: id}, ServerID: id, ProjectKey: "p", Data: body})
	}
	if err := st.MergeEntities(ctx, "column", "p", columns, true); err != nil {
		t.Fatal(err)
	}
	current, err := st.Task(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewTaskColumn(ctx, current, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyTaskColumn(ctx, preview); err != nil {
		t.Fatal(err)
	}
}

func TestColumnMoveSyncPreflightAndMinimalPatch(t *testing.T) {
	for _, scenario := range []string{"success", "source-conflict", "missing-column", "completed", "missing-status", "null-status", "parent", "lost-reply", "post-completed", "post-missing-status", "post-null-status"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			columnMoveFixture(t, st)
			remote := map[string]any{"id": "t", "projectId": "p", "title": "Keep title", "content": "Keep content", "columnId": "a", "sortOrder": 123, "status": 0, "future": map[string]any{"keep": true}}
			switch scenario {
			case "source-conflict":
				remote["columnId"] = "changed"
			case "completed":
				remote["status"] = 2
			case "missing-status":
				delete(remote, "status")
			case "null-status":
				remote["status"] = nil
			case "parent":
				remote["parentId"] = "parent"
			}
			posts := 0
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /open/v1/project/p/column":
					columns := []map[string]any{}
					if scenario != "missing-column" {
						columns = append(columns, map[string]any{"id": "b", "projectId": "p", "name": "Done"})
					}
					writeJSON(t, w, columns)
				case "GET /open/v1/project/p/task/t":
					writeJSON(t, w, remote)
				case "POST /open/v1/task/t":
					posts++
					var patch map[string]any
					if err := json.Unmarshal(readBody(t, r), &patch); err != nil {
						t.Error(err)
					}
					if len(patch) != 3 || patch["id"] != "t" || patch["projectId"] != "p" || patch["columnId"] != "b" {
						t.Errorf("not a minimal column patch: %+v", patch)
					}
					remote["columnId"] = "b"
					switch scenario {
					case "post-completed":
						remote["status"] = 2
					case "post-missing-status":
						delete(remote, "status")
					case "post-null-status":
						remote["status"] = nil
					}
					if scenario == "lost-reply" {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			sy := testSyncer(t, st, server)
			result, err := sy.Push(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "success" {
				if posts != 1 || result.Pushed != 1 || result.ErrorCount() != 0 {
					t.Fatalf("successful move: posts=%d result=%+v", posts, result)
				}
				got, err := st.Task(ctx, "t")
				if err != nil || got.ColumnId != "b" || got.Status.Done() || got.Content != "Keep content" || got.SortOrder != 123 {
					t.Fatalf("move changed unrelated fields: %+v %v", got, err)
				}
			} else if scenario == "lost-reply" {
				if posts != 1 || result.Pushed != 0 {
					t.Fatalf("uncertain write was retried: posts=%d result=%+v", posts, result)
				}
				if _, err := sy.Push(ctx); err != nil {
					t.Fatal(err)
				}
				if posts != 1 {
					t.Fatal("uncertain column write was replayed")
				}
			} else if scenario == "post-completed" || scenario == "post-missing-status" || scenario == "post-null-status" {
				if posts != 1 || result.Pushed != 0 || result.ErrorCount() == 0 {
					t.Fatalf("unproven read-back settled: posts=%d result=%+v", posts, result)
				}
				var raw []byte
				if err := st.DB().QueryRow(`SELECT payload FROM outbox WHERE task_id='t'`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				_, metadata, err := store.DecodeTaskEditPayload(raw)
				if err != nil || metadata == nil || metadata.Phase != store.FeatureMismatch {
					t.Fatalf("unproven status did not retain uncertain mismatch: %+v %v", metadata, err)
				}
				if _, err := st.RetryFailed(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := sy.Push(ctx); err != nil {
					t.Fatal(err)
				}
				if posts != 1 {
					t.Fatal("unproven column status authorized another POST")
				}
			} else if posts != 0 || result.Pushed != 0 || result.ErrorCount() == 0 {
				t.Fatalf("unsafe preflight sent: posts=%d result=%+v", posts, result)
			}
		})
	}
}

func TestLegacyColumnWritesNeverGainNewSendSemantics(t *testing.T) {
	for _, op := range []string{store.OpTaskUpdate, store.OpTaskMove, store.OpTaskComplete} {
		for _, field := range []string{"column_id", "COLUMN_ID", "Column_Id"} {
			t.Run(op+"/"+field, func(t *testing.T) {
				payload, _ := json.Marshal(map[string]any{field: "b"})
				item := store.OutboxItem{OutboxEntry: store.OutboxEntry{Op: op, TaskID: "t", ProjectID: "p", Payload: payload}}
				if err := validateColumnProtocol(item); err == nil {
					t.Fatal("legacy column field became a writable patch")
				}
			})
		}
	}
}

func TestColumnFrozenDestinationMismatchCannotRearm(t *testing.T) {
	column := model.TaskEdit{ColumnId: model.Ptr("b")}
	parent := model.TaskEdit{ParentId: model.Ptr("parent")}
	for _, tc := range []struct {
		name   string
		root   model.TaskEdit
		frozen model.TaskEdit
	}{
		{"different column", column, model.TaskEdit{ColumnId: model.Ptr("other")}},
		{"empty column", column, model.TaskEdit{ColumnId: model.Ptr("")}},
		{"column root parent snapshot", column, parent},
		{"parent root column snapshot", parent, column},
		{"mixed snapshot", column, model.TaskEdit{ColumnId: model.Ptr("b"), ParentId: model.Ptr("parent")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			columnMoveFixture(t, st)
			baseline := model.TaskEdit{ColumnId: model.Ptr("a")}
			if tc.root.ParentId != nil {
				baseline = model.TaskEdit{ParentId: model.Ptr("")}
			}
			metadata := store.FeaturePayloadMetadata{
				Version: store.FeatureExtensionsPayloadVersion, Fields: store.FeatureFields{Extensions: true},
				Phase: store.FeatureRejected, ExtensionBaseline: &baseline,
				Snapshot: &store.FeatureSnapshot{Extensions: &tc.frozen},
			}
			raw, err := store.EncodeTaskEditPayload(tc.root, metadata)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE outbox SET payload=? WHERE task_id='t'`, string(raw)); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Errorf("mismatching frozen column reached network: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})
			result, err := testSyncer(t, st, server).Push(ctx)
			if err != nil || requests != 0 || result.Pushed != 0 || result.ErrorCount() == 0 {
				t.Fatalf("frozen mismatch was not parked before rearm: requests=%d result=%+v %v", requests, result, err)
			}
			if err := st.DB().QueryRow(`SELECT payload FROM outbox WHERE task_id='t'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			_, retained, err := store.DecodeTaskEditPayload(raw)
			if err != nil || retained == nil || retained.Phase != store.FeatureRejected {
				t.Fatalf("preflight changed the retained send phase: %+v %v", retained, err)
			}

			metadata.Phase = store.FeatureArmed
			armed, err := store.EncodeTaskEditPayload(tc.root, metadata)
			if err != nil {
				t.Fatal(err)
			}
			item := store.OutboxItem{OutboxEntry: store.OutboxEntry{Op: store.OpTaskUpdate, TaskID: "t", ProjectID: "p", Payload: armed}}
			if err := validateColumnProtocol(item); err != nil {
				t.Fatalf("new send guard blocked armed read-only recovery: %v", err)
			}
			for _, op := range []string{store.OpTaskComplete, store.OpTaskMove} {
				item.Op = op
				if err := validateColumnProtocol(item); err == nil {
					t.Errorf("armed column metadata bypassed the operation guard: %s", op)
				}
			}
		})
	}
}

func TestMatchingFrozenColumnCanRearm(t *testing.T) {
	edit := model.TaskEdit{ColumnId: model.Ptr("b")}
	baseline := model.TaskEdit{ColumnId: model.Ptr("a")}
	metadata := store.FeaturePayloadMetadata{
		Version: store.FeatureExtensionsPayloadVersion, Fields: store.FeatureFields{Extensions: true},
		Phase: store.FeatureRejected, ExtensionBaseline: &baseline,
		Snapshot: &store.FeatureSnapshot{Extensions: &edit},
	}
	raw, err := store.EncodeTaskEditPayload(edit, metadata)
	if err != nil {
		t.Fatal(err)
	}
	item := store.OutboxItem{OutboxEntry: store.OutboxEntry{Op: store.OpTaskUpdate, TaskID: "t", ProjectID: "p", Payload: raw}}
	if err := validateColumnProtocol(item); err != nil {
		t.Fatalf("matching rejected column cannot retry: %v", err)
	}
}

func TestColumnTaskStatusRequiresExplicitNumericZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"status":null}`, `{"status":2}`, `{"status":-1}`, `{"status":"0"}`, `{"status":false}`} {
		if err := checkColumnTaskStatus([]byte(raw)); err == nil {
			t.Errorf("unproven open status accepted: %s", raw)
		}
	}
	if err := checkColumnTaskStatus([]byte(`{"status":0}`)); err != nil {
		t.Fatalf("explicit open status rejected: %v", err)
	}
}
