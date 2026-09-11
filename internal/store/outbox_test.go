package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func enqueueN(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := s.Enqueue(ctx, OutboxEntry{
			Target:  TargetOpenAPI,
			Op:      "task.update",
			TaskID:  fmt.Sprintf("t%d", i+1),
			Payload: []byte(`{"title":"x"}`),
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

func TestOutboxRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	seq, err := s.Enqueue(ctx, OutboxEntry{
		Target:    TargetV2,
		Op:        "focus.push",
		ProjectID: "p1",
		Payload:   []byte(`{"id":"f1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d, want 1", len(items))
	}
	got := items[0]

	if got.Seq != seq || got.Target != TargetV2 || got.Op != "focus.push" ||
		got.ProjectID != "p1" || got.TaskID != "" || string(got.Payload) != `{"id":"f1"}` ||
		got.State != OutboxInflight || got.Attempts != 1 {
		t.Fatalf("claimed %+v", got)
	}
	if got.CreatedAt.IsZero() || got.InflightAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", got)
	}
	if got.LeaseToken == "" {
		t.Fatal("claimed without a lease token")
	}

	again, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("claimed a leased row: %+v", again)
	}

	if err := s.MarkDone(ctx, seq, got.LeaseToken); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{}) {
		t.Fatalf("counts %+v, want empty", counts)
	}
	if err := s.MarkDone(ctx, seq, got.LeaseToken); !errors.Is(err, ErrNoOutboxRow) {
		t.Fatalf("second done: %v, want ErrNoOutboxRow", err)
	}
}

func TestOutboxFailAndRequeue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	seq, token := items[0].Seq, items[0].LeaseToken

	if err := s.MarkFailed(ctx, seq, token, "401 unauthorized"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || counts.Pending != 0 || counts.Inflight != 0 {
		t.Fatalf("counts %+v", counts)
	}

	if got, _, err := s.Claim(ctx, 10, 0); err != nil || len(got) != 0 {
		t.Fatalf("claim after fail: %v %d", err, len(got))
	}

	if err := s.Requeue(ctx, seq, token, "timeout"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("requeue of a parked row: %v, want ErrLeaseLost", err)
	}

	n, err := s.RetryFailed(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry failed: %v %d", err, n)
	}
	items, _, err = s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim after retry: %v %d", err, len(items))
	}
	if items[0].Attempts != 2 || items[0].LastError != "401 unauthorized" {
		t.Fatalf("attempts %d, last_error %q", items[0].Attempts, items[0].LastError)
	}

	if err := s.Requeue(ctx, items[0].Seq, items[0].LeaseToken, "timeout"); err != nil {
		t.Fatal(err)
	}
	if counts, err = s.OutboxCounts(ctx); err != nil || counts.Pending != 1 {
		t.Fatalf("counts after requeue: %v %+v", err, counts)
	}
	items, _, err = s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim after requeue: %v %d", err, len(items))
	}
	if items[0].Attempts != 3 || items[0].LastError != "timeout" {
		t.Fatalf("attempts %d, last_error %q", items[0].Attempts, items[0].LastError)
	}
}

func TestOutboxCountsTheAttemptAtClaim(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	for want := 1; want <= 3; want++ {

		items, _, err := s.Claim(ctx, 1, 0)
		if err != nil || len(items) != 1 {
			t.Fatalf("claim %d: %v %d", want, err, len(items))
		}
		if items[0].Attempts != want {
			t.Fatalf("attempt %d counted as %d", want, items[0].Attempts)
		}
	}
	var attempts int
	if err := s.DB().QueryRowContext(ctx, `SELECT attempts FROM outbox`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("stored attempts %d, want 3", attempts)
	}
}

func TestOutboxFencesTheLostLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v %d", err, len(first))
	}
	second, _, err := s.Claim(ctx, 1, 0)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: %v %d", err, len(second))
	}
	seq := first[0].Seq
	if second[0].Seq != seq {
		t.Fatalf("second claim took seq %d, want %d", second[0].Seq, seq)
	}
	if second[0].LeaseToken == first[0].LeaseToken {
		t.Fatal("the two claims share a lease token")
	}

	stale := first[0].LeaseToken
	if err := s.MarkDone(ctx, seq, stale); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("done with a stale token: %v, want ErrLeaseLost", err)
	}
	if err := s.MarkFailed(ctx, seq, stale, "boom"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("fail with a stale token: %v, want ErrLeaseLost", err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Inflight != 1 {
		t.Fatalf("counts %+v, want the row still with its second holder", counts)
	}

	if err := s.MarkDone(ctx, seq, second[0].LeaseToken); err != nil {
		t.Fatalf("done with the current token: %v", err)
	}
	if counts, err = s.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("counts %+v (%v)", counts, err)
	}
}

func TestReclaimStaleDropsTheLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)
	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	if _, _, err := s.ReclaimStale(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, items[0].Seq, items[0].LeaseToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("done after the lease was reclaimed: %v, want ErrLeaseLost", err)
	}
}

func TestReclaimStaleKeepsALeaseTakenInTheSameSecond(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	if frac := time.Duration(time.Now().Nanosecond()); frac > 900*time.Millisecond {
		time.Sleep(time.Second - frac)
	}
	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	n, _, err := s.ReclaimStale(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d rows, want the lease just handed out left alone", n)
	}

	if err := s.MarkDone(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatalf("done under a lease that should still be held: %v", err)
	}
}

func TestOutboxFinishersJoinATransaction(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)
	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	it := items[0]

	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Inflight != 1 {
		t.Fatalf("counts %+v, want the entry back after the rollback", counts)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		return MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken)
	}); err != nil {
		t.Fatal(err)
	}
	if counts, err = s.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("counts %+v (%v)", counts, err)
	}

	enqueueN(t, s, 2)
	items, _, err = s.Claim(ctx, 2, time.Minute)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if err := markFailedTx(ctx, tx, items[0].Seq, items[0].LeaseToken, "401"); err != nil {
			return err
		}
		return requeueTx(ctx, tx, items[1].Seq, items[1].LeaseToken, "timeout")
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts, err = s.OutboxCounts(ctx); err != nil || counts.Failed != 1 || counts.Pending != 1 {
		t.Fatalf("counts %+v (%v)", counts, err)
	}
}

func TestOutboxReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v %d", err, len(first))
	}
	second, _, err := s.Claim(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Seq != first[0].Seq {
		t.Fatalf("expired lease not reclaimed: %+v", second)
	}

	n, _, err := s.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 || counts.Inflight != 0 {
		t.Fatalf("counts %+v", counts)
	}
}

func TestOutboxConcurrentClaimNeverDoubles(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	const total = 40
	enqueueN(t, s, total)

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				items, _, err := s.Claim(ctx, 3, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(items) == 0 {
					return
				}
				mu.Lock()
				for _, it := range items {
					seen[it.Seq]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("claimed %d distinct rows, want %d", len(seen), total)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("seq %d claimed %d times", seq, n)
		}
	}
}

func TestClaimStrandsNothingWhenCancelled(t *testing.T) {
	s := testStore(t)
	enqueueN(t, s, 50)
	cancelled := 0
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func(d time.Duration) {
			time.Sleep(d)
			cancel()
		}(time.Duration(i%100) * time.Microsecond)
		items, _, err := s.Claim(ctx, 5, time.Minute)
		cancel()
		if err == nil {

			for _, it := range items {
				if uerr := s.Unclaim(context.Background(), it.Seq, it.LeaseToken, ""); uerr != nil {
					t.Fatalf("unclaim: %v", uerr)
				}
			}
			continue
		}
		cancelled++
		if len(items) != 0 {
			t.Fatalf("round %d: claim failed with %v and handed back %d entries anyway", i, err, len(items))
		}
		var inflight int
		if qerr := s.DB().QueryRowContext(context.Background(),
			`SELECT count(*) FROM outbox WHERE state = ?`, string(OutboxInflight)).Scan(&inflight); qerr != nil {
			t.Fatal(qerr)
		}
		if inflight > 0 {
			t.Fatalf("round %d: %v left %d entries in flight under a token nobody holds", i, err, inflight)
		}
	}
	if cancelled == 0 {
		t.Skip("no claim was cancelled mid-statement; the window this pins was never entered")
	}
}

func TestEnqueueRejectsBadEntry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: "web", Op: "task.create"}); err == nil {
		t.Error("unknown target accepted")
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI}); err == nil {
		t.Error("empty op accepted")
	}
}

func TestClaimKeepsALeaseTakenInTheSameSecond(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	if frac := time.Duration(time.Now().Nanosecond()); frac > 900*time.Millisecond {
		time.Sleep(time.Second - frac)
	}
	items, _, err := s.Claim(ctx, 1, 500*time.Millisecond)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}
	again, _, err := s.Claim(ctx, 1, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("claimed %+v, want the lease just handed out left alone", again)
	}

	if err := s.MarkDone(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatalf("done under a lease that should still be held: %v", err)
	}
}

func TestLeaseIsRoundedUpToWholeSeconds(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	if frac := time.Duration(time.Now().Nanosecond()); frac > 900*time.Millisecond {
		time.Sleep(time.Second - frac)
	}
	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %d", err, len(items))
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE outbox SET inflight_at = inflight_at - 1`); err != nil {
		t.Fatal(err)
	}

	again, _, err := s.Claim(ctx, 1, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("claimed %+v, want a lease of 1.5s to still cover an entry a second old", again)
	}
	if n, _, err := s.ReclaimStale(ctx, 1500*time.Millisecond); err != nil || n != 0 {
		t.Fatalf("reclaimed %d rows (%v), want ReclaimStale to round the lease the same way", n, err)
	}
	if err := s.MarkDone(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatalf("done under a lease that should still be held: %v", err)
	}
}

func TestStaleWorkerCannotClearAMarkItNoLongerOwns(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 || first[0].Rev != 1 {
		t.Fatalf("claim: %v %+v", err, first)
	}

	second, _, err := s.Claim(ctx, 1, 0)
	if err != nil || len(second) != 1 || second[0].Seq != first[0].Seq {
		t.Fatalf("second claim: %v %+v", err, second)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, second[0].Seq, second[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "t1", second[0].Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the dirty mark was not cleared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку до пятницы")}); err != nil {
		t.Fatal(err)
	}

	var fenced error
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		fenced = MarkDoneTx(ctx, tx, first[0].Seq, first[0].LeaseToken)
		if fenced != nil {
			return nil
		}
		_, err := MarkPushedTx(ctx, tx, "t1", first[0].Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(fenced, ErrNoOutboxRow) {
		t.Fatalf("stale worker got %v, want ErrNoOutboxRow", fenced)
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 't1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 1 {
		t.Fatalf("dirty %d, want the mark of the edit nobody has pushed", dirty)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку на почте")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result %+v, want the unpushed edit kept", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку до пятницы" {
		t.Fatalf("the pull overwrote an unpushed edit: %q", got.Title)
	}
}

func TestAParkUnderALostLeaseReleasesNothing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("parcel at the post office")}); err != nil {
		t.Fatal(err)
	}
	stale := claimAll(t, s)
	if len(stale) != 1 || stale[0].Rev != 1 {
		t.Fatalf("claimed %+v", stale)
	}

	if _, _, err := s.ReclaimStale(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fresh := claimAll(t, s)
	if len(fresh) != 1 || fresh[0].LeaseToken == stale[0].LeaseToken {
		t.Fatalf("reclaimed %+v, want a claim of its own", fresh)
	}

	err := s.MarkFailed(ctx, stale[0].Seq, stale[0].LeaseToken, "422 unprocessable")
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the stale worker's park = %v, want %v", err, ErrLeaseLost)
	}
	if c := mustCounts(t, s); c != (OutboxCounts{Inflight: 1}) {
		t.Errorf("queue %+v, want the entry still in flight under the worker that owns it", c)
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = ?`, "t1").Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != stale[0].Rev {
		t.Fatalf("dirty %d, want the mark of the edit that is still on its way out", dirty)
	}
	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel on Lenina")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result %+v, want the unpushed edit kept", res)
	}
}

func mustCounts(t *testing.T, s *Store) OutboxCounts {
	t.Helper()
	c, err := s.OutboxCounts(context.Background())
	if err != nil {
		t.Fatalf("outbox counts: %v", err)
	}
	return c
}

func TestHasQueuedCreate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if queued, err := s.HasQueuedCreate(ctx, "t1"); err != nil || queued {
		t.Fatalf("HasQueuedCreate on an empty queue = %v, %v", queued, err)
	}
	seq, err := s.Enqueue(ctx, OutboxEntry{
		Target: TargetOpenAPI, Op: OpTaskCreate, TaskID: "t1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if queued, err := s.HasQueuedCreate(ctx, "t1"); err != nil || !queued {
		t.Fatalf("HasQueuedCreate on a pending create = %v, %v", queued, err)
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %+v", err, items)
	}
	if queued, err := s.HasQueuedCreate(ctx, "t1"); err != nil || !queued {
		t.Fatalf("HasQueuedCreate on an inflight create = %v, %v", queued, err)
	}

	if err := s.MarkFailed(ctx, seq, items[0].LeaseToken, "rejected"); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.HasQueuedCreate(ctx, "t1"); err != nil || queued {
		t.Fatalf("HasQueuedCreate on a parked create = %v, %v", queued, err)
	}
	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.HasQueuedCreate(ctx, "t1"); err != nil || !queued {
		t.Fatalf("HasQueuedCreate after RetryFailed = %v, %v", queued, err)
	}

	if queued, err := s.HasQueuedCreate(ctx, "t2"); err != nil || queued {
		t.Fatalf("HasQueuedCreate for another task = %v, %v", queued, err)
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{
		Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: "t3", ProjectID: "p1",
	}); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.HasQueuedCreate(ctx, "t3"); err != nil || queued {
		t.Fatalf("HasQueuedCreate for a task with only an update queued = %v, %v", queued, err)
	}
}

func TestUnclaimGivesBackTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("claim: %v %+v", err, items)
	}
	if err := s.Unclaim(ctx, items[0].Seq, items[0].LeaseToken, "never sent"); err != nil {
		t.Fatalf("Unclaim: %v", err)
	}
	again, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(again) != 1 {
		t.Fatalf("claim again: %v %+v", err, again)
	}
	if again[0].Attempts != 1 {
		t.Errorf("attempts = %d after a claim, an unclaim and a claim; want 1", again[0].Attempts)
	}
	if again[0].State != OutboxInflight || again[0].LastError != "never sent" {
		t.Errorf("claimed %+v, want the entry back in flight with the reason it was put back", again[0])
	}

	if err := s.Requeue(ctx, again[0].Seq, again[0].LeaseToken, "sent and failed"); err != nil {
		t.Fatal(err)
	}
	third, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(third) != 1 {
		t.Fatalf("claim a third time: %v %+v", err, third)
	}
	if third[0].Attempts != 2 {
		t.Errorf("attempts = %d after a requeue; want the send to have been counted", third[0].Attempts)
	}
}

func TestUnclaimNeedsTheLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueN(t, s, 1)

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %+v", err, items)
	}

	stolen, _, err := s.Claim(ctx, 10, 0)
	if err != nil || len(stolen) != 1 {
		t.Fatalf("take over: %v %+v", err, stolen)
	}
	if err := s.Unclaim(ctx, items[0].Seq, items[0].LeaseToken, "never sent"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Unclaim with a lost lease = %v, want %v", err, ErrLeaseLost)
	}
	if err := s.Unclaim(ctx, items[0].Seq, stolen[0].LeaseToken, "never sent"); err != nil {
		t.Errorf("Unclaim by the holder: %v", err)
	}
	if err := s.Unclaim(ctx, 999, "whatever", ""); !errors.Is(err, ErrNoOutboxRow) {
		t.Errorf("Unclaim of a row that is gone = %v, want %v", err, ErrNoOutboxRow)
	}
}

func TestFailedCreates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if n, err := s.FailedCreates(ctx); err != nil || n != 0 {
		t.Fatalf("FailedCreates on an empty queue = %d, %v", n, err)
	}
	for _, e := range []OutboxEntry{
		{Target: TargetOpenAPI, Op: OpTaskCreate, TaskID: LocalIDPrefix + "t1", ProjectID: "p1"},
		{Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: "t2", ProjectID: "p1"},
	} {
		if _, err := s.Enqueue(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim: %v %+v", err, items)
	}

	if n, err := s.FailedCreates(ctx); err != nil || n != 0 {
		t.Fatalf("FailedCreates with nothing parked = %d, %v", n, err)
	}

	for _, item := range items {
		if err := s.MarkFailed(ctx, item.Seq, item.LeaseToken, "rejected"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.FailedCreates(ctx); err != nil || n != 1 {
		t.Fatalf("FailedCreates with a parked create and a parked update = %d, %v; want the create alone", n, err)
	}

	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FailedCreates(ctx); err != nil || n != 0 {
		t.Fatalf("FailedCreates after the parked entries were raised = %d, %v", n, err)
	}
}

func TestFailedCreatesLeavesOutTheCreateTheServerAlreadyTook(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	for _, e := range []OutboxEntry{
		{Target: TargetOpenAPI, Op: OpTaskCreate, TaskID: LocalIDPrefix + "t1", ProjectID: "p1"},
		{Target: TargetOpenAPI, Op: OpTaskCreate, TaskID: "srv1", ProjectID: "p1"},
	} {
		if _, err := s.Enqueue(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range claimAll(t, s) {
		if err := s.MarkFailed(ctx, item.Seq, item.LeaseToken, "parked"); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := s.FailedCreates(ctx); err != nil || n != 1 {
		t.Errorf("FailedCreates with two parked creates, one of them the server's = %d, %v; "+
			"want the one still waiting for an id", n, err)
	}
}

func TestDropParkedSaysWhetherTheEntryWentOut(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	for _, title := range []string{"parcel", "flowers"} {
		if _, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	items := claimAll(t, s)
	if len(items) != 2 {
		t.Fatalf("claimed %+v, want both creates", items)
	}
	for _, item := range items {
		if err := s.MarkFailed(ctx, item.Seq, item.LeaseToken, "the answer was lost"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE outbox SET attempts = 0 WHERE seq = ?`, items[1].Seq); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %+v, want both parked creates", dropped)
	}
	if !dropped[0].Requested {
		t.Errorf("entry %d has %d attempt(s) on it and came back as one nothing was ever asked about; "+
			"the record then tells the user the server never had their task",
			dropped[0].Seq, dropped[0].Attempts)
	}
	if dropped[1].Requested {
		t.Errorf("entry %d has no attempt on it and came back as one that had gone out; "+
			"the record then leaves a task in doubt that nothing was ever asked about",
			dropped[1].Seq)
	}
}

func TestDropParkedReportsAndReleasesWhatItThrewAway(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(
		openTask("t1", "p1", "parcel"), openTask("t2", "p1", "flowers"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("parcel at the post office")}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Enqueue(ctx, OutboxEntry{
		Target: TargetOpenAPI, Op: OpTaskDelete, TaskID: "gone", ProjectID: "p1",
	}); err != nil {
		t.Fatal(err)
	}
	parked := claimAll(t, s)
	if len(parked) != 2 {
		t.Fatalf("claimed %+v", parked)
	}
	before := time.Now().Add(-time.Second)
	for _, item := range parked {
		if err := s.MarkFailed(ctx, item.Seq, item.LeaseToken, "422 unprocessable"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET dirty = ? WHERE id = ?`, parked[0].Rev, "t1"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE outbox SET failed_at = NULL WHERE seq = ?`, parked[1].Seq); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t2", model.TaskEdit{Title: model.Ptr("flowers on Friday")}); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %+v, want both parked entries", dropped)
	}
	if dropped[0].Seq != parked[0].Seq || dropped[1].Seq != parked[1].Seq {
		t.Errorf("dropped %d and %d, want the parked entries in the order they were made", dropped[0].Seq, dropped[1].Seq)
	}
	edit := dropped[0]
	if edit.Op != OpTaskUpdate || edit.TaskID != "t1" || edit.ProjectID != "p1" {
		t.Errorf("the dropped edit reads %+v", edit)
	}
	if edit.Title != "parcel at the post office" {
		t.Errorf("the dropped edit names the task %q, want the title the cache holds", edit.Title)
	}
	if edit.Attempts != 1 || edit.Reason != "422 unprocessable" {
		t.Errorf("the dropped edit was tried %d time(s) because %q", edit.Attempts, edit.Reason)
	}
	if edit.QueuedAt.Before(before) || edit.ParkedAt.Before(edit.QueuedAt) {
		t.Errorf("queued at %v, parked at %v, want the change first and the park after it", edit.QueuedAt, edit.ParkedAt)
	}
	if dropped[1].Op != OpTaskDelete || dropped[1].TaskID != "gone" || dropped[1].Title != "" {
		t.Errorf("the dropped delete reads %+v, want a mutation with no cached task to name", dropped[1])
	}
	if !dropped[1].ParkedAt.IsZero() {
		t.Errorf("an entry parked before the column existed is dated %v, want nothing invented", dropped[1].ParkedAt)
	}

	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Pending: 1}) {
		t.Fatalf("queue %+v, want the parked entries gone and the unsent edit left", counts)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(
		openTask("t1", "p1", "parcel on Lenina"), openTask("t2", "p1", "flowers")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 || res.Skipped != 1 {
		t.Fatalf("result %+v, want the thawed row taken and the queued one skipped", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "parcel on Lenina" {
		t.Errorf("the row of the dropped mutation is still frozen: %+v", got)
	}
	if got, err = s.Task(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if got.Title != "flowers on Friday" {
		t.Errorf("the pull wrote over an edit that is still queued: %+v", got)
	}

	again, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second pass dropped %+v", again)
	}
}

func TestDropParkedTakesTheOfflineTaskWithTheCreate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 {
		t.Fatalf("claimed %+v", items)
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Op != OpTaskCreate || dropped[0].TaskID != created.Id {
		t.Fatalf("dropped %+v, want the parked create", dropped)
	}
	if !dropped[0].TaskRemoved {
		t.Error("the line about the create does not say the task went with it")
	}
	if dropped[0].Title != "Zabrat posylku" {
		t.Errorf("the dropped create names the task %q: the title was read after the row was gone", dropped[0].Title)
	}
	if _, err := s.Task(ctx, created.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the task is still cached (%v): nothing will ever send it or correct it", err)
	}
	orphans, err := s.OrphanedLocalTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("left behind %+v", orphans)
	}
	if c := mustCounts(t, s); c != (OutboxCounts{}) {
		t.Errorf("queue %+v, want nothing left", c)
	}
}

func TestDropParkedTakesWhatWasQueuedForTheTaskToo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 {
		t.Fatalf("claimed %+v", items)
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, created.Id, model.TaskEdit{Title: model.Ptr("Zabrat posylku na pochte")}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t2", "p1", "flowers"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t2", model.TaskEdit{Title: model.Ptr("flowers on Friday")}); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || !dropped[0].TaskRemoved {
		t.Fatalf("dropped %+v, want the create alone, with the task going with it", dropped)
	}
	if c := mustCounts(t, s); c != (OutboxCounts{Pending: 1}) {
		t.Fatalf("queue %+v, want the edit of the dropped task gone and the other one kept", c)
	}
	var left string
	if err := s.DB().QueryRowContext(ctx, `SELECT task_id FROM outbox`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "t2" {
		t.Errorf("what is left in the queue belongs to %q", left)
	}
	if _, err := s.Task(ctx, created.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the task is still cached: %v", err)
	}
}

func TestDropParkedCountsAndDropsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "parcel"}); err != nil {
		t.Fatal(err)
	}
	enqueueUpdate(t, s, "t9", "A")
	for _, item := range claimAll(t, s) {
		if err := s.MarkFailed(ctx, item.Seq, item.LeaseToken, "the answer was lost"); err != nil {
			t.Fatal(err)
		}
	}

	var seen int
	dropped, err := s.DropParkedConfirmed(ctx, func(parkedCreates int) error {
		seen = parkedCreates
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("the callback was told of %d parked create(s), want 1", seen)
	}
	if len(dropped) != 2 {
		t.Errorf("dropped %d entr(y/ies), want the two the count was read off", len(dropped))
	}
	if c := mustCounts(t, s); c != (OutboxCounts{}) {
		t.Fatalf("queue %+v, want everything parked thrown away", c)
	}
}

func TestDropParkedConfirmedDropsNothingWhenRefused(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueUpdate(t, s, "t1", "A")

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "boom"); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("the user said no")
	dropped, err := s.DropParkedConfirmed(ctx, func(int) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("DropParkedConfirmed = %+v, %v; want the callback's error", dropped, err)
	}
	if len(dropped) != 0 {
		t.Errorf("DropParkedConfirmed threw away %d entr(y/ies) after the callback refused, want 0", len(dropped))
	}
	if c := mustCounts(t, s); c != (OutboxCounts{Failed: 1}) {
		t.Fatalf("queue %+v, want the entry left parked", c)
	}
}
