package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func latestVersion(t *testing.T) int {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	return ms[len(ms)-1].version
}

func openAtVersion(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version > version {
			break
		}
		if err := applyMigration(ctx, db, m, ms[len(ms)-1].version, path); err != nil {
			t.Fatalf("apply migration %04d_%s: %v", m.version, m.name, err)
		}
	}
	return db
}

func tableColumns(t *testing.T, q execer, table string) map[string]bool {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("columns of %s: %v", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	return out
}

func objectSQL(t *testing.T, q execer, name string) string {
	t.Helper()
	var text string
	if err := q.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE name = ?`, name).Scan(&text); err != nil {
		t.Fatalf("definition of %s: %v", name, err)
	}
	return text
}

func TestMigrateFromScratchIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	want := latestVersion(t)

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	got, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("version %d, want %d", got, want)
	}
	tables := []string{"meta", "projects", "tasks", "items", "item_identities", "outbox", "focus_sessions", "timer_state", "events", "listing", "undo_log"}
	for _, name := range tables {
		var n int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %s missing", name)
		}
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.create"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	got, err = s2.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("version after reopen %d, want %d", got, want)
	}
	counts, err := s2.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 {
		t.Fatalf("reopen dropped data: pending %d, want 1", counts.Pending)
	}
}

func TestDirtyScanUsesIndex(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var plan string
	err := s.DB().QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM tasks WHERE dirty <> 0`).
		Scan(new(int), new(int), new(int), &plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_tasks_dirty") {
		t.Fatalf("query plan %q does not use idx_tasks_dirty", plan)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	future := latestVersion(t) + 1
	if err := s.SetMeta(ctx, schemaVersionKey, strconv.Itoa(future)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, path)
	if err == nil {
		s2.Close()
		t.Fatal("open succeeded on a newer schema")
	}
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *VersionError", err)
	}
	if ve.Found != future || ve.Supported != future-1 {
		t.Fatalf("got %+v", ve)
	}
}

func TestConcurrentOpenAppliesEachMigrationOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := Open(ctx, path)
			errs[i] = err
			if s != nil {
				s.Close()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := latestVersion(t); got != want {
		t.Fatalf("schema version %d, want %d", got, want)
	}
}

func TestVersionErrorDoesNotSendTheUserToDeleteTheQueue(t *testing.T) {
	msg := (&VersionError{Found: 9, Supported: 2, Path: "/tmp/cache.db"}).Error()
	if !strings.Contains(msg, "update tt") {
		t.Errorf("the message does not name the remedy that keeps the queue: %q", msg)
	}
	if strings.Contains(msg, "delet") && !strings.Contains(msg, "Do not delete") {
		t.Errorf("the message offers deleting the cache, which is where the unsent queue lives: %q", msg)
	}
}

func TestMigrateBringsAVersionOneCacheUpToDate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 1)

	for _, column := range []string{"rev", "lease_token", "failed_at", "sent_at"} {
		if tableColumns(t, db, "outbox")[column] {
			t.Fatalf("0001 already creates outbox.%s: it was edited after it shipped, "+
				"and a cache written before the edit will never get the column", column)
		}
	}
	if got := objectSQL(t, db, "idx_tasks_dirty"); !strings.Contains(got, "dirty = 1") {
		t.Fatalf("0001 creates idx_tasks_dirty as %q, want the flag form it shipped with", got)
	}

	seed := []string{
		`INSERT INTO projects (id, name, search) VALUES ('p1', 'Личное', 'личное')`,
		`INSERT INTO tasks (id, project_id, title, search, raw, dirty) VALUES ('t1', 'p1', 'Забрать посылку', 'забрать посылку', '{"id":"t1"}', 1)`,
		`INSERT INTO tasks (id, project_id, title, search, local, dirty) VALUES ('local-ab', 'p1', 'offline', 'offline', 1, 1)`,
		`INSERT INTO items (task_id, id, title) VALUES ('t1', 'i1', 'паспорт')`,
		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, attempts, last_error, state) VALUES ('openapi', 'task.update', 't1', 'p1', '{"title":"x"}', 111, 2, 'boom', 'pending')`,
		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, state, inflight_at) VALUES ('openapi', 'task.create', 'local-ab', 'p1', '{"id":"local-ab"}', 112, 'inflight', 999)`,
		`INSERT INTO outbox (target, op, payload, created_at, state) VALUES ('v2', 'focus.push', '{"id":"f1"}', 113, 'failed')`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open a cache written by the older tt: %v", err)
	}
	defer s.Close()
	got, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == 1 {
		t.Fatal("the cache is still at version 1: the schema change has no migration of its own")
	}
	if want := latestVersion(t); got != want {
		t.Fatalf("version %d, want %d", got, want)
	}

	columns := tableColumns(t, s.DB(), "outbox")
	for _, column := range []string{"rev", "lease_token", "failed_at", "sent_at"} {
		if !columns[column] {
			t.Errorf("outbox.%s is missing after the upgrade", column)
		}
	}
	if sqlText := objectSQL(t, s.DB(), "idx_outbox_task_seq"); !strings.Contains(sqlText, "task_id") {
		t.Errorf("idx_outbox_task_seq is %q, want the index the claim's per-task guard reads", sqlText)
	}
	if sqlText := objectSQL(t, s.DB(), "idx_tasks_dirty"); !strings.Contains(sqlText, "dirty <> 0") {
		t.Errorf("idx_tasks_dirty is %q, want the counter form", sqlText)
	}
	var plan string
	if err := s.DB().QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM tasks WHERE dirty <> 0`).
		Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_tasks_dirty") {
		t.Errorf("query plan %q does not use idx_tasks_dirty", plan)
	}

	var (
		title   string
		dirty   int64
		items   int
		project string
	)
	if err := s.DB().QueryRowContext(ctx,
		`SELECT title, dirty, (SELECT count(*) FROM items WHERE task_id = 't1'), project_id FROM tasks WHERE id = 't1'`).
		Scan(&title, &dirty, &items, &project); err != nil {
		t.Fatal(err)
	}
	if title != "Забрать посылку" || dirty != 1 || items != 1 || project != "p1" {
		t.Errorf("task after the upgrade: title %q dirty %d items %d project %q", title, dirty, items, project)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Pending: 1, Inflight: 1, Failed: 1}) {
		t.Errorf("queue after the upgrade: %+v", counts)
	}
	var attempts int
	var lastError string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT attempts, last_error FROM outbox WHERE op = 'task.update'`).Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || lastError != "boom" {
		t.Errorf("queued entry after the upgrade: attempts %d, last_error %q", attempts, lastError)
	}

	seq, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.update", TaskID: "t2", Rev: 4})
	if err != nil {
		t.Fatalf("enqueue on the upgraded cache: %v", err)
	}
	claimed, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim on the upgraded cache: %v", err)
	}
	var found *OutboxItem
	for i := range claimed {
		if claimed[i].Seq == seq {
			found = &claimed[i]
		}
	}
	if found == nil {
		t.Fatalf("the new entry was not claimed back: %+v", claimed)
	}
	if found.Rev != 4 || found.LeaseToken == "" {
		t.Errorf("claimed %+v, want the revision and a lease token", found)
	}
	if _, err := s.RetryFailed(ctx); err != nil {
		t.Fatalf("retry failed on the upgraded cache: %v", err)
	}
}

func TestMigrateBackfillsTheRevisionOfQueuedEntries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 1)

	seed := []string{
		`INSERT INTO projects (id, name, search) VALUES ('p1', 'Личное', 'личное')`,
		`INSERT INTO tasks (id, project_id, title, search, raw, dirty) VALUES ('t1', 'p1', 'Забрать посылку', 'забрать посылку', '{"id":"t1"}', 1)`,
		`INSERT INTO tasks (id, project_id, title, search, raw, dirty) VALUES ('t2', 'p1', 'Полить цветы', 'полить цветы', '{"id":"t2"}', 0)`,
		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, state) VALUES ('openapi', 'task.update', 't1', 'p1', '{"title":"x"}', 111, 'pending')`,

		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, state) VALUES ('openapi', 'task.move', 't2', 'p1', '{"projectId":"p2"}', 112, 'pending')`,

		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, state) VALUES ('openapi', 'task.delete', 'gone', 'p1', '{}', 113, 'pending')`,
		`INSERT INTO outbox (target, op, payload, created_at, state) VALUES ('v2', 'focus.push', '{"id":"f1"}', 114, 'pending')`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open a cache written by the older tt: %v", err)
	}
	defer s.Close()

	revOf := func(op string) sql.NullInt64 {
		t.Helper()
		var rev sql.NullInt64
		if err := s.DB().QueryRowContext(ctx, `SELECT rev FROM outbox WHERE op = ?`, op).Scan(&rev); err != nil {
			t.Fatalf("rev of %s: %v", op, err)
		}
		return rev
	}
	if got := revOf("task.update"); !got.Valid || got.Int64 != 1 {
		t.Errorf("the entry of a marked task got rev %+v, want the 1 the flag stood for", got)
	}
	for _, op := range []string{"task.move", "task.delete", "focus.push"} {
		if got := revOf(op); got.Valid {
			t.Errorf("%s got rev %d, want none: it has no mark to clear", op, got.Int64)
		}
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var edit *OutboxItem
	for i := range items {
		if items[i].Op == "task.update" {
			edit = &items[i]
		}
	}
	if edit == nil {
		t.Fatalf("the queued edit was not claimed: %+v", items)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		ok, err := MarkPushedTx(ctx, tx, edit.TaskID, edit.Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the mark of a task queued before the upgrade was not cleared")
		}
		return MarkDoneTx(ctx, tx, edit.Seq, edit.LeaseToken)
	}); err != nil {
		t.Fatal(err)
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 't1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 0 {
		t.Errorf("dirty %d once the queued edit was pushed, want a clean row", dirty)
	}
}

func TestLocalItemKeysCameWithASchemaVersionOfTheirOwn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "shopping",
		Items: []model.Item{{Title: "milk"}, {Title: "bread"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var synthesized int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM items WHERE task_id = ? AND id LIKE ?`,
		created.Id, LocalIDPrefix+"%").Scan(&synthesized); err != nil {
		t.Fatal(err)
	}
	if synthesized == 0 {

		t.Fatal("no key of tt's own reached items.id: the format changed and this test did not")
	}

	const beforeLocalItemKeys = 2
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version <= beforeLocalItemKeys {
		t.Fatalf("a cache holding %d key(s) of tt's own is at schema version %d: "+
			"the tt that wrote %d opens it and sends them to the server as ids of its own",
			synthesized, version, beforeLocalItemKeys)
	}

	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	err = applyMigration(ctx, s.DB(), ms[0], beforeLocalItemKeys, s.Path())
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("a tt whose migrations end at %d made of the cache: %v; want a *VersionError", beforeLocalItemKeys, err)
	}
	if ve.Found != version || ve.Supported != beforeLocalItemKeys {
		t.Fatalf("got %+v, want the version on disk against the one that build supports", ve)
	}
}

func TestMigrateRecoversTheChecklistOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 4)

	if tableColumns(t, db, "items")["position"] {
		t.Fatalf("0004 already creates items.position: it was edited after it shipped, " +
			"and a cache written before the edit will never get the column")
	}
	seed := []string{
		`INSERT INTO projects (id, name, search) VALUES ('p1', 'Личное', 'личное')`,
		`INSERT INTO tasks (id, project_id, title, search, raw) VALUES ('t1', 'p1', 'Сборы', 'сборы', '{"id":"t1"}')`,

		`INSERT INTO items (task_id, id, title, sort_order) VALUES ('t1', 'ffffffffffffffffffffffff', 'паспорт', 0)`,
		`INSERT INTO items (task_id, id, title, sort_order) VALUES ('t1', '000000000000000000000001', 'билеты', 0)`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open a cache written by the older tt: %v", err)
	}
	defer s.Close()

	task, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Items) != 2 {
		t.Fatalf("read %d items, want 2", len(task.Items))
	}
	if task.Items[0].Title != "паспорт" || task.Items[1].Title != "билеты" {
		t.Errorf("checklist after the upgrade reads %q, %q; want the order it was written in",
			task.Items[0].Title, task.Items[1].Title)
	}
}
