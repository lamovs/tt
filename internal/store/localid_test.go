package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestNewLocalID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewLocalID()
		if err != nil {
			t.Fatal(err)
		}
		if !IsLocalID(id) {
			t.Fatalf("%q is not recognised as local", id)
		}
		if got := len(strings.TrimPrefix(id, LocalIDPrefix)); got != 16 {
			t.Fatalf("hex part of %q is %d chars", id, got)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if IsLocalID("abc123") {
		t.Error("server id reported as local")
	}
}

func seedLocalTask(t *testing.T, s *Store, localID string) {
	t.Helper()
	ctx := context.Background()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO tasks (id, project_id, title, local, dirty) VALUES (?, 'p1', 'buy milk', 1, 1)`, []any{localID}},
		{`INSERT INTO items (task_id, id, title) VALUES (?, 'i1', 'step')`, []any{localID}},
		{`INSERT INTO listing (pos, task_id, created_at) VALUES (3, ?, 0)`, []any{localID}},
		{`INSERT INTO focus_sessions (id, task_id, kind, started_at) VALUES ('f1', ?, 'focus', 0)`, []any{localID}},
		{`INSERT INTO timer_state (id, kind, task_id, started_at, planned_sec) VALUES (1, 'focus', ?, 0, 1500)`, []any{localID}},
		{`INSERT INTO undo_log (at, kind, payload) VALUES (0, 'task.complete', ?)`, []any{`{"task_id":"` + localID + `"}`}},
	}
	for _, st := range stmts {
		if _, err := s.DB().ExecContext(ctx, st.q, st.args...); err != nil {
			t.Fatalf("seed %q: %v", st.q, err)
		}
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{
		Target:  TargetOpenAPI,
		Op:      "task.create",
		TaskID:  localID,
		Payload: []byte(`{"id":"` + localID + `","title":"buy milk"}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func countRefs(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	ctx := context.Background()
	queries := map[string]string{
		"tasks":          `SELECT count(*) FROM tasks WHERE id = ?`,
		"items":          `SELECT count(*) FROM items WHERE task_id = ?`,
		"listing":        `SELECT count(*) FROM listing WHERE task_id = ?`,
		"focus_sessions": `SELECT count(*) FROM focus_sessions WHERE task_id = ?`,
		"timer_state":    `SELECT count(*) FROM timer_state WHERE task_id = ?`,
		"outbox":         `SELECT count(*) FROM outbox WHERE task_id = ?`,
		"outbox_payload": `SELECT count(*) FROM outbox WHERE instr(payload, ?) > 0`,
		"undo_payload":   `SELECT count(*) FROM undo_log WHERE instr(payload, ?) > 0`,
	}
	out := map[string]int{}
	for name, q := range queries {
		var n int
		if err := s.DB().QueryRowContext(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		out[name] = n
	}
	return out
}

func TestReplaceLocalIDMovesEveryReference(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	localID, err := NewLocalID()
	if err != nil {
		t.Fatal(err)
	}
	seedLocalTask(t, s, localID)

	if err := s.ReplaceLocalID(ctx, localID, "srv-1"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	for table, n := range countRefs(t, s, localID) {
		if n != 0 {
			t.Errorf("%s still holds %d references to the local id", table, n)
		}
	}
	for table, n := range countRefs(t, s, "srv-1") {
		if n != 1 {
			t.Errorf("%s holds %d references to the server id, want 1", table, n)
		}
	}
	var local int
	if err := s.DB().QueryRowContext(ctx, `SELECT local FROM tasks WHERE id = 'srv-1'`).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if local != 0 {
		t.Error("task still flagged as local after the swap")
	}

	events, err := s.EventsSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != IDReplacedKind {
		t.Fatalf("events %+v", events)
	}
	var got struct {
		OldID string `json:"old_id"`
		NewID string `json:"new_id"`
	}
	if err := json.Unmarshal(events[0].Payload, &got); err != nil {
		t.Fatalf("payload %s: %v", events[0].Payload, err)
	}
	if got.OldID != localID || got.NewID != "srv-1" {
		t.Fatalf("payload %+v", got)
	}
}

func TestReplaceLocalIDIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	localID, err := NewLocalID()
	if err != nil {
		t.Fatal(err)
	}
	seedLocalTask(t, s, localID)
	before := countRefs(t, s, localID)

	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if err := replaceLocalID(ctx, tx, localID, "srv-1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	for table, n := range countRefs(t, s, localID) {
		if n != before[table] {
			t.Errorf("%s: %d references after rollback, want %d", table, n, before[table])
		}
	}
	for table, n := range countRefs(t, s, "srv-1") {
		if n != 0 {
			t.Errorf("%s: %d references to the server id survived the rollback", table, n)
		}
	}
	if events, err := s.EventsSince(ctx, 0, 0); err != nil || len(events) != 0 {
		t.Fatalf("journal after rollback: %v %+v", err, events)
	}

	if _, err := s.DB().ExecContext(ctx, `ALTER TABLE outbox RENAME TO outbox_hidden`); err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceLocalID(ctx, localID, "srv-1")
	if _, back := s.DB().ExecContext(ctx, `ALTER TABLE outbox_hidden RENAME TO outbox`); back != nil {
		t.Fatal(back)
	}
	if err == nil {
		t.Fatal("the swap succeeded with no queue to rewrite")
	}
	for table, n := range countRefs(t, s, localID) {
		if n != before[table] {
			t.Errorf("%s: %d references after the failed swap, want %d", table, n, before[table])
		}
	}
	for table, n := range countRefs(t, s, "srv-1") {
		if n != 0 {
			t.Errorf("%s: %d references to the server id survived the failed swap", table, n)
		}
	}
}

func TestReplaceLocalIDMergesIntoAPulledRow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	localID, err := NewLocalID()
	if err != nil {
		t.Fatal(err)
	}
	seedLocalTask(t, s, localID)

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "buy milk"))); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceLocalID(ctx, localID, "srv-1"); err != nil {
		t.Fatalf("swap onto a row the pull already brought in: %v", err)
	}

	tasks, err := s.Tasks(ctx, TaskFilter{Status: StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Id != "srv-1" {
		t.Fatalf("tasks %+v, want only the server's row", tasks)
	}
	for table, n := range countRefs(t, s, localID) {
		if n != 0 {
			t.Errorf("%s still holds %d references to the local id", table, n)
		}
	}

	want := map[string]int{
		"tasks": 1, "items": 0, "listing": 1, "focus_sessions": 1,
		"timer_state": 1, "outbox": 1, "outbox_payload": 1, "undo_payload": 1,
	}
	for table, n := range countRefs(t, s, "srv-1") {
		if n != want[table] {
			t.Errorf("%s holds %d references to the server id, want %d", table, n, want[table])
		}
	}

	queued := claimAll(t, s)
	if len(queued) != 1 || queued[0].TaskID != "srv-1" {
		t.Fatalf("queued %+v", queued)
	}
	if strings.Contains(string(queued[0].Payload), localID) {
		t.Errorf("the payload still names the local id: %s", queued[0].Payload)
	}

	ids, err := s.ResolveRefs(ctx, []string{"1"})
	if err != nil || len(ids) != 1 || ids[0] != "srv-1" {
		t.Fatalf("resolved to %v (%v)", ids, err)
	}
}

func TestMergedIDSwapDropsTheTwinsRevisions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, created.Id, model.TaskEdit{Content: model.Ptr("до пятницы")}); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 || first[0].Op != OpTaskCreate {
		t.Fatalf("claim the create: %v %+v", err, first)
	}

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, first[0].Seq, first[0].LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, first[0].TaskID, first[0].Rev); err != nil {
			return err
		}
		return ReplaceLocalIDTx(ctx, tx, created.Id, "srv-1")
	}); err != nil {
		t.Fatal(err)
	}
	var rev sql.NullInt64
	if err := s.DB().QueryRowContext(ctx, `SELECT rev FROM outbox WHERE task_id = 'srv-1'`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev.Valid {
		t.Errorf("the retargeted entry kept the twin's revision %d", rev.Int64)
	}

	if _, err := s.UpdateTask(ctx, "srv-1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "srv-1", model.TaskEdit{Title: model.Ptr("Забрать посылку до пятницы")}); err != nil {
		t.Fatal(err)
	}

	var stale OutboxItem
	for _, it := range claimAll(t, s) {
		if it.Op == OpTaskUpdate && it.Seq == first[0].Seq+1 {
			stale = it
		}
	}
	if stale.Seq == 0 {
		t.Fatal("the entry the twin left behind is gone")
	}
	var cleared bool
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, stale.Seq, stale.LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "srv-1", stale.Rev)
		if err != nil {
			return err
		}
		cleared = ok
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if cleared {
		t.Error("the twin's revision cleared the mark of the server's row")
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 'srv-1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty == 0 {
		t.Error("the row is no longer marked for the syncer, but its edits have not been pushed")
	}

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "srv-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку до пятницы" {
		t.Errorf("the pull overwrote an unpushed edit: %q", got.Title)
	}
}

func TestSyncerCommitsTheIDSwapWithTheQueueRow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Rev != 1 {
		t.Fatalf("queued %+v", items)
	}
	it := items[0]

	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, it.TaskID, it.Rev); err != nil {
			return err
		}
		if err := ReplaceLocalIDTx(ctx, tx, created.Id, "srv-1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := s.Task(ctx, created.Id); err != nil {
		t.Fatalf("the local task did not come back: %v", err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Inflight != 1 {
		t.Fatalf("counts %+v, want the create still queued", counts)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, it.TaskID, it.Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the dirty mark was not cleared")
		}
		return ReplaceLocalIDTx(ctx, tx, created.Id, "srv-1")
	}); err != nil {
		t.Fatal(err)
	}
	if counts, err = s.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("counts %+v (%v)", counts, err)
	}
	var local, dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT local, dirty FROM tasks WHERE id = 'srv-1'`).
		Scan(&local, &dirty); err != nil {
		t.Fatal(err)
	}
	if local != 0 || dirty != 0 {
		t.Fatalf("local %d dirty %d, want a clean server row", local, dirty)
	}
}

func TestReplaceLocalIDRejectsBadArgs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.ReplaceLocalID(ctx, "srv-1", "srv-2"); err == nil {
		t.Error("non-local source accepted")
	}
	if err := s.ReplaceLocalID(ctx, LocalIDPrefix+"aa", LocalIDPrefix+"bb"); err == nil {
		t.Error("local target accepted")
	}
	if err := s.ReplaceLocalID(ctx, LocalIDPrefix+"aa", ""); err == nil {
		t.Error("empty target accepted")
	}
}

func TestRenamingIDSwapKeepsTheRevisions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, created.Id, model.TaskEdit{Content: model.Ptr("до пятницы")}); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 || first[0].Op != OpTaskCreate {
		t.Fatalf("claim the create: %v %+v", err, first)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, first[0].Seq, first[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, first[0].TaskID, first[0].Rev)
		if err != nil {
			return err
		}
		if ok {
			return errors.New("the mark was cleared although the row has been edited since")
		}
		return ReplaceLocalIDTx(ctx, tx, created.Id, "srv-1")
	}); err != nil {
		t.Fatal(err)
	}

	rest := claimAll(t, s)
	if len(rest) != 1 || rest[0].TaskID != "srv-1" {
		t.Fatalf("queued %+v", rest)
	}
	if rest[0].Rev != 2 {
		t.Fatalf("the surviving entry carries revision %d, want the 2 it was queued with", rest[0].Rev)
	}
	var cleared bool
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, rest[0].Seq, rest[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "srv-1", rest[0].Rev)
		if err != nil {
			return err
		}
		cleared = ok
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("nothing is left to clear the mark: the row would be skipped by every pull from now on")
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 'srv-1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 0 {
		t.Errorf("dirty %d after the last entry was pushed, want a clean row", dirty)
	}
}

func TestMarkPushedTakesTheIDTheMutationWentOutWith(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	sent := claimAll(t, s)
	if len(sent) != 1 || sent[0].Op != OpTaskCreate || sent[0].TaskID != created.Id || sent[0].Rev != 1 {
		t.Fatalf("queued %+v, want the create of the twin at revision 1", sent)
	}
	it := sent[0]

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "srv-1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, it.TaskID, it.Rev); err != nil {
			return err
		}
		return ReplaceLocalIDTx(ctx, tx, it.TaskID, "srv-1")
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result %+v, want the unpushed edit kept", res)
	}
	got, err := s.Task(ctx, "srv-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку на почте" {
		t.Fatalf("the pull overwrote an unpushed edit: %q", got.Title)
	}
	queued := claimAll(t, s)
	if len(queued) != 1 || queued[0].Op != OpTaskUpdate || queued[0].TaskID != "srv-1" || queued[0].Rev != 1 {
		t.Fatalf("queued %+v, want the edit of the server's row still waiting", queued)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, queued[0].Seq, queued[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, queued[0].TaskID, queued[0].Rev)
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
	if res, err = s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку с почты"))); err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("result %+v, want the row taking the server's copy again", res)
	}
}

func TestMarkPushedAfterTheIDSwapClearsTheWrongMark(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"}); err != nil {
		t.Fatal(err)
	}
	sent := claimAll(t, s)
	if len(sent) != 1 || sent[0].Op != OpTaskCreate || sent[0].Rev != 1 {
		t.Fatalf("queued %+v, want the create of the twin at revision 1", sent)
	}
	it := sent[0]

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "srv-1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}

	var cleared bool
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, it.Seq, it.LeaseToken); err != nil {
			return err
		}
		if err := ReplaceLocalIDTx(ctx, tx, it.TaskID, "srv-1"); err != nil {
			return err
		}

		ok, err := MarkPushedTx(ctx, tx, "srv-1", it.Rev)
		if err != nil {
			return err
		}
		cleared = ok
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("the coincidence no longer matches: MarkPushedTx has changed and its warning needs rewriting")
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv-1", "p1", "Забрать посылку")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("result %+v, want the row taken for clean", res)
	}
	got, err := s.Task(ctx, "srv-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку" {
		t.Fatalf("title %q: the edit survived, so the trap this test describes is gone", got.Title)
	}

	queued := claimAll(t, s)
	if len(queued) != 1 || queued[0].Op != OpTaskUpdate {
		t.Fatalf("queued %+v, want the edit still waiting to be sent", queued)
	}
}

func TestAParkedCreateStillHoldsItsOfflineTask(t *testing.T) {
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
	if _, err := s.Task(ctx, created.Id); err != nil {
		t.Fatalf("the task of a parked create is gone: %v", err)
	}
	orphans, err := s.OrphanedLocalTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("a task whose create can still be raised is reported as orphaned: %+v", orphans)
	}

	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatal(err)
	}
	sent := claimAll(t, s)
	if len(sent) != 1 {
		t.Fatalf("claimed %+v", sent)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, sent[0].Seq, sent[0].LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, sent[0].TaskID, sent[0].Rev); err != nil {
			return err
		}
		return ReplaceLocalIDTx(ctx, tx, created.Id, "srv-1")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, "srv-1"); err != nil {
		t.Fatalf("the task the server took is not in the cache: %v", err)
	}
	if orphans, err = s.OrphanedLocalTasks(ctx); err != nil || len(orphans) != 0 {
		t.Fatalf("orphans %+v (%v)", orphans, err)
	}
}

func TestOrphanedLocalTasksFindsARowNoCreateWillResolve(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel"))); err != nil {
		t.Fatal(err)
	}
	kept, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Polit cvety"})
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM outbox WHERE task_id = ? AND op = ?`, stranded.Id, OpTaskCreate); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.OrphanedLocalTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("found %+v, want the one row nothing will ever send", orphans)
	}
	got := orphans[0]
	if got.ID != stranded.Id || got.ProjectID != "p1" || got.Title != "Zabrat posylku" {
		t.Errorf("reported %+v", got)
	}
	if got.CreatedTime.IsZero() {
		t.Error("nothing to tell the user how long it has been sitting there")
	}

	if _, err := s.Task(ctx, kept.Id); err != nil {
		t.Fatalf("the task with a queued create was touched: %v", err)
	}
}

// TestReplaceLocalIDMovesTheRowsThatPointAtTheOldID covers both branches of
// the swap: a local parent that is renamed into its server id, and one that
// merges into a row a pull already brought in. In both cases the rows naming
// the old id are other rows, and a child left with a local parent could not
// be edited again.
func TestReplaceLocalIDMovesTheRowsThatPointAtTheOldID(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "renamed", true: "merged"}[merged], func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			seedProjects(t, s, model.Project{Id: "p1", Name: "P"})
			if merged {
				if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("srv1", "p1", "Parent"))); err != nil {
					t.Fatal(err)
				}
			}
			parent, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child", ParentId: parent.Id})
			if err != nil {
				t.Fatalf("create a child of a parent waiting for its own create: %v", err)
			}
			holder, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Holder", ChildIds: []string{parent.Id}})
			if err != nil {
				t.Fatal(err)
			}

			if err := s.ReplaceLocalID(ctx, parent.Id, "srv1"); err != nil {
				t.Fatalf("swap: %v", err)
			}

			got, err := s.Task(ctx, child.Id)
			if err != nil || got.ParentId != "srv1" {
				t.Fatalf("child parent = %q (%v), want srv1", got.ParentId, err)
			}
			kept, err := s.Task(ctx, holder.Id)
			if err != nil || len(kept.ChildIds) != 1 || kept.ChildIds[0] != "srv1" {
				t.Fatalf("cached child ids = %v (%v), want srv1", kept.ChildIds, err)
			}
			var stale int
			if err := s.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM tasks WHERE parent_id = ? OR instr(child_ids, ?) > 0`,
				parent.Id, parent.Id).Scan(&stale); err != nil {
				t.Fatal(err)
			}
			if stale != 0 {
				t.Errorf("%d cached rows still point at the local id", stale)
			}
		})
	}
}

func TestDropParkedLeavesNoReferenceToTheTaskItRemoved(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "P"})

	parent, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("queue the child's link to the parent: %v", err)
	}

	holder := openTask("srv-holder", "p1", "Holder")
	holder.ChildIds = []string{parent.Id, "srv-child"}
	if _, err := s.SyncProject(ctx, "p1", fromServer(holder)); err != nil {
		t.Fatal(err)
	}

	items := claimAll(t, s)
	var parentCreate OutboxItem
	for _, it := range items {
		if it.Op == OpTaskCreate && it.TaskID == parent.Id {
			parentCreate = it
		}
	}
	if parentCreate.Seq == 0 {
		t.Fatalf("no queued create for the parent among %+v", items)
	}
	if err := s.MarkFailed(ctx, parentCreate.Seq, parentCreate.LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DropParked(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Task(ctx, parent.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the dropped parent is still in the cache: %v", err)
	}
	gotChild, err := s.Task(ctx, child.Id)
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ParentId != "" {
		t.Errorf("child parent_id = %q, want cleared once the parent is gone", gotChild.ParentId)
	}

	gotHolder, err := s.Task(ctx, "srv-holder")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotHolder.ChildIds) != 1 || gotHolder.ChildIds[0] != "srv-child" {
		t.Errorf("holder child ids = %+v, want only the id that was never dropped", gotHolder.ChildIds)
	}
}

// queuedLinkParents reads back which task each queued relationship hangs its
// own task under, the way the push reads it.
func queuedLinkParents(t *testing.T, st *Store) map[string]string {
	t.Helper()
	queue, err := st.RecoveryQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range queue.Tasks {
		parent, hangs, err := TaskExtensionParent(entry.Item)
		if err != nil {
			t.Fatal(err)
		}
		if hangs {
			out[entry.Item.TaskID] = parent
		}
	}
	return out
}

// The relationship that hangs a task under another one is queued against the
// child, so throwing the parent away leaves it behind pointing at an id
// nothing holds any more; the next push parks it over a parent that is not in
// the cache and a second tt sync --drop-parked is what it takes to be rid of
// it. Relationships hung under anything else are none of this drop's
// business.
func TestDroppingAnOrphanedLocalTaskTakesTheRelationshipsHungUnderIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	var tasks []model.Task
	for _, title := range []string{"Parent", "Child", "Other parent", "Other child"} {
		task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	parent, child, other, otherChild := tasks[0], tasks[1], tasks[2], tasks[3]
	for _, link := range []struct{ child, parent string }{{child.Id, parent.Id}, {otherChild.Id, other.Id}} {
		if _, err := st.UpdateTask(ctx, link.child, model.TaskEdit{ParentId: model.Ptr(link.parent)}); err != nil {
			t.Fatalf("queue the relationship of %s: %v", link.child, err)
		}
	}
	for _, item := range claimAll(t, st) {
		if item.TaskID == parent.Id {
			if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "rejected"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := st.MarkDone(ctx, item.Seq, item.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	if got := queuedLinkParents(t, st); got[child.Id] != parent.Id {
		t.Fatalf("queued relationships before the drop = %v, want the child hung under the parent", got)
	}

	dropped, err := st.DropParked(ctx)
	if err != nil || len(dropped) != 1 {
		t.Fatalf("tt sync --drop-parked = %+v %v, want the parked create thrown away", dropped, err)
	}
	if _, err := st.Task(ctx, parent.Id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the dropped parent = %v, want it gone from the cache", err)
	}
	links := queuedLinkParents(t, st)
	if _, left := links[child.Id]; left {
		t.Fatalf("queued relationships after the drop = %v, want nothing left hung under the dropped task", links)
	}
	if links[otherChild.Id] != other.Id {
		t.Fatalf("queued relationships after the drop = %v, want the relationship of another parent untouched", links)
	}
	// The child itself is not what the reader threw away, and the cache no
	// longer shows it under a task that is gone.
	stored, err := st.Task(ctx, child.Id)
	if err != nil || stored.ParentId != "" {
		t.Fatalf("the child after the drop = %+v %v, want it kept and detached", stored, err)
	}
}

func TestDropParkedChildLinkAllowsFuturePull(t *testing.T) {
	ctx := context.Background()
	st, parent := localParentFixture(t)
	if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].TaskID != parent.Id {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := st.MarkFailed(ctx, claimed[0].Seq, claimed[0].LeaseToken, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DropParked(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dirty int
	if err := st.DB().QueryRowContext(ctx, "SELECT dirty FROM tasks WHERE id='child'").Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	result, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: model.Task{Id: "child", ProjectId: "p1", Title: "Updated on server"}}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Task(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("queue=%+v dirty=%d pull=%+v title=%q parent=%q", counts, dirty, result, child.Title, child.ParentId)
	if child.Title != "Updated on server" {
		t.Fatal("child remains dirty with empty queue; future server updates are ignored")
	}
}

func parkLocalParent(t *testing.T, st *Store, parent model.Task) {
	t.Helper()
	ctx := context.Background()
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].TaskID != parent.Id {
		t.Fatalf("claim parent: %+v %v", claimed, err)
	}
	if err := st.MarkFailed(ctx, claimed[0].Seq, claimed[0].LeaseToken, "rejected"); err != nil {
		t.Fatal(err)
	}
}

func TestDropParkedChildLinkPreservesOtherFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit model.TaskEdit
	}{
		{"estimates", model.TaskEdit{EstimatedPomo: model.Ptr(3), EstimatedDuration: model.Ptr(int64(120))}},
		{"title", model.TaskEdit{Title: model.Ptr("Edited child")}},
		{"repeat", model.TaskEdit{RepeatFlag: model.Ptr("RRULE:FREQ=DAILY;INTERVAL=1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			edit := tc.edit
			edit.ParentId = model.Ptr(parent.Id)
			if _, err := st.UpdateTask(ctx, "child", edit); err != nil {
				t.Fatal(err)
			}
			parkLocalParent(t, st, parent)
			if _, err := st.DropParked(ctx); err != nil {
				t.Fatal(err)
			}
			claimed, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 1 || claimed[0].TaskID != "child" {
				t.Fatalf("retained operation: %+v %v", claimed, err)
			}
			got, metadata, err := DecodeTaskEditPayload(claimed[0].Payload)
			want, _ := json.Marshal(tc.edit)
			actual, _ := json.Marshal(got)
			if err != nil || string(actual) != string(want) {
				t.Fatalf("edit = %s, want %s (%v)", actual, want, err)
			}
			if metadata != nil && metadata.ExtensionBaseline != nil && metadata.ExtensionBaseline.ParentId != nil {
				t.Fatal("retained baseline still names a parent")
			}
			send, present, post, err := st.PrepareFeatureSend(ctx, claimed[0])
			if err != nil || present != (metadata != nil) || present && !post {
				t.Fatalf("prepare retained operation: %+v %v %v %v", send, present, post, err)
			}
			var dirty int64
			if err := st.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 'child'`).Scan(&dirty); err != nil {
				t.Fatal(err)
			}
			if dirty != claimed[0].Rev || dirty == 0 {
				t.Fatalf("dirty = %d, retained revision = %d", dirty, claimed[0].Rev)
			}
		})
	}
}

func TestDropParkedChildLinkPreservesOtherRevisions(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(map[bool]string{false: "older", true: "newer"}[newer], func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			edits := []model.TaskEdit{{Title: model.Ptr("Edited child")}, {ParentId: model.Ptr(parent.Id)}}
			if newer {
				edits[0], edits[1] = edits[1], edits[0]
			}
			for _, edit := range edits {
				if _, err := st.UpdateTask(ctx, "child", edit); err != nil {
					t.Fatal(err)
				}
			}
			parkLocalParent(t, st, parent)
			if _, err := st.DropParked(ctx); err != nil {
				t.Fatal(err)
			}
			incoming := fromServer(openTask("child", "p1", "Server title"))
			if _, err := st.SyncProject(ctx, "p1", incoming); err != nil {
				t.Fatal(err)
			}
			child, err := st.Task(ctx, "child")
			if err != nil || child.Title != "Edited child" {
				t.Fatalf("pending edit lost: %+v %v", child, err)
			}
			claimed, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			if err := st.Tx(ctx, func(tx *sql.Tx) error {
				cleared, err := MarkPushedTx(ctx, tx, "child", claimed[0].Rev)
				if err != nil {
					return err
				}
				if !cleared {
					return errors.New("retained revision cannot clear dirty")
				}
				return markDone(ctx, tx, claimed[0].Seq, claimed[0].LeaseToken)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SyncProject(ctx, "p1", incoming); err != nil {
				t.Fatal(err)
			}
			child, err = st.Task(ctx, "child")
			if err != nil || child.Title != "Server title" {
				t.Fatalf("pull after confirmation: %+v %v", child, err)
			}
		})
	}
}

func TestDropParkedRefusesFrozenMixedChildLinkAtomically(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "armed", true: "rejected"}[rejected], func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr(parent.Id), EstimatedPomo: model.Ptr(3)}); err != nil {
				t.Fatal(err)
			}
			claimed, _, err := st.Claim(ctx, 2, time.Minute)
			if err != nil || len(claimed) != 2 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			if err := st.Unclaim(ctx, claimed[0].Seq, claimed[0].LeaseToken, "waiting"); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := st.PrepareFeatureSend(ctx, claimed[1]); err != nil {
				t.Fatal(err)
			}
			if rejected {
				if err := st.RejectFeature(ctx, claimed[1]); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Unclaim(ctx, claimed[1].Seq, claimed[1].LeaseToken, "waiting"); err != nil {
				t.Fatal(err)
			}
			parkLocalParent(t, st, parent)
			var before string
			if err := st.DB().QueryRowContext(ctx, `SELECT payload FROM outbox WHERE seq = ?`, claimed[1].Seq).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DropParked(ctx); err == nil || !strings.Contains(err.Error(), "frozen request") {
				t.Fatalf("drop = %v, want an explicit refusal", err)
			}
			if _, err := st.Task(ctx, parent.Id); err != nil {
				t.Fatalf("parent lost on refusal: %v", err)
			}
			var after string
			if err := st.DB().QueryRowContext(ctx, `SELECT payload FROM outbox WHERE seq = ?`, claimed[1].Seq).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("frozen payload changed on refusal")
			}
			counts, err := st.OutboxCounts(ctx)
			if err != nil || counts.Pending != 1 || counts.Failed != 1 {
				t.Fatalf("queue changed: %+v %v", counts, err)
			}
		})
	}
}
