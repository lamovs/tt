package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerTaskQueriesPreserveWireAndRaw(t *testing.T) {
	ctx := context.Background()
	const raw = `{"id":"t","projectId":"p","title":"task","parentId":"parent","columnId":"column","future":{"preserve":true}}`
	for _, tc := range []struct {
		name, path, body string
		call             func(*Client) ([]RawTask, error)
	}{
		{"completed", "/open/v1/task/completed", `{"projectIds":["p"],"startDate":"2026-09-01T00:00:00+0300","endDate":"2026-09-11T23:59:59+0300"}`, func(c *Client) ([]RawTask, error) {
			return c.GetCompletedTasks(ctx, CompletedTaskFilter{ProjectIDs: Ptr([]string{"p"}), StartDate: Ptr("2026-09-01T00:00:00+0300"), EndDate: Ptr("2026-09-11T23:59:59+0300")})
		}},
		{"filter", "/open/v1/task/filter", `{"priority":[0,5],"status":[0],"tag":[],"kind":["NOTE"]}`, func(c *Client) ([]RawTask, error) {
			return c.FilterTasks(ctx, TaskFilter{Priority: Ptr([]int{0, 5}), Status: Ptr([]int{0}), Tag: Ptr([]string{}), Kind: Ptr([]string{"NOTE"})})
		}},
		{"search", "/open/v1/task/search", `{"keywords":"quotes \"yes\" & punctuation?","tags":["work"],"status":[2]}`, func(c *Client) ([]RawTask, error) {
			return c.SearchTasks(ctx, TaskSearch{Keywords: Ptr("quotes \"yes\" & punctuation?"), Tags: Ptr([]string{"work"}), Status: Ptr([]int{2})})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.RequestURI() != tc.path {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				var got, want any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("request = %#v, want %#v", got, want)
				}
				_, _ = io.WriteString(w, "["+raw+"]")
			}))
			defer server.Close()
			out, err := tc.call(testClient(t, server))
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || out[0].Task.ID != "t" || string(out[0].Raw) != raw {
				t.Fatalf("response = %+v", out)
			}
		})
	}
}

func TestServerQueriesDoNotTurnMalformedOrCappedResultsIntoEmptySuccess(t *testing.T) {
	ctx := context.Background()
	for _, body := range []string{"null", "[null]", "[{}]", `[{"id":"t"}]`, `[{"id":"t","projectId":"p"},{"id":"t","projectId":"p"}]`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			out, err := testClient(t, server).GetCompletedTasks(ctx, CompletedTaskFilter{})
			if !errors.Is(err, ErrIncompleteAnswer) || out != nil {
				t.Fatalf("out=%v err=%v", out, err)
			}
		})
	}
	var rows []map[string]any
	for i := 0; i < TaskQueryLimit; i++ {
		rows = append(rows, map[string]any{"id": strings.Repeat("x", i+1), "projectId": "p"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(rows) }))
	defer server.Close()
	out, err := testClient(t, server).FilterTasks(ctx, TaskFilter{})
	if err != nil || len(out) != TaskQueryLimit {
		t.Fatalf("capped rows discarded: len=%d err=%v", len(out), err)
	}
}

func TestTaskMoveConfirmationAndNoReplay(t *testing.T) {
	ctx := context.Background()
	input := []TaskMove{{FromProjectID: "p", ToProjectID: "q", TaskID: "t"}}
	for _, tc := range []struct {
		name, body string
		status     int
		success    bool
	}{
		{"confirmed", `[{"id":"t","etag":"new","future":true}]`, 200, true},
		{"empty array", "[]", 200, false},
		{"missing etag", `[{"id":"t"}]`, 200, false},
		{"wrong task", `[{"id":"other","etag":"new"}]`, 200, false},
		{"duplicate task", `[{"id":"t","etag":"new"},{"id":"t","etag":"new"}]`, 200, false},
		{"empty success", "", 201, false},
		{"server error", "", 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task/move" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var got []TaskMove
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil || !reflect.DeepEqual(got, input) {
					t.Errorf("moves = %+v, err=%v", got, err)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			out, err := NewClient("test", "test", WithBaseURL(server.URL), WithMaxRetries(4)).MoveTasks(ctx, input)
			if (err == nil) != tc.success || calls.Load() != 1 {
				t.Fatalf("out=%v err=%v calls=%d", out, err, calls.Load())
			}
			if tc.success && (len(out) != 1 || string(out[0].Raw) != `{"id":"t","etag":"new","future":true}`) {
				t.Fatalf("lost result: %+v", out)
			}
		})
	}
}

func TestQueryAndMoveValidationAvoidsNetwork(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "[]") }))
	defer server.Close()
	c := testClient(t, server)
	checks := []func() error{
		func() error { _, err := c.SearchTasks(ctx, TaskSearch{Keywords: Ptr(" ")}); return err },
		func() error { _, err := c.FilterTasks(ctx, TaskFilter{Priority: Ptr([]int{2})}); return err },
		func() error {
			_, err := c.GetCompletedTasks(ctx, CompletedTaskFilter{StartDate: Ptr("2026-09-11")})
			return err
		},
		func() error {
			_, err := c.GetCompletedTasks(ctx, CompletedTaskFilter{StartDate: Ptr("2026-09-12T00:00:00Z"), EndDate: Ptr("2026-09-11T00:00:00Z")})
			return err
		},
		func() error { _, err := c.MoveTasks(ctx, nil); return err },
		func() error {
			_, err := c.MoveTasks(ctx, []TaskMove{{TaskID: "t", FromProjectID: "p", ToProjectID: "p"}})
			return err
		},
		func() error {
			_, err := c.MoveTasks(ctx, []TaskMove{{TaskID: "t", FromProjectID: "p", ToProjectID: "q"}, {TaskID: "t", FromProjectID: "p", ToProjectID: "q"}})
			return err
		},
	}
	for i, check := range checks {
		if err := check(); err == nil {
			t.Errorf("case %d accepted invalid input", i)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs issued %d requests", calls.Load())
	}
}
