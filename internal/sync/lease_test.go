package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func openSharedStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store %s: %v", path, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPushMakesNoDuplicatesWhenASecondWorkerJoinsMidPass(t *testing.T) {
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "cache.db")
	stA := openSharedStore(t, dbPath)
	stB := openSharedStore(t, dbPath)

	seedProject(t, stA, model.Project{Id: "p1", Name: "Inbox"})

	const queued = 24
	titles := make([]string, 0, queued)
	for i := range queued {
		title := fmt.Sprintf("task-%02d", i)
		if _, err := stA.CreateTask(ctx, openTask("", "p1", title)); err != nil {
			t.Fatalf("queue create %s: %v", title, err)
		}
		titles = append(titles, title)
	}

	const hold = 150 * time.Millisecond

	var mu sync.Mutex
	seen := map[string]int{}
	nextID := 0
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body api.Task
		if err := json.Unmarshal(readBody(t, r), &body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		time.Sleep(hold)
		mu.Lock()
		seen[body.Title]++
		nextID++
		id := fmt.Sprintf("srv%d", nextID)
		mu.Unlock()

		writeJSON(t, w, api.Task{ID: id, ProjectID: "p1", Title: body.Title})
	})

	newSyncer := func(st *store.Store) *Syncer {
		client := api.NewClient("test-token", "0.0.0-test",
			api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(500*time.Millisecond))
		s := New(st, client, Options{})
		if s.lease != time.Second {
			t.Fatalf("lease = %v, want 1s", s.lease)
		}
		return s
	}
	workerA := newSyncer(stA)
	workerB := newSyncer(stB)

	var resB Result
	var errB error
	done := make(chan struct{})
	go func() {
		defer close(done)

		time.Sleep(2100 * time.Millisecond)
		resB, errB = workerB.Push(ctx)
	}()

	resA, errA := workerA.Push(ctx)
	<-done

	if errA != nil {
		t.Logf("worker A: %v", errA)
	}
	if errB != nil {
		t.Logf("worker B: %v", errB)
	}
	t.Logf("worker A: pushed=%d requeued=%d failed=%d errors=%d",
		resA.Pushed, resA.Requeued, resA.Failed, len(resA.Errors))
	t.Logf("worker B: pushed=%d requeued=%d failed=%d errors=%d",
		resB.Pushed, resB.Requeued, resB.Failed, len(resB.Errors))

	mu.Lock()
	defer mu.Unlock()

	var dupes []string
	total := 0
	for _, title := range titles {
		total += seen[title]
		if seen[title] > 1 {
			dupes = append(dupes, fmt.Sprintf("%s x%d", title, seen[title]))
		}
	}
	t.Logf("creates the server answered: %d for %d queued tasks", total, queued)
	if len(dupes) == 0 && resB.Pushed == 0 && len(seen) == queued {

		t.Skip("worker B sent nothing; the two passes did not overlap")
	}
	if len(dupes) > 0 {
		t.Errorf("the server was asked to create %d task(s) twice: %v - "+
			"a worker went on sending entries whose lease had run out and which the other had claimed",
			len(dupes), dupes)
	}
}

func TestPushDoesNotResendACreateTheProcessDiedOn(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	dying := openSharedStore(t, dbPath)
	next := openSharedStore(t, dbPath)

	seedProject(t, dying, model.Project{Id: "p1", Name: "Spisok"})
	if _, err := dying.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	arrived := make(chan struct{})
	release := make(chan struct{})
	var posts atomic.Int64
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if posts.Add(1) == 1 {
			close(arrived)
			<-release
		}
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1", Title: "Zabrat posylku"})
	})

	client := api.NewClient("test-token", "0.0.0-test",
		api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(30*time.Second))
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_, _ = New(dying, client, Options{}).Push(ctx)
	}()
	<-arrived

	if _, err := next.DB().ExecContext(ctx,
		`UPDATE outbox SET inflight_at = inflight_at - 86400`); err != nil {
		t.Fatalf("age the lease: %v", err)
	}

	res, err := New(next, client, Options{}).Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("%d create(s) went out, want the one the dying pass sent - an ambiguous create is "+
			"parked, not repeated (result %+v)", n, res)
	}
	if c := outboxCounts(t, next); c.Failed != 1 {
		t.Errorf("outbox %+v, want the create parked", c)
	}

	if res.Failed != 1 {
		t.Errorf("result = %+v, want the parked create counted by the pass that parked it", res)
	}

	if res.ParkedUnsent != 1 {
		t.Errorf("result = %+v, want the create the claim parked counted apart from the ones a send parked", res)
	}

	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none: no request was made about the entry the claim parked", res.Errors)
	}

	close(release)
	<-dead
}

func TestPushSendsNothingUnderALeaseAnotherClaimTook(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	cachedTasks(t, st, "p1", openTask("t1", "p1", "Zabrat posylku"))
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatalf("edit task: %v", err)
	}

	items, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %+v, want the edit", items)
	}
	if _, _, err := st.Claim(ctx, 10, 0); err != nil {
		t.Fatalf("take the lease over: %v", err)
	}

	var calls int
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(t, w, api.Task{ID: "t1", ProjectID: "p1"})
	})

	var res Result
	swap, err := testSyncer(t, st, server).pushOne(ctx, items[0], &res, requeued{})
	if err != nil {
		t.Fatalf("pushOne: %v", err)
	}
	if swap != (idSwap{}) {
		t.Errorf("swap = %+v, want nothing: no request was made", swap)
	}
	if calls != 0 {
		t.Errorf("%d request(s) went out, want none: the entry is another claim's", calls)
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0], store.ErrLeaseLost) {
		t.Errorf("errors = %v, want the lost lease reported", res.Errors)
	}
	if res.Pushed != 0 || res.Requeued != 0 || res.Failed != 0 {
		t.Errorf("result = %+v, want nothing written to an entry another claim holds", res)
	}
}

func TestPushDoesNotParkACreateOverTheStampOfARefusedRequest(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "Spisok"})
	if _, err := st.CreateTask(ctx, openTask("", "p1", "Zabrat posylku")); err != nil {
		t.Fatalf("create task: %v", err)
	}

	var (
		posts   atomic.Int64
		expired atomic.Bool
	)
	expired.Store(true)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/open/v1/task" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts.Add(1)
		if expired.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errorCode":"invalid_token"}`))
			return
		}
		writeJSON(t, w, api.Task{ID: "srv1", ProjectID: "p1", Title: "Zabrat posylku"})
	})

	syncer := testSyncer(t, st, server)
	if _, err := syncer.Push(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("Push under an expired token = %v, want the pass to end on the refusal", err)
	}
	if c := outboxCounts(t, st); c.Pending != 1 {
		t.Fatalf("outbox %+v, want the create back in line: the server would not look at it", c)
	}

	expired.Store(false)
	if _, _, err := st.Claim(ctx, 1, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE outbox SET inflight_at = inflight_at - 86400`); err != nil {
		t.Fatalf("age the lease: %v", err)
	}

	res, err := syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Failed != 0 || res.ParkedUnsent != 0 {
		t.Errorf("result = %+v, want nothing parked: no request has gone out since the one the server "+
			"refused to look at", res)
	}
	if n := posts.Load(); n != 2 {
		t.Errorf("%d create(s) went out, want the refused one and the one the pass made after the login", n)
	}
	if res.Pushed != 1 {
		t.Errorf("result = %+v, want the create sent", res)
	}
	if c := outboxCounts(t, st); c != (store.OutboxCounts{}) {
		t.Errorf("outbox %+v, want the create gone from the queue", c)
	}
}
