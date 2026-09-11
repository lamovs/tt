package sync

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestStage1CFirstInboxAliasRetainsPrefetchedEpoch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := serve(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open/v1/project":
			writeJSON(t, w, []any{})
		case inboxPath:
			wait := false
			once.Do(func() {
				close(arrived)
				wait = true
			})
			if wait {
				<-release
			}
			w.Write([]byte(`{"tasks":[{"id":"inbox-task","projectId":"inbox42","title":"Inbox","status":0}],"columns":[]}`))
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	})
	type pullResult struct {
		result Result
		err    error
	}
	done := make(chan pullResult, 1)
	go func() {
		result, err := testSyncer(t, st, server).Pull(ctx)
		done <- pullResult{result: result, err: err}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first inbox request did not reach the controlled response")
	}
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "local-project", Name: "Local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{
		ProjectId: "local-project", Title: "concurrent registration",
		RepeatFlag: "RRULE:FREQ=DAILY;INTERVAL=1",
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	first := <-done
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.result.Pulled != 0 || first.result.Skipped != 1 {
		t.Fatalf("first result=%+v, want stale inbox response skipped as one unit", first.result)
	}
	if _, err := st.Task(ctx, "inbox-task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale first inbox response was applied: %v", err)
	}
	second, err := testSyncer(t, st, server).Pull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Pulled != 1 || second.Skipped != 0 {
		t.Fatalf("fresh inbox result=%+v, want one imported task", second)
	}
	got, err := st.Task(ctx, "inbox-task")
	if err != nil || got.ProjectId != "inbox42" {
		t.Fatalf("fresh inbox task=%+v err=%v", got, err)
	}
}
