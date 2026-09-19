package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	tasksync "github.com/movsar/tt/internal/sync"
)

// The seam between a batch and the push: a document goes through PlanBatch
// and ApplyBatchPlan and then out to a fake server, because a relationship
// the cache accepts is not a relationship the server has. The link leaves the
// queue under the id the server gave the parent, never the local id it was
// queued with while the create of the parent was still waiting.
func TestBatchOfAParentAndItsChildPushesTheLinkUnderTheServerID(t *testing.T) {
	ctx := context.Background()
	_, st := actionStore(t)

	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nTrip\n\tBook tickets\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if _, err := ApplyBatchPlan(ctx, st, plan); err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}

	ids := map[string]string{"Trip": "srv-parent", "Book tickets": "srv-child"}
	var sentParent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			title, _ := body["title"].(string)
			batchPushJSON(t, w, api.Task{ID: ids[title], ProjectID: "work", Title: title})
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/srv-child":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode update: %v", err)
			}
			sentParent, _ = body["parentId"].(string)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/open/v1/project/work/task/srv-parent":
			batchPushJSON(t, w, map[string]any{"id": "srv-parent", "projectId": "work", "title": "Trip", "status": 0})
		case r.URL.Path == "/open/v1/project/work/task/srv-child":
			out := map[string]any{"id": "srv-child", "projectId": "work", "title": "Book tickets", "status": 0}
			if sentParent != "" {
				out["parentId"] = sentParent
			}
			batchPushJSON(t, w, out)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewClient("synthetic-token", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(time.Second))
	result, err := tasksync.New(st, client, tasksync.Options{}).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if result.ErrorCount() != 0 {
		t.Fatalf("push reported errors: %+v", result)
	}
	if sentParent != "srv-parent" {
		t.Errorf("the child link was sent with parentId %q, want the server id of the parent srv-parent", sentParent)
	}
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts != (store.OutboxCounts{}) {
		t.Errorf("outbox = %+v, want it drained after the push", counts)
	}
	child := readTask(t, st, "srv-child")
	if child.ParentId != "srv-parent" {
		t.Errorf("cached child parent = %q, want the server id of the parent srv-parent", child.ParentId)
	}
}

// batchPushJSON answers a request of the fake server the way the provider
// does, so the push reads the reply rather than the failure of a reader.
func batchPushJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write the reply of the fake server: %v", err)
	}
}

// The form the closing end of the parent rule shuts, seen from the store out
// to the server: a parent created offline, a child hung under it while the
// create of the parent is still queued, and a reader who closes the parent
// before the sync. Closing it first parked the relationship on the push -
// the freeze reads the closed parent out of the cache - and parked it again
// on every retry, where the server reads the parent closed too, until a pull
// wrote the empty parent of the server over the child. The completion waits
// for the sync that sends the relationship instead, and goes through
// afterwards with the relationship on the server.
func TestCompletingAParentWaitsForTheSyncThatSendsTheChildLink(t *testing.T) {
	ctx := context.Background()
	_, st := actionStore(t)

	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Trip"})
	if err != nil {
		t.Fatalf("create the parent: %v", err)
	}
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Book tickets"})
	if err != nil {
		t.Fatalf("create the child: %v", err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("hang the child under the queued parent: %v", err)
	}
	if _, err := st.CompleteTaskIfUnchanged(ctx, readTask(t, st, parent.Id), store.CompleteOptions{}); !errors.Is(err, store.ErrChildLinkUnsent) {
		t.Fatalf("completion before the sync = %v, want it refused", err)
	}

	ids := map[string]string{"Trip": "srv-parent", "Book tickets": "srv-child"}
	var sentParent string
	completed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			title, _ := body["title"].(string)
			batchPushJSON(t, w, api.Task{ID: ids[title], ProjectID: "work", Title: title})
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/srv-child":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode update: %v", err)
			}
			sentParent, _ = body["parentId"].(string)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/project/work/task/srv-parent/complete":
			completed = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/open/v1/project/work/task/srv-parent":
			status := 0
			if completed {
				status = 2
			}
			batchPushJSON(t, w, map[string]any{"id": "srv-parent", "projectId": "work", "title": "Trip", "status": status})
		case r.URL.Path == "/open/v1/project/work/task/srv-child":
			out := map[string]any{"id": "srv-child", "projectId": "work", "title": "Book tickets", "status": 0}
			if sentParent != "" {
				out["parentId"] = sentParent
			}
			batchPushJSON(t, w, out)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := api.NewClient("synthetic-token", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(time.Second))

	result, err := tasksync.New(st, client, tasksync.Options{}).Push(ctx)
	if err != nil || result.ErrorCount() != 0 {
		t.Fatalf("Push = %+v, err = %v", result, err)
	}
	if sentParent != "srv-parent" {
		t.Fatalf("the child link was sent with parentId %q, want the server id of the parent srv-parent", sentParent)
	}

	done, err := st.CompleteTaskIfUnchanged(ctx, readTask(t, st, "srv-parent"), store.CompleteOptions{})
	if err != nil || !done.Task.Status.Done() {
		t.Fatalf("completion after the sync = %+v %v, want the parent closed", done.Task, err)
	}
	result, err = tasksync.New(st, client, tasksync.Options{}).Push(ctx)
	if err != nil || result.ErrorCount() != 0 {
		t.Fatalf("Push of the completion = %+v, err = %v", result, err)
	}
	if !completed {
		t.Error("the completion never reached the server")
	}
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts != (store.OutboxCounts{}) {
		t.Errorf("outbox = %+v, want it drained after both pushes", counts)
	}
	if child := readTask(t, st, "srv-child"); child.ParentId != "srv-parent" {
		t.Errorf("cached child parent = %q, want the relationship kept after the parent was closed", child.ParentId)
	}
}

// One batch is one change to the cache and one undo group, but not one
// request: the tasks go out one by one, and the server may take some of them
// and refuse others. What a refusal leaves behind is written down here,
// because nothing above this line ever sees a server say no.
func TestBatchWhoseTaskTheServerRefusesKeepsTheRestAndParksTheRefused(t *testing.T) {
	ctx := context.Background()
	_, st := actionStore(t)

	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nTrip\n\tBook tickets\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}
	child := applied.Results[1].Task.Id

	var creates []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			title, _ := body["title"].(string)
			creates = append(creates, title)
			if title == "Book tickets" {
				// The server takes the parent and refuses the child, the way
				// it refuses anything it will not have: for good, not for now.
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			batchPushJSON(t, w, api.Task{ID: "srv-parent", ProjectID: "work", Title: title})
		case r.URL.Path == "/open/v1/project/work/task/srv-parent":
			batchPushJSON(t, w, map[string]any{"id": "srv-parent", "projectId": "work", "title": "Trip", "status": 0})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := api.NewClient("synthetic-token", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(time.Second))
	result, err := tasksync.New(st, client, tasksync.Options{}).Push(ctx)
	if err != nil {
		// A refused task is a result of the push, not a failure of it: the
		// tasks the server took are sent and reported all the same.
		t.Fatalf("Push: %v", err)
	}
	if len(creates) != 2 {
		t.Errorf("creates = %v, want one attempt for each task of the batch", creates)
	}
	if result.Pushed != 1 || result.Failed != 2 {
		t.Errorf("result = %+v, want the parent sent and the child's create and link parked", result)
	}
	// What the reader is told: which entries stopped, and the one command
	// that puts them back in line.
	if result.ErrorCount() != 2 {
		t.Fatalf("errors = %v, want one for the refused create and one for the link that has nowhere to go", result.Errors)
	}
	for _, want := range []string{"create task", "http 400"} {
		if !strings.Contains(result.Errors[0].Error(), want) {
			t.Errorf("the refusal reads %q, want it to say %q", result.Errors[0], want)
		}
	}
	for _, err := range result.Errors {
		if !strings.Contains(err.Error(), "tt sync --retry-failed") {
			t.Errorf("%q does not say how to put the entry back in line", err)
		}
	}

	// What is left in the queue: both entries parked, none in flight and none
	// waiting, so nothing of the batch is silently dropped and a retry has
	// something to retry.
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatalf("OutboxCounts: %v", err)
	}
	if counts != (store.OutboxCounts{Failed: 2}) {
		t.Errorf("outbox = %+v, want the two entries of the child parked and nothing else left", counts)
	}

	// What is left in the cache: the whole batch, the way one undo still
	// reverses it. The parent answers to the id the server gave it; the child
	// keeps the local id its create was refused under, and keeps hanging
	// under the parent.
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want both tasks of the batch still cached", tasks)
	}
	parent := readTask(t, st, "srv-parent")
	if parent.Title != "Trip" {
		t.Errorf("parent = %+v, want the task the server took under its own id", parent)
	}
	refused := readTask(t, st, child)
	if refused.Title != "Book tickets" || refused.ParentId != parent.Id {
		t.Errorf("refused child = %+v, want it cached under %s, waiting for a retry", refused, parent.Id)
	}
}
