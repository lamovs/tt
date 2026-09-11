package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testSyncer(t *testing.T, st *store.Store, server *httptest.Server) *Syncer {
	t.Helper()
	return testSyncerWith(t, st, server, Options{})
}

func testSyncerWith(t *testing.T, st *store.Store, server *httptest.Server, opts Options) *Syncer {
	t.Helper()
	return testSyncerWithToken(t, st, server, opts, "test-token")
}

func testSyncerWithToken(t *testing.T, st *store.Store, server *httptest.Server, opts Options, token string) *Syncer {
	t.Helper()
	client := api.NewClient(token, "0.0.0-test",
		api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(5*time.Second))
	return New(st, client, opts)
}

func syncCredentialFixtureForms(token string) []string {
	jsonBytes, _ := json.Marshal(token)
	return []string{
		token,
		strings.Trim(string(jsonBytes), `"`),
		url.QueryEscape(token),
		base64.StdEncoding.EncodeToString([]byte(token)),
	}
}

func assertSyncCredentialFormsAbsent(t *testing.T, text string, forms []string) {
	t.Helper()
	for _, form := range forms {
		if form != "" && strings.Contains(text, form) {
			t.Error("diagnostic contains a credential fixture form")
		}
	}
}

func outboxLastError(t *testing.T, st *store.Store, op string) string {
	t.Helper()
	var lastError string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT last_error FROM outbox WHERE op = ?`, op).Scan(&lastError); err != nil {
		t.Fatalf("read last_error of %s: %v", op, err)
	}
	return lastError
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func seedProject(t *testing.T, st *store.Store, ps ...model.Project) {
	t.Helper()
	if err := st.ReplaceProjects(context.Background(), ps); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
}

func seedTasks(t *testing.T, st *store.Store, projectID string, tasks ...model.Task) {
	t.Helper()
	seeded := make([]store.ServerTask, 0, len(tasks))
	for _, task := range tasks {
		body := map[string]any{"id": task.Id, "title": task.Title}
		if task.Items != nil {
			items := make([]map[string]any, len(task.Items))
			for i, item := range task.Items {
				items[i] = map[string]any{
					"id": item.Id, "title": item.Title, "status": item.Status.Wire(),
					"sortOrder": item.SortOrder, "startDate": item.StartDate.String(),
					"isAllDay": item.IsAllDay, "timeZone": item.TimeZone,
					"completedTime": item.CompletedTime.String(),
				}
			}
			body["items"] = items
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, store.ServerTask{Task: task, Raw: raw})
	}
	if _, err := st.SyncProject(context.Background(), projectID, seeded); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}
}

func openTask(id, projectID, title string) model.Task {
	return model.Task{Id: id, ProjectId: projectID, Title: title, Status: model.TaskOpen}
}

func taskRow(t *testing.T, st *store.Store, id string) (raw string, dirty int64, local int) {
	t.Helper()
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT raw, dirty, local FROM tasks WHERE id = ?`, id).Scan(&raw, &dirty, &local)
	if err != nil {
		t.Fatalf("read task row %s: %v", id, err)
	}
	return raw, dirty, local
}

func attempts(t *testing.T, st *store.Store, taskID, op string) int {
	t.Helper()
	var n int
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT attempts FROM outbox WHERE task_id = ? AND op = ?`, taskID, op).Scan(&n)
	if err != nil {
		t.Fatalf("read attempts of %s %s: %v", op, taskID, err)
	}
	return n
}

func outboxTaskID(t *testing.T, st *store.Store, op string) string {
	t.Helper()
	var id string
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT task_id FROM outbox WHERE op = ?`, op).Scan(&id)
	if err != nil {
		t.Fatalf("read the task id of %s: %v", op, err)
	}
	return id
}

func outboxCounts(t *testing.T, st *store.Store) store.OutboxCounts {
	t.Helper()
	c, err := st.OutboxCounts(context.Background())
	if err != nil {
		t.Fatalf("outbox counts: %v", err)
	}
	return c
}

func TestLeaseForOutlastsTheClient(t *testing.T) {

	client := api.NewClient("t", "0.0.0-test",
		api.WithTimeout(30*time.Second), api.WithMaxRetries(3))
	if got, want := leaseFor(client.MaxCallDuration()), 212*time.Second; got != want {
		t.Errorf("leaseFor(%v) = %v, want %v", client.MaxCallDuration(), got, want)
	}

	if got := leaseFor(0); got != maxLease {
		t.Errorf("leaseFor(0) = %v, want %v", got, maxLease)
	}
	if got := leaseFor(time.Hour); got != maxLease {
		t.Errorf("leaseFor(1h) = %v, want %v", got, maxLease)
	}
}

func TestNewTakesItsLeaseFromTheClient(t *testing.T) {
	st := testStore(t)
	client := api.NewClient("t", "0.0.0-test",
		api.WithTimeout(2*time.Second), api.WithMaxRetries(2))
	s := New(st, client, Options{})
	if got, want := s.lease, leaseFor(client.MaxCallDuration()); got != want {
		t.Errorf("lease = %v, want %v", got, want)
	}
}

func TestDue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := New(st, api.NewClient("t", "0.0.0-test"), Options{})

	due, err := s.Due(ctx, time.Minute)
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if !due {
		t.Fatal("a cache that has never been pulled into is due")
	}

	if err := st.SetMeta(ctx, LastSyncKey, stamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	if due, err = s.Due(ctx, time.Minute); err != nil || due {
		t.Fatalf("Due right after a sync = %v (%v), want false", due, err)
	}
	if due, err = s.Due(ctx, 0); err != nil || !due {
		t.Fatalf("Due with no interval = %v (%v), want true", due, err)
	}

	if err := st.SetMeta(ctx, LastSyncKey, stamp(time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if due, err = s.Due(ctx, time.Minute); err != nil || !due {
		t.Fatalf("Due two hours later = %v (%v), want true", due, err)
	}
}

func TestLastSyncStampIsReadableBothWays(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := New(st, api.NewClient("t", "0.0.0-test"), Options{})

	now := time.Now()
	if err := st.SetMeta(ctx, LastSyncKey, stamp(now)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LastSync(ctx)
	if err != nil || !ok {
		t.Fatalf("LastSync = %v, %v, %v", got, ok, err)
	}
	if diff := got.Sub(now); diff > time.Second || diff < -time.Second {
		t.Errorf("LastSync = %v, want within a second of %v", got, now)
	}
	raw, _, err := st.Meta(ctx, LastSyncKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("stamp %q does not parse as RFC 3339: %v", raw, err)
	}
}

func TestRunPushesBeforeItPulls(t *testing.T) {
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

	var paths []string
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/open/v1/task":
			writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1", Title: "Забрать посылку"})
		case r.URL.Path == inboxPath:
			w.Write([]byte(emptyInboxAnswer))
		case r.URL.Path == "/open/v1/project":
			writeJSON(t, w, []api.Project{{ID: "p1", Name: "Личное"}})
		case r.URL.Path == "/open/v1/project/p1/data":
			w.Write([]byte(`{"project":{"id":"p1","name":"Личное"},"tasks":[` +
				`{"id":"srv1","projectId":"p1","title":"Забрать посылку","status":0}],"columns":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res, err := testSyncer(t, st, server).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Pushed != 1 || res.Pulled != 1 || res.Projects != 1 {
		t.Fatalf("result = %+v, want one push and one task in one project", res)
	}
	if len(paths) == 0 || paths[0] != "POST /open/v1/task" {
		t.Fatalf("requests %v, want the create first", paths)
	}
	if _, err := st.Task(ctx, created.Id); err == nil {
		t.Errorf("task %s is still cached under its local id", created.Id)
	}
	if _, dirty, local := taskRow(t, st, "srv1"); dirty != 0 || local != 0 {
		t.Errorf("task srv1: dirty %d, local %d; want a clean server row", dirty, local)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want empty", c)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestResultCapsWhatItCarries(t *testing.T) {
	var res Result
	for i := 0; i < maxResultErrors+10; i++ {
		res.addError(fmt.Errorf("failure %d", i))
	}
	for i := 0; i < maxResultWarnings+5; i++ {
		res.addWarning(api.Warning{Field: "priority", Value: strconv.Itoa(i), Detail: "unknown"})
	}
	if len(res.Errors) != maxResultErrors {
		t.Errorf("%d errors kept, want the cap of %d", len(res.Errors), maxResultErrors)
	}
	if len(res.Warnings) != maxResultWarnings {
		t.Errorf("%d warnings kept, want the cap of %d", len(res.Warnings), maxResultWarnings)
	}
	if got := res.Errors[len(res.Errors)-1].Error(); !strings.Contains(got, "and 11 more") {
		t.Errorf("last error = %q, want the count of the ones left out", got)
	}
	if got := res.Warnings[len(res.Warnings)-1].Value; got != "6" {
		t.Errorf("last warning counts %q, want 6", got)
	}

	var pull Result
	for i := 0; i < maxResultErrors+3; i++ {
		pull.addError(fmt.Errorf("pull failure %d", i))
	}
	res.merge(pull)
	if len(res.Errors) != maxResultErrors {
		t.Errorf("%d errors after merging, want the cap of %d", len(res.Errors), maxResultErrors)
	}

	if got := res.Errors[len(res.Errors)-1].Error(); !strings.Contains(got, "and 64 more") {
		t.Errorf("last error after merging = %q, want the whole count", got)
	}
}

func TestResultUnderTheCapIsUntouched(t *testing.T) {
	var res Result
	res.addError(errors.New("one"))
	res.addWarning(api.Warning{Field: "priority", Detail: "unknown"})
	var pull Result
	pull.addError(errors.New("two"))
	res.merge(pull)
	if len(res.Errors) != 2 || len(res.Warnings) != 1 {
		t.Fatalf("errors %v, warnings %v; want both as they were", res.Errors, res.Warnings)
	}
	if res.Errors[0].Error() != "one" || res.Errors[1].Error() != "two" {
		t.Errorf("errors = %v, want them in the order they happened", res.Errors)
	}
}

func TestResultCountsWhatTheCapLeftOut(t *testing.T) {
	warnings := func(n int) int {
		var res Result
		for i := 0; i < n; i++ {
			res.addWarning(api.Warning{Field: "priority", Value: strconv.Itoa(i), Detail: "unknown"})
		}
		return res.WarningCount()
	}
	failures := func(n int) int {
		var res Result
		for i := 0; i < n; i++ {
			res.addError(fmt.Errorf("failure %d", i))
		}
		return res.ErrorCount()
	}
	for _, n := range []int{0, 1, maxResultWarnings - 1, maxResultWarnings, maxResultWarnings + 7} {
		if got := warnings(n); got != n {
			t.Errorf("WarningCount after %d warning(s) = %d", n, got)
		}
	}
	for _, n := range []int{0, 1, maxResultErrors - 1, maxResultErrors, maxResultErrors + 7} {
		if got := failures(n); got != n {
			t.Errorf("ErrorCount after %d failure(s) = %d", n, got)
		}
	}

	var push, pull Result
	for i := 0; i < maxResultWarnings+7; i++ {
		push.addWarning(api.Warning{Field: "priority", Value: strconv.Itoa(i), Detail: "unknown"})
	}
	for i := 0; i < maxResultErrors+7; i++ {
		push.addError(fmt.Errorf("push failure %d", i))
	}
	for i := 0; i < maxResultWarnings+3; i++ {
		pull.addWarning(api.Warning{Field: "dueDate", Value: strconv.Itoa(i), Detail: "unknown"})
	}
	for i := 0; i < maxResultErrors+3; i++ {
		pull.addError(fmt.Errorf("pull failure %d", i))
	}
	push.merge(pull)
	if got, want := push.WarningCount(), 2*maxResultWarnings+10; got != want {
		t.Errorf("WarningCount after merging = %d, want %d", got, want)
	}
	if got, want := push.ErrorCount(), 2*maxResultErrors+10; got != want {
		t.Errorf("ErrorCount after merging = %d, want %d", got, want)
	}
}
