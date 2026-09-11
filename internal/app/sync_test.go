package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

func TestExplicitSyncOutcomesAndFocusOrdering(t *testing.T) {
	for _, mode := range []string{"success", "expired Browser", "pull failure", "cancel pull", "lost create reply", "disabled focus"} {
		t.Run(mode, func(t *testing.T) {
			a, st := actionStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			created, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Created offline", Body: "body"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			if _, err = st.StartTimer(ctx, store.TimerStartOptions{TaskID: created.Task.Id, FocusType: 1}, now); err != nil {
				t.Fatal(err)
			}
			if _, err = st.StopTimer(ctx, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.FocusUpload.Enabled = mode != "disabled focus"
			var mu sync.Mutex
			var paths []string
			task := map[string]any{}
			var focusRecord map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				var answer any
				switch {
				case r.Method == "POST" && r.URL.Path == "/open/v1/task":
					if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
						t.Error(err)
					}
					task["id"] = "remote-task"
					if mode == "lost create reply" {
						cancel()
						return
					}
					answer = task
				case r.URL.Path == "/open/v1/project":
					if mode == "pull failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if mode == "cancel pull" {
						cancel()
						return
					}
					answer = []api.Project{{ID: "work", Name: "Work"}, {ID: "home", Name: "Home"}}
				case strings.HasSuffix(r.URL.Path, "/data"):
					answer = map[string]any{"tasks": []any{}, "columns": []any{}}
					if r.URL.Path == "/open/v1/project/work/data" {
						answer = map[string]any{"project": api.Project{ID: "work", Name: "Work"}, "tasks": []any{task}, "columns": []any{}}
					}
				case r.Method == "GET" && r.URL.Path == "/open/v1/focus":
					answer = []any{}
				case r.Method == "POST" && r.URL.Path == "/open/v1/focus":
					var request api.FocusCreate
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.TaskID != "remote-task" {
						t.Errorf("focus preceded ID promotion: %s", request.TaskID)
					}
					focusRecord = map[string]any{"id": "focus-remote", "type": request.Type, "startTime": request.StartTime, "endTime": request.EndTime, "duration": request.Duration * 1000, "pauseDuration": request.PauseDuration, "note": request.Note, "tasks": []any{map[string]any{"taskId": request.TaskID}}}
					answer = focusRecord
				case r.URL.Path == "/open/v1/focus/focus-remote":
					answer = focusRecord
				case r.Method == "GET" && (r.URL.Path == "/open/v1/project/group" || r.URL.Path == "/open/v1/tag" || r.URL.Path == "/open/v1/habit" || r.URL.Path == "/open/v1/countdown" || strings.HasSuffix(r.URL.Path, "/column")):
					answer = []any{}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if err := json.NewEncoder(w).Encode(answer); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := func() (*api.Client, error) {
				return api.NewClient("synthetic-token", "test", api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(time.Second)), nil
			}
			var phases []string
			var topics []focus.TopicClientFactory
			if mode == "expired Browser" {
				topics = append(topics, func() (focus.TopicClient, error) {
					return &syncTopicClient{err: &webapi.StatusError{StatusCode: 401}}, nil
				})
			}
			out := RunSync(ctx, st, cfg, client, func(phase string) { phases = append(phases, phase) }, topics...)
			if mode == "lost create reply" {
				if !out.Canceled || out.Tasks.Pushed != 0 {
					t.Fatalf("ambiguous create reported confirmation: %+v", out)
				}
				counts, err := st.OutboxCounts(context.Background())
				if err != nil || counts.Failed != 1 {
					t.Fatalf("uncertain create not held: %+v %v", counts, err)
				}
				again := RunSync(context.Background(), st, config.Default(), client, nil)
				_ = again
				mu.Lock()
				creates := 0
				for _, path := range paths {
					if path == "POST /open/v1/task" {
						creates++
					}
				}
				mu.Unlock()
				if creates != 1 {
					t.Fatalf("uncertain create retried %d times", creates)
				}
				return
			}
			if out.Tasks.Pushed != 1 {
				t.Fatalf("lost confirmed push: %+v", out)
			}
			state, err := a.State(context.Background(), []string{created.Task.Id})
			if err != nil || state.Replacements[created.Task.Id] != "remote-task" {
				t.Fatalf("promotion state: %+v %v", state, err)
			}
			if mode == "pull failure" || mode == "cancel pull" {
				if out.Err == nil || out.FocusAttempted || (mode == "cancel pull" && !out.Canceled) {
					t.Fatalf("partial result: %+v", out)
				}
				mu.Lock()
				for _, path := range paths {
					if strings.Contains(path, "focus") {
						t.Error("focus ran after failed/canceled task pass")
					}
				}
				mu.Unlock()
				return
			}
			if out.Err != nil || out.Canceled || out.Tasks.ErrorCount() != 0 {
				t.Fatalf("sync: %+v", out)
			}
			if mode == "success" || mode == "expired Browser" {
				if out.RefreshFailed() != (mode == "expired Browser") {
					t.Fatalf("catalog result %+v", out.Refreshes)
				}
				if !out.FocusAttempted || out.Focus.Uploaded != 1 || len(out.Focus.Errors) > 0 {
					t.Fatalf("focus: %+v", out.Focus)
				}
				want := []string{"Refreshing Timer topics", "Sending task queue", "Reading remote tasks", "Refreshing resource catalogs", "Uploading optional focus history"}
				if !reflect.DeepEqual(phases, want) {
					t.Fatalf("phases %v", phases)
				}
				again := RunSync(context.Background(), st, cfg, client, nil)
				if again.Err != nil || again.FocusAttempted {
					t.Fatalf("focus repeated: %+v", again)
				}
			} else if out.FocusAttempted {
				t.Fatal("disabled focus ran")
			}
		})
	}
}

func TestExplicitSyncNoCredentialsOrPreCanceledWorkDoesNotMutate(t *testing.T) {
	a, st := actionStore(t)
	ctx := context.Background()
	if _, err := a.Create(ctx, Draft{ProjectID: "work", Title: "Offline"}); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	calls := 0
	client := func() (*api.Client, error) { calls++; return nil, errors.New("no token found") }
	out := RunSync(ctx, st, config.Default(), client, nil)
	if out.Err == nil || out.Canceled || before != dumpCache(t, st) || calls != 1 {
		t.Fatalf("credential failure: %+v", out)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	out = RunSync(canceled, st, config.Default(), client, nil)
	if !out.Canceled || calls != 1 || before != dumpCache(t, st) {
		t.Fatal("canceled sync touched work")
	}
}
