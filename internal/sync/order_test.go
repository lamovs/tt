package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestPushKeepsTwoEditsOfOneTaskInOrder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "original"))

	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("A")}); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("B")}); err != nil {
		t.Fatalf("second edit: %v", err)
	}

	serverTitle := "original"
	var applied []string
	refused := false
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, map[string]any{"id": "t1", "projectId": "p1", "title": serverTitle})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task/t1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(readBody(t, r), &body); err != nil {
			t.Errorf("decode request body: %v", err)
		}

		if body.Title == "A" && !refused {
			refused = true
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errorCode":"unknown_exception"}`))
			return
		}
		applied = append(applied, body.Title)
		serverTitle = body.Title
		w.WriteHeader(http.StatusOK)
	})

	syncer := testSyncer(t, st, server)

	if _, err := syncer.Push(ctx); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("the first pass applied %v, want nothing: the edit ahead of B failed", applied)
	}
	if c := outboxCounts(t, st); c.Pending != 2 {
		t.Fatalf("after the first pass outbox %+v, want both edits still queued", c)
	}

	if _, err := syncer.Push(ctx); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Fatalf("after the second pass outbox %+v, want empty", c)
	}
	if want := []string{"A", "B"}; !slices.Equal(applied, want) {
		t.Errorf("the server was sent %v, want %v", applied, want)
	}
	cached, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if cached.Title != "B" {
		t.Fatalf("cached title = %q, want the last edit B", cached.Title)
	}
	if serverTitle != "B" {
		t.Errorf("server title = %q, want the last edit B; applied in order %v - the older edit was "+
			"replayed over the newer one", serverTitle, applied)
	}
}

func TestClaimHoldsBackTheLaterEditOfATaskInFlight(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "original"))

	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("A")}); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("B")}); err != nil {
		t.Fatalf("second edit: %v", err)
	}

	first, _, err := st.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim took %d entries, want 1", len(first))
	}

	second, _, err := st.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second claim took entry %d of task %q while entry %d of the same task is in flight",
			second[0].Seq, second[0].TaskID, first[0].Seq)
	}
}

func TestPushFinishesACreateAndTheEditBehindItInOnePass(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var updated string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			writeJSON(t, w, map[string]any{"id": "srv1", "projectId": "p1", "title": "Zabrat posylku"})
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/srv1":
			var body struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(readBody(t, r), &body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			updated = body.Title
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/task/srv1":
			writeJSON(t, w, map[string]any{"id": "srv1", "projectId": "p1", "title": updated})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 2 {
		t.Fatalf("result = %+v, want the create and the edit behind it both sent", res)
	}
	if updated != "Zabrat posylku na pochte" {
		t.Errorf("the server was sent %q, want the edit that was queued behind the create", updated)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}
