package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResourceRoutesAndPresence(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, method, path, request, response string
		call                                  func(*Client) (any, error)
	}{
		{"groups", "GET", "/open/v1/project/group", "", `[{"id":"g","name":"Group"}]`, func(c *Client) (any, error) { return c.ListProjectGroups(ctx) }},
		{"group create", "POST", "/open/v1/project/group", `{"name":"Group"}`, `{"id":"g","name":"Group"}`, func(c *Client) (any, error) { return c.CreateProjectGroup(ctx, ProjectGroupCreate{Name: "Group"}) }},
		{"group update", "POST", "/open/v1/project/group/g%2Fa%3Fx%23", `{"name":"Renamed"}`, `{"id":"g","name":"Renamed"}`, func(c *Client) (any, error) {
			return c.UpdateProjectGroup(ctx, "g/a?x#", ProjectGroupUpdate{Name: Ptr("Renamed")})
		}},
		{"group delete", "DELETE", "/open/v1/project/group/g", "", "", func(c *Client) (any, error) { return nil, c.DeleteProjectGroup(ctx, "g") }},
		{"columns", "GET", "/open/v1/project/p/column", "", `[{"id":"c","projectId":"p","name":"Column"}]`, func(c *Client) (any, error) { return c.ListColumns(ctx, "p") }},
		{"column create", "POST", "/open/v1/project/p/column", `{"name":"Column"}`, `{"id":"c","projectId":"p","name":"Column"}`, func(c *Client) (any, error) { return c.CreateColumn(ctx, "p", ColumnCreate{Name: "Column"}) }},
		{"column update", "POST", "/open/v1/project/p%2Fx/column/c%3Fq%23", `{"name":"Renamed"}`, `{"id":"c","projectId":"p","name":"Renamed"}`, func(c *Client) (any, error) {
			return c.UpdateColumn(ctx, "p/x", "c?q#", ColumnUpdate{Name: Ptr("Renamed")})
		}},
		{"tags", "GET", "/open/v1/tag", "", `[{"name":"work","label":"Work"}]`, func(c *Client) (any, error) { return c.ListTags(ctx) }},
		{"tag create", "POST", "/open/v1/tag", `{"name":"work","label":"Work"}`, `{"name":"work","label":"Work"}`, func(c *Client) (any, error) { return c.CreateTag(ctx, TagCreate{Name: "work", Label: "Work"}) }},
		{"habits", "GET", "/open/v1/habit", "", `[{"id":"h","name":"Habit"}]`, func(c *Client) (any, error) { return c.ListHabits(ctx) }},
		{"habit get", "GET", "/open/v1/habit/h%2F1%3F", "", `{"id":"h","name":"Habit"}`, func(c *Client) (any, error) { return c.GetHabit(ctx, "h/1?") }},
		{"habit create", "POST", "/open/v1/habit", `{"name":"Habit","goal":0,"recordEnable":false}`, `{"id":"h","name":"Habit"}`, func(c *Client) (any, error) {
			return c.CreateHabit(ctx, HabitCreate{Name: Ptr("Habit"), Goal: Ptr(0.0), RecordEnable: Ptr(false)})
		}},
		{"habit update", "POST", "/open/v1/habit/h", `{"sortOrder":0,"status":0,"reminders":[],"exDates":[],"goal":0}`, `{"id":"h","name":"Habit"}`, func(c *Client) (any, error) {
			return c.UpdateHabit(ctx, "h", HabitUpdate{SortOrder: Ptr(int64(0)), Status: Ptr(0), Reminders: Ptr([]string{}), ExDates: Ptr([]string{}), Goal: Ptr(0.0)})
		}},
		{"sections", "GET", "/open/v1/habit/sections", "", `[{"id":"s","name":"Section"}]`, func(c *Client) (any, error) { return c.ListHabitSections(ctx) }},
		{"checkin upsert", "POST", "/open/v1/habit/h/checkin", `{"stamp":20260911,"value":0,"goal":0,"status":0}`, `{"habitId":"h","year":2026,"checkins":[{"stamp":20260911,"value":0}]}`, func(c *Client) (any, error) {
			return c.UpsertHabitCheckin(ctx, "h", HabitCheckinInput{Stamp: 20260911, Value: Ptr(0.0), Goal: Ptr(0.0), Status: Ptr(0)})
		}},
		{"checkin history", "GET", "/open/v1/habit/checkins?from=20260901&habitIds=h%2F1%2Cx%3Fy&to=20260911", "", `[{"habitId":"h","checkins":[]}]`, func(c *Client) (any, error) {
			return c.GetHabitCheckins(ctx, HabitCheckinQuery{HabitIDs: []string{"h/1", "x?y"}, From: 20260901, To: 20260911})
		}},
		{"comments", "GET", "/open/v1/project/p/task/t/comments", "", `[{"id":"c","title":"Note"}]`, func(c *Client) (any, error) { return c.ListTaskComments(ctx, "p", "t") }},
		{"comment add", "POST", "/open/v1/project/p/task/t/comment", `{"title":"Note\nwith quotes: \"yes\""}`, `{"id":"c","title":"Note"}`, func(c *Client) (any, error) {
			return c.AddTaskComment(ctx, "p", "t", CommentCreate{Title: "Note\nwith quotes: \"yes\""})
		}},
		{"comment delete", "DELETE", "/open/v1/project/p%2Fx/task/t%3Fy/comment/c%23z", "", "", func(c *Client) (any, error) { return nil, c.DeleteTaskComment(ctx, "p/x", "t?y", "c#z") }},
		{"focus delete", "DELETE", "/open/v1/focus/f%2F1%3Fx?type=1", "", `{"id":"f/1?x","type":1}`, func(c *Client) (any, error) { return c.DeleteFocus(ctx, "f/1?x", FocusTiming) }},
		{"countdowns", "GET", "/open/v1/countdown", "", `[{"id":"d","date":20260911}]`, func(c *Client) (any, error) { return c.ListCountdowns(ctx) }},
		{"project get", "GET", "/open/v1/project/p%2Fx", "", `{"id":"p","name":"Project"}`, func(c *Client) (any, error) { return c.GetProject(ctx, "p/x") }},
		{"project page", "GET", "/open/v1/project?limit=200&offset=400", "", `[{"id":"p","name":"Project"}]`, func(c *Client) (any, error) { return c.ListProjectsPage(ctx, 400, 200) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != tc.method || r.URL.RequestURI() != tc.path {
					t.Errorf("request = %s %s; want %s %s", r.Method, r.URL.RequestURI(), tc.method, tc.path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if tc.request == "" {
					if len(body) != 0 {
						t.Errorf("unexpected request body: %s", body)
					}
				} else {
					var got, want any
					if err := json.Unmarshal(body, &got); err != nil {
						t.Error(err)
					}
					if err := json.Unmarshal([]byte(tc.request), &want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("body = %s; want %s", body, tc.request)
					}
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			if _, err := tc.call(testClient(t, server)); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

func TestResourceRawSnapshotsAndMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		value      func() any
		raw        func(any) json.RawMessage
	}{
		{"group", `{"id":"g","name":"G","future":{"v":1}}`, func() any { return &ProjectGroup{} }, func(v any) json.RawMessage { return v.(*ProjectGroup).Raw }},
		{"column", `{"id":"c","projectId":"p","future":false}`, func() any { return &Column{} }, func(v any) json.RawMessage { return v.(*Column).Raw }},
		{"project", `{"id":"p","groupId":"g","future":0}`, func() any { return &Project{} }, func(v any) json.RawMessage { return v.(*Project).Raw }},
		{"tag", `{"name":"tag","label":"Tag","future":[]}`, func() any { return &Tag{} }, func(v any) json.RawMessage { return v.(*Tag).Raw }},
		{"habit", `{"id":"h","goal":0,"future":null}`, func() any { return &Habit{} }, func(v any) json.RawMessage { return v.(*Habit).Raw }},
		{"section", `{"id":"s","future":"unknown"}`, func() any { return &HabitSection{} }, func(v any) json.RawMessage { return v.(*HabitSection).Raw }},
		{"checkin", `{"habitId":"h","checkins":[{"stamp":20260911,"value":0}],"future":true}`, func() any { return &HabitCheckin{} }, func(v any) json.RawMessage { return v.(*HabitCheckin).Raw }},
		{"comment", `{"id":"c","title":"comment","replyCommentId":"r","future":0}`, func() any { return &Comment{} }, func(v any) json.RawMessage { return v.(*Comment).Raw }},
		{"countdown", `{"id":"c","date":20260911,"showCalendarType":99,"future":0}`, func() any { return &Countdown{} }, func(v any) json.RawMessage { return v.(*Countdown).Raw }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.value()
			if err := json.Unmarshal([]byte(tc.body), v); err != nil {
				t.Fatal(err)
			}
			if string(tc.raw(v)) != tc.body {
				t.Fatalf("raw snapshot changed: %s", tc.raw(v))
			}
			encoded, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"Raw"`) || strings.Contains(string(encoded), `"raw"`) {
				t.Fatalf("raw snapshot leaked into DTO: %s", encoded)
			}
		})
	}
	for _, tc := range []struct {
		name string
		call func(*Client) (any, error)
	}{
		{"groups", func(c *Client) (any, error) { return c.ListProjectGroups(context.Background()) }},
		{"tags", func(c *Client) (any, error) { return c.ListTags(context.Background()) }},
		{"habits", func(c *Client) (any, error) { return c.ListHabits(context.Background()) }},
		{"sections", func(c *Client) (any, error) { return c.ListHabitSections(context.Background()) }},
		{"comments", func(c *Client) (any, error) { return c.ListTaskComments(context.Background(), "p", "t") }},
		{"countdowns", func(c *Client) (any, error) { return c.ListCountdowns(context.Background()) }},
		{"columns", func(c *Client) (any, error) { return c.ListColumns(context.Background(), "p") }},
	} {
		for _, body := range []string{"null", "[null]", "[{}]"} {
			t.Run(tc.name+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
				defer server.Close()
				if _, err := tc.call(testClient(t, server)); !errors.Is(err, ErrIncompleteAnswer) {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

func TestResourceWritesAreNeverRetried(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*Client) (any, error)
	}{
		{"group", func(c *Client) (any, error) { return c.CreateProjectGroup(ctx, ProjectGroupCreate{Name: "G"}) }},
		{"column", func(c *Client) (any, error) { return c.CreateColumn(ctx, "p", ColumnCreate{Name: "C"}) }},
		{"tag", func(c *Client) (any, error) { return c.CreateTag(ctx, TagCreate{Name: "tag", Label: "Tag"}) }},
		{"habit", func(c *Client) (any, error) { return c.CreateHabit(ctx, HabitCreate{Name: Ptr("H")}) }},
		{"checkin", func(c *Client) (any, error) {
			return c.UpsertHabitCheckin(ctx, "h", HabitCheckinInput{Stamp: 20260911})
		}},
		{"comment", func(c *Client) (any, error) { return c.AddTaskComment(ctx, "p", "t", CommentCreate{Title: "note"}) }},
		{"focus delete", func(c *Client) (any, error) { return c.DeleteFocus(ctx, "f", FocusTiming) }},
	} {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusCreated} {
			t.Run(tc.name+"/"+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(status) }))
				defer server.Close()
				_, err := tc.call(NewClient("test", "test", WithBaseURL(server.URL), WithMaxRetries(4)))
				if err == nil || calls.Load() != 1 {
					t.Fatalf("err = %v, calls = %d", err, calls.Load())
				}
				if status == http.StatusCreated && !errors.Is(err, ErrIncompleteAnswer) {
					t.Fatalf("unconfirmed allocation: %v", err)
				}
			})
		}
	}
}

func TestResourceValidationAvoidsNetwork(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, `{"id":"x"}`) }))
	defer server.Close()
	c := testClient(t, server)
	checks := []func() error{
		func() error { _, err := c.GetHabit(ctx, ".."); return err },
		func() error {
			_, err := c.CreateProjectGroup(ctx, ProjectGroupCreate{Name: strings.Repeat("x", 65)})
			return err
		},
		func() error { _, err := c.CreateColumn(ctx, "p", ColumnCreate{Name: " "}); return err },
		func() error { _, err := c.UpdateColumn(ctx, "p", "c", ColumnUpdate{}); return err },
		func() error { _, err := c.CreateTag(ctx, TagCreate{Name: "UPPER", Label: "UPPER"}); return err },
		func() error { _, err := c.CreateTag(ctx, TagCreate{Name: "tag", Label: "Other"}); return err },
		func() error { _, err := c.CreateHabit(ctx, HabitCreate{}); return err },
		func() error {
			_, err := c.CreateHabit(ctx, HabitCreate{Name: Ptr("H"), Goal: Ptr(math.Inf(1))})
			return err
		},
		func() error { _, err := c.UpsertHabitCheckin(ctx, "h", HabitCheckinInput{Stamp: 20260230}); return err },
		func() error {
			_, err := c.UpsertHabitCheckin(ctx, "h", HabitCheckinInput{Stamp: 20260911, Value: Ptr(math.NaN())})
			return err
		},
		func() error {
			_, err := c.GetHabitCheckins(ctx, HabitCheckinQuery{HabitIDs: []string{"h,x"}, From: 20260901, To: 20260911})
			return err
		},
		func() error {
			_, err := c.GetHabitCheckins(ctx, HabitCheckinQuery{HabitIDs: []string{"h"}, From: 20260912, To: 20260911})
			return err
		},
		func() error { _, err := c.AddTaskComment(ctx, "p", "t", CommentCreate{}); return err },
		func() error { _, err := c.DeleteFocus(ctx, "f", FocusType(2)); return err },
		func() error { _, err := c.ListProjectsPage(ctx, -1, 200); return err },
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

func TestResourceCollectionsRejectDuplicatesAndPreserveEmptyArrays(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, body string
		call       func(*Client) (any, error)
	}{
		{"groups", `[{"id":"g"},{"id":"g"}]`, func(c *Client) (any, error) { return c.ListProjectGroups(ctx) }},
		{"columns", `[{"id":"c","projectId":"p"},{"id":"c","projectId":"p"}]`, func(c *Client) (any, error) { return c.ListColumns(ctx, "p") }},
		{"projects", `[{"id":"p"},{"id":"p"}]`, func(c *Client) (any, error) { return c.ListProjectsPage(ctx, 0, 200) }},
		{"tags", `[{"name":"tag"},{"name":"tag"}]`, func(c *Client) (any, error) { return c.ListTags(ctx) }},
		{"habits", `[{"id":"h"},{"id":"h"}]`, func(c *Client) (any, error) { return c.ListHabits(ctx) }},
		{"comments", `[{"id":"c"},{"id":"c"}]`, func(c *Client) (any, error) { return c.ListTaskComments(ctx, "p", "t") }},
		{"countdowns", `[{"id":"c"},{"id":"c"}]`, func(c *Client) (any, error) { return c.ListCountdowns(ctx) }},
		{"checkin years", `[{"habitId":"h","year":2026,"checkins":[]},{"habitId":"h","year":2026,"checkins":[]}]`, func(c *Client) (any, error) {
			return c.GetHabitCheckins(ctx, HabitCheckinQuery{HabitIDs: []string{"h"}, From: 20260901, To: 20260911})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response atomic.Value
			response.Store(tc.body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response.Load().(string)) }))
			defer server.Close()
			c := testClient(t, server)
			if _, err := tc.call(c); !errors.Is(err, ErrIncompleteAnswer) {
				t.Fatalf("duplicate list error: %v", err)
			}
			response.Store("[]")
			out, err := tc.call(c)
			if err != nil {
				t.Fatal(err)
			}
			v := reflect.ValueOf(out)
			if v.Kind() != reflect.Slice || v.IsNil() || v.Len() != 0 {
				t.Fatalf("empty array became %#v", out)
			}
		})
	}
}

func TestHabitCheckinHistoryValidatesDatesAndYearIdentity(t *testing.T) {
	for _, body := range []string{
		`{"habitId":"h","checkins":null}`,
		`{"habitId":"h","checkins":[{"stamp":20260230}]}`,
		`{"habitId":"h","year":2026,"checkins":[{"stamp":20250911}]}`,
		`{"habitId":"h","year":2026,"checkins":[{"stamp":20260911},{"stamp":20260911}]}`,
	} {
		var out HabitCheckin
		if err := json.Unmarshal([]byte(body), &out); !errors.Is(err, ErrIncompleteAnswer) {
			t.Fatalf("body=%s err=%v", body, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"habitId":"h","year":2025,"checkins":[{"stamp":20250911}]},{"habitId":"h","year":2026,"checkins":[{"stamp":20260911}]}]`)
	}))
	defer server.Close()
	out, err := testClient(t, server).GetHabitCheckins(context.Background(), HabitCheckinQuery{HabitIDs: []string{"h"}, From: 20250911, To: 20260911})
	if err != nil || len(out) != 2 {
		t.Fatalf("distinct years collapsed: out=%+v err=%v", out, err)
	}
}
