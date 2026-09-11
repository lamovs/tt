package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func enqueueUpdate(t *testing.T, s *Store, taskID, title string) int64 {
	t.Helper()
	seq, err := s.Enqueue(context.Background(), OutboxEntry{
		Target:  TargetOpenAPI,
		Op:      OpTaskUpdate,
		TaskID:  taskID,
		Payload: []byte(`{"title":"` + title + `"}`),
	})
	if err != nil {
		t.Fatalf("enqueue %s of %s: %v", title, taskID, err)
	}
	return seq
}

func seqsOf(items []OutboxItem) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.Seq
	}
	return out
}

func TestClaimHandsOutOneEntryPerTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	a := enqueueUpdate(t, s, "t1", "A")
	enqueueUpdate(t, s, "t1", "B")
	other := enqueueUpdate(t, s, "t2", "C")

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Seq != a || items[1].Seq != other {
		t.Fatalf("claimed %v, want the first entry of each task (%d, %d)", seqsOf(items), a, other)
	}
}

func TestClaimLeavesTheLaterEntryWhileTheEarlierIsInFlight(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	a := enqueueUpdate(t, s, "t1", "A")
	enqueueUpdate(t, s, "t1", "B")

	first, _, err := s.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Seq != a {
		t.Fatalf("first claim took %v, want entry %d", seqsOf(first), a)
	}
	second, _, err := s.Claim(ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second claim took entry %d of task %q while entry %d of the same task is in flight",
			second[0].Seq, second[0].TaskID, a)
	}
}

func TestClaimTakesTheStaleEntryAheadOfTheOneItBlocks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	a := enqueueUpdate(t, s, "t1", "A")
	enqueueUpdate(t, s, "t1", "B")

	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Seq != a {
		t.Fatalf("first claim took %v, want entry %d", seqsOf(first), a)
	}
	again, _, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Seq != a {
		t.Fatalf("claim after the lease ran out took %v, want the stale entry %d alone", seqsOf(again), a)
	}
}

func TestClaimIsNotHeldBackByAParkedEntry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	enqueueUpdate(t, s, "t1", "A")
	b := enqueueUpdate(t, s, "t1", "B")

	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim took %d entries, want 1", len(first))
	}
	if err := s.MarkFailed(ctx, first[0].Seq, first[0].LeaseToken, "boom"); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Seq != b {
		t.Fatalf("claim behind a parked entry took %v, want entry %d", seqsOf(items), b)
	}
}

func TestClaimDoesNotGroupEntriesWithoutATask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	var seqs []int64
	for i := 0; i < 3; i++ {
		seq, err := s.Enqueue(ctx, OutboxEntry{
			Target:  TargetV2,
			Op:      "focus.push",
			Payload: []byte(`{"id":"f1"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(seqs) {
		t.Fatalf("claimed %v, want all of %v", seqsOf(items), seqs)
	}
}

func TestRenewExtendsTheLeaseOfTheHolder(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueUpdate(t, s, "t1", "A")

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE outbox SET inflight_at = inflight_at - 59 WHERE seq = ?`, items[0].Seq); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(ctx, items[0].Seq, items[0].LeaseToken, time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	stolen, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stolen) != 0 {
		t.Fatalf("claim took entry %d after its lease was renewed", stolen[0].Seq)
	}
}

func TestRenewRefusesAnEntryTakenOver(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueUpdate(t, s, "t1", "A")

	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(first))
	}
	second, _, err := s.Claim(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim took %d entries, want 1", len(second))
	}
	err = s.Renew(ctx, first[0].Seq, first[0].LeaseToken, time.Minute)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Renew under the old token = %v, want ErrLeaseLost", err)
	}

	if err := s.MarkDone(ctx, second[0].Seq, second[0].LeaseToken); err != nil {
		t.Fatal(err)
	}
	err = s.Renew(ctx, second[0].Seq, second[0].LeaseToken, time.Minute)
	if !errors.Is(err, ErrNoOutboxRow) {
		t.Fatalf("Renew of a dropped entry = %v, want ErrNoOutboxRow", err)
	}
}

func TestRenewRefusesALeaseAlreadyRunOut(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	enqueueUpdate(t, s, "t1", "A")

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.Renew(ctx, items[0].Seq, items[0].LeaseToken, 0); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Renew under a spent lease = %v, want ErrLeaseLost", err)
	}
}

func TestMarkSentIsFencedOnTheLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	created, err := s.CreateTask(ctx, openTask("", "p1", "parcel"))
	if err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TaskID != created.Id {
		t.Fatalf("claimed %+v, want the create of %s", items, created.Id)
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := s.MarkSent(ctx, items[0].Seq, "not-the-token"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkSent under a foreign token = %v, want ErrLeaseLost", err)
	}
}

func TestClaimParksAnAmbiguousCreateItReclaims(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	created, err := s.CreateTask(ctx, openTask("", "p1", "parcel"))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirtyOf(t, s, created.Id); got == 0 {
		t.Fatalf("dirty after the create = %d, want the row marked", got)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}

	next, parked, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 0 {
		t.Fatalf("claim after the lease ran out took %v, want nothing: the create is ambiguous", seqsOf(next))
	}
	if parked != 1 {
		t.Errorf("the claim reported %d parked entr(y/ies), want the create it took out of the queue", parked)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || counts.Pending != 0 || counts.Inflight != 0 {
		t.Fatalf("counts %+v, want the create parked (Failed 1)", counts)
	}
	if got := dirtyOf(t, s, created.Id); got != 0 {
		t.Errorf("dirty of the parked create = %d, want the row released as any park releases it", got)
	}
	if n, err := s.FailedCreates(ctx); err != nil || n != 1 {
		t.Fatalf("FailedCreates = %d, %v; want the parked create counted before a retry", n, err)
	}

	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	raised, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(raised) != 1 {
		t.Fatalf("claim after RetryFailed took %v, want the raised create", seqsOf(raised))
	}
}

func TestClaimReclaimsACreateThatWasNeverSent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	if _, err := s.CreateTask(ctx, openTask("", "p1", "parcel")); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}

	next, _, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Seq != items[0].Seq {
		t.Fatalf("claim after the lease ran out took %v, want the create back", seqsOf(next))
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 0 {
		t.Fatalf("counts %+v, want nothing parked", counts)
	}
}

func TestClaimReclaimsACreateTheServerHasNamed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	created, err := s.CreateTask(ctx, openTask("", "p1", "parcel"))
	if err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceLocalID(ctx, created.Id, "srv1"); err != nil {
		t.Fatalf("adopt the server id: %v", err)
	}

	next, _, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 {
		t.Fatalf("claim after the lease ran out took %v, want the create back", seqsOf(next))
	}
	if next[0].TaskID != "srv1" {
		t.Fatalf("reclaimed entry addresses %q, want the server id", next[0].TaskID)
	}
}

func TestReclaimStaleParksAnAmbiguousCreate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	created, err := s.CreateTask(ctx, openTask("", "p1", "parcel"))
	if err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}

	n, parked, err := s.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ReclaimStale put %d ambiguous create(s) back in line, want 0", n)
	}
	if parked != 1 {
		t.Errorf("ReclaimStale reported %d parked entr(y/ies), want the ambiguous create counted", parked)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || counts.Pending != 0 {
		t.Fatalf("counts %+v, want the create parked (Failed 1)", counts)
	}
	if got := dirtyOf(t, s, created.Id); got != 0 {
		t.Errorf("dirty of the parked create = %d, want the row released", got)
	}
}

func TestRetryFailedCountsAndRaisesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	if _, err := s.CreateTask(ctx, openTask("", "p1", "parcel")); err != nil {
		t.Fatal(err)
	}
	enqueueUpdate(t, s, "t9", "A")

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("claimed %d entries, want 2", len(items))
	}
	for _, it := range items {
		if err := s.MarkFailed(ctx, it.Seq, it.LeaseToken, "boom"); err != nil {
			t.Fatal(err)
		}
	}

	var seen int
	n, err := s.RetryFailedConfirmed(ctx, func(parkedCreates int) error {
		seen = parkedCreates
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("the callback was told of %d parked create(s), want 1", seen)
	}
	if n != 2 {
		t.Errorf("RetryFailedConfirmed raised %d entries, want 2", n)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 2 || counts.Failed != 0 {
		t.Fatalf("counts %+v, want everything back in line", counts)
	}
}

func TestRetryFailedConfirmedRaisesNothingWhenRefused(t *testing.T) {
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
	n, err := s.RetryFailedConfirmed(ctx, func(int) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("RetryFailedConfirmed = %d, %v; want the callback's error", n, err)
	}
	if n != 0 {
		t.Errorf("RetryFailedConfirmed raised %d entries after the callback refused, want 0", n)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 1 || counts.Pending != 0 {
		t.Fatalf("counts %+v, want the entry left parked", counts)
	}
}

func sentStampOf(t *testing.T, s *Store, seq int64) sql.NullInt64 {
	t.Helper()
	var sent sql.NullInt64
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT sent_at FROM outbox WHERE seq = ?`, seq).Scan(&sent)
	if err != nil {
		t.Fatalf("read the sent stamp of %d: %v", seq, err)
	}
	return sent
}

func TestRetryFailedClearsTheStampOfARequestAlreadySent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	if _, err := s.CreateTask(ctx, openTask("", "p1", "parcel")); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "the answer was lost"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	if sent := sentStampOf(t, s, items[0].Seq); sent.Valid {
		t.Errorf("sent_at = %d after the retry, want it cleared: the stamp belongs to the request the "+
			"next pass makes", sent.Int64)
	}

	raised, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(raised) != 1 {
		t.Fatalf("claim after RetryFailed took %v, want the raised create", seqsOf(raised))
	}
	next, _, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Seq != items[0].Seq {
		t.Fatalf("claim after the lease ran out took %v, want the create back: nothing was sent for it "+
			"in the pass that died", seqsOf(next))
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 0 {
		t.Errorf("counts %+v, want nothing parked: the retry the user asked for has not been made yet", counts)
	}
}

func TestClaimClearsTheStampOfTheRequestBefore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})
	if _, err := s.CreateTask(ctx, openTask("", "p1", "parcel")); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkSent(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := s.Unclaim(ctx, items[0].Seq, items[0].LeaseToken, "the token had expired"); err != nil {
		t.Fatal(err)
	}

	again, parked, err := s.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || parked != 0 {
		t.Fatalf("claim took %v and parked %d, want the create handed out", seqsOf(again), parked)
	}
	if sent := sentStampOf(t, s, items[0].Seq); sent.Valid {
		t.Errorf("sent_at = %d after the claim, want it cleared: the stamp is about the request the pass "+
			"before was refused, and this claim is a request of its own", sent.Int64)
	}

	next, parked, err := s.Claim(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if parked != 0 || len(next) != 1 || next[0].Seq != items[0].Seq {
		t.Fatalf("claim after the lease ran out took %v and parked %d, want the create back: nothing has "+
			"gone out for it since the request the server refused", seqsOf(next), parked)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 0 {
		t.Errorf("counts %+v, want nothing parked over a request that was never made", counts)
	}
}
