package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, server *httptest.Server, opts ...Option) *Client {
	t.Helper()
	base := []Option{WithBaseURL(server.URL)}
	c := NewClient("test-token", "0.0.0-test", append(base, opts...)...)
	c.retryBaseDelay = time.Millisecond
	c.retryMaxDelay = 20 * time.Millisecond
	return c
}

func TestListProjects_Success(t *testing.T) {
	var gotAuth, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if r.Method != http.MethodGet || r.URL.Path != "/open/v1/project" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{
			{ID: "p1", Name: "Inbox", Kind: "TASK"},
			{ID: "p2", Name: "Work", Closed: true, ViewMode: "kanban"},
		})
	}))
	defer server.Close()

	c := testClient(t, server)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 || projects[0].ID != "p1" || projects[1].Name != "Work" || !projects[1].Closed {
		t.Fatalf("unexpected projects: %+v", projects)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotUA != "tt/0.0.0-test" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "tt/0.0.0-test")
	}
}

func TestGetProjectData_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open/v1/project/p1/data" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ProjectData{
			Project: Project{ID: "p1", Name: "Inbox"},
			Tasks: []Task{
				{ID: "t1", ProjectID: "p1", Title: "Buy milk", Priority: 3, Status: 0, Kind: "CHECKLIST",
					Items: []ChecklistItem{{ID: "i1", Title: "milk", Status: 0}}},
			},
			Columns: []Column{{ID: "c1", ProjectID: "p1", Name: "Todo"}},
		})
	}))
	defer server.Close()

	c := testClient(t, server)
	data, err := c.GetProjectData(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetProjectData: %v", err)
	}
	if data.Project.ID != "p1" {
		t.Errorf("project id = %q", data.Project.ID)
	}
	if len(data.Tasks) != 1 || data.Tasks[0].Priority != 3 || data.Tasks[0].Kind != "CHECKLIST" {
		t.Fatalf("unexpected tasks: %+v", data.Tasks)
	}
	if len(data.Tasks[0].Items) != 1 || data.Tasks[0].Items[0].Title != "milk" {
		t.Fatalf("unexpected items: %+v", data.Tasks[0].Items)
	}
	if len(data.Columns) != 1 || data.Columns[0].Name != "Todo" {
		t.Fatalf("unexpected columns: %+v", data.Columns)
	}
}

func TestGetTask_LiveObservedFields(t *testing.T) {
	body := `{
		"id": "t1",
		"projectId": "p1",
		"title": "Buy milk",
		"content": "2% milk, the big carton",
		"desc": "some non-description value",
		"createdTime": "2026-08-01T12:00:00.000+0000",
		"modifiedTime": "2026-08-02T09:30:00.000+0000",
		"columnId": "col1",
		"columnName": "Todo",
		"assigneeUsername": "someone",
		"etag": "abc123",
		"etimestamp": 1776548142542,
		"isFloating": true,
		"progress": 42,
		"tags": ["errand", "home"]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer server.Close()

	c := testClient(t, server)
	task, err := c.GetTask(context.Background(), "p1", "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.CreatedTime == "" || task.ModifiedTime == "" {
		t.Errorf("CreatedTime/ModifiedTime not decoded: %+v", task)
	}
	if task.ColumnID != "col1" || task.ColumnName != "Todo" {
		t.Errorf("ColumnID/ColumnName not decoded: %+v", task)
	}
	if task.AssigneeUsername != "someone" {
		t.Errorf("AssigneeUsername not decoded: %+v", task)
	}
	if task.Etag != "abc123" {
		t.Errorf("Etag not decoded: %+v", task)
	}
	if task.Etimestamp != 1776548142542 {
		t.Errorf("Etimestamp = %d, want %d", task.Etimestamp, 1776548142542)
	}
	if !task.IsFloating {
		t.Errorf("IsFloating not decoded: %+v", task)
	}
	if task.Progress != 42 {
		t.Errorf("Progress not decoded: %+v", task)
	}
	if len(task.Tags) != 2 || task.Tags[0] != "errand" {
		t.Errorf("Tags not decoded: %+v", task)
	}
	if task.Content == "" || task.Desc == "" || task.Content == task.Desc {
		t.Errorf("Content and Desc must be distinct fields, both populated: content=%q desc=%q", task.Content, task.Desc)
	}
}

func TestUnauthorized_NoRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errorCode":"unauthorized"}`))
	}))
	defer server.Close()

	c := testClient(t, server)
	_, err := c.ListProjects(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.StatusCode != 401 || se.Method != "GET" || se.Path != "/open/v1/project" {
		t.Errorf("unexpected StatusError: %+v", se)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (401 must not retry)", calls)
	}
}

func TestServerError_RetriesExhausted(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer server.Close()

	c := testClient(t, server)
	_, err := c.ListProjects(context.Background())
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("err = %v, want ErrServerError", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 500 {
		t.Fatalf("err = %v, want *StatusError{StatusCode:500}", err)
	}
	if calls != int32(DefaultMaxRetries) {
		t.Errorf("calls = %d, want %d", calls, DefaultMaxRetries)
	}
}

func TestRateLimited_RetryAfterThenSuccess(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"errorCode":"rate_limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer server.Close()

	c := testClient(t, server)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p1" {
		t.Fatalf("unexpected projects: %+v", projects)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestBadJSON_NoRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	}))
	defer server.Close()

	c := testClient(t, server)
	_, err := c.ListProjects(context.Background())
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DecodeError", err)
	}
	if de.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", de.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (bad JSON must not retry)", calls)
	}
}

func TestContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not have been called with an already-canceled context")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.ListProjects(ctx)
	if err == nil {
		t.Fatal("expected an error for a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
}

func TestContextDeadlineDuringRetryWait(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	c := testClient(t, server)

	c.retryMaxDelay = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.ListProjects(ctx)
	if err == nil {
		t.Fatal("expected an error")
	}

	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestUpdateTask_PartialUpdateOmitsUntouchedFields(t *testing.T) {
	var rawBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "t1", ProjectID: "p1", Title: "Buy milk"})
	}))
	defer server.Close()

	c := testClient(t, server)
	update := TaskUpdate{
		ID:        "t1",
		ProjectID: "p1",
		DueDate:   Ptr("2026-09-10T09:00:00+0000"),
	}
	if err := c.UpdateTask(context.Background(), update); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	if _, ok := rawBody["dueDate"]; !ok {
		t.Error(`request body missing "dueDate", want it present`)
	}
	for _, untouched := range []string{"title", "content", "desc", "priority", "reminders", "items", "startDate"} {
		if _, ok := rawBody[untouched]; ok {
			t.Errorf("request body contains %q, want it omitted (untouched field must not be sent)", untouched)
		}
	}
	if _, ok := rawBody["id"]; !ok {
		t.Error(`request body missing "id"`)
	}
	if _, ok := rawBody["projectId"]; !ok {
		t.Error(`request body missing "projectId"`)
	}
}

func TestUpdateTask_ExplicitEmptyClearsField(t *testing.T) {
	var rawBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&rawBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "t1", ProjectID: "p1"})
	}))
	defer server.Close()

	c := testClient(t, server)
	update := TaskUpdate{
		ID:        "t1",
		ProjectID: "p1",
		Desc:      Ptr(""),
	}
	if err := c.UpdateTask(context.Background(), update); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	raw, ok := rawBody["desc"]
	if !ok {
		t.Fatal(`request body missing "desc", want it present as an explicit empty value`)
	}
	if string(raw) != `""` {
		t.Errorf("desc = %s, want an empty string literal", raw)
	}
}

func TestCompleteTask_DeleteTask_DeleteProject(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := testClient(t, server)

	if err := c.CompleteTask(context.Background(), "p1", "t1"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/open/v1/project/p1/task/t1/complete" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}

	if err := c.DeleteTask(context.Background(), "p1", "t1"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/open/v1/project/p1/task/t1" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}

	if err := c.DeleteProject(context.Background(), "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/open/v1/project/p1" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
}

func TestNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	c := testClient(t, server)
	_, err := c.GetTask(context.Background(), "p1", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	d, ok := parseRetryAfter("3")
	if !ok || d != 3*time.Second {
		t.Fatalf("parseRetryAfter(3) = %v, %v", d, ok)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC()
	d, ok := parseRetryAfter(future.Format(http.TimeFormat))
	if !ok {
		t.Fatal("parseRetryAfter did not parse an HTTP-date")
	}
	if d <= 0 || d > 3*time.Second {
		t.Errorf("parseRetryAfter(date) = %v, want roughly 2s", d)
	}
}

func TestParseRetryAfter_Invalid(t *testing.T) {
	if _, ok := parseRetryAfter("not-a-value"); ok {
		t.Error("parseRetryAfter should reject a garbage value")
	}
}

func TestCreateIsNotResentAfterAnAnswerThatNeverArrived(t *testing.T) {
	var creates int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&creates, 1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}

		conn.Close()
	}))
	defer server.Close()

	c := testClient(t, server)
	if _, err := c.CreateTask(context.Background(), TaskCreate{ProjectID: "p1", Title: "Buy milk"}); err == nil {
		t.Fatal("CreateTask reported success on an answer that never arrived")
	}
	if n := atomic.LoadInt32(&creates); n != 1 {
		t.Errorf("the create went out %d time(s) for one call, want once", n)
	}
}

func TestMutationsAreNotResentOnAServerError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	c := testClient(t, server)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"create", func() error {
			_, err := c.CreateTask(context.Background(), TaskCreate{ProjectID: "p1", Title: "Buy milk"})
			return err
		}},
		{"update", func() error {
			return c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"})
		}},
		{"complete", func() error { return c.CompleteTask(context.Background(), "p1", "t1") }},
		{"delete", func() error { return c.DeleteTask(context.Background(), "p1", "t1") }},
	} {
		atomic.StoreInt32(&calls, 0)
		if err := tc.call(); err == nil {
			t.Errorf("%s: no error on a 502", tc.name)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("%s went out %d time(s), want once", tc.name, n)
		}
	}
}

func TestReadIsStillRetried(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer server.Close()

	c := testClient(t, server)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || atomic.LoadInt32(&calls) != 2 {
		t.Errorf("%d project(s) after %d call(s), want the second attempt to have answered", len(projects), calls)
	}
}

func TestRequestTimeoutIsRetried(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer server.Close()

	c := testClient(t, server)
	if _, err := c.ListProjects(context.Background()); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("calls = %d, want 2: a 408 asks to be tried again", n)
	}
	if !isRetryableStatus(http.StatusRequestTimeout) {
		t.Error("isRetryableStatus does not know 408")
	}
}

func TestSuccessWithNoBodyIsNotAnAnswer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no body at all", ""},
		{"a bare null", "null"},
		{"whitespace only", "  \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c := testClient(t, server)
			projects, err := c.ListProjects(context.Background())
			if !errors.Is(err, ErrIncompleteAnswer) {
				t.Fatalf("err = %v, want ErrIncompleteAnswer", err)
			}
			var de *DecodeError
			if !errors.As(err, &de) || de.StatusCode != 200 {
				t.Errorf("err = %v, want a *DecodeError carrying the status", err)
			}

			if errors.Is(err, ErrNoSuchProject) {
				t.Errorf("err = %v, want a body that said nothing read as silence rather than as an "+
					"answer about a project", err)
			}
			if projects != nil {
				t.Errorf("projects = %+v, want nothing handed back", projects)
			}
		})
	}
}

func TestSuccessWithAnEmptyObjectIsNotAnAnswer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"an empty object", "{}"},
		{"an empty object with room in it", "{\n  \n}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c := testClient(t, server)
			data, err := c.GetProjectDataRaw(context.Background(), "p1")
			if !errors.Is(err, ErrIncompleteAnswer) {
				t.Fatalf("err = %v, want ErrIncompleteAnswer", err)
			}

			if !errors.Is(err, ErrNoSuchProject) {
				t.Errorf("err = %v, want ErrNoSuchProject as well: a caller that goes round the queue "+
					"until this answer changes goes round for ever", err)
			}
			if data != nil {
				t.Errorf("data = %+v, want nothing handed back", data)
			}
		})
	}
}

func TestAnAnswerWithNoTaskListIsNotAnAnswer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"the project and nothing else", `{"project":{"id":"p1","name":"Lichnoe"}}`},
		{"every key but the tasks", `{"project":{"id":"p1","name":"Lichnoe"},"columns":[]}`},
		{"a task list that is the literal null", `{"project":{"id":"p1","name":"Lichnoe"},"tasks":null,"columns":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c := testClient(t, server)
			data, err := c.GetProjectDataRaw(context.Background(), "p1")
			if !errors.Is(err, ErrIncompleteAnswer) {
				t.Fatalf("err = %v, want ErrIncompleteAnswer", err)
			}

			if errors.Is(err, ErrNoSuchProject) {
				t.Errorf("err = %v, want an answer that named the project read as silence about its "+
					"tasks rather than as the project being gone", err)
			}
			if data != nil {
				t.Errorf("data = %+v, want nothing handed back", data)
			}
		})
	}
}

func TestAnEmptyProjectStillAnswers(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a project with an empty task list", `{"project":{"id":"p1","name":"Lichnoe"},"tasks":[],"columns":[]}`},
		{"an answer with no project in it at all", `{"tasks":[],"columns":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer server.Close()

			c := testClient(t, server)
			data, err := c.GetProjectDataRaw(context.Background(), "p1")
			if err != nil {
				t.Fatalf("GetProjectDataRaw: %v", err)
			}
			if len(data.Tasks) != 0 {
				t.Errorf("tasks = %+v, want none", data.Tasks)
			}
		})
	}
}

func TestSuccessWithNoBodyIsFineWhereNothingWasAsked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testClient(t, server)
	if err := c.CompleteTask(context.Background(), "p1", "t1"); err != nil {
		t.Errorf("CompleteTask: %v", err)
	}
	if err := c.DeleteTask(context.Background(), "p1", "t1"); err != nil {
		t.Errorf("DeleteTask: %v", err)
	}
	if err := c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"}); err != nil {
		t.Errorf("UpdateTask: %v", err)
	}
}

func TestUpdateTakesEveryDocumentedSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"200 with the task", http.StatusOK, `{"id":"t1","projectId":"p1","title":"Buy milk"}`},
		{"201 created, no body", http.StatusCreated, ""},
		{"204 no content", http.StatusNoContent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					w.Write([]byte(tc.body))
				}
			}))
			defer server.Close()

			c := testClient(t, server)
			if err := c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"}); err != nil {
				t.Errorf("UpdateTask: %v", err)
			}
		})
	}
}

func TestRateLimitCarriesTheRetryAfterItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		known  bool
		want   time.Duration
	}{
		{"seconds", "120", true, 2 * time.Minute},
		{"zero seconds", "0", true, 0},
		{"a negative count", "-5", true, 0},
		{"no header at all", "", false, 0},
		{"something this build cannot read", "in a little while", false, 0},
		{"an http-date already past", "Mon, 02 Jan 2006 15:04:05 GMT", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()

			c := testClient(t, server, WithMaxRetries(1))
			err := c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"})
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a *StatusError", err)
			}
			if se.RetryAfterKnown != tc.known || se.RetryAfter != tc.want {
				t.Errorf("RetryAfter = %v (known %v), want %v (known %v)",
					se.RetryAfter, se.RetryAfterKnown, tc.want, tc.known)
			}
		})
	}
}

func TestRateLimitTurnsAnHTTPDateIntoAWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(2*time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	c := testClient(t, server, WithMaxRetries(1))
	err := c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}

	if !se.RetryAfterKnown || se.RetryAfter < 118*time.Second || se.RetryAfter > 2*time.Minute {
		t.Errorf("RetryAfter = %v (known %v), want about two minutes", se.RetryAfter, se.RetryAfterKnown)
	}
}

func TestCreateWithoutAnIDInTheAnswerIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errorId":"a3f","errorCode":"exceed_quota","errorMessage":"quota exceeded"}`))
	}))
	defer server.Close()

	c := testClient(t, server)
	task, err := c.CreateTask(context.Background(), TaskCreate{ProjectID: "p1", Title: "Buy milk"})
	if !errors.Is(err, ErrIncompleteAnswer) {
		t.Fatalf("err = %v, want ErrIncompleteAnswer", err)
	}
	if task != nil {
		t.Errorf("task = %+v, want nothing handed back", task)
	}
}

func TestAnswerOverTheReadLimitSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("["))
		w.Write(bytes.Repeat([]byte("x"), maxResponseBodyRead))
	}))
	defer server.Close()

	c := testClient(t, server)
	_, err := c.ListProjects(context.Background())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	var big *ResponseTooLargeError
	if !errors.As(err, &big) {
		t.Fatalf("err = %v, want a *ResponseTooLargeError", err)
	}
	if big.Limit != maxResponseBodyRead {
		t.Errorf("the error names a limit of %d, want %d", big.Limit, maxResponseBodyRead)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxResponseBodyRead)) {
		t.Errorf("the message does not name the limit: %v", err)
	}
}

func TestAnswerExactlyAtTheReadLimitIsRead(t *testing.T) {
	pad := maxResponseBodyRead - len(`[{"id":"p1","name":""}]`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"p1","name":"` + strings.Repeat("x", pad) + `"}]`))
	}))
	defer server.Close()

	c := testClient(t, server)
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || len(projects[0].Name) != pad {
		t.Errorf("got %d project(s), name of %d byte(s)", len(projects), len(projects[0].Name))
	}
}

func TestAServerBodyCannotWriteALineOfItsOwn(t *testing.T) {
	const body = "{\"errorCode\":\"task_not_exist\",\n \"errorMessage\":\"no such task\"}\r\n" +
		"    entry 999: task.create \"anything at all\"\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(body))
	}))
	defer server.Close()

	_, err := testClient(t, server).GetTask(context.Background(), "p1", "t1")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if strings.ContainsAny(se.Error(), "\r\n") {
		t.Errorf("the message reads %q, want it on the one line it is printed as", se.Error())
	}
	if se.Body != body {
		t.Error("the typed body did not retain the raw response bytes")
	}
	if strings.Contains(se.Error(), "task_not_exist") {
		t.Error("ordinary error text includes the server body")
	}
}

func TestAStatusDiagnosticDoesNotDependOnItsBody(t *testing.T) {
	const want = "ticktick: GET tasks request: http 502"
	for _, body := range []string{"", " ", "upstream refused", "\nforged line"} {
		e := &StatusError{StatusCode: 502, Method: "GET", Path: "/open/v1/task",
			Body: body, Err: ErrServerError}
		if got := e.Error(); got != want {
			t.Errorf("message = %q, want fixed status diagnostic", got)
		}
	}
}

func TestADecodeDiagnosticUsesOnlyCategoryAndOffset(t *testing.T) {
	var syntaxTarget any
	syntaxErr := json.Unmarshal([]byte("{"), &syntaxTarget)
	e := &DecodeError{StatusCode: 200, Method: "GET", Path: "/open/v1/task",
		Body: "<html>login</html>", Err: syntaxErr}
	if got, want := e.Error(), "ticktick: GET tasks request: decode http 200 response: malformed JSON at byte 1"; got != want {
		t.Errorf("message = %q, want fixed JSON category and offset", got)
	}
	if strings.Contains(e.Error(), "html") {
		t.Error("decode diagnostic includes response body")
	}
}

func TestTruncateBodyRetainsRawBytes(t *testing.T) {
	want := string([]byte{'"', '\\', 'n', '\n', '\r', '\t', 0, 0x1b, 0xff})
	got := truncateBody([]byte(want))
	if got != want {
		t.Error("retained body changed controls, backslashes, or invalid UTF-8")
	}
	quoted := strconv.Quote(got)
	roundTrip, err := strconv.Unquote(quoted)
	if err != nil || roundTrip != want {
		t.Error("quoting and unquoting the retained body did not recover its bytes")
	}
	if OneLine("\n") != OneLine(`\n`) {
		t.Error("OneLine test premise changed; update its non-invertible contract")
	}
}

func TestTruncateBodyMakesAWhitespaceBodyNothing(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"no body at all":                            {"", ""},
		"a body of one space":                       {" ", ""},
		"a body of one tab":                         {"\t", ""},
		"a body of one newline":                     {"\n", ""},
		"a body of whitespace of every run":         {" \t\r\n ", ""},
		"a space Unicode has of its own":            {string(rune(0x00a0)), ""},
		"a body that said something between spaces": {"  ok  ", "  ok  "},
		"a body that said something around a tab":   {"ok\tok", "ok\tok"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := truncateBody([]byte(c.in)); got != c.want {
				t.Errorf("retained body differs in whitespace case %q", name)
			}
		})
	}
}

func TestTruncateBodyCutInsideARune(t *testing.T) {
	body := append(bytes.Repeat([]byte("x"), maxErrorBodyLen-1), string(rune(0x00e9))...)
	got := truncateBody(body)
	want := append(bytes.Repeat([]byte("x"), maxErrorBodyLen-1), 0xc3)
	if !bytes.Equal([]byte(got), want) {
		t.Error("byte truncation changed or repaired a partial UTF-8 rune")
	}
}

func TestARedirectAwayFromHTTPSIsRefusedBeforeTheTokenGoes(t *testing.T) {
	var plainCalls int32
	var plainAuth string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&plainCalls, 1)
		plainAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer secure.Close()

	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Client
	}{
		{

			name: "the client's own http.Client",
			build: func(t *testing.T) *Client {
				c := testClient(t, secure)
				c.httpClient.Transport = secure.Client().Transport
				return c
			},
		},
		{

			name: "an http.Client handed in through WithHTTPClient",
			build: func(t *testing.T) *Client {
				return testClient(t, secure, WithHTTPClient(secure.Client()))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			atomic.StoreInt32(&plainCalls, 0)
			plainAuth = ""

			_, err := tc.build(t).ListProjects(context.Background())

			if got := atomic.LoadInt32(&plainCalls); got != 0 {
				t.Errorf("the plaintext hop was made %d time(s), want none at all", got)
			}
			if plainAuth != "" {
				t.Errorf("the plaintext hop was sent Authorization %q, want nothing", plainAuth)
			}

			if err == nil {
				t.Fatal("ListProjects followed a redirect from https to http and called it a success")
			}
			if !errors.Is(err, errInsecureRedirect) {
				t.Fatalf("err = %v, want the refusal of a redirect away from https", err)
			}

			if msg := err.Error(); !strings.Contains(msg, "redirect") || !strings.Contains(msg, "https") {
				t.Errorf("err = %v, want a message naming the redirect it refused", msg)
			}
		})
	}
}

func TestARedirectToASchemeThatIsNeitherIsRefusedByTheSameRule(t *testing.T) {
	for _, scheme := range []string{"ftp", "gopher"} {
		t.Run(scheme, func(t *testing.T) {
			secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", scheme+"://127.0.0.1:1/x")
				w.WriteHeader(http.StatusFound)
			}))
			defer secure.Close()

			c := testClient(t, secure, WithHTTPClient(secure.Client()), WithMaxRetries(1))

			if _, err := c.ListProjects(context.Background()); !errors.Is(err, errInsecureRedirect) {
				t.Fatalf("err = %v, want the refusal of a redirect away from https", err)
			}
		})
	}
}

func TestTheRefusalReadsTheHopBeforeItAndNotTheBaseURL(t *testing.T) {
	var bottomCalls int32
	var bottomAuth string
	bottom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bottomCalls, 1)
		bottomAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer bottom.Close()

	middle := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, bottom.URL+r.URL.Path, http.StatusFound)
	}))
	defer middle.Close()

	top := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, middle.URL+r.URL.Path, http.StatusFound)
	}))
	defer top.Close()

	c := testClient(t, top)
	c.httpClient.Transport = middle.Client().Transport
	_, err := c.ListProjects(context.Background())

	if got := atomic.LoadInt32(&bottomCalls); got != 0 {
		t.Errorf("the hop below https was made %d time(s), want none at all", got)
	}
	if bottomAuth != "" {
		t.Errorf("the hop below https was sent Authorization %q, want nothing", bottomAuth)
	}
	if !errors.Is(err, errInsecureRedirect) {
		t.Fatalf("err = %v, want the refusal of a redirect away from https", err)
	}
}

func TestARedirectWithinPlainHTTPIsStillFollowedWithTheToken(t *testing.T) {
	var gotAuth string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer final.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+r.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	projects, err := testClient(t, server).ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects over a plain-http redirect: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p1" {
		t.Fatalf("unexpected projects: %+v", projects)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("the hop redirected to was sent Authorization %q, want %q", gotAuth, "Bearer test-token")
	}
}

func TestARedirectThatKeepsOrGainsTLSIsStillFollowed(t *testing.T) {
	var gotAuth string
	final := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
	}))
	defer final.Close()

	sendOn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+r.URL.Path, http.StatusFound)
	})
	secure := httptest.NewTLSServer(sendOn)
	defer secure.Close()
	plain := httptest.NewServer(sendOn)
	defer plain.Close()

	for _, tc := range []struct {
		name string
		from *httptest.Server
	}{
		{"https to https", secure},
		{"http to https", plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAuth = ""

			c := testClient(t, tc.from)

			c.httpClient.Transport = final.Client().Transport

			projects, err := c.ListProjects(context.Background())
			if err != nil {
				t.Fatalf("ListProjects: %v", err)
			}
			if len(projects) != 1 || projects[0].ID != "p1" {
				t.Fatalf("unexpected projects: %+v", projects)
			}
			if gotAuth != "Bearer test-token" {
				t.Errorf("the hop redirected to was sent Authorization %q, want %q", gotAuth, "Bearer test-token")
			}
		})
	}
}

func TestARedirectLoopStillStopsAtTen(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	c := testClient(t, server, WithMaxRetries(1))
	_, err := c.ListProjects(context.Background())
	if err == nil {
		t.Fatal("ListProjects followed a redirect loop and called it a success")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want the chain stopped at ten redirects", err)
	}

	if got := atomic.LoadInt32(&calls); got != 10 {
		t.Errorf("the loop was walked %d time(s), want 10", got)
	}
}

func TestTheRefusalSurvivesAPolicyTheCallersClientAlreadyHad(t *testing.T) {
	t.Run("the step down off https is refused all the same", func(t *testing.T) {
		var plainCalls int32
		var plainAuth string
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&plainCalls, 1)
			plainAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer plain.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		hc := &http.Client{
			Transport:     secure.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return nil },
		}

		_, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

		if got := atomic.LoadInt32(&plainCalls); got != 0 {
			t.Errorf("the plaintext hop was made %d time(s), want none at all", got)
		}
		if plainAuth != "" {
			t.Errorf("the plaintext hop was sent Authorization %q, want nothing", plainAuth)
		}
		if err == nil {
			t.Fatal("ListProjects followed a redirect from https to http and called it a success")
		}
		if !errors.Is(err, errInsecureRedirect) {
			t.Fatalf("err = %v, want the refusal of a redirect away from https", err)
		}
	})

	t.Run("and the caller's policy still decides a hop the refusal allows", func(t *testing.T) {
		var finalCalls int32
		final := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&finalCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer final.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, final.URL+r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		errCallerStopped := errors.New("the caller's policy stopped this chain")
		hc := &http.Client{
			Transport:     final.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return errCallerStopped },
		}

		_, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

		if got := atomic.LoadInt32(&finalCalls); got != 0 {
			t.Errorf("the hop the caller's policy refused was made %d time(s), want none at all", got)
		}
		if !errors.Is(err, errCallerStopped) {
			t.Fatalf("err = %v, want the refusal the caller's own policy gave", err)
		}
	})

	t.Run("and a hop both halves allow is followed with the token", func(t *testing.T) {
		var gotAuth string
		final := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer final.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, final.URL+r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		var policyCalls int32
		hc := &http.Client{
			Transport: final.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				atomic.AddInt32(&policyCalls, 1)
				return nil
			},
		}

		projects, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())
		if err != nil {
			t.Fatalf("ListProjects over a redirect neither half objects to: %v", err)
		}
		if len(projects) != 1 || projects[0].ID != "p1" {
			t.Fatalf("unexpected projects: %+v", projects)
		}
		if gotAuth != "Bearer test-token" {
			t.Errorf("the hop redirected to was sent Authorization %q, want %q", gotAuth, "Bearer test-token")
		}
		if got := atomic.LoadInt32(&policyCalls); got != 1 {
			t.Errorf("the caller's policy was consulted %d time(s), want 1", got)
		}
	})
}

func TestACallersPolicyCannotRewriteAHopPastTheRefusal(t *testing.T) {
	t.Run("a hop rewritten down to plain http is refused all the same", func(t *testing.T) {
		var plainCalls int32
		var plainAuth string
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&plainCalls, 1)
			plainAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer plain.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		hc := &http.Client{
			Transport: secure.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				u, err := url.Parse(plain.URL + req.URL.Path)
				if err != nil {
					return err
				}
				req.URL = u
				return nil
			},
		}

		_, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

		if got := atomic.LoadInt32(&plainCalls); got != 0 {
			t.Errorf("the plaintext hop was made %d time(s), want none at all", got)
		}
		if plainAuth != "" {
			t.Errorf("the plaintext hop was sent Authorization %q, want nothing", plainAuth)
		}
		if !errors.Is(err, errInsecureRedirect) {
			t.Fatalf("err = %v, want the refusal of a redirect away from https", err)
		}
	})

	t.Run("and http.ErrUseLastResponse reaches net/http as itself", func(t *testing.T) {
		var finalCalls int32
		final := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&finalCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer final.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, final.URL+r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		hc := &http.Client{
			Transport:     final.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		}

		_, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

		if got := atomic.LoadInt32(&finalCalls); got != 0 {
			t.Errorf("the hop the caller's policy declined was made %d time(s), want none at all", got)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusFound {
			t.Fatalf("err = %v, want the 302 handed back as the answer", err)
		}
	})

	t.Run("and it does so on the hop the refusal would have refused", func(t *testing.T) {
		var plainCalls int32
		var plainAuth string
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&plainCalls, 1)
			plainAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Project{{ID: "p1", Name: "Inbox"}})
		}))
		defer plain.Close()

		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
		}))
		defer secure.Close()

		hc := &http.Client{
			Transport:     secure.Client().Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		}

		_, err := testClient(t, secure, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

		if got := atomic.LoadInt32(&plainCalls); got != 0 {
			t.Errorf("the plaintext hop was made %d time(s), want none at all", got)
		}
		if plainAuth != "" {
			t.Errorf("the plaintext hop was sent Authorization %q, want nothing", plainAuth)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusFound {
			t.Fatalf("err = %v, want the 302 the caller's policy asked to keep", err)
		}
	})
}

func TestACallerCapBeyondTenIsNotCutShortAtTen(t *testing.T) {

	const callerCap = 12

	var hops int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	errCallerCap := errors.New("the caller's own cap on the chain")
	hc := &http.Client{
		Transport: server.Client().Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= callerCap {
				return errCallerCap
			}
			return nil
		},
	}

	_, err := testClient(t, server, WithHTTPClient(hc), WithMaxRetries(1)).ListProjects(context.Background())

	if !errors.Is(err, errCallerCap) {
		t.Fatalf("err = %v, want the cap the caller's own policy imposed", err)
	}
	if got := atomic.LoadInt32(&hops); got != callerCap {
		t.Errorf("the chain ran %d hop(s), want the %d the caller's policy allows", got, callerCap)
	}
}

func TestWithHTTPClientDoesNotWriteIntoTheCallersClient(t *testing.T) {
	t.Run("the redirect policy goes on this package's copy", func(t *testing.T) {
		transport := &http.Transport{}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("cookiejar.New: %v", err)
		}
		hc := &http.Client{Transport: transport, Jar: jar}

		c := NewClient("test-token", "0.0.0-test", WithHTTPClient(hc))

		if hc.CheckRedirect != nil {
			t.Error("NewClient put a redirect policy on the caller's http.Client")
		}
		if c.httpClient == hc {
			t.Fatal("the client kept the caller's http.Client itself rather than a copy of it")
		}

		if c.httpClient.CheckRedirect == nil {
			t.Error("the client was built with no redirect policy on it at all")
		}
		if c.httpClient.Transport != transport {
			t.Error("the copy did not keep the caller's transport")
		}
		if c.httpClient.Jar != jar {
			t.Error("the copy did not keep the caller's cookie jar")
		}
	})

	t.Run("and so does a timeout set by a later option", func(t *testing.T) {
		hc := &http.Client{Timeout: 7 * time.Second}

		c := NewClient("test-token", "0.0.0-test", WithHTTPClient(hc), WithTimeout(3*time.Second))

		if hc.Timeout != 7*time.Second {
			t.Errorf("the caller's http.Client Timeout = %v, want the %v it arrived with",
				hc.Timeout, 7*time.Second)
		}

		if c.httpClient.Timeout != 3*time.Second {
			t.Errorf("the client's Timeout = %v, want the %v WithTimeout set", c.httpClient.Timeout, 3*time.Second)
		}
	})
}
