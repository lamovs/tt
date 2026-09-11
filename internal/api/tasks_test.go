package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetTaskRawPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"id":"t1","projectId":"p1","title":"milk","items":[],"future":{"x":1}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/open/v1/project/p1/task/t1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	got, err := testClient(t, server).GetTaskRaw(context.Background(), "p1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.ID != "t1" || got.Task.Title != "milk" {
		t.Fatalf("decoded task = %+v", got.Task)
	}
	if !bytes.Equal(got.Raw, body) {
		t.Fatalf("raw task = %s, want %s", got.Raw, body)
	}
}

func TestGetTaskRawPreservesTheActualSuccessStatusOnDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":[]}`))
	}))
	defer server.Close()

	_, err := testClient(t, server).GetTaskRaw(context.Background(), "p1", "t1")
	var decodeError *DecodeError
	if !errors.As(err, &decodeError) {
		t.Fatalf("error = %T %v, want DecodeError", err, err)
	}
	if decodeError.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", decodeError.StatusCode, http.StatusCreated)
	}
}

func TestCreateTaskWithoutAProjectGoesToTheInbox(t *testing.T) {
	var (
		calls  int
		method string
		path   string
		body   map[string]json.RawMessage
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		method, path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(Task{ID: "srv-1", ProjectID: "inbox1", Title: "no project"}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	got, err := testClient(t, server).CreateTask(context.Background(), TaskCreate{Title: "no project"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if method != "POST" || path != "/open/v1/task" {
		t.Errorf("got %s %s, want POST /open/v1/task", method, path)
	}
	if got, ok := body["projectId"]; !ok || string(got) != `""` {
		t.Errorf("projectId = %s, want the empty value the inbox create sends", got)
	}
	if got.ProjectID != "inbox1" {
		t.Errorf("projectId = %q, want the inbox the server named", got.ProjectID)
	}
}
