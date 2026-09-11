package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func cachedTasks(t *testing.T, st *store.Store, projectID string, tasks ...model.Task) {
	t.Helper()
	seedProject(t, st, model.Project{Id: projectID, Name: "Личное"})
	seedTasks(t, st, projectID, tasks...)
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}
	return b
}

func TestPushCreateSwapsTheLocalID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	var sent api.Task
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.Unmarshal(readBody(t, r), &sent); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1", Title: "Забрать посылку"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v, want a single clean push", res)
	}
	if sent.ID != "" || sent.Title != "Забрать посылку" || sent.ProjectID != "p1" {
		t.Errorf("sent %+v, want the task without its local id", sent)
	}
	if _, dirty, local := taskRow(t, st, "srv1"); dirty != 0 || local != 0 {
		t.Errorf("task srv1: dirty %d, local %d; want a clean server row", dirty, local)
	}
	if _, err := st.Task(ctx, created.Id); err == nil {
		t.Errorf("the local id %s still names a row", created.Id)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func TestPushOmitsAnUnusableLocalPrefixedResponseID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	responseID := store.LocalIDPrefix + "response-credential-Dd6_+/="

	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		creates++
		writeJSON(t, w, api.Task{ID: responseID, ProjectID: "p1"})
	})
	s := testSyncer(t, st, server)

	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 || res.Failed != 0 || len(res.Errors) != 1 {
		t.Fatalf("result counts or diagnostic count do not describe one settled create")
	}
	if !strings.Contains(res.Errors[0].Error(), "unusable local-prefixed id") {
		t.Error("the create did not report why the response id was unusable")
	}
	if strings.Contains(res.Errors[0].Error(), responseID) {
		t.Error("the create diagnostic echoed the unusable response id")
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the accepted create settled", c)
	}
	if _, dirty, local := taskRow(t, st, created.Id); dirty != 0 || local != 1 {
		t.Errorf("local task state: dirty %d, local %d; want a settled local row", dirty, local)
	}

	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if creates != 1 {
		t.Errorf("the accepted create was sent %d times, want once", creates)
	}
}

func TestPushKeepsTheMarkOfANewerEdit(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}

	edited := false
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
			return
		}
		if r.URL.Path != "/open/v1/task/t1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if edited {

			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		edited = true
		if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Content: model.Ptr("отделение на Ленина")}); err != nil {
			t.Errorf("second edit: %v", err)
		}
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 || res.Requeued != 1 {
		t.Fatalf("result = %+v, want the first entry pushed and the second back in line", res)
	}
	if _, dirty, _ := taskRow(t, st, "t1"); dirty == 0 {
		t.Error("the mark of the second edit was cleared by the first push")
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the second edit still queued", c)
	}
}

func TestPushCompleteWithItemsGoesAsAnUpdate(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	task := openTask("t1", "p1", "Собрать рюкзак")
	task.Items = []model.Item{{Id: "i1", Title: "паспорт", Status: model.ItemOpen}}
	cachedTasks(t, st, "p1", task)
	if _, err := st.CompleteTask(ctx, "t1", store.CompleteOptions{}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	var path, body string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
			return
		}
		path = r.URL.Path
		body = string(readBody(t, r))
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 {
		t.Fatalf("result = %+v, want the completion pushed", res)
	}
	if path != "/open/v1/task/t1" {
		t.Errorf("path = %q, want the update endpoint", path)
	}
	if !strings.Contains(body, `"items"`) || !strings.Contains(body, `"status":2`) {
		t.Errorf("body = %s, want the closed task and its items", body)
	}
}

func TestPushCompleteWithoutItemsUsesTheCompleteEndpoint(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.CompleteTask(ctx, "t1", store.CompleteOptions{}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	var path string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 {
		t.Fatalf("result = %+v, want the completion pushed", res)
	}
	if path != "/open/v1/project/p1/task/t1/complete" {
		t.Errorf("path = %q, want the complete endpoint", path)
	}
}

func seedMove(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	moved, err := st.MoveTask(ctx, "t1", "p2", store.MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatalf("move task: %v", err)
	}
	return moved.Id
}

func TestPushMoveCreatesThenDeletes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	copyID := seedMove(t, st)

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			var sent api.Task
			if err := json.Unmarshal(readBody(t, r), &sent); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			if sent.ProjectID != "p2" || sent.Title != "Забрать посылку" {
				t.Errorf("created %+v, want the task in p2", sent)
			}
			writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p2", Title: "Забрать посылку"})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p2/data":

			writeJSON(t, w, api.ProjectData{
				Project: api.Project{ID: "p2", Name: "Работа"},
				Tasks:   []api.Task{{ID: "srv2", ProjectID: "p2", Title: "Забрать посылку"}},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 2 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v, want both halves pushed", res)
	}
	want := []string{
		"POST /open/v1/task",
		"GET /open/v1/project/p2/data",
		"DELETE /open/v1/project/p1/task/t1",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}

	if _, err := st.Task(ctx, "srv2"); err != nil {
		t.Errorf("the copy is not cached under the server id: %v", err)
	}
	if _, err := st.Task(ctx, copyID); err == nil {
		t.Errorf("the local id %s still names a row", copyID)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func TestPushMoveHoldsTheDeleteUntilTheCopyExists(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	held, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(held) != 1 || held[0].Op != store.OpTaskCreate {
		t.Fatalf("claimed %+v, want the create of the copy", held)
	}

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls %v, want nothing sent", calls)
	}
	if res.Pushed != 0 || res.Failed != 0 || res.Requeued != 1 {
		t.Errorf("result = %+v, want the delete put back and nothing parked", res)
	}

	if len(res.Unsent) != 1 || !strings.Contains(res.Unsent[0].Error(), "waits for the copy") {
		t.Errorf("unsent = %v, want one line saying what the delete is waiting for", res.Unsent)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none: waiting for the copy is not a failure", res.Errors)
	}
	if c := outboxCounts(t, st); c.Pending != 1 || c.Inflight != 1 {
		t.Errorf("outbox %+v, want the delete back in line and the create still held", c)
	}

	if got := attempts(t, st, "t1", store.OpTaskMoveDrop); got != 0 {
		t.Errorf("attempts = %d, want the skip to charge none", got)
	}
}

func TestPushMoveParksTheDeleteWhenTheCopyIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)

		w.WriteHeader(http.StatusBadRequest)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(calls) != 1 || calls[0] != "POST /open/v1/task" {
		t.Fatalf("calls %v, want no delete attempted", calls)
	}
	if res.Failed != 2 {
		t.Errorf("result = %+v, want the create and the delete both parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 2 {
		t.Errorf("outbox %+v, want both entries parked and visible", c)
	}
	if !hasError(res, "no server id was ever recorded for the copy") {
		t.Errorf("errors %v, want one saying no id for the copy was recorded", res.Errors)
	}
	if hasError(res, "was never created") {
		t.Errorf("errors %v, want none claiming the copy does not exist", res.Errors)
	}
}

func TestPushParksAnEditThatAsksForAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.Enqueue(ctx, store.OutboxEntry{
		Target:    store.TargetOpenAPI,
		Op:        store.OpTaskMove,
		TaskID:    "t1",
		ProjectID: "p1",
		Payload:   mustJSON(t, model.TaskEdit{ProjectId: model.Ptr("p2"), Title: model.Ptr("Забрать")}),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var calls int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want the entry parked without a request", calls)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the entry parked", res)
	}
	if !hasError(res, "this update endpoint cannot move a task between lists") {
		t.Errorf("errors %v, want one naming the reason", res.Errors)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasError(res Result, want string) bool {
	for _, err := range res.Errors {
		if strings.Contains(err.Error(), want) {
			return true
		}
	}
	return false
}

func TestPushParksAMutationTheServerCouldNotFind(t *testing.T) {

	for _, tc := range []struct {
		name        string
		wantPending int
		mutate      func(t *testing.T, st *store.Store)
	}{
		{"delete", 0, func(t *testing.T, st *store.Store) {
			if err := st.DeleteTask(context.Background(), "t1"); err != nil {
				t.Fatalf("delete task: %v", err)
			}
		}},
		{"complete", 0, func(t *testing.T, st *store.Store) {
			if _, err := st.CompleteTask(context.Background(), "t1", store.CompleteOptions{}); err != nil {
				t.Fatalf("complete task: %v", err)
			}
		}},
		{"complete with a checklist", 1, func(t *testing.T, st *store.Store) {
			ctx := context.Background()
			if _, err := st.AddTaskItem(ctx, "t1", "молоко"); err != nil {
				t.Fatalf("add a checklist: %v", err)
			}
			if _, err := st.CompleteTask(ctx, "t1", store.CompleteOptions{}); err != nil {
				t.Fatalf("complete task: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seed := openTask("t1", "p1", "Забрать посылку")
			seed.Items = []model.Item{}
			cachedTasks(t, st, "p1", seed)
			tc.mutate(t, st)

			var requests int
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusNotFound)
			})

			res, err := testSyncer(t, st, server).Push(ctx)
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Pushed != 0 {
				t.Errorf("result = %+v, want nothing counted as accepted by the server", res)
			}
			if len(res.Errors) == 0 {
				t.Error("the 404 was not reported anywhere")
			}

			if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != tc.wantPending {
				t.Errorf("outbox %+v, want failed=1 pending=%d", c, tc.wantPending)
			}
			for _, e := range res.Errors {
				if !strings.Contains(e.Error(), "--retry-failed") {
					t.Errorf("parked with %q, which does not say how to put it back", e)
				}
			}
			if requests == 0 {
				t.Error("nothing was sent at all")
			}
		})
	}
}

func TestPushParksARejectedRequest(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	edited, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")})
	if err != nil {
		t.Fatal(err)
	}

	credential := `sync-credential-"quote"\slash/+`
	forms := syncCredentialFixtureForms(credential)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Join(forms, "|")))
	})

	res, err := testSyncerWithToken(t, st, server, Options{}, credential).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v, want the entry parked and reported", res)
	}
	if res.Pushed != 0 {
		t.Errorf("result = %+v, want nothing counted as accepted by the server", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want one parked entry", c)
	}
	assertSyncCredentialFormsAbsent(t, res.Errors[0].Error(), forms)
	assertSyncCredentialFormsAbsent(t, outboxLastError(t, st, store.OpTaskUpdate), forms)

	if _, dirty, _ := taskRow(t, st, "t1"); dirty != 0 {
		t.Errorf("tasks.dirty = %d after the park, want the row let go of", dirty)
	}
	got, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != edited.Title {
		t.Errorf("cached title %q, want the edit the user made, %q", got.Title, edited.Title)
	}
}

func TestPushRequeuesATransientFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}

	credential := `sync-credential-"quote"\slash/+`
	forms := syncCredentialFixtureForms(credential)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Join(forms, "|")))
	})

	res, err := testSyncerWithToken(t, st, server, Options{}, credential).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the entry back in line", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the entry pending again", c)
	}
	if got := attempts(t, st, "t1", store.OpTaskUpdate); got != 1 {
		t.Errorf("attempts = %d, want one request recorded", got)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %d, want one transient failure", len(res.Errors))
	}
	assertSyncCredentialFormsAbsent(t, res.Errors[0].Error(), forms)
	assertSyncCredentialFormsAbsent(t, outboxLastError(t, st, store.OpTaskUpdate), forms)
}

func TestPushParksAMalformedCreateAnswerWithoutPersistingItsBody(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
		t.Fatalf("create task: %v", err)
	}
	credential := `sync-decode-credential-"quote"\slash/+`
	forms := syncCredentialFixtureForms(credential)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[" + strings.Join(forms, "|")))
	})

	res, err := testSyncerWithToken(t, st, server, Options{}, credential).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 || res.Pushed != 0 || len(res.Errors) != 1 {
		t.Fatal("malformed create answer did not leave one parked diagnostic")
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want one parked create", c)
	}
	assertSyncCredentialFormsAbsent(t, res.Errors[0].Error(), forms)
	assertSyncCredentialFormsAbsent(t, outboxLastError(t, st, store.OpTaskCreate), forms)
}

func TestPushStopsOnARejectedToken(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1",
		openTask("t1", "p1", "Забрать посылку"),
		openTask("t2", "p1", "Позвонить в сервис"))
	for _, id := range []string{"t1", "t2"} {
		if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Другое название")}); err != nil {
			t.Fatal(err)
		}
	}

	var calls int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := testSyncer(t, st, server).Push(ctx)
	if !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("Push error = %v, want %v", err, api.ErrUnauthorized)
	}
	if calls != 1 {
		t.Errorf("%d requests, want the pass to stop after the first", calls)
	}

	if c := outboxCounts(t, st); c.Pending != 2 || c.Inflight != 0 || c.Failed != 0 {
		t.Errorf("outbox %+v, want both entries pending", c)
	}
}

func TestPushLeavesAnUnsupportedTargetInTheQueue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.Enqueue(ctx, store.OutboxEntry{Target: store.TargetV2, Op: "focus.push"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the entry left in the queue", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the entry pending", c)
	}

	if len(res.Unsent) != 1 || !strings.Contains(res.Unsent[0].Error(), "not supported yet") {
		t.Errorf("unsent = %v, want one line saying why the entry stayed", res.Unsent)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none: an entry this build cannot send is not a failed pass", res.Errors)
	}
}

func TestPushSendsEachEntryOncePerPass(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1",
		openTask("t1", "p1", "Забрать посылку"),
		openTask("t2", "p1", "Позвонить в сервис"))
	for _, id := range []string{"t1", "t2"} {
		if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Другое название")}); err != nil {
			t.Fatal(err)
		}
	}

	var paths []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	client := api.NewClient("test-token", "0.0.0-test",
		api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(5*time.Second))

	s := New(st, client, Options{})

	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if want := []string{"/open/v1/task/t1", "/open/v1/task/t2"}; !slices.Equal(paths, want) {
		t.Errorf("requests %v, want %v - one attempt per entry per pass", paths, want)
	}
	if res.Requeued != 2 {
		t.Errorf("result = %+v, want both entries requeued", res)
	}
	if c := outboxCounts(t, st); c.Pending != 2 || c.Inflight != 0 {
		t.Errorf("outbox %+v, want both entries pending again", c)
	}
}

func TestPushKeepsAnEditWaitingForACreateAnotherWorkerHolds(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if !store.IsLocalID(created.Id) {
		t.Fatalf("created task id %q, want a local one", created.Id)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	held, _, err := st.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatalf("claim the create: %v", err)
	}
	if len(held) != 1 || held[0].Op != store.OpTaskCreate {
		t.Fatalf("claimed %+v, want the create taken into flight", held)
	}

	var calls int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("result = %+v, want nothing parked", res)
	}
	if calls != 0 {
		t.Errorf("%d request(s) made, want none: the task has no id the server would know", calls)
	}
	if c := outboxCounts(t, st); c.Pending != 1 || c.Inflight != 1 || c.Failed != 0 {
		t.Errorf("outbox %+v, want the edit back in line and the create still held", c)
	}

	if got := attempts(t, st, created.Id, store.OpTaskUpdate); got != 0 {
		t.Errorf("the waiting edit has %d attempt(s) against it, want none", got)
	}
}

func TestPushParksACreateA5xxLeavesInDoubt(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
			if _, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку")); err != nil {
				t.Fatalf("create task: %v", err)
			}
			var creates int
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
					creates++
				}
				w.WriteHeader(status)
			})

			s := testSyncer(t, st, server)
			res, err := s.Push(ctx)
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Failed != 1 || res.Requeued != 0 {
				t.Fatalf("result = %+v, want the create parked", res)
			}
			if c := outboxCounts(t, st); c.Failed != 1 {
				t.Errorf("outbox %+v, want the create parked", c)
			}

			if _, err := s.Push(ctx); err != nil {
				t.Fatalf("second Push: %v", err)
			}
			if creates != 1 {
				t.Errorf("the create went out %d time(s) across two passes, want once", creates)
			}
		})
	}
}

func TestPushRequeuesAnEditA5xxLeavesInDoubt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the edit back in line", res)
	}
}

func TestPushRequeuesAnEditAfterARequestTimeout(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the edit back in line", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the entry pending again", c)
	}
}

func TestPushParksACreateAnsweredWithoutATask(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}
	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		creates++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errorId":"a3f","errorCode":"exceed_quota"}`))
	})

	s := testSyncer(t, st, server)
	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 || res.Pushed != 0 {
		t.Fatalf("result = %+v, want the create parked and nothing counted as pushed", res)
	}
	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if creates != 1 {
		t.Errorf("the create went out %d time(s) across two passes, want once", creates)
	}
}

func TestPushParksAnEditWhoseCreateIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 2 {
		t.Fatalf("result = %+v, want the create and the edit both parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 2 || c.Pending != 0 {
		t.Errorf("outbox %+v, want both entries parked", c)
	}
	for _, e := range res.Errors {
		if !strings.Contains(e.Error(), "--retry-failed") {
			t.Errorf("parked with %q, which does not say how to put it back", e)
		}
	}

	var edit string
	for _, e := range res.Errors {
		if strings.Contains(e.Error(), store.OpTaskUpdate) {
			edit = e.Error()
		}
	}
	if edit == "" {
		t.Fatalf("errors = %v, want the parked edit among them", res.Errors)
	}
	if !strings.Contains(edit, "not known here") {
		t.Errorf("parked with %q, want it to leave open what the server has", edit)
	}
}

func TestPushSendsTheEditBehindACreateUnderTheServerID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	var paths []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 2 || res.Failed != 0 {
		t.Fatalf("result = %+v, want both entries pushed", res)
	}
	want := []string{"/open/v1/task", "/open/v1/task/srv1", "/open/v1/project/p1/task/srv1"}
	if !slices.Equal(paths, want) {
		t.Errorf("requests %v, want %v", paths, want)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
	if _, dirty, local := taskRow(t, st, "srv1"); dirty != 0 || local != 0 {
		t.Errorf("task srv1: dirty %d, local %d; want a clean server row", dirty, local)
	}
}

func TestPushReachesTheWholeQueueAfterAFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ids := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, openTask(id, "p1", "Задача "+id))
	}
	cachedTasks(t, st, "p1", tasks...)
	for _, id := range ids {
		if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Другое " + id)}); err != nil {
			t.Fatal(err)
		}
	}

	var paths []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, api.Task{ID: "x", ProjectID: "p1"})
			return
		}
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/open/v1/task/t1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(t, w, api.Task{ID: "x", ProjectID: "p1"})
	})

	client := api.NewClient("test-token", "0.0.0-test",
		api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(5*time.Second))
	res, err := New(st, client, Options{}).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(paths) != len(ids) {
		t.Errorf("%d of %d queued mutations sent in one pass: %v", len(paths), len(ids), paths)
	}
	if res.Pushed != 5 || res.Requeued != 1 {
		t.Errorf("result = %+v, want five through and one back in line", res)
	}
}

func TestPushCancelPutsTheBatchBack(t *testing.T) {
	base := context.Background()
	st := testStore(t)
	ids := []string{"t1", "t2", "t3", "t4"}
	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, openTask(id, "p1", "Задача "+id))
	}
	cachedTasks(t, st, "p1", tasks...)
	for _, id := range ids {
		if _, err := st.UpdateTask(base, id, model.TaskEdit{Title: model.Ptr("Другое " + id)}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	if _, err := testSyncer(t, st, server).Push(ctx); err == nil {
		t.Fatal("Push returned no error after the context was cancelled")
	}
	if c := outboxCounts(t, st); c.Inflight != 0 || c.Pending != len(ids) {
		t.Errorf("outbox %+v, want all %d entries back in line", c, len(ids))
	}
	for _, id := range ids {
		if got := attempts(t, st, id, store.OpTaskUpdate); got != 0 {
			t.Errorf("%s has %d attempt(s) against it, want none", id, got)
		}
	}
}

func TestPushSpendsNoAttemptOnAnEntryItNeverSent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1",
		openTask("t1", "p1", "Забрать посылку"),
		openTask("t2", "p1", "Позвонить в сервис"))
	for _, id := range []string{"t1", "t2"} {
		if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Другое")}); err != nil {
			t.Fatal(err)
		}
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	s := testSyncer(t, st, server)
	for i := 0; i < 3; i++ {
		if _, err := s.Push(ctx); err == nil {
			t.Fatalf("pass %d: want the rejected token to end the pass", i)
		}
	}
	if got := attempts(t, st, "t2", store.OpTaskUpdate); got != 0 {
		t.Errorf("t2 has %d attempt(s) against it after three passes that never sent it", got)
	}
}

func TestPushStopsOnAForbiddenAnswer(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1",
		openTask("t1", "p1", "Забрать посылку"),
		openTask("t2", "p1", "Позвонить в сервис"))
	for _, id := range []string{"t1", "t2"} {
		if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Другое название")}); err != nil {
			t.Fatal(err)
		}
	}

	var calls int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := testSyncer(t, st, server).Push(ctx)
	var se *api.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		t.Fatalf("Push error = %v, want the 403", err)
	}
	if calls != 1 {
		t.Errorf("%d requests, want the pass to stop after the first", calls)
	}
	if c := outboxCounts(t, st); c.Pending != 2 || c.Failed != 0 || c.Inflight != 0 {
		t.Errorf("outbox %+v, want both entries back in line and nothing parked", c)
	}
}

func TestPushParksACreateWhoseAnswerMayHaveBeenLost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	server.Close()

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 || res.Requeued != 0 {
		t.Fatalf("result = %+v, want the create parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 {
		t.Errorf("outbox %+v, want the create parked", c)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "--retry-failed") {
		t.Errorf("errors = %v, want one that says how to put the entry back", res.Errors)
	}
}

func TestPushRequeuesAnEditWhoseAnswerMayHaveBeenLost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	server.Close()

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the edit back in line", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the edit pending again", c)
	}
}

func TestPushParksACreateTheUserCancelled(t *testing.T) {
	base := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(base, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		creates++
		cancel()
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	s := testSyncer(t, st, server)
	res, _ := s.Push(ctx)
	if res.Failed != 1 || res.Requeued != 0 {
		t.Fatalf("result = %+v, want the cancelled create parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want the create parked", c)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "--retry-failed") {
		t.Errorf("errors = %v, want one that says how to put the entry back", res.Errors)
	}

	var parked *ParkedError
	if len(res.Errors) != 1 || !errors.As(res.Errors[0], &parked) {
		t.Errorf("errors = %v, want the park marked as one so the report survives the interrupt filter", res.Errors)
	}

	if _, err := s.Push(base); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if creates != 1 {
		t.Errorf("the create went out %d times, want once", creates)
	}
}

func TestPushCommitRollsBackWhenTheLeaseIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {

		if _, _, err := st.Claim(ctx, 10, 0); err != nil {
			t.Errorf("take the lease over: %v", err)
		}
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 0 {
		t.Errorf("result = %+v, want nothing counted as pushed", res)
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], store.ErrLeaseLost) {
		t.Errorf("errors = %v, want the lost lease reported", res.Errors)
	}

	if _, _, local := taskRow(t, st, created.Id); local != 1 {
		t.Errorf("task %s: local %d, want the row still the local one it was created as", created.Id, local)
	}
	if _, err := st.Task(ctx, "srv1"); err == nil {
		t.Error("the server id was swapped in by a worker that had lost the entry")
	}

	if c := outboxCounts(t, st); c.Failed != 1 {
		t.Errorf("outbox %+v, want the create parked rather than put back in line", c)
	}
}

func TestPushCreateMergesIntoARowThePullBroughtIn(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {

		seedTasks(t, st, "p1", openTask("srv1", "p1", "Забрать посылку"))
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 {
		t.Fatalf("result = %+v, want the create pushed", res)
	}
	if _, err := st.Task(ctx, created.Id); err == nil {
		t.Errorf("the local twin %s is still cached beside the server's row", created.Id)
	}
	if _, dirty, local := taskRow(t, st, "srv1"); dirty != 0 || local != 0 {
		t.Errorf("task srv1: dirty %d, local %d; want the pulled row as it was", dirty, local)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func TestPushCancelAfterTheLastEntryIsStillReported(t *testing.T) {
	base := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(base, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {

		cancel()
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Push = %v, want the cancellation reported", err)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the cancelled create parked", res)
	}
}

func TestSettleTimeoutOutlastsTheDatabaseWait(t *testing.T) {
	st := testStore(t)
	var ms int64
	if err := st.DB().QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	busy := time.Duration(ms) * time.Millisecond
	if busy <= 0 {
		t.Fatalf("busy_timeout = %v: a database that waits for nothing is not what these were sized against", busy)
	}
	if settleTimeout <= busy {
		t.Errorf("settleTimeout is %v and busy_timeout %v: the deadline gives up before the database does, on exactly the writes whose failure costs a duplicate task", settleTimeout, busy)
	}
}

func TestPushOnACancelledContextSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a pass on a cancelled context made a request")
	})
	if _, err := testSyncer(t, st, server).Push(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Push = %v, want the cancellation reported", err)
	}
}

func TestPushCommitOutlastsACancelledContext(t *testing.T) {
	base := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(base, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	items, _, err := st.Claim(base, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %d entr(y/ies), %v; want the queued create", len(items), err)
	}

	ctx, cancel := context.WithCancel(base)
	cancel()
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the commit made a request")
	})
	if err := testSyncer(t, st, server).commit(ctx, items[0], "srv1"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the entry gone rather than back in line", c)
	}
	if _, dirty, local := taskRow(t, st, "srv1"); dirty != 0 || local != 0 {
		t.Errorf("task srv1: dirty %d, local %d; want a clean server row", dirty, local)
	}
	if _, err := st.Task(base, created.Id); err == nil {
		t.Errorf("the local id %s still names a row", created.Id)
	}
}

func TestPushSendsAnEntryRaisedFromTheParkedState(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Другое")}); err != nil {
		t.Fatal(err)
	}

	var requests int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
			return
		}
		requests++
		if requests == 1 {

			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	s := testSyncer(t, st, server)
	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if c := outboxCounts(t, st); c.Failed != 1 {
		t.Fatalf("outbox %+v, want the rejected entry parked", c)
	}

	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if requests != 1 {
		t.Fatalf("the parked entry went out again after %d request(s), want it left alone", requests)
	}

	n, err := st.RetryFailed(ctx)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if n != 1 {
		t.Errorf("RetryFailed raised %d entr(y/ies), want 1", n)
	}
	if c := outboxCounts(t, st); c.Pending != 1 || c.Failed != 0 {
		t.Fatalf("outbox %+v, want the entry back in line", c)
	}

	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 1 || requests != 2 {
		t.Errorf("result = %+v after %d request(s), want the raised entry sent", res, requests)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func TestPushParksACreateWhoseCommitDidNotLand(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `DROP TABLE events`); err != nil {
		t.Fatalf("drop the journal: %v", err)
	}

	serverID := "response-task-credential-Aa9_+/="
	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		creates++
		writeJSON(t, w, api.Task{ID: serverID, ProjectID: "p1"})
	})

	s := testSyncer(t, st, server)
	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v, want the create parked and nothing counted as pushed", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Fatalf("outbox %+v, want the create parked - if it is empty the commit did not fail "+
			"and this test no longer injects what it means to", c)
	}
	var parked *ParkedError
	if len(res.Errors) != 1 || !errors.As(res.Errors[0], &parked) {
		t.Errorf("errors = %v, want one park the report can be recognised by", res.Errors)
	}
	if len(res.Errors) == 1 {
		got := res.Errors[0].Error()
		if strings.Contains(got, serverID) {
			t.Error("the immediate accepted-create diagnostic echoed the unrecorded response id")
		}
		if strings.Contains(got, "--retry-failed") {
			t.Error("the accepted-create diagnostic points at an unsafe retry")
		}
	}
	if strings.Contains(outboxLastError(t, st, store.OpTaskCreate), serverID) {
		t.Error("the persisted accepted-create diagnostic echoed the unrecorded response id")
	}

	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if creates != 1 {
		t.Errorf("the create went out %d time(s) across two passes, want once", creates)
	}
}

func TestAdoptServerIDReaddressesEverythingTheCreateRenamed(t *testing.T) {
	const localID = "tt-local-c0ffee"

	batch := func() []store.OutboxItem {
		return []store.OutboxItem{
			{
				OutboxEntry: store.OutboxEntry{
					Op: store.OpTaskUpdate, TaskID: localID, Rev: 4,
					Payload: []byte(`{"title":"Zabrat posylku na pochte"}`),
				},
				Seq: 2,
			},
			{
				OutboxEntry: store.OutboxEntry{
					Op: store.OpTaskMoveDrop, TaskID: "t9", Rev: 7,
					Payload: []byte(`{"copy_id":"` + localID + `","copy_project_id":"p2"}`),
				},
				Seq: 3,
			},
			{
				OutboxEntry: store.OutboxEntry{
					Op: store.OpTaskComplete, TaskID: "t8", Rev: 9, Payload: []byte(`{}`),
				},
				Seq: 4,
			},
		}
	}

	renamed := batch()
	adoptServerID(renamed, localID, idSwap{serverID: "srv1"})
	if renamed[0].TaskID != "srv1" {
		t.Errorf("the edit is addressed at %q, want the id the server gave the task", renamed[0].TaskID)
	}
	if renamed[0].Rev != 4 {
		t.Errorf("the edit carries revision %d, want the 4 it was queued with: the row was renamed, "+
			"not replaced, and the revision still counts against it", renamed[0].Rev)
	}
	if got := string(renamed[1].Payload); !strings.Contains(got, "srv1") || strings.Contains(got, localID) {
		t.Errorf("the delete half of the move waits for %s, want it to name the copy by the "+
			"server's id: sent as it stands it reads that the copy was never made", got)
	}
	if renamed[1].TaskID != "t9" || renamed[1].Rev != 7 {
		t.Errorf("the delete half is now %q at revision %d, want its own task and revision untouched: "+
			"the create renamed the copy it names, not the task it deletes", renamed[1].TaskID, renamed[1].Rev)
	}
	if renamed[2].TaskID != "t8" || renamed[2].Rev != 9 || string(renamed[2].Payload) != `{}` {
		t.Errorf("the entry of another task came out %+v, want it as it was", renamed[2])
	}

	merged := batch()
	adoptServerID(merged, localID, idSwap{serverID: "srv1", merged: true})
	if merged[0].TaskID != "srv1" || merged[0].Rev != 0 {
		t.Errorf("after a merged swap the edit is %q at revision %d, want srv1 at 0",
			merged[0].TaskID, merged[0].Rev)
	}
	if merged[1].Rev != 7 {
		t.Errorf("the entry of another task lost its revision (%d), want the 7 it was queued with: "+
			"its own row was not touched by the swap", merged[1].Rev)
	}
}

func TestTheEntriesNothingWasSentForAreCapped(t *testing.T) {
	var res Result
	for i := 0; i < maxResultUnsent+7; i++ {
		res.addUnsent(fmt.Errorf("outbox %d: task.update of t1 waits for the create of that task, still queued", i))
	}
	if len(res.Unsent) != maxResultUnsent {
		t.Errorf("%d entries kept, want the cap of %d", len(res.Unsent), maxResultUnsent)
	}

	if got := res.Unsent[len(res.Unsent)-1].Error(); !strings.Contains(got, "and 8 more") {
		t.Errorf("last entry = %q, want the count of the ones left out", got)
	}
	if got := res.UnsentCount(); got != maxResultUnsent+7 {
		t.Errorf("UnsentCount = %d, want all %d of them", got, maxResultUnsent+7)
	}

	var other Result
	other.addUnsent(errors.New(`outbox 99: target "v2" is not supported yet`))
	res.merge(other)
	if len(res.Unsent) != maxResultUnsent {
		t.Errorf("%d entries after merging, want the cap of %d", len(res.Unsent), maxResultUnsent)
	}
	if got := res.UnsentCount(); got != maxResultUnsent+8 {
		t.Errorf("UnsentCount after merging = %d, want %d", got, maxResultUnsent+8)
	}
}

func outboxRev(t *testing.T, st *store.Store, taskID, op string) int64 {
	t.Helper()
	var rev sql.NullInt64
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT rev FROM outbox WHERE task_id = ? AND op = ?`, taskID, op).Scan(&rev)
	if err != nil {
		t.Fatalf("read the revision of %s %s: %v", op, taskID, err)
	}
	return rev.Int64
}

func TestAMergedSwapBlanksTheQueuedRevisions(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {

			seedTasks(t, st, "p1", openTask("srv1", "p1", "Старое название"))
			for _, title := range []string{"Первая правка", "Вторая правка"} {
				if _, err := st.UpdateTask(ctx, "srv1", model.TaskEdit{Title: model.Ptr(title)}); err != nil {
					t.Errorf("edit srv1: %v", err)
				}
			}
			writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := testSyncer(t, st, server).Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if rev := outboxRev(t, st, "srv1", store.OpTaskUpdate); rev != 0 {
		t.Errorf("the queued edit still carries revision %d, want none: the row it was counted "+
			"against was dropped by the swap, and the row it would be compared against now counts "+
			"from one of its own", rev)
	}
	if _, dirty, _ := taskRow(t, st, "srv1"); dirty == 0 {
		t.Fatal("srv1 was marked pushed by the revision of a row that no longer exists")
	}

	seedTasks(t, st, "p1", openTask("srv1", "p1", "Старое название"))
	got, err := st.Task(ctx, "srv1")
	if err != nil {
		t.Fatalf("read srv1: %v", err)
	}
	if got.Title != "Вторая правка" {
		t.Errorf("title after the pull = %q, want the unpushed edit kept", got.Title)
	}
}

func TestPushTakesAnUpdateAnsweredWithNoBody(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusNoContent} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
			if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
				t.Fatal(err)
			}
			var requests int
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {

					writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
					return
				}
				requests++
				w.WriteHeader(status)
			})

			res, err := testSyncer(t, st, server).Push(ctx)
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Pushed != 1 || res.Failed != 0 {
				t.Fatalf("result = %+v, want the edit pushed", res)
			}
			if requests != 1 {
				t.Errorf("%d request(s) made, want one", requests)
			}
			if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
				t.Errorf("outbox %+v, want the entry gone", c)
			}
			if _, dirty, _ := taskRow(t, st, "t1"); dirty != 0 {
				t.Errorf("tasks.dirty = %d, want the row marked pushed", dirty)
			}
		})
	}
}

func TestPushParksACreateAnUnknownAnswerLeavesInDoubt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"408 request timeout", http.StatusRequestTimeout},
		{"304 not modified", http.StatusNotModified},
		{"307 without a location", http.StatusTemporaryRedirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
			if _, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
				t.Fatalf("create task: %v", err)
			}
			var creates int
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
					creates++
					if creates == 1 {

						w.WriteHeader(tc.status)
						return
					}
				}
				writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
			})

			s := testSyncer(t, st, server)
			res, err := s.Push(ctx)
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if res.Failed != 1 || res.Requeued != 0 {
				t.Fatalf("result = %+v, want the create parked", res)
			}
			if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
				t.Errorf("outbox %+v, want the create parked", c)
			}

			if _, err := s.Push(ctx); err != nil {
				t.Fatalf("second Push: %v", err)
			}
			if creates != 1 {
				t.Errorf("the create went out %d time(s) across two passes, want once", creates)
			}
		})
	}
}

func TestPushHoldsACreateWhoseParkCouldNotBeWritten(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER block_the_park BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'failed'
		BEGIN SELECT RAISE(ABORT, 'the park could not be written'); END;`); err != nil {
		t.Fatalf("block the park: %v", err)
	}

	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
			creates++
			if creates == 1 {

				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	s := testSyncer(t, st, server)
	if _, err := s.Push(ctx); err == nil {
		t.Fatal("Push came back clean although the park could not be written")
	}
	if c := outboxCounts(t, st); c.Inflight != 1 || c.Pending != 0 || c.Failed != 0 {
		t.Fatalf("outbox %+v, want the create left in flight", c)
	}

	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if creates != 1 {
		t.Errorf("the create went out %d time(s) across two passes, want once", creates)
	}
}

func TestPushStopsOnARateLimitAndSaysWhenToComeBack(t *testing.T) {
	soon := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{"seconds", "120", "asks for 2m0s"},
		{"an http-date", soon, "asks for 1m"},
		{"no header at all", "", "did not say"},
		{"a header this build cannot read", "in a little while", "did not say"},
		{"a header the server has already let expire", "0", "no wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"), openTask("t2", "p1", "Pozvonit v servis"))
			for _, id := range []string{"t1", "t2"} {
				if _, err := st.UpdateTask(ctx, id, model.TaskEdit{Title: model.Ptr("Drugoe nazvanie")}); err != nil {
					t.Fatal(err)
				}
			}
			var calls int
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			})

			start := time.Now()
			res, err := testSyncer(t, st, server).Push(ctx)
			elapsed := time.Since(start)
			if !errors.Is(err, api.ErrRateLimited) {
				t.Fatalf("Push = %v, want the pass to end on the rate limit", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the pass ended with %q, want it to say %q", err, tc.want)
			}
			if calls != 1 {
				t.Errorf("%d request(s) made, want the pass to stop at the first refusal", calls)
			}
			if elapsed > 5*time.Second {
				t.Errorf("the pass took %v, want it to end rather than sit out the limit", elapsed)
			}
			if res.Failed != 0 {
				t.Errorf("result = %+v, want nothing parked: a 429 refuses the request before acting on it", res)
			}

			if res.Requeued != 1 {
				t.Errorf("result = %+v, want the claimed edit reported as back in line", res)
			}
			if c := outboxCounts(t, st); c.Pending != 2 || c.Failed != 0 {
				t.Errorf("outbox %+v, want both edits back in line", c)
			}
		})
	}
}

func TestPushWritesTheServerIDDownWhenTheCommitFails(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	serverID := "response-task-credential-Bb8_+/="
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER block_the_commit BEFORE DELETE ON outbox
		BEGIN SELECT RAISE(ABORT, 'the commit could not be written'); END;`); err != nil {
		t.Fatalf("block the commit: %v", err)
	}

	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
			creates++
		}
		writeJSON(t, w, api.Task{ID: serverID, ProjectID: "p1"})
	})

	s := testSyncer(t, st, server)
	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 || res.Pushed != 0 {
		t.Fatalf("result = %+v, want the create parked and nothing counted as pushed", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 {
		t.Fatalf("outbox %+v, want the create parked - if it is empty the commit did not fail "+
			"and this test no longer injects what it means to", c)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %d, want one accepted-create diagnostic", len(res.Errors))
	}
	if strings.Contains(res.Errors[0].Error(), serverID) {
		t.Error("the immediate accepted-create diagnostic echoed the recorded response id")
	}
	if strings.Contains(outboxLastError(t, st, store.OpTaskCreate), serverID) {
		t.Error("the persisted accepted-create diagnostic echoed the recorded response id")
	}

	if _, err := st.Task(ctx, serverID); err != nil {
		t.Fatal("the cache does not know the task by the server id used for recovery")
	}
	if _, err := st.Task(ctx, created.Id); err == nil {
		t.Error("the row is still in the cache under its local id as well")
	}
	if got := outboxTaskID(t, st, store.OpTaskCreate); got != serverID {
		t.Error("the parked entry does not retain the server id used for recovery")
	}

	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER block_the_commit`); err != nil {
		t.Fatalf("unblock the commit: %v", err)
	}
	if n, err := st.RetryFailed(ctx); err != nil || n != 1 {
		t.Fatalf("RetryFailed = %d, %v", n, err)
	}
	res, err = s.Push(ctx)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if creates != 1 {
		t.Fatalf("the create went out %d time(s) across two passes: a second task on the server", creates)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the raised entry finished rather than parked again", c)
	}

	if res.Pushed != 1 {
		t.Errorf("result = %+v, want the raised entry accounted for", res)
	}

	if len(res.Unsent) != 1 || !strings.Contains(res.Unsent[0].Error(), "nothing was sent") {
		t.Errorf("unsent = %v, want the pass to say why the entry went without a request", res.Unsent)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none: the entry was finished, not failed", res.Errors)
	}
}

func TestPushKeepsTheBatchOnTheLocalIDWhenTheIDCouldNotBeWritten(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx, `DROP TABLE events`); err != nil {
		t.Fatalf("drop the journal: %v", err)
	}

	var paths []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if want := []string{"/open/v1/task"}; !slices.Equal(paths, want) {
		t.Errorf("requests %v, want %v - the edit went out under an id nothing wrote down", paths, want)
	}
	if res.Failed != 2 || res.Pushed != 0 {
		t.Fatalf("result = %+v, want the create and the edit behind it both parked", res)
	}

	if c := outboxCounts(t, st); c.Failed != 2 || c.Pending != 0 {
		t.Fatalf("outbox %+v, want both entries parked", c)
	}
	if got := outboxTaskID(t, st, store.OpTaskUpdate); got != created.Id {
		t.Errorf("the parked edit is addressed at %q, want the local id its queue row still carries", got)
	}

	if _, dirty, local := taskRow(t, st, created.Id); dirty != 0 || local != 1 {
		t.Errorf("the cached row: dirty %d, local %d; want the mark released and the row still local", dirty, local)
	}
}

func TestPushSendsNoCreateForATaskTheServerAlreadyMade(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx,
		`UPDATE outbox SET task_id = 'srv1' WHERE op = 'task.create'`); err != nil {
		t.Fatalf("readdress the create: %v", err)
	}

	var creates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/open/v1/task" {
			creates++
		}
		writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if creates != 0 {
		t.Errorf("POST /open/v1/task went out %d time(s), want none: the server has the task already", creates)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the entry finished rather than left to be sent again", c)
	}
	if len(res.Unsent) != 1 || !strings.Contains(res.Unsent[0].Error(), "srv1") {
		t.Errorf("unsent = %v, want one naming the task nothing was sent for", res.Unsent)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none: an entry closed without a request is not a failure", res.Errors)
	}
}

func TestPushOmitsTheServerIDWhenTheParkWillNotLand(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	serverID := "response-task-credential-Cc7_+/="
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку")); err != nil {
		t.Fatalf("create task: %v", err)
	}
	for _, ddl := range []string{

		`CREATE TRIGGER refuse_the_swap BEFORE UPDATE OF id ON tasks
		 BEGIN SELECT RAISE(ABORT, 'the swap will not be written'); END`,

		`CREATE TRIGGER refuse_the_park BEFORE UPDATE ON outbox WHEN NEW.state = 'failed'
		 BEGIN SELECT RAISE(ABORT, 'the park will not be written'); END`,
	} {
		if _, err := st.DB().ExecContext(ctx, ddl); err != nil {
			t.Fatalf("stage the failing write: %v", err)
		}
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, api.Task{ID: serverID, ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err == nil {
		t.Fatal("Push came back clean while neither the commit nor the park landed")
	}
	var diagnostic error
	for _, e := range res.Errors {
		if strings.Contains(e.Error(), "do not put this entry back in line") {
			diagnostic = e
		}
	}
	if diagnostic == nil {
		t.Fatal("the pass lost the accepted-create warning when the park failed")
	}
	if strings.Contains(diagnostic.Error(), serverID) {
		t.Error("the immediate accepted-create diagnostic echoed the unrecorded response id")
	}

	var parked *ParkedError
	if !errors.As(diagnostic, &parked) {
		t.Error("the accepted-create warning lost its parked classification")
	}
}

func TestAnAttemptIsLeftOnlyWhereTheRequestWasMade(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		attempts int
		queued   store.OutboxCounts
	}{

		{"a rate the server will not take", http.StatusTooManyRequests, 0, store.OutboxCounts{}},
		{"a token the server will not take", http.StatusUnauthorized, 0, store.OutboxCounts{}},

		{"the server broke", http.StatusInternalServerError, 1, store.OutboxCounts{Pending: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
			created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
			if err != nil {
				t.Fatalf("create task: %v", err)
			}
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})

			if _, err := testSyncer(t, st, server).Push(ctx); err != nil && tc.attempts != 0 {
				t.Fatalf("Push: %v", err)
			}
			if n := attempts(t, st, created.Id, store.OpTaskCreate); n != tc.attempts {
				t.Errorf("attempts = %d after http %d, want %d", n, tc.status, tc.attempts)
			}

			if err := st.DeleteTask(ctx, created.Id); err != nil {
				t.Fatalf("delete task: %v", err)
			}
			if c := outboxCounts(t, st); c != tc.queued {
				t.Errorf("outbox %+v after the delete, want %+v", c, tc.queued)
			}
		})
	}
}

func TestAParkedCreateCarriesTheMarkItsRecordReads(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("result = %+v, want the refused create parked", res)
	}

	if n := attempts(t, st, created.Id, store.OpTaskCreate); n < 1 {
		t.Fatalf("the parked create carries %d attempt(s), want the one the claim charged", n)
	}
	dropped, err := st.DropParked(ctx)
	if err != nil {
		t.Fatalf("DropParked: %v", err)
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped %+v, want the parked create", dropped)
	}
	if !dropped[0].Requested {
		t.Errorf("entry %d was parked by a pass that took it up and came back unmarked; the record "+
			"then tells the user the server never had the task", dropped[0].Seq)
	}
	if !dropped[0].TaskRemoved {
		t.Errorf("dropped %+v, want the task of a create made offline gone with its entry", dropped[0])
	}
}

func TestPushParksTheServerSOwnCreateWithoutWarningOffTheRetry(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := st.ReplaceLocalID(ctx, created.Id, "srv1"); err != nil {
		t.Fatalf("take the server's id on: %v", err)
	}

	if _, err := st.DB().ExecContext(ctx,
		`CREATE TRIGGER refuse_the_commit BEFORE DELETE ON outbox
		 BEGIN SELECT RAISE(ABORT, 'the entry will not be dropped'); END`); err != nil {
		t.Fatalf("stage the failing commit: %v", err)
	}

	var requests int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if requests != 0 {
		t.Errorf("%d request(s) went out, want none: the server made this task already", requests)
	}
	if res.Failed != 1 {
		t.Fatalf("result = %+v, want the entry parked", res)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want the one entry parked", c)
	}
	var parked *ParkedError
	for _, e := range res.Errors {
		if errors.As(e, &parked) {
			break
		}
	}
	if parked == nil {
		t.Fatalf("errors = %v, want the park among them", res.Errors)
	}
	if !strings.Contains(parked.Error(), "srv1") || !strings.Contains(parked.Error(), "raising it costs nothing") {
		t.Errorf("parked with %q, want it to name the task and say the entry can be raised for nothing",
			parked)
	}

	if strings.Contains(parked.Error(), "do not put this entry back in line") {
		t.Errorf("parked with %q, which warns the user off a retry that sends nothing", parked)
	}
}
