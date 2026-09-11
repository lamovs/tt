package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func serverQueryFixture(t *testing.T, handler http.HandlerFunc) (*ServerTaskQueries, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewServerTaskQueries(st, func() (*api.Client, error) {
		return api.NewClient("synthetic", "test", api.WithBaseURL(server.URL), api.WithHTTPClient(server.Client())), nil
	}), st
}

func TestServerTaskQueryCacheNeverMergesPrunesOrRenumbersTasks(t *testing.T) {
	var calls atomic.Int32
	service, st := serverQueryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/open/v1/task/completed" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `[{"id":"remote","projectId":"p","title":"Server only","status":2,"future":{"n":9007199254740993}}]`)
	})
	ctx := context.Background()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p", Name: "Project"}}); err != nil {
		t.Fatal(err)
	}
	local, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "Local only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetListing(ctx, []string{local.Id}); err != nil {
		t.Fatal(err)
	}
	before, err := st.Task(ctx, local.Id)
	if err != nil {
		t.Fatal(err)
	}
	var queueBefore int
	if err := st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&queueBefore); err != nil {
		t.Fatal(err)
	}
	q := ServerTaskQuery{Mode: "completed", ProjectIDs: []string{"p"}}
	if _, err := service.List(ctx, q, false); err == nil || calls.Load() != 0 {
		t.Fatal("cache miss used network or returned invented coverage")
	}
	listing, err := service.List(ctx, q, true)
	if err != nil || len(listing.Tasks) != 1 || listing.Tasks[0].Id != "remote" || listing.Meta.Completeness != "unknown" || listing.Meta.Source != "remote" {
		t.Fatalf("%+v %v", listing, err)
	}
	service.client = func() (*api.Client, error) { t.Fatal("cached query requested a token"); return nil, nil }
	cached, err := service.List(ctx, q, false)
	if err != nil || cached.Meta.Source != "local" || !cached.Meta.FetchedAt.Equal(listing.Meta.FetchedAt) || !strings.Contains(string(cached.Raw[0]), "9007199254740993") {
		t.Fatalf("%+v %v", cached, err)
	}
	after, err := st.Task(ctx, local.Id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("local task mutated")
	}
	if _, err := st.Task(ctx, "remote"); err == nil {
		t.Fatal("query row entered authoritative task cache")
	}
	refs, err := st.Listing(ctx)
	if err != nil || !reflect.DeepEqual(refs, []string{local.Id}) {
		t.Fatal("query replaced numbered references")
	}
	var queueAfter int
	if err := st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&queueAfter); err != nil || queueAfter != queueBefore {
		t.Fatal("query enqueued mutation")
	}
}

func TestServerTaskQueryInvalidRemotePreservesPriorCache(t *testing.T) {
	body := `[{"id":"t","projectId":"p","title":"old"}]`
	service, _ := serverQueryFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
	ctx := context.Background()
	q := ServerTaskQuery{Mode: "search", Text: "needle"}
	if _, err := service.List(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"null", `[{"id":"t"}]`, `[{"id":"t","projectId":"p"},{"id":"t","projectId":"p"}]`, "{}"} {
		body = invalid
		if _, err := service.List(ctx, q, true); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
		cached, err := service.List(ctx, q, false)
		if err != nil || cached.Tasks[0].Title != "old" {
			t.Fatal("failed fetch replaced cache")
		}
	}
	body = "[]"
	empty, err := service.List(ctx, q, true)
	if err != nil || len(empty.Tasks) != 0 || empty.Meta.Completeness != "unknown" {
		t.Fatalf("%+v %v", empty, err)
	}
}

func TestServerTaskQueryPartialCoverageWarningsAndWirePresence(t *testing.T) {
	service, _ := serverQueryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if string(request["priority"]) != "[0,5]" || string(request["status"]) != "[0]" || request["tags"] != nil || string(request["tag"]) != `["a","b"]` {
			t.Errorf("%s", request)
		}
		io.WriteString(w, "[")
		for i := 0; i < api.TaskQueryLimit; i++ {
			if i > 0 {
				io.WriteString(w, ",")
			}
			fmt.Fprintf(w, `{"id":"t%d","projectId":"p","dueDate":"malformed"}`, i)
		}
		io.WriteString(w, "]")
	})
	q := ServerTaskQuery{Mode: "filter", Priority: []int{5, 0}, Status: []int{0}, Tags: []string{"b", "a"}}
	listing, err := service.List(context.Background(), q, true)
	if err != nil || listing.Meta.Completeness != "partial" || len(listing.Tasks) != 200 || len(listing.Warnings) != 200 {
		t.Fatalf("rows=%d warnings=%d %+v %v", len(listing.Tasks), len(listing.Warnings), listing.Meta, err)
	}
	q.Tags = []string{"a", "b"}
	q.Priority = []int{0, 5}
	if _, err := service.List(context.Background(), q, false); err != nil {
		t.Fatal("set-equivalent query did not use same cache", err)
	}
}

func TestServerTaskQueryValidationAndCacheIdentity(t *testing.T) {
	service, st := serverQueryFixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid query used network") })
	for _, q := range []ServerTaskQuery{
		{Mode: "all"}, {Mode: "search", Text: " "}, {Mode: "completed", Tags: []string{"tag"}},
		{Mode: "filter", Text: "text"}, {Mode: "filter", Priority: []int{2}}, {Mode: "filter", Status: []int{1}},
		{Mode: "search", Text: "a", Kind: []string{"TEXT"}}, {Mode: "filter", Kind: []string{"OTHER"}},
		{Mode: "filter", From: "2026-09-11"}, {Mode: "filter", From: "2026-09-12T00:00:00Z", To: "2026-09-11T00:00:00Z"},
		{Mode: "filter", Tags: []string{""}}, {Mode: "filter", ProjectIDs: []string{"p", "p"}},
	} {
		if _, err := service.List(context.Background(), q, true); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	q := ServerTaskQuery{Mode: "filter"}
	if err := st.SetMeta(context.Background(), q.cacheKey(), `{"version":1,"query":{"mode":"search"},"rows":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), q, false); err == nil {
		t.Fatal("accepted mismatched cache identity")
	}
}

func TestServerTaskQueryNormalizationPreservesFractionalBounds(t *testing.T) {
	q, err := normalizeServerQuery(ServerTaskQuery{Mode: "completed", From: "2026-09-11T13:00:00.123456789+03:00", To: "2026-09-11T13:00:00.123456790+03:00"})
	if err != nil || q.From != "2026-09-11T10:00:00.123456789Z" || q.To != "2026-09-11T10:00:00.12345679Z" {
		t.Fatalf("%+v %v", q, err)
	}
}
