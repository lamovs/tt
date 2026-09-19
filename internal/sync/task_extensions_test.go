package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestDroppedParentKeepsChildEstimatesSendable(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p", Name: "P"})
	child := model.Task{Id: "child", ProjectId: "p", Title: "Child"}
	if _, err := st.SyncProject(ctx, "p", []store.ServerTask{{Task: child}}); err != nil {
		t.Fatal(err)
	}
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id), EstimatedPomo: model.Ptr(3)}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].TaskID != parent.Id {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := st.MarkFailed(ctx, claimed[0].Seq, claimed[0].LeaseToken, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DropParked(ctx); err != nil {
		t.Fatal(err)
	}
	posts := 0
	remote := map[string]any{"id": child.Id, "projectId": "p", "title": child.Title, "status": 0}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/child" {
			posts++
			var patch map[string]any
			if err := json.Unmarshal(readBody(t, r), &patch); err != nil {
				t.Error(err)
			}
			if _, ok := patch["parentId"]; ok {
				t.Error("discarded relationship was sent")
			}
			remote["focusSummaries"] = patch["focusSummaries"]
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p/task/child" {
			writeJSON(t, w, remote)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	result, err := testSyncer(t, st, server).Push(ctx)
	if err != nil || result.ErrorCount() != 0 || result.Pushed != 1 || posts != 1 {
		t.Fatalf("push: %+v posts=%d %v", result, posts, err)
	}
	stored, err := st.Task(ctx, child.Id)
	if err != nil || stored.EstimatedPomo != 3 || stored.ParentId != "" {
		t.Fatalf("child: %+v %v", stored, err)
	}
	if _, dirty, _ := taskRow(t, st, child.Id); dirty != 0 {
		t.Fatalf("dirty after push: %d", dirty)
	}
	if counts := outboxCounts(t, st); counts != (store.OutboxCounts{}) {
		t.Fatalf("queue: %+v", counts)
	}
}

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

// queuedUpdatePayload reads the payload of the one queued task.update, so a
// test can look at a pending entry from inside a request handler.
func queuedUpdatePayload(t *testing.T, st *store.Store) string {
	t.Helper()
	var payload string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT payload FROM outbox WHERE op = ?`, store.OpTaskUpdate).Scan(&payload); err != nil {
		t.Errorf("read the queued update: %v", err)
	}
	return payload
}

// childLinkFixture queues what tt batch queues for a task written under
// another one: two creates and the update that hangs the second task under
// the first, named by the local id the first one still has.
func childLinkFixture(t *testing.T, st *store.Store) (model.Task, model.Task) {
	t.Helper()
	ctx := context.Background()
	seedProject(t, st, model.Project{Id: "p", Name: "P"})
	parent, err := st.CreateTask(ctx, openTask("", "p", "Parent"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, openTask("", "p", "Child"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("queue the link to a parent waiting for its own create: %v", err)
	}
	return parent, child
}

func TestPushSendsAChildLinkUnderTheIDTheParentWasGiven(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	parent, _ := childLinkFixture(t, st)

	ids := map[string]string{"Parent": "srv-parent", "Child": "srv-child"}
	var queuedLink, sentParent string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
			var body map[string]any
			if err := json.Unmarshal(readBody(t, r), &body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			title, _ := body["title"].(string)
			if title == "Child" {
				// The create of the parent is settled by now, so the entry
				// still waiting must already name the id it came back with.
				queuedLink = queuedUpdatePayload(t, st)
			}
			writeJSON(t, w, api.Task{ID: ids[title], ProjectID: "p", Title: title})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/srv-child" {
			var body map[string]any
			if err := json.Unmarshal(readBody(t, r), &body); err != nil {
				t.Errorf("decode update: %v", err)
			}
			sentParent, _ = body["parentId"].(string)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch r.URL.Path {
		case "/open/v1/project/p/task/srv-parent":
			writeJSON(t, w, map[string]any{"id": "srv-parent", "projectId": "p", "title": "Parent", "status": 0})
		case "/open/v1/project/p/task/srv-child":
			out := map[string]any{"id": "srv-child", "projectId": "p", "title": "Child", "status": 0}
			if sentParent != "" {
				out["parentId"] = sentParent
			}
			writeJSON(t, w, out)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 3 || res.Failed != 0 || res.ErrorCount() != 0 {
		t.Fatalf("result = %+v, want both creates and the link settled", res)
	}
	if strings.Contains(queuedLink, parent.Id) || !strings.Contains(queuedLink, "srv-parent") {
		t.Errorf("the waiting link did not follow the id swap: %s", queuedLink)
	}
	if sentParent != "srv-parent" {
		t.Errorf("the link was sent with parentId %q, want srv-parent", sentParent)
	}
	got, err := st.Task(ctx, "srv-child")
	if err != nil || got.ParentId != "srv-parent" {
		t.Fatalf("cached child = %+v (%v), want it under srv-parent", got, err)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func TestPushParksAChildLinkWhoseParentNeverReachedTheServer(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	parent, _ := childLinkFixture(t, st)

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
			var body map[string]any
			if err := json.Unmarshal(readBody(t, r), &body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			if title, _ := body["title"].(string); title == "Parent" {
				http.Error(w, "no", http.StatusBadRequest)
				return
			}
			writeJSON(t, w, api.Task{ID: "srv-child", ProjectID: "p", Title: "Child"})
			return
		}
		if r.Method == http.MethodPost {
			t.Errorf("a link to an unresolved parent was sent to %s", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			return
		}
		writeJSON(t, w, map[string]any{"id": "srv-child", "projectId": "p", "title": "Child", "status": 0})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 || res.Failed != 2 {
		t.Fatalf("result = %+v, want the create of the child pushed and both the parent and the link parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 2 || c.Pending != 0 || c.Inflight != 0 {
		t.Fatalf("outbox %+v, want the two parked entries", c)
	}
	if got := outboxLastError(t, st, store.OpTaskUpdate); !strings.Contains(got, "unresolved identity") {
		t.Errorf("the parked link says %q, want it to name the unresolved parent", got)
	}
	if got, err := st.Task(ctx, "srv-child"); err != nil || got.ParentId != parent.Id {
		t.Fatalf("cached child = %+v (%v), want it still pointing at the local parent", got, err)
	}
}
