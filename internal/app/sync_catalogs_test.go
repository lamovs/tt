package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

type syncTopicClient struct {
	reads int
	err   error
}

func (c *syncTopicClient) CredentialFingerprint() string { return "browser" }
func (c *syncTopicClient) ListTopics(context.Context) ([]webapi.Topic, error) {
	c.reads++
	return []webapi.Topic{{ID: "work-id", Name: "Work", Raw: json.RawMessage(`{"id":"work-id","name":"Work"}`)}}, c.err
}
func (c *syncTopicClient) CreateTopicFocus(context.Context, webapi.FocusCreate) (webapi.BatchResponse, error) {
	return webapi.BatchResponse{}, errors.New("unexpected Browser write")
}
func (c *syncTopicClient) GetTopicFocus(context.Context, string, int) (webapi.FocusCreate, error) {
	return webapi.FocusCreate{}, errors.New("unexpected Browser read-back")
}

func TestSyncCatalogsBrowserFailurePreservesCacheAndContinuesOpenWork(t *testing.T) {
	for _, browserFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "expired Browser"}[browserFailure], func(t *testing.T) {
			_, st := actionStore(t)
			ctx := context.Background()
			if err := st.ReplaceFocusTopics(ctx, []store.FocusTopic{{ID: "old", Name: "Old", Raw: json.RawMessage(`{}`)}}, "old-browser", time.Now()); err != nil {
				t.Fatal(err)
			}
			browser := &syncTopicClient{}
			if browserFailure {
				browser.err = &webapi.StatusError{StatusCode: 401}
			}
			paths := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths[r.URL.Path]++
				if r.Method != "GET" {
					t.Error("catalog sync wrote remote data")
				}
				w.Header().Set("Content-Type", "application/json")
				var result any = []any{}
				switch {
				case r.URL.Path == "/open/v1/project":
					result = []api.Project{{ID: "work", Name: "Work"}, {ID: "home", Name: "Home"}}
				case strings.HasSuffix(r.URL.Path, "/data"):
					result = map[string]any{"tasks": []any{}, "columns": []any{}}
				case r.URL.Path == "/open/v1/tag":
					result = []any{map[string]any{"name": "study", "label": "Study"}}
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			client := func() (*api.Client, error) {
				return api.NewClient("synthetic", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1)), nil
			}
			out := RunSync(ctx, st, config.Default(), client, nil, func() (focus.TopicClient, error) { return browser, nil })
			if out.Err != nil || out.Tasks.ErrorCount() != 0 || out.RefreshFailed() != browserFailure {
				t.Fatalf("sync %+v", out)
			}
			if browser.reads != 1 {
				t.Fatalf("Browser reads %d", browser.reads)
			}
			for _, path := range []string{"/open/v1/project", "/open/v1/project/group", "/open/v1/tag", "/open/v1/habit", "/open/v1/countdown", "/open/v1/project/work/column"} {
				if paths[path] == 0 {
					t.Errorf("missing refresh %s", path)
				}
			}
			topics, err := st.FocusTopics(ctx)
			want := "work-id"
			if browserFailure {
				want = "old"
			}
			if err != nil || len(topics) != 1 || topics[0].ID != want {
				t.Fatalf("topics %+v %v", topics, err)
			}
			tags, err := st.Entities(ctx, "tag", "", false)
			if err != nil || len(tags) != 1 {
				t.Fatalf("tags not refreshed: %+v %v", tags, err)
			}
		})
	}
}

func TestCatalogRefreshRepeatsOnlyRememberedHistoryScope(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	query := ResourceQuery{Kind: "focus", FocusType: api.FocusTiming, From: "2026-09-10T10:00:00Z", To: "2026-09-10T11:00:00Z"}
	focusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open/v1/focus" {
			focusCalls++
			if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
				t.Error("history bounds missing")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()
	resources := NewResources(st, func() (*api.Client, error) {
		return api.NewClient("synthetic", "test", api.WithBaseURL(server.URL)), nil
	})
	if _, err := resources.List(ctx, query, true); err != nil {
		t.Fatal(err)
	}
	for _, result := range resources.RefreshCatalogs(ctx) {
		if result.State == "failed" {
			t.Fatal(result)
		}
	}
	if focusCalls != 2 {
		t.Fatalf("focus requests %d", focusCalls)
	}
}
