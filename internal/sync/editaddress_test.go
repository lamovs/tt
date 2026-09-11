package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func queuedPayloads(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `SELECT payload FROM outbox ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a queued payload: %v", err)
		}
		out = append(out, string(payload))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	return out
}

func TestPushKeepsAnEditWhoseTaskAnotherClientMoved(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "p2", Name: "Rabota"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))

	const serverProject = "p2"
	serverTitle := "Zabrat posylku"

	var updates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/t1":
			updates++
			var body struct {
				ProjectID string `json:"projectId"`
				Title     string `json:"title"`
			}
			if err := json.Unmarshal(readBody(t, r), &body); err != nil {
				t.Errorf("decode update body: %v", err)
			}
			if body.ProjectID == serverProject {
				if body.Title != "" {
					serverTitle = body.Title
				}
				writeJSON(t, w, api.Task{ID: "t1", ProjectID: serverProject, Title: serverTitle})
				return
			}

			writeJSON(t, w, map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/task/t1":

			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorCode":"resource_not_found","errorMessage":"task not found: t1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p2/task/t1":
			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p2", Title: serverTitle})
		case r.URL.Path == "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}, {ID: "p2", Name: "Rabota"}})
		case r.URL.Path == "/open/v1/project/p1/data":
			writeJSON(t, w, map[string]any{"tasks": []any{}, "columns": []any{}})
		case r.URL.Path == "/open/v1/project/p2/data":
			_, _ = w.Write([]byte(`{"project":{"id":"p2","name":"Rabota"},"tasks":[` +
				`{"id":"t1","projectId":"p2","title":"` + serverTitle + `","status":0}],"columns":[]}`))
		case r.URL.Path == inboxPath:
			_, _ = w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	const edited = "Zabrat posylku na pochte"
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr(edited)}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	res, err := testSyncer(t, st, server).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if updates != 1 {
		t.Fatalf("update requests = %d, want the one the queue held", updates)
	}
	if serverTitle == edited {
		t.Fatal("the server applied the update at the wrong address; nothing to report")
	}
	if res.Pushed != 0 {
		t.Errorf("result = %+v, want nothing counted as pushed: the update was applied in no part", res)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the edit parked rather than dropped", res)
	}
	if !hasError(res, "not in list p1 any more") {
		t.Errorf("errors %v, want one saying where the task is not", res.Errors)
	}

	queued := queuedPayloads(t, st)
	if len(queued) != 1 || !strings.Contains(queued[0], edited) {
		t.Errorf("the queue holds %v, want the user's edit kept", queued)
	}
}

func TestPushRequeuesAnEditItCouldNotConfirm(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var reads, probes, updates int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/data":

			probes++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errorCode":"unknown_exception"}`))
		case r.Method == http.MethodGet:
			reads++
			if reads == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errorCode":"unknown_exception"}`))
				return
			}
			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
		default:
			updates++
			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
		}
	})

	s := testSyncer(t, st, server)
	res, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if reads != 1 {
		t.Fatalf("%d read(s) after the update, want one", reads)
	}
	if probes != 1 {
		t.Fatalf("the list was asked about %d time(s), want the one question the refused read raises", probes)
	}
	if res.Pushed != 0 || res.Failed != 0 || res.Requeued != 1 {
		t.Errorf("result = %+v, want the entry back in line rather than parked", res)
	}
	if !hasError(res, "could not be read back") {
		t.Errorf("errors %v, want one saying the update could not be confirmed", res.Errors)
	}
	if c := outboxCounts(t, st); c.Pending != 1 || c.Failed != 0 {
		t.Errorf("outbox %+v, want the edit waiting for the next pass", c)
	}

	res, err = s.Push(ctx)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if res.Pushed != 1 {
		t.Errorf("second pass = %+v, want the edit sent", res)
	}
	if updates != 2 {
		t.Errorf("the update went out %d time(s), want the second pass to send it again", updates)
	}

	if probes != 1 {
		t.Errorf("the list was asked about %d time(s), want no question on the pass that got its answer",
			probes)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the edit gone from the queue", c)
	}
}

func TestPushKeepsAnEditWhoseConfirmingReadWasRefused(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var reads int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errorCode":"invalid_request"}`))
			return
		}
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if reads != 1 {
		t.Fatalf("%d read(s) after the update, want one", reads)
	}
	if res.Pushed != 0 || res.Failed != 1 {
		t.Errorf("result = %+v, want the entry kept rather than counted as sent", res)
	}
	if !hasError(res, "could not be read back") {
		t.Errorf("errors %v, want one saying the update could not be confirmed", res.Errors)
	}
	if payloads := queuedPayloads(t, st); len(payloads) != 1 ||
		!strings.Contains(payloads[0], "Zabrat posylku na pochte") {
		t.Errorf("queue holds %v, want the parked entry to keep the edit", payloads)
	}
}

func TestPushRequeuesAnEditTheInterruptLeftUnconfirmed(t *testing.T) {
	base := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(base, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			cancel()
			return
		}
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Push = %v, want the pass to end on the interrupt", err)
	}
	if res.Failed != 0 {
		t.Errorf("result = %+v, want nothing parked over an interrupted read", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Errorf("outbox %+v, want the edit back in line", c)
	}
}

func TestPushKeepsAnEditTheAnswerPlacesInAnotherProject(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	responseProjectID := "response-project-credential-Aa9_+/="
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {

			writeJSON(t, w, api.Task{ID: "t1", ProjectID: responseProjectID, Title: "Zabrat posylku"})
			return
		}
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 0 || res.Failed != 1 {
		t.Errorf("result = %+v, want the entry kept rather than counted as sent", res)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %d, want one project-mismatch diagnostic", len(res.Errors))
	}
	if !hasError(res, "is in a different list") {
		t.Error("the parked edit did not retain the static project-mismatch diagnostic")
	}
	if strings.Contains(res.Errors[0].Error(), responseProjectID) {
		t.Error("the immediate mismatch diagnostic echoed the response project id")
	}
	if strings.Contains(outboxLastError(t, st, store.OpTaskUpdate), responseProjectID) {
		t.Error("the persisted mismatch diagnostic echoed the response project id")
	}
}

func TestPushKeepsAnEditWhoseListAnotherClientDeleted(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/t1":

			writeJSON(t, w, map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/task/t1":

			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errorCode":"unknown_exception"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/data":

			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Pushed != 0 || res.Failed != 1 || res.Requeued != 0 {
		t.Errorf("result = %+v, want the edit kept rather than sent round again", res)
	}
	if !hasError(res, "is not on the server any more") {
		t.Errorf("errors %v, want one saying the list the edit is addressed at is gone", res.Errors)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want the edit out of the line rather than waiting in it", c)
	}

	if payloads := queuedPayloads(t, st); len(payloads) != 1 ||
		!strings.Contains(payloads[0], "Zabrat posylku na pochte") {
		t.Errorf("queue holds %v, want the parked entry to keep the edit", payloads)
	}
}

func TestPushMakesNoConfirmingReadAfterTheLeaseIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/t1":

			if _, _, err := st.Claim(ctx, 10, 0); err != nil {
				t.Errorf("take the lease over: %v", err)
			}
			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/task/t1":
			writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "GET") {
			t.Errorf("calls %v, want no read after the update: the entry had stopped being this worker's", calls)
		}
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], store.ErrLeaseLost) {
		t.Errorf("errors = %v, want the lost lease reported", res.Errors)
	}
	if res.Pushed != 0 || res.Failed != 0 || res.Requeued != 0 {
		t.Errorf("result = %+v, want nothing written to an entry another claim holds", res)
	}
	if c := outboxCounts(t, st); c.Inflight != 1 || c.Failed != 0 {
		t.Errorf("outbox %+v, want the entry left to the claim that took it", c)
	}
}

func TestPushAsksNothingAboutTheListAfterTheLeaseIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task/t1":
			writeJSON(t, w, map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p1/task/t1":

			if _, _, err := st.Claim(ctx, 10, 0); err != nil {
				t.Errorf("take the lease over: %v", err)
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errorCode":"unknown_exception"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, c := range calls {
		if c == "GET /open/v1/project/p1/data" {
			t.Errorf("calls %v, want no question about the list: the entry had stopped being this worker's", calls)
		}
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], store.ErrLeaseLost) {
		t.Errorf("errors = %v, want the lost lease reported", res.Errors)
	}
	if res.Pushed != 0 || res.Failed != 0 || res.Requeued != 0 {
		t.Errorf("result = %+v, want nothing written to an entry another claim holds", res)
	}
	if c := outboxCounts(t, st); c.Inflight != 1 || c.Failed != 0 {
		t.Errorf("outbox %+v, want the entry left to the claim that took it", c)
	}
}
