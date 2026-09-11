package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

func resourceFixture(t *testing.T, handler http.HandlerFunc) (*Resources, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewResources(st, func() (*api.Client, error) {
		return api.NewClient("synthetic", "test", api.WithBaseURL(server.URL), api.WithHTTPClient(server.Client())), nil
	}), st
}

func queueResource(t *testing.T, r *Resources, m store.EntityMutation) store.EntityMutationOutcome {
	t.Helper()
	p, err := r.Prepare(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Apply(context.Background(), p.ID, m.Ref.Kind, m.Action)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResourcesCreateConfirmAndRawRoundtrip(t *testing.T) {
	var posts atomic.Int32
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts.Add(1)
			io.WriteString(w, `{"id":"g","name":"Work","future":{"n":9007199254740993}}`)
		} else {
			io.WriteString(w, `[{"id":"g","name":"Work","future":{"n":9007199254740993}}]`)
		}
	})
	out := queueResource(t, r, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Work"}`)})
	if posts.Load() != 0 {
		t.Fatal("preview or apply used network")
	}
	result := r.Sync(context.Background())
	if result.Confirmed != 1 || len(result.Errors) != 0 {
		t.Fatalf("%+v", result)
	}
	entity, err := st.Entity(context.Background(), out.Entity.Ref)
	if err != nil || entity.Dirty || entity.ServerID != "g" || !strings.Contains(string(entity.Data), "9007199254740993") {
		t.Fatalf("%+v %v", entity, err)
	}
	if result := r.Sync(context.Background()); result.Confirmed != 0 || posts.Load() != 1 {
		t.Fatalf("replayed create: %+v", result)
	}
}

func TestResourcesUnknownCreateNeverReplays(t *testing.T) {
	var posts atomic.Int32
	r, _ := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) { posts.Add(1); w.WriteHeader(http.StatusCreated) })
	queueResource(t, r, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Work"}`)})
	if result := r.Sync(context.Background()); result.Failed != 1 {
		t.Fatalf("%+v", result)
	}
	ops, err := r.Queue(context.Background())
	if err != nil || len(ops) != 1 || ops[0].Phase != "uncertain" {
		t.Fatalf("%+v %v", ops, err)
	}
	if err := r.Cancel(context.Background(), ops[0].Item.Seq, ops[0].Revision); !errors.Is(err, store.ErrEntityUncertain) {
		t.Fatalf("unsafe cancel: %v", err)
	}
	if err := r.Recover(context.Background(), ops[0].Item.Seq, ops[0].Revision); err != nil {
		t.Fatal(err)
	}
	r.Sync(context.Background())
	if posts.Load() != 1 {
		t.Fatal("uncertain allocation replayed")
	}
}

func TestResourceArmedReadGuardDoesNotBecomeWriteRejection(t *testing.T) {
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, `[{"id":"g","name":"Original"}]`) })
	ctx := context.Background()
	listing, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true)
	if err != nil {
		t.Fatal(err)
	}
	queueResource(t, r, store.EntityMutation{Ref: listing.Entities[0].Ref, Action: "update", Patch: json.RawMessage(`{"name":"Local"}`)})
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := st.ArmEntityOperation(ctx, claimed[0].Item.Seq, claimed[0].Item.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET state='failed',inflight_at=NULL,lease_token=NULL WHERE seq=?`, claimed[0].Item.Seq); err != nil {
		t.Fatal(err)
	}
	ops, err := r.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(ctx, ops[0].Item.Seq, ops[0].Revision); err != nil {
		t.Fatal(err)
	}
	r.client = func() (*api.Client, error) {
		return api.NewClient("synthetic", "test", api.WithRequestGuard(func(context.Context) (func(), error) { return nil, errors.New("logged out") })), nil
	}
	if result := r.Sync(ctx); result.Failed != 1 {
		t.Fatalf("guard result: %+v", result)
	}
	ops, err = r.Queue(ctx)
	if err != nil || len(ops) != 1 || ops[0].Phase != "uncertain" {
		t.Fatalf("read refusal incorrectly proved rejected write: %+v %v", ops, err)
	}
}

func TestRemoteResourceListingIncludesPendingLocalOverlay(t *testing.T) {
	r, _ := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, `[{"id":"g","name":"Remote"}]`) })
	queueResource(t, r, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Local"}`)})
	listing, err := r.List(context.Background(), ResourceQuery{Kind: "folder"}, true)
	if err != nil || len(listing.Entities) != 2 || listing.Meta.Pending != 1 {
		t.Fatalf("pending overlay hidden by remote list: %+v %v", listing, err)
	}
}

func TestResourceReadRejectsCrossParentIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		query    ResourceQuery
		response string
	}{
		{"column", ResourceQuery{Kind: "column", ProjectID: "p"}, `[{"id":"column","projectId":"other","name":"Column"}]`},
		{"checkin", ResourceQuery{Kind: "checkin", HabitID: "h", From: "20260911", To: "20260911"}, `[{"habitId":"other","checkins":[{"stamp":20260911,"value":1}]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, test.response) })
			if _, err := r.List(context.Background(), test.query, true); err == nil {
				t.Fatal("cross-parent response accepted")
			}
			rows, err := st.Entities(context.Background(), test.query.Kind, "", true)
			if err != nil || len(rows) != 0 {
				t.Fatalf("invalid response entered cache: %+v %v", rows, err)
			}
		})
	}
}

func TestResourcesStalePreviewAndRemoteConflict(t *testing.T) {
	var changed atomic.Bool
	var posts atomic.Int32
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts.Add(1)
		}
		if changed.Load() {
			io.WriteString(w, `[{"id":"g","name":"Server"}]`)
		} else {
			io.WriteString(w, `[{"id":"g","name":"Original"}]`)
		}
	})
	ctx := context.Background()
	listing, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true)
	if err != nil {
		t.Fatal(err)
	}
	ref := listing.Entities[0].Ref
	p, err := r.Prepare(ctx, store.EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"Local"}`)})
	if err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	if _, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(ctx, p.ID, "folder", "update"); !errors.Is(err, store.ErrEntityChanged) {
		t.Fatalf("stale preview applied: %v", err)
	}
	changed.Store(false)
	if _, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true); err != nil {
		t.Fatal(err)
	}
	queueResource(t, r, store.EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"Local"}`)})
	changed.Store(true)
	if result := r.Sync(ctx); result.Failed != 1 || posts.Load() != 0 {
		t.Fatalf("conflict overwritten: %+v", result)
	}
	entity, _ := st.Entity(ctx, ref)
	if !strings.Contains(string(entity.Data), "Local") || !strings.Contains(string(entity.Base), "Server") {
		t.Fatalf("overlay lost: %+v", entity)
	}
}

func TestResourcePreflightFenceRetainsNewerConcurrentRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var gets, posts atomic.Int32
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			posts.Add(1)
			io.WriteString(w, `{"id":"g","name":"Local"}`)
			return
		}
		switch gets.Add(1) {
		case 1:
			io.WriteString(w, `[{"id":"g","name":"Original"}]`)
		case 2:
			close(entered)
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
			io.WriteString(w, `[{"id":"g","name":"Original"}]`)
		default:
			io.WriteString(w, `[{"id":"g","name":"Newer remote"}]`)
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	listing, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true)
	if err != nil {
		t.Fatal(err)
	}
	ref := listing.Entities[0].Ref
	queueResource(t, r, store.EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"Local"}`)})
	result := make(chan ResourceSyncResult, 1)
	go func() { result <- r.Sync(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("preflight did not start")
	}
	if _, err := r.List(ctx, ResourceQuery{Kind: "folder"}, true); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case outcome := <-result:
		if outcome.Failed != 1 || posts.Load() != 0 {
			t.Fatalf("stale preflight wrote over newer server snapshot: %+v posts=%d", outcome, posts.Load())
		}
	case <-ctx.Done():
		t.Fatal("preflight did not finish")
	}
	entity, err := st.Entity(ctx, ref)
	if err != nil || !strings.Contains(string(entity.Base), "Newer remote") || !strings.Contains(string(entity.Data), "Local") {
		t.Fatalf("newer snapshot or local overlay lost: %+v %v", entity, err)
	}
}

func TestCheckinNaturalDateRecovery(t *testing.T) {
	var posted atomic.Bool
	var posts atomic.Int32
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts.Add(1)
			posted.Store(true)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if posted.Load() {
			io.WriteString(w, `[{"habitId":"h","checkins":[{"id":"server-day","stamp":20260911,"value":1.0,"future":true}]}]`)
		} else {
			io.WriteString(w, `[{"habitId":"h","checkins":[]}]`)
		}
	})
	ctx := context.Background()
	mutation := store.EntityMutation{Ref: store.EntityRef{Kind: "checkin"}, ProjectKey: "h", Action: "create", Patch: json.RawMessage(`{"stamp":20260911,"value":1}`)}
	if _, err := r.Prepare(ctx, mutation); err == nil {
		t.Fatal("invented empty baseline")
	}
	q := ResourceQuery{Kind: "checkin", HabitID: "h", From: "20260911", To: "20260911"}
	if _, err := r.List(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	out := queueResource(t, r, mutation)
	if result := r.Sync(ctx); result.Failed != 1 {
		t.Fatalf("%+v", result)
	}
	ops, _ := r.Queue(ctx)
	if len(ops) != 1 || ops[0].RemoteID != "h/20260911" {
		t.Fatalf("date identity lost: %+v", ops)
	}
	if err := r.Recover(ctx, ops[0].Item.Seq, ops[0].Revision); err != nil {
		t.Fatal(err)
	}
	if result := r.Sync(ctx); result.Confirmed != 1 || posts.Load() != 1 {
		t.Fatalf("%+v posts=%d", result, posts.Load())
	}
	entity, err := st.Entity(ctx, out.Entity.Ref)
	if err != nil || entity.Dirty || !strings.Contains(string(entity.Data), "future") {
		t.Fatalf("%+v %v", entity, err)
	}
}

func TestFocusManualImportPreservesTimerAndAcceptanceIdentity(t *testing.T) {
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) { t.Error("unexpected network") })
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	active, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := r.PrepareFocusRecord(ctx, store.FocusRecord{FocusType: 1, Start: now.Add(-time.Hour), End: now.Add(-30 * time.Minute), PauseSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.ApplyFocusRecord(ctx, preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.ApplyFocusRecord(ctx, preview.ID)
	if err != nil || first.ID != second.ID {
		t.Fatalf("duplicated session: %+v %v", second, err)
	}
	zone := time.FixedZone("offset", 3*60*60)
	other, err := r.PrepareFocusRecord(ctx, store.FocusRecord{FocusType: 1, Start: now.Add(-time.Hour).In(zone), End: now.Add(-30 * time.Minute).In(zone), PauseSeconds: 30})
	if err != nil || other.ID != preview.ID {
		t.Fatalf("same interval changed acceptance identity by timezone: %+v %v", other, err)
	}
	state, err := st.ReadTimer(ctx, now)
	if err != nil || state.State == nil || state.State.SessionID != active.State.SessionID {
		t.Fatal("manual record replaced active timer")
	}
	ids, err := st.PendingFocusSessionIDs(ctx, false)
	if err != nil || len(ids) != 1 || ids[0] != first.ID {
		t.Fatalf("not in existing uploader: %v %v", ids, err)
	}
}

func TestFocusDeletionRecoversByExact404WithoutReplay(t *testing.T) {
	var deleted atomic.Bool
	var deletes atomic.Int32
	r, st := resourceFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "DELETE" {
			deletes.Add(1)
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if deleted.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"id":"f","type":1,"startTime":"2026-09-11T10:00:00Z","endTime":"2026-09-11T10:10:00Z","duration":600,"pauseDuration":0}`)
	})
	ctx := context.Background()
	listing, err := r.List(ctx, ResourceQuery{Kind: "focus", FocusType: 1, ID: "f"}, true)
	if err != nil {
		t.Fatal(err)
	}
	entity := listing.Entities[0]
	queueResource(t, r, store.EntityMutation{Ref: entity.Ref, ProjectKey: entity.ProjectKey, Action: "delete", Patch: json.RawMessage(`{}`)})
	if result := r.Sync(ctx); result.Failed != 1 {
		t.Fatalf("%+v", result)
	}
	ops, _ := r.Queue(ctx)
	if len(ops) != 1 {
		t.Fatal("lost deletion")
	}
	if err := r.Recover(ctx, ops[0].Item.Seq, ops[0].Revision); err != nil {
		t.Fatal(err)
	}
	if result := r.Sync(ctx); result.Confirmed != 1 || deletes.Load() != 1 {
		t.Fatalf("%+v", result)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM focus_remote_deletions WHERE remote_id='f' AND focus_type=1`).Scan(&n); err != nil || n != 1 {
		t.Fatal("deletion suppression not retained")
	}
}
