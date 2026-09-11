package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const inboxPath = "/open/v1/project/" + inboxID + "/data"

const emptyInboxAnswer = `{"tasks":[],"columns":[]}`

func projectList(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Личное"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(body))
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestPullKeepsTheUntouchedPayload(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0,`+
		`"someFutureField":{"nested":[1,2,3]}}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 1 || res.Pulled != 1 {
		t.Fatalf("result = %+v, want one task in one project", res)
	}
	task, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.Title != "Забрать посылку" || task.ProjectId != "p1" {
		t.Errorf("task = %+v, want the server's title and project", task)
	}
	raw, dirty, local := taskRow(t, st, "t1")
	if !strings.Contains(raw, "someFutureField") {
		t.Errorf("raw payload = %s, want the field this build does not decode", raw)
	}
	if dirty != 0 || local != 0 {
		t.Errorf("pulled row: dirty %d, local %d; want both zero", dirty, local)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
		t.Errorf("%s not written after a complete pull (%v)", LastSyncKey, err)
	}
}

func TestPullLeavesUnpushedEditAlone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","projectId":"p1","title":"Что-то другое","status":0}],"columns":[]}`))
	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Skipped != 1 || res.Pulled != 0 {
		t.Fatalf("result = %+v, want the row skipped", res)
	}
	task, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "Забрать посылку на почте" {
		t.Errorf("title = %q, want the local edit kept", task.Title)
	}
}

func TestPullReportsConversionWarnings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0,"priority":7}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("result = %+v, want the task cached", res)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Field != "priority" {
		t.Fatalf("warnings = %v, want one about the priority", res.Warnings)
	}
	task, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Priority != model.PriorityNone {
		t.Errorf("priority = %v, want the fallback", task.Priority)
	}
}

func TestPullLeavesOutATaskWithNoID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"","projectId":"p1","title":"Ничей","status":0},`+
		`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("result = %+v, want the task beside it cached", res)
	}
	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Fatalf("the task the answer could be read for is not in the cache: %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Field != "id" {
		t.Fatalf("warnings = %v, want one saying a task was left out", res.Warnings)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
		t.Errorf("%s not written (%v): a task left out is not an incomplete pull", LastSyncKey, err)
	}
}

func TestPullCachesATaskUnderTheProjectItWasAskedFor(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","title":"Забрать посылку","status":0}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("result = %+v, want the task cached", res)
	}
	task, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.ProjectId != "p1" {
		t.Errorf("task is cached under list %q, want the p1 its answer came from", task.ProjectId)
	}

	held, err := st.Tasks(ctx, store.TaskFilter{ProjectID: "p1", Status: store.StatusAll})
	if err != nil {
		t.Fatalf("read the list: %v", err)
	}
	if len(held) != 1 {
		t.Errorf("list p1 holds %d task(s), want the one the answer carried", len(held))
	}
}

func TestPullSurvivesAProjectThatVanished(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Личное"}, {ID: "p2", Name: "Работа"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Личное"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0}],"columns":[]}`))
		case "/open/v1/project/p2/data":
			w.WriteHeader(http.StatusNotFound)
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 1 || res.Pulled != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v, want one project done and one reported", res)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s written after an incomplete pull", LastSyncKey)
	}
}

func TestPullStopsOnAServerFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open/v1/project" {
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Личное"}})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := testSyncer(t, st, server).Pull(ctx); err == nil {
		t.Fatal("Pull returned no error on a 500")
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s written after a failed pull", LastSyncKey)
	}
}

func TestPullRefusesToEmptyTheCacheOnAnAnswerWithNoProjects(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	done := openTask("t1", "p1", "Забрать посылку")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "p1", done, openTask("t2", "p1", "Купить хлеб"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == inboxPath {
			w.Write([]byte(emptyInboxAnswer))
			return
		}
		if r.URL.Path != "/open/v1/project" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, []api.Project{})
	})

	_, err := testSyncer(t, st, server).Pull(ctx)
	if err == nil {
		t.Fatal("Pull emptied the cache without a word")
	}

	for _, want := range []string{"queued changes are still going out", "--allow-project-drop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refused with %q, want it to carry %q", err, want)
		}
	}
	assertCacheKept(t, st, 2, 1)
}

func TestPullTakesAnEmptyProjectListForAnEmptyCache(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == inboxPath {
			w.Write([]byte(emptyInboxAnswer))
			return
		}
		writeJSON(t, w, []api.Project{})
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 0 || res.Deleted != 0 {
		t.Errorf("result = %+v, want an empty pass", res)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
		t.Errorf("%s not written after a complete pull (%v)", LastSyncKey, err)
	}
}

func TestPullFailsOnAProjectListItCouldNotRead(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no body at all", ""},
		{"a bare null", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
			seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"), openTask("t2", "p1", "Купить хлеб"))

			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tc.body))
			})

			if _, err := testSyncer(t, st, server).Pull(ctx); err == nil {
				t.Fatal("Pull reported success on an answer it could not read")
			}
			assertCacheKept(t, st, 2, 1)
		})
	}
}

func TestPullCountsTheTasksOfAProjectThatVanished(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"},
	)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	seedTasks(t, st, "p2", openTask("t2", "p2", "Отчёт"), openTask("t3", "p2", "Позвонить в банк"))

	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 2 {
		t.Errorf("result = %+v, want the two tasks of p2 counted as dropped", res)
	}
	left, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("%d task(s) left in the cache, want only t1", len(left))
	}
}

func TestPullKeepsTheUnpushedRowsOfAProjectThatVanished(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"},
	)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))
	seedTasks(t, st, "p2", openTask("t2", "p2", "Отчёт"), openTask("t3", "p2", "Позвонить в банк"))
	if _, err := st.UpdateTask(ctx, "t2", model.TaskEdit{Title: model.Ptr("Отчёт за квартал")}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[`+
		`{"id":"t1","projectId":"p1","title":"Забрать посылку","status":0}],"columns":[]}`))

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 1 || res.Kept != 1 {
		t.Errorf("result = %+v, want t3 dropped and t2 kept", res)
	}
	if _, err := st.Task(ctx, "t2"); err != nil {
		t.Errorf("the unpushed edit of t2 went with its project: %v", err)
	}
}

func assertCacheKept(t *testing.T, st *store.Store, tasks, projects int) {
	t.Helper()
	ctx := context.Background()
	cached, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != tasks {
		t.Errorf("%d task(s) left in the cache, want %d", len(cached), tasks)
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != projects {
		t.Errorf("%d project(s) left in the cache, want %d", len(ps), projects)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s was stamped by a pass that wrote nothing (%v)", LastSyncKey, err)
	}
}

func shortList(t *testing.T, ids ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == inboxPath {
			w.Write([]byte(emptyInboxAnswer))
			return
		}
		if r.URL.Path == "/open/v1/project" {
			ps := make([]api.Project, 0, len(ids))
			for _, id := range ids {
				ps = append(ps, api.Project{ID: id, Name: id})
			}
			writeJSON(t, w, ps)
			return
		}
		for _, id := range ids {
			if r.URL.Path == "/open/v1/project/"+id+"/data" {
				fmt.Fprintf(w, `{"project":{"id":%q,"name":%q},"tasks":[],"columns":[]}`, id, id)
				return
			}
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPullRefusesAListThatLostMostOfTheProjects(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "p2", Name: "Rabota"},
		model.Project{Id: "p3", Name: "Uchyoba"},
	)
	done := openTask("t2", "p2", "Otchet za kvartal")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	seedTasks(t, st, "p2", done)
	seedTasks(t, st, "p3", openTask("t3", "p3", "Kupit hleb"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == inboxPath {
			w.Write([]byte(emptyInboxAnswer))
			return
		}
		if r.URL.Path != "/open/v1/project" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}})
	})

	_, err := testSyncer(t, st, server).Pull(ctx)
	if err == nil {
		t.Fatal("Pull acted on a list that had lost two projects of three")
	}
	for _, want := range []string{`"Rabota" (p2)`, `"Uchyoba" (p3)`, "--allow-project-drop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refused with %q, want it to carry %q", err, want)
		}
	}
	assertCacheKept(t, st, 3, 3)
	if _, err := st.Task(ctx, "t2"); err != nil {
		t.Errorf("the completed task of a project the answer left out is gone: %v", err)
	}
}

func TestPullDrawsTheLineAtMoreThanHalfTheProjects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listed  []string
		refused bool
	}{
		{"two of four left", []string{"p1", "p2"}, false},
		{"one of four left", []string{"p1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st,
				model.Project{Id: "p1", Name: "p1"},
				model.Project{Id: "p2", Name: "p2"},
				model.Project{Id: "p3", Name: "p3"},
				model.Project{Id: "p4", Name: "p4"},
			)
			server := serve(t, shortList(t, tc.listed...))

			_, err := testSyncer(t, st, server).Pull(ctx)
			if tc.refused {
				if err == nil {
					t.Fatal("Pull acted on a list that had lost three projects of four")
				}
				assertCacheKept(t, st, 0, 4)
				return
			}
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			ps, err := st.Projects(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ps) != len(tc.listed) {
				t.Errorf("%d project(s) cached, want the %d the server listed", len(ps), len(tc.listed))
			}
		})
	}
}

func TestPullRefusesToDropAListHoldingCompletedTasks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		closed  bool
		refused bool
	}{
		{"a list with a completed task under it", true, true},
		{"a list with nothing closed under it", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st,
				model.Project{Id: "p1", Name: "Lichnoe"},
				model.Project{Id: "p2", Name: "Rabota"},
			)
			t2 := openTask("t2", "p2", "Otchet za kvartal")
			if tc.closed {
				t2.Status = model.TaskDone
				t2.CompletedTime = model.NewTime(time.Now())
			}
			seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
			seedTasks(t, st, "p2", t2)

			server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Lichnoe"},"tasks":[`+
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0}],"columns":[]}`))

			res, err := testSyncer(t, st, server).Pull(ctx)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Pull: %v", err)
				}
				if res.Deleted != 1 {
					t.Errorf("result = %+v, want the open task of p2 dropped with its project", res)
				}
				if _, err := st.Task(ctx, "t2"); err == nil {
					t.Error("the task of the dropped list is still cached, so the answer was not acted on")
				}
				return
			}
			if err == nil {
				t.Fatal("Pull dropped a list holding the only copy of a completed task")
			}

			for _, want := range []string{`"Rabota" (p2)`, "1 completed", "--allow-project-drop"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refused with %q, want it to carry %q", err, want)
				}
			}
			assertCacheKept(t, st, 2, 2)
			if _, err := st.Task(ctx, "t2"); err != nil {
				t.Errorf("the completed task of the list the answer left out is gone: %v", err)
			}
		})
	}
}

func TestPullNamesTheListsWithTheMostAtStakeFirst(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	var projects []model.Project
	for i := 1; i <= 12; i++ {
		projects = append(projects, model.Project{
			Id:   fmt.Sprintf("p%02d", i),
			Name: fmt.Sprintf("List %02d", i),
		})
	}
	seedProject(t, st, projects...)

	done := openTask("t12", "p12", "Otchet za kvartal")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "p12", done)

	_, err := testSyncer(t, st, serve(t, shortList(t, "p01"))).Pull(ctx)
	if err == nil {
		t.Fatal("Pull acted on a list that had lost eleven projects of twelve")
	}
	if want := `"List 12" (p12): 1 completed`; !strings.Contains(err.Error(), want) {
		t.Errorf("refused with %q, want it to carry %q - the count is unreadable without it", err, want)
	}
}

func TestPullDoesNotWeighTheInboxAmongTheCachedLists(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inbox    bool
		projects int
	}{
		{"with an inbox in the cache", true, 4},
		{"without one", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			cached := []model.Project{
				{Id: "p1", Name: "Lichnoe"},
				{Id: "p2", Name: "Rabota"},
				{Id: "p3", Name: "Uchyoba"},
			}
			if tc.inbox {
				cached = append(cached, model.Project{Id: "inbox42", Name: inboxName})
			}
			seedProject(t, st, cached...)
			seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
			seedTasks(t, st, "p2", openTask("t2", "p2", "Otchet za kvartal"))
			seedTasks(t, st, "p3", openTask("t3", "p3", "Kupit hleb"))

			_, err := testSyncer(t, st, serve(t, shortList(t, "p1"))).Pull(ctx)
			if err == nil {
				t.Fatal("Pull acted on a list that had lost two of the three lists the user made")
			}
			if want := "did not send 2 of the 3 list(s)"; !strings.Contains(err.Error(), want) {
				t.Errorf("refused with %q, want it to carry %q - the inbox is no list of the user's", err, want)
			}
			assertCacheKept(t, st, 3, tc.projects)
		})
	}
}

func TestPullCountsOnlyTheUsersOwnListsInTheRefusal(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "inbox42", Name: inboxName},
	)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))

	_, err := testSyncer(t, st, serve(t, shortList(t))).Pull(ctx)
	if err == nil {
		t.Fatal("Pull emptied the cache on an answer that carries nothing")
	}
	if want := "the cache holds 1"; !strings.Contains(err.Error(), want) {
		t.Errorf("refused with %q, want it to carry %q", err, want)
	}
	assertCacheKept(t, st, 1, 2)
}

func TestPullWeighsAnInboxTheAnswerDropped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "inbox42", Name: inboxName},
	)
	done := openTask("t2", "inbox42", "Otchet za kvartal")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	seedTasks(t, st, "inbox42", done)

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Lichnoe"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0}],"columns":[]}`))
		case inboxPath:
			w.Write([]byte(`{"tasks":[` +
				`{"id":"t3","projectId":"inbox77","title":"Kupit hleb","status":0}],"columns":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	_, err := testSyncer(t, st, server).Pull(ctx)
	if err == nil {
		t.Fatal("Pull dropped a cached inbox holding the only copy of a completed task")
	}
	for _, want := range []string{"did not send 1 of the 2 list(s)", "(inbox42)", "1 completed", "--allow-project-drop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refused with %q, want it to carry %q", err, want)
		}
	}
	assertCacheKept(t, st, 2, 2)
}

func TestPullAllowProjectDropLetsTheCacheFollow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed []string
	}{
		{"a list that lost most of the projects", []string{"p1"}},
		{"a list with nothing on it at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st,
				model.Project{Id: "p1", Name: "p1"},
				model.Project{Id: "p2", Name: "p2"},
				model.Project{Id: "p3", Name: "p3"},
			)
			done := openTask("t2", "p2", "Otchet za kvartal")
			done.Status = model.TaskDone
			done.CompletedTime = model.NewTime(time.Now())
			seedTasks(t, st, "p2", done)
			server := serve(t, shortList(t, tc.listed...))

			syncer := testSyncerWith(t, st, server, Options{AllowProjectDrop: true})
			if _, err := syncer.Pull(ctx); err != nil {
				t.Fatalf("Pull: %v", err)
			}
			ps, err := st.Projects(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(ps) != len(tc.listed) {
				t.Errorf("%d project(s) cached, want the %d the server listed", len(ps), len(tc.listed))
			}
			if _, err := st.Task(ctx, "t2"); err == nil {
				t.Error("the task of the dropped project is still cached, so nothing was let through")
			}
			if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
				t.Errorf("%s not written after a pass that went through (%v)", LastSyncKey, err)
			}
		})
	}
}

func TestPullCountsTheCompletedTasksThatWentWithTheProjects(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "p1"},
		model.Project{Id: "p2", Name: "p2"},
		model.Project{Id: "p3", Name: "p3"},
	)
	closed := func(id, projectID, title string) model.Task {
		task := openTask(id, projectID, title)
		task.Status = model.TaskDone
		task.CompletedTime = model.NewTime(time.Now())
		return task
	}
	seedTasks(t, st, "p2",
		closed("t1", "p2", "Otchet za kvartal"),
		closed("t2", "p2", "Sdat klyuchi"),
		openTask("t3", "p2", "Zabrat posylku"),
	)
	seedTasks(t, st, "p3", openTask("t4", "p3", "Pozvonit v servis"))

	if _, err := st.UpdateTask(ctx, "t2", model.TaskEdit{Title: model.Ptr("Sdat klyuchi sosedu")}); err != nil {
		t.Fatal(err)
	}

	syncer := testSyncerWith(t, st, serve(t, shortList(t, "p1")), Options{AllowProjectDrop: true})
	res, err := syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 3 || res.Kept != 1 {
		t.Fatalf("result = %+v, want the three rows dropped and the unpushed one kept", res)
	}
	if res.CompletedDropped != 1 {
		t.Errorf("%d completed task(s) reported gone, want 1: the pass deleted %d rows and only one of "+
			"them was closed and past recovering", res.CompletedDropped, res.Deleted)
	}
}

func TestPullReportsNoCompletedLossWhereThereWasNone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "p1"}, model.Project{Id: "p2", Name: "p2"})
	seedTasks(t, st, "p2", openTask("t1", "p2", "Zabrat posylku"))

	syncer := testSyncerWith(t, st, serve(t, shortList(t, "p1")), Options{AllowProjectDrop: true})
	res, err := syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 1 || res.CompletedDropped != 0 {
		t.Errorf("result = %+v, want the open task counted as deleted and nothing counted as gone for good", res)
	}
}

func TestPullCachesTheInbox(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{})
		case inboxPath:
			w.Write([]byte(`{"tasks":[` +
				`{"id":"t1","projectId":"inbox42","title":"Zabrat posylku","status":0}],"columns":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 1 || res.Pulled != 1 {
		t.Fatalf("result = %+v, want the inbox gone through as a list of its own", res)
	}
	task, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if task.ProjectId != "inbox42" {
		t.Errorf("task project = %q, want the id the server stamped on the task itself", task.ProjectId)
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Id != "inbox42" || ps[0].Name != inboxName {
		t.Errorf("projects = %+v, want one list %q under the id its tasks carry", ps, inboxName)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
		t.Errorf("%s not written after a complete pull (%v)", LastSyncKey, err)
	}
}

func TestPullDoesNotTakeTheInboxForAVanishedProject(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "inbox42", Name: inboxName},
	)
	done := openTask("t2", "inbox42", "Otchet za kvartal")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "inbox42", done)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Lichnoe"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0}],"columns":[]}`))
		case inboxPath:
			w.Write([]byte(`{"tasks":[` +
				`{"id":"t3","projectId":"inbox42","title":"Kupit hleb","status":0}],"columns":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if _, err := testSyncer(t, st, server).Pull(ctx); err != nil {
		t.Fatalf("Pull refused an answer that dropped nothing: %v", err)
	}
	if _, err := st.Task(ctx, "t2"); err != nil {
		t.Errorf("the completed task of the inbox went with a list nobody deleted: %v", err)
	}
	if _, err := st.Task(ctx, "t3"); err != nil {
		t.Errorf("the task the inbox answer carried is not cached: %v", err)
	}
}

func TestPullKeepsTheInboxItCachedWhenTheAnswerHasNothingInIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "inbox42", Name: inboxName})
	done := openTask("t1", "inbox42", "Otchet za kvartal")
	done.Status = model.TaskDone
	done.CompletedTime = model.NewTime(time.Now())
	seedTasks(t, st, "inbox42", done, openTask("t2", "inbox42", "Zabrat posylku"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{})
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if res.Deleted != 1 || res.Kept != 1 {
		t.Errorf("result = %+v, want the open row dropped and the closed one kept", res)
	}
	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Errorf("the completed task of an inbox that answered empty is gone: %v", err)
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Id != "inbox42" {
		t.Errorf("projects = %+v, want the inbox still on the list", ps)
	}
}

func TestPullInventsNoInboxItCannotName(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{})
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 0 {
		t.Errorf("result = %+v, want an empty pass", res)
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("projects = %+v, want nothing cached under a name this pass made up", ps)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || !ok {
		t.Errorf("%s not written after a complete pull (%v)", LastSyncKey, err)
	}
}

func TestPullSurvivesAnInboxItCouldNotRead(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "inbox42", Name: inboxName})
	seedTasks(t, st, "inbox42", openTask("t1", "inbox42", "Zabrat posylku"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == inboxPath {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(t, w, []api.Project{})
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("an unreadable inbox ended the pull: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Errorf("errors = %v, want the inbox reported once", res.Errors)
	}
	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Errorf("the cached inbox task went with an answer that never came: %v", err)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s written after a pass that could not read the inbox", LastSyncKey)
	}
}

func TestPullTakesNoListForTheInboxOnAPrefixAlone(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "inboxarhiv", Name: "Inbox arhiv", Kind: inboxKind, SortOrder: 3})
	seedTasks(t, st, "inboxarhiv", openTask("t1", "inboxarhiv", "Zabrat posylku"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "inboxarhiv", Name: "Inbox arhiv"}})
		case "/open/v1/project/inboxarhiv/data":
			w.Write([]byte(`{"project":{"id":"inboxarhiv","name":"Inbox arhiv"},"tasks":[` +
				`{"id":"t1","projectId":"inboxarhiv","title":"Zabrat posylku","status":0}],"columns":[]}`))
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 1 {
		t.Errorf("result = %+v, want the one list gone through once and not twice", res)
	}
	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Errorf("the task of the user's own list went with an answer read for the inbox: %v", err)
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("projects = %+v, want the user's one list", ps)
	}
	if ps[0].Name == inboxName || ps[0].SortOrder == inboxSortOrder {
		t.Errorf("project = %+v, want the list as the server lists it, not as this package names the inbox", ps[0])
	}
}

func TestPullRefusesAnInboxAnswerThatNamesARealList(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Lichnoe"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"), openTask("t2", "p1", "Kupit hleb"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Lichnoe"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0},` +
				`{"id":"t2","projectId":"p1","title":"Kupit hleb","status":0}],"columns":[]}`))
		case inboxPath:

			w.Write([]byte(`{"tasks":[` +
				`{"id":"t9","projectId":"p1","title":"Iz inboksa","status":0}],"columns":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Projects != 1 || res.Deleted != 0 {
		t.Errorf("result = %+v, want the one real list gone through once and nothing deleted", res)
	}
	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Errorf("t1 went with an inbox answer misnaming the real list: %v", err)
	}
	if _, err := st.Task(ctx, "t2"); err != nil {
		t.Errorf("t2 went with an inbox answer misnaming the real list: %v", err)
	}
	if _, err := st.Task(ctx, "t9"); err == nil {
		t.Errorf("the inbox's own task was cached under the real list's id")
	}
	ps, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Id != "p1" {
		t.Errorf("projects = %+v, want the server's one list and no inbox row invented for it", ps)
	}
}

func TestPullLeavesAProjectAloneWhenTheAnswerCarriesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "p2", Name: "Rabota"},
	)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	seedTasks(t, st, "p2", openTask("t2", "p2", "Otchet za kvartal"), openTask("t3", "p2", "Pozvonit v bank"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}, {ID: "p2", Name: "Rabota"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Lichnoe"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0}],"columns":[]}`))
		case "/open/v1/project/p2/data":
			w.Write([]byte(`{}`))
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("result = %+v, want nothing dropped on an answer that said nothing", res)
	}
	for _, id := range []string{"t2", "t3"} {
		if _, err := st.Task(ctx, id); err != nil {
			t.Errorf("task %s went with an answer that carried no task list: %v", id, err)
		}
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want the one project reported", res.Errors)
	}

	if want := `"Rabota" (p2)`; !strings.Contains(res.Errors[0].Error(), want) {
		t.Errorf("reported %q, want it to name %s", res.Errors[0], want)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s written after a pass that could not read one of the projects", LastSyncKey)
	}
}

func TestPullLeavesAProjectAloneWhenTheAnswerNamesNoTasks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st,
		model.Project{Id: "p1", Name: "Lichnoe"},
		model.Project{Id: "p2", Name: "Rabota"},
	)
	seedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	seedTasks(t, st, "p2", openTask("t2", "p2", "Otchet za kvartal"), openTask("t3", "p2", "Pozvonit v bank"))

	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Lichnoe"}, {ID: "p2", Name: "Rabota"}})
		case "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Lichnoe"},"tasks":[` +
				`{"id":"t1","projectId":"p1","title":"Zabrat posylku","status":0}],"columns":[]}`))
		case "/open/v1/project/p2/data":
			w.Write([]byte(`{"project":{"id":"p2","name":"Rabota"},"columns":[]}`))
		case inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("result = %+v, want nothing dropped on an answer that named no tasks", res)
	}
	for _, id := range []string{"t2", "t3"} {
		if _, err := st.Task(ctx, id); err != nil {
			t.Errorf("task %s went with an answer that carried no task list: %v", id, err)
		}
	}

	if _, err := st.Task(ctx, "t1"); err != nil {
		t.Errorf("the task of the list that did answer is gone: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want the one project reported", res.Errors)
	}
	if want := `"Rabota" (p2)`; !strings.Contains(res.Errors[0].Error(), want) {
		t.Errorf("reported %q, want it to name %s", res.Errors[0], want)
	}
	if _, ok, err := st.Meta(ctx, LastSyncKey); err != nil || ok {
		t.Errorf("%s written after a pass that could not read one of the projects", LastSyncKey)
	}
}

func TestPullDropsTheRowsOfAProjectTheServerSaysIsEmpty(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Личное"})
	seedTasks(t, st, "p1", openTask("t1", "p1", "Забрать посылку"))

	server := serve(t, projectList(t, `{"project":{"id":"p1","name":"Личное"},"tasks":[],"columns":[]}`))
	res, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("result = %+v, want the row of the emptied project dropped", res)
	}
	if _, err := st.Task(ctx, "t1"); err == nil {
		t.Error("the task of a project the server answered empty is still cached")
	}
}

func TestTheLinesAPassPrintsLeaveTheCommandToTheCommand(t *testing.T) {
	ctx := context.Background()
	lines := map[string]error{}

	for name, err := range map[string]error{
		"a list the server does not have": api.ErrNotFound,
		"an answer with nothing in it":    api.ErrIncompleteAnswer,
		"a failure of any other kind":     errors.New("connection refused"),
	} {
		reported, _ := projectFetchFailure("Работа", "p1", err)
		lines[name] = reported
	}

	cached := []model.Project{
		{Id: "p1", Name: "Работа"},
		{Id: "p2", Name: "Личное"},
		{Id: "p3", Name: "Дом"},
	}
	listed := []api.Project{{ID: "p1", Name: "Работа"}}
	gone := []stake{{project: cached[1]}, {project: cached[2]}}
	lines["an answer carrying no lists at all"] = refuseTheDrop(cached, nil, gone, false)
	lines["an answer that would drop most of the cache"] = refuseTheDrop(cached, listed, gone, false)
	withClosed := []stake{{project: cached[1], completed: 4}, {project: cached[2]}}
	lines["an answer that would drop closed tasks"] = refuseTheDrop(cached, listed, withClosed, false)

	st := testStore(t)
	refusing := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := testSyncer(t, st, refusing).Pull(ctx); err != nil {
		lines["a token the server refuses"] = err
	} else {
		t.Error("a pull against a server that refuses the token came back with no failure")
	}

	if err := st.SetMeta(ctx, LastSyncKey, "the day before yesterday"); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	if _, _, err := testSyncer(t, st, refusing).LastSync(ctx); err != nil {
		lines["a stamp that will not parse"] = err
	} else {
		t.Error("a stamp that is not a timestamp was read as one")
	}

	for name, err := range lines {
		if err == nil {
			t.Errorf("%s: no line was built at all", name)
			continue
		}

		if msg := err.Error(); strings.HasPrefix(msg, "sync:") || strings.Contains(msg, ": sync:") {
			t.Errorf("%s: %q names the command, which the command puts in front of it", name, err)
		}
	}
}
