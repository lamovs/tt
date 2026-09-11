package sync

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type moveServer struct {
	t *testing.T

	tasks map[string]string

	calls []string

	failDelete bool
}

func (m *moveServer) handle(w http.ResponseWriter, r *http.Request) {
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
		m.tasks["srv2"] = "p2"
		writeJSON(m.t, w, api.Task{ID: "srv2", ProjectID: "p2", Title: "Забрать посылку"})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/open/v1/project/"), "/data")
		out := api.ProjectData{Project: api.Project{ID: id}, Tasks: []api.Task{}}
		for taskID, projectID := range m.tasks {
			if projectID == id {
				out.Tasks = append(out.Tasks, api.Task{ID: taskID, ProjectID: projectID})
			}
		}
		writeJSON(m.t, w, out)
	case r.Method == http.MethodDelete:
		if m.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		delete(m.tasks, parts[len(parts)-1])
		w.WriteHeader(http.StatusOK)
	default:
		m.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPushMoveDropKeepsTheOriginalWhenTheCopyIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	srv := &moveServer{t: t, tasks: map[string]string{"t1": "p1"}, failDelete: true}
	syncer := testSyncer(t, st, serve(t, srv.handle))

	res, err := syncer.Push(ctx)
	if err != nil {
		t.Fatalf("first Push: %v", err)
	}
	if res.Pushed != 1 || res.Requeued != 1 {
		t.Fatalf("first pass = %+v, want the create pushed and the delete put back", res)
	}
	if _, ok := srv.tasks["srv2"]; !ok {
		t.Fatalf("the copy was not created on the server: %v", srv.tasks)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Fatalf("outbox %+v, want the delete back in line", c)
	}

	delete(srv.tasks, "srv2")
	srv.failDelete = false
	srv.calls = nil

	res, err = syncer.Push(ctx)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if _, ok := srv.tasks["t1"]; !ok {
		t.Fatalf("the original was deleted with the copy gone; the account holds %v, calls %v",
			srv.tasks, srv.calls)
	}
	for _, call := range srv.calls {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("calls %v, want no delete sent while the copy is not in the target list", srv.calls)
		}
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the delete parked", res)
	}
	if !hasError(res, "is not in list p2") {
		t.Errorf("errors %v, want one naming the list the copy is missing from", res.Errors)
	}
}

func TestPushMoveDropParksAnEntryThatDoesNotSayWhereTheCopyIs(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	if _, err := st.DB().ExecContext(ctx,
		`UPDATE outbox SET payload = ? WHERE op = ?`,
		`{"copy_id":"srv2"}`, store.OpTaskMoveDrop); err != nil {
		t.Fatalf("write the old payload: %v", err)
	}

	srv := &moveServer{t: t, tasks: map[string]string{"t1": "p1", "srv2": "p2"}}
	res, err := testSyncer(t, st, serve(t, srv.handle)).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, call := range srv.calls {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("calls %v, want no delete sent for an entry with nowhere to look", srv.calls)
		}
	}
	if _, ok := srv.tasks["t1"]; !ok {
		t.Error("the original was deleted without the copy being found")
	}
	if res.Failed != 1 || !hasError(res, "not which list it was moved to") {
		t.Errorf("result = %+v, want the entry parked and the reason said", res)
	}
}

func TestPushMoveDropParksUnderALocalOriginal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"})
	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	held, _, err := st.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatalf("claim the create: %v", err)
	}
	if len(held) != 1 || held[0].TaskID != created.Id {
		t.Fatalf("claim returned %+v, want the create of %s", held, created.Id)
	}
	if err := st.MarkFailed(ctx, held[0].Seq, held[0].LeaseToken, "the answer was lost"); err != nil {
		t.Fatalf("park the create: %v", err)
	}
	if _, err := st.MoveTask(ctx, created.Id, "p2", store.MoveOptions{ByRecreate: true}); err != nil {
		t.Fatalf("move task: %v", err)
	}

	srv := &moveServer{t: t, tasks: map[string]string{}}
	res, err := testSyncer(t, st, serve(t, srv.handle)).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(srv.calls) != 1 || !strings.HasPrefix(srv.calls[0], "POST /open/v1/task") {
		t.Fatalf("calls %v, want only the create of the copy", srv.calls)
	}
	if !hasError(res, "nothing is left in the queue to create the task this delete addresses") {
		t.Errorf("errors %v, want one saying the delete has no id to go out under", res.Errors)
	}
	if !hasError(res, "moved into") {
		t.Errorf("errors %v, want the line to say the task was being moved", res.Errors)
	}
}

func TestPushMoveDropWaitsWhenTheAnswerCarriesNoTaskList(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p2"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data"):

			_, _ = w.Write([]byte(`{"project":{"id":"p2","name":"Rabota"}}`))
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
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("calls %v, want no delete: nothing said where the copy is", calls)
		}
	}
	if res.Failed != 0 || res.Requeued != 1 {
		t.Errorf("result = %+v, want the delete back in line rather than parked", res)
	}
	if c := outboxCounts(t, st); c.Pending != 1 || c.Failed != 0 {
		t.Errorf("outbox %+v, want the delete waiting for an answer that carries the task list", c)
	}
}

func TestPushMoveDropSendsNoDeleteAfterTheLeaseIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p2"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data"):

			if _, _, err := st.Claim(ctx, 10, 0); err != nil {
				t.Errorf("take the lease over: %v", err)
			}
			writeJSON(t, w, api.ProjectData{
				Project: api.Project{ID: "p2"},
				Tasks:   []api.Task{{ID: "srv2", ProjectID: "p2"}},
			})
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
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("calls %v, want no delete: the entry had stopped being this worker's", calls)
		}
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], store.ErrLeaseLost) {
		t.Errorf("errors = %v, want the lost lease reported", res.Errors)
	}
	if res.Failed != 0 {
		t.Errorf("result = %+v, want nothing written to an entry another claim holds", res)
	}
}

func TestPushMoveDropParksWhenTheTargetListIsGone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedMove(t, st)

	var calls []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			writeJSON(t, w, api.Task{ID: "srv2", ProjectID: "p2"})
		case r.Method == http.MethodGet && r.URL.Path == "/open/v1/project/p2/data":

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
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("calls %v, want no delete: the copy was never found", calls)
		}
	}
	if res.Failed != 1 || res.Requeued != 0 {
		t.Errorf("result = %+v, want the delete parked rather than put back for a pass that would be "+
			"answered the same way", res)
	}
	if !hasError(res, "is not on the server any more") {
		t.Errorf("errors %v, want one saying the list the task was moved into is gone", res.Errors)
	}
	if c := outboxCounts(t, st); c.Failed != 1 || c.Pending != 0 {
		t.Errorf("outbox %+v, want the delete out of the line rather than waiting in it", c)
	}
}

func TestMoveDropWaitsWhereTheCreateOfTheOriginalIsStillQueued(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"})

	created, err := st.CreateTask(ctx, openTask("", "p1", "Забрать посылку"))
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if queued, err := st.HasQueuedCreate(ctx, created.Id); err != nil || !queued {
		t.Fatalf("HasQueuedCreate = %v, %v; want the create of %s still owed", queued, err, created.Id)
	}

	syncer := testSyncer(t, st, serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	item := store.OutboxItem{
		Seq: 7, Op: store.OpTaskMoveDrop, TaskID: created.Id, ProjectID: "p1",
	}
	_, outcome, err := syncer.dropUnderALocalOriginal(ctx, item,
		store.MoveDrop{CopyID: "srv2", CopyProjectID: "p2"})
	if outcome != pushSkip {
		t.Errorf("outcome = %v, want pushSkip: the create is still coming, so the entry goes "+
			"back in line rather than out of the queue", outcome)
	}
	if err == nil {
		t.Fatal("nothing was said about an entry that was held back")
	}
	var unsent *unsentError
	if !errors.As(err, &unsent) {
		t.Errorf("err = %q, want it marked as an entry nothing was sent for: unmarked, it is "+
			"counted as a failure and `tt sync` comes back 1 on a pass that did what it should", err)
	}
	if !strings.Contains(err.Error(), "waits for the create") {
		t.Errorf("err = %q, want it to say what the entry is waiting for", err)
	}
	if !strings.Contains(err.Error(), "srv2") {
		t.Errorf("err = %q, want it to name the copy the task was moved into", err)
	}
}
