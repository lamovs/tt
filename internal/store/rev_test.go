package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func dirtyOf(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	var d int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT dirty FROM tasks WHERE id = ?`, id).Scan(&d); err != nil {
		t.Fatalf("read dirty of %s: %v", id, err)
	}
	return d
}

func titleOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	task, err := s.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("load task %s: %v", id, err)
	}
	return task.Title
}

func TestDropParkedKeepsTheMarkOfALaterEdit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})

	server := openTask("t1", "p1", "server title")
	if _, err := s.SyncProject(ctx, "p1", fromServer(server)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("edit A")}); err != nil {
		t.Fatal(err)
	}
	if got := dirtyOf(t, s, "t1"); got != 1 {
		t.Fatalf("dirty after edit A = %d, want 1", got)
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "boom"); err != nil {
		t.Fatal(err)
	}
	if got := dirtyOf(t, s, "t1"); got != 0 {
		t.Fatalf("dirty after park = %d, want 0", got)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("edit B")}); err != nil {
		t.Fatal(err)
	}
	if got := dirtyOf(t, s, "t1"); got == items[0].Rev {
		t.Fatalf("dirty after edit B = %d, the revision the parked entry still carries", got)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 || counts.Failed != 1 {
		t.Fatalf("counts %+v, want pending 1 failed 1", counts)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 {
		t.Fatalf("dropped %d entries, want 1", len(dropped))
	}
	counts, err = s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 {
		t.Fatalf("counts after drop %+v, want pending 1", counts)
	}
	if got := dirtyOf(t, s, "t1"); got == 0 {
		t.Errorf("DropParked cleared the mark of edit B: dirty = 0 with a pending entry for the row")
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(server))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Errorf("SyncProject result %+v, want Skipped 1", res)
	}
	if got := titleOf(t, s, "t1"); got != "edit B" {
		t.Errorf("title after pull = %q, want %q", got, "edit B")
	}
}

func TestARetriedParkedEntryKeepsTheMarkOfALaterEdit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})

	server := openTask("t1", "p1", "server title")
	if _, err := s.SyncProject(ctx, "p1", fromServer(server)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("edit A")}); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("claimed %d entries, want 1", len(items))
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "boom"); err != nil {
		t.Fatal(err)
	}
	seqA := items[0].Seq

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("edit B")}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}

	claimed, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var a OutboxItem
	for _, it := range claimed {
		if it.Seq == seqA {
			a = it
		}
	}
	if a.Seq != seqA {
		t.Fatalf("retried entry %d not claimed, got %+v", seqA, claimed)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, a.Seq, a.LeaseToken); err != nil {
			return err
		}
		_, err := MarkPushedTx(ctx, tx, a.TaskID, a.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := dirtyOf(t, s, "t1"); got == 0 {
		t.Errorf("the answer to A cleared the mark of edit B: dirty = 0 while B is still queued")
	}
}
