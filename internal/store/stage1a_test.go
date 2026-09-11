package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestMigrationSixAddsOnlyAnEmptyIdentityRegistry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 5)
	seed := []string{
		`INSERT INTO projects (id, name, search) VALUES ('p1', 'Personal', 'personal')`,
		`INSERT INTO tasks (id, project_id, title, search, raw, dirty) VALUES ('t1', 'p1', 'Task', 'task', '{ "id": "t1", "future": 7 }', 1)`,
		`INSERT INTO items (task_id, id, title, position) VALUES ('t1', 'i1', 'Item', 0)`,
		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, state) VALUES ('openapi', 'task.update', 't1', 'p1', '{ "title": "changed" }', 11, 'pending')`,
		`INSERT INTO undo_log (at, kind, payload) VALUES (12, 'task.update', '{ "op": "task.update", "task_id": "t1" }')`,
		`INSERT INTO events (at, kind, payload) VALUES (13, 'task.updated', '{ "id": "t1" }')`,
	}
	for _, statement := range seed {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	before := readStage1ABytes(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := &Store{db: openAtVersion(t, path, 6), path: path}
	defer store.Close()
	if version, err := store.SchemaVersion(ctx); err != nil || version != 6 {
		t.Fatalf("schema version = %d, %v; want 6", version, err)
	}
	if after := readStage1ABytes(t, store.DB()); !reflect.DeepEqual(after, before) {
		t.Fatalf("migration rewrote historical bytes\nbefore: %#v\nafter:  %#v", before, after)
	}
	var identities int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM item_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if identities != 0 {
		t.Fatalf("migration backfilled %d identity rows", identities)
	}
	if _, ok, err := store.Meta(ctx, "item_identity_epoch"); err != nil || ok {
		t.Fatalf("migration created the lazy epoch: ok=%v err=%v", ok, err)
	}
}

func readStage1ABytes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	ctx := context.Background()
	queries := []string{
		`SELECT raw FROM tasks WHERE id = 't1'`,
		`SELECT payload FROM outbox WHERE task_id = 't1'`,
		`SELECT payload FROM undo_log ORDER BY seq LIMIT 1`,
		`SELECT payload FROM events ORDER BY seq LIMIT 1`,
		`SELECT title FROM items WHERE task_id = 't1' AND id = 'i1'`,
	}
	out := make([]string, 0, len(queries))
	for _, query := range queries {
		var value string
		if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
			t.Fatalf("read fixture bytes with %q: %v", query, err)
		}
		out = append(out, value)
	}
	return out
}

func TestMigrationSixIdentityConstraints(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	if _, err := store.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Task"))); err != nil {
		t.Fatal(err)
	}

	valid := []struct {
		key, serverID, state string
	}{
		{"", "", "recovery_idle"},
		{"key-unbound", "", "unbound"},
		{"key-bound", "server-1", "bound"},
		{"key-uncertain-new", "", "uncertain"},
		{"key-uncertain-old", "server-2", "uncertain"},
		{"key-abandoned-new", "", "abandoned"},
		{"key-abandoned-old", "server-3", "abandoned"},
	}
	for _, row := range valid {
		var serverID any
		if row.serverID != "" {
			serverID = row.serverID
		}
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO item_identities (task_id, item_key, server_id, state) VALUES ('t1', ?, ?, ?)`,
			row.key, serverID, row.state); err != nil {
			t.Errorf("valid row %+v: %v", row, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM item_identities WHERE task_id = 't1' AND item_key = ''`); err != nil {
		t.Fatal(err)
	}
	invalid := []struct {
		key, serverID, state string
	}{
		{"", "server-4", "recovery_idle"},
		{"", "", "bound"},
		{"key", "", "recovery_pending"},
		{"key", "", "bound"},
		{"key", "server-5", "unbound"},
		{"key", "local-server", "bound"},
		{"key", "server-5", "unknown"},
	}
	for _, row := range invalid {
		var serverID any
		if row.serverID != "" {
			serverID = row.serverID
		}
		_, err := store.DB().ExecContext(ctx,
			`INSERT INTO item_identities (task_id, item_key, server_id, state) VALUES ('t1', ?, ?, ?)`,
			row.key, serverID, row.state)
		if err == nil {
			t.Errorf("invalid row %+v was accepted", row)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO item_identities (task_id, item_key, server_id, state) VALUES ('t1', 'other-bound', 'server-1', 'bound')`); err == nil {
		t.Error("two bound keys claimed the same server id")
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO item_identities (task_id, item_key, server_id, state) VALUES ('t1', 'old-server-1', 'server-1', 'abandoned')`); err != nil {
		t.Fatalf("abandoned history incorrectly blocked reuse of a server id: %v", err)
	}
}

func TestItemIdentityFollowsTaskRenameAndDelete(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	if _, err := store.SyncProject(ctx, "p1", fromServer(openTask("local-task", "p1", "Task"))); err != nil {
		t.Fatal(err)
	}
	if err := setItemIdentity(ctx, store.DB(), ItemIdentity{TaskID: "local-task", ItemKey: "key-1", ServerID: "server-1", State: ItemBound}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE tasks SET id = 'server-task' WHERE id = 'local-task'`); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.DB().QueryRowContext(ctx, `SELECT task_id FROM item_identities WHERE item_key = 'key-1'`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if taskID != "server-task" {
		t.Fatalf("identity task id = %q after rename", taskID)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM tasks WHERE id = 'server-task'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM item_identities`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("delete left %d identity rows", count)
	}
}

func TestStableItemKeysSurviveReorderAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	if _, err := store.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Task"))); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []ItemIdentity{
		{TaskID: "t1", ItemKey: "local-key-a", ServerID: "server-a", State: ItemBound},
		{TaskID: "t1", ItemKey: "local-key-b", ServerID: "server-b", State: ItemBound},
	} {
		if err := setItemIdentity(ctx, store.DB(), identity); err != nil {
			t.Fatal(err)
		}
	}
	items := []model.Item{
		{Key: "local-key-a", Id: "stale-a", Title: "same", SortOrder: 1},
		{Key: "local-key-b", Id: "stale-b", Title: "same", SortOrder: 2},
	}
	if err := replaceItems(ctx, store.DB(), "t1", items); err != nil {
		t.Fatal(err)
	}
	items[0], items[1] = items[1], items[0]
	if err := replaceItems(ctx, store.DB(), "t1", items); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Key != "local-key-b" || got.Items[0].Id != "server-b" || got.Items[1].Key != "local-key-a" || got.Items[1].Id != "server-a" {
		t.Fatalf("reopened duplicate-title items = %+v", got.Items)
	}
}

func TestExplicitItemKeysRefuseMissingOrDuplicateValuesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Task")
	task.Items = []model.Item{{Id: "server-old", Title: "old"}}
	if _, err := store.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}
	for _, items := range [][]model.Item{
		{{Key: "key-1", Title: "a"}, {Title: "b"}},
		{{Key: "key-1", Title: "a"}, {Key: "key-1", Title: "b"}},
	} {
		if err := replaceItems(ctx, store.DB(), "t1", items); err == nil {
			t.Fatalf("invalid keys %+v were accepted", items)
		}
		got, err := store.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != 1 || got.Items[0].Title != "old" {
			t.Fatalf("failed replacement changed items: %+v", got.Items)
		}
	}
}

func TestNewItemKeyChecksCurrentRowsAndRetainedHistory(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Task")
	task.Items = []model.Item{{Id: "local-current", Title: "current"}}
	if _, err := store.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}
	if err := setItemIdentity(ctx, store.DB(), ItemIdentity{TaskID: "t1", ItemKey: "local-history", State: ItemAbandoned}); err != nil {
		t.Fatal(err)
	}
	candidates := []string{"local-current", "local-history", "local-fresh"}
	key, err := newItemKeyWith(ctx, store.DB(), "t1", func() (string, error) {
		if len(candidates) == 0 {
			return "", errors.New("exhausted candidates")
		}
		candidate := candidates[0]
		candidates = candidates[1:]
		return candidate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if key != "local-fresh" {
		t.Fatalf("key = %q, want local-fresh", key)
	}
	if _, err := newItemKeyWith(ctx, store.DB(), "t1", func() (string, error) {
		return "server-id", nil
	}); err == nil {
		t.Fatal("non-local generated item key was accepted")
	}
}

func TestControlRegistrationIsSeparateAndDoesNotResetRecovery(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	if _, err := store.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Task"))); err != nil {
		t.Fatal(err)
	}
	if err := ensureItemControl(ctx, store.DB(), "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE item_identities SET state = 'recovery_pending' WHERE task_id = 't1' AND item_key = ''`); err != nil {
		t.Fatal(err)
	}
	if err := ensureItemControl(ctx, store.DB(), "t1"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM item_identities WHERE task_id = 't1' AND item_key = ''`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "recovery_pending" {
		t.Fatalf("control state = %q, want recovery_pending", state)
	}
	identities, err := itemIdentities(ctx, store.DB(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Fatalf("control row appeared in item identities: %+v", identities)
	}
}

func TestVersionSixMakesVersionFiveBinaryRefuse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	store := &Store{db: openAtVersion(t, path, 6), path: path}
	defer store.Close()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	err = applyMigration(ctx, store.DB(), migrations[0], 5, store.Path())
	var versionError *VersionError
	if !errors.As(err, &versionError) || versionError.Found != 6 || versionError.Supported != 5 {
		t.Fatalf("old migration runner error = %#v, want version 6 > 5 refusal", err)
	}
}

func TestFeaturePayloadCodecPreservesKeysAndPrivateMetadata(t *testing.T) {
	items := []model.Item{{Key: "local-a", Title: "same"}, {Key: "local-b", Title: "same", SortOrder: 2}}
	metadata := FeaturePayloadMetadata{
		Version:  FeaturePayloadVersion,
		Fields:   FeatureFields{Items: true},
		ItemKeys: []string{"local-a", "local-b"},
		Phase:    FeaturePrepared,
	}
	payload, err := EncodeTaskPayload(model.Task{Id: "local-task", Title: "Task", Items: items}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"_tt"`) || !strings.Contains(string(payload), `"item_keys"`) {
		t.Fatalf("private metadata missing from payload: %s", payload)
	}
	decoded, gotMetadata, err := DecodeTaskPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotMetadata == nil || !reflect.DeepEqual(gotMetadata.ItemKeys, metadata.ItemKeys) {
		t.Fatalf("metadata = %+v", gotMetadata)
	}
	if decoded.Items[0].Key != "local-a" || decoded.Items[1].Key != "local-b" {
		t.Fatalf("decoded keys = %+v", decoded.Items)
	}
	public, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "local-a") || strings.Contains(string(public), `"_tt"`) {
		t.Fatalf("private metadata reached public model JSON: %s", public)
	}
}

func TestFeatureTaskCodecKeepsATouchedOnlyCreateMask(t *testing.T) {
	task := model.Task{Title: "repeat task", RepeatFlag: "RRULE:FREQ=DAILY"}
	metadata := FeaturePayloadMetadata{
		Version: FeaturePayloadVersion,
		Fields:  FeatureFields{RepeatFlag: true},
		Phase:   FeaturePrepared,
	}
	payload, err := EncodeTaskPayload(task, metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, decoded, err := DecodeTaskPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded == nil || decoded.Fields != metadata.Fields {
		t.Fatalf("decoded mask = %+v, want %+v", decoded, metadata.Fields)
	}

	invalid := task
	invalid.Items = []model.Item{{Key: "local-key", Title: "item"}}
	if _, err := EncodeTaskPayload(invalid, metadata); err == nil {
		t.Fatal("nonempty task items outside the feature mask were accepted")
	}
	clear := model.Task{Title: "clear", Items: []model.Item{}, Reminders: []string{}}
	clearMetadata := FeaturePayloadMetadata{
		Version: FeaturePayloadVersion,
		Fields:  FeatureFields{Items: true, Reminders: true},
		Phase:   FeaturePrepared,
	}
	if _, err := EncodeTaskPayload(clear, clearMetadata); err != nil {
		t.Fatalf("explicit empty task arrays were refused: %v", err)
	}
	clear.Items = nil
	if _, err := EncodeTaskPayload(clear, clearMetadata); err == nil {
		t.Fatal("null marked task items were accepted")
	}
}

func TestFeatureEditCodecValidatesAndPreservesFrozenSnapshot(t *testing.T) {
	items := model.NewEditList([]model.Item{{Key: "local-a", Title: "milk"}})
	emptyBindings := []FeatureItemBinding{}
	snapshotItems := []FeatureItemSnapshot{{Key: "local-a", Title: "milk", Status: 0, SortOrder: 0}}
	repeat := ""
	metadata := FeaturePayloadMetadata{
		Version:  FeaturePayloadVersion,
		Fields:   FeatureFields{Items: true, RepeatFlag: true},
		ItemKeys: []string{"local-a"},
		Phase:    FeatureArmed,
		Snapshot: &FeatureSnapshot{
			Items:         &snapshotItems,
			PriorBindings: &emptyBindings,
			RepeatFlag:    &repeat,
		},
	}
	payload, err := EncodeTaskEditPayload(model.TaskEdit{Items: items, RepeatFlag: &repeat}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	decoded, gotMetadata, err := DecodeTaskEditPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Items == nil || (*decoded.Items)[0].Key != "local-a" || gotMetadata.Snapshot == nil || gotMetadata.Snapshot.RepeatFlag == nil {
		t.Fatalf("decoded edit=%+v metadata=%+v", decoded, gotMetadata)
	}
}

func TestFeatureUndoCodecSurvivesUndoPersistence(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	items := model.NewEditList([]model.Item{{Key: "local-a", Title: "milk"}})
	action := UndoAction{Op: OpTaskUpdate, TaskID: "t1", Before: &model.TaskEdit{Items: items}}
	payload, err := EncodeUndoAction(action, FeatureUndoMetadata{
		Version:  FeaturePayloadVersion,
		Fields:   FeatureFields{Items: true},
		ItemKeys: []string{"local-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (1, ?, ?)`, OpTaskUpdate, payload); err != nil {
		t.Fatal(err)
	}
	entry, err := store.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Action.Before == nil || entry.Action.Before.Items == nil || (*entry.Action.Before.Items)[0].Key != "local-a" {
		t.Fatalf("persisted undo = %+v", entry.Action)
	}
	var stored []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT payload FROM undo_log WHERE seq = ?`, entry.Seq).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	_, metadata, err := DecodeUndoAction(stored)
	if err != nil || metadata == nil || !reflect.DeepEqual(metadata.ItemKeys, []string{"local-a"}) {
		t.Fatalf("stored metadata = %+v, err=%v", metadata, err)
	}
}

func TestFeatureUndoCodecAcceptsAnExplicitEmptyItemSnapshot(t *testing.T) {
	items := model.NewEditList([]model.Item{})
	action := UndoAction{Op: OpTaskUpdate, TaskID: "t1", Before: &model.TaskEdit{Items: items}}
	payload, err := EncodeUndoAction(action, FeatureUndoMetadata{
		Version:  FeaturePayloadVersion,
		Fields:   FeatureFields{Items: true},
		ItemKeys: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, metadata, err := DecodeUndoAction(payload)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil || decoded.Before == nil || decoded.Before.Items == nil || len(*decoded.Before.Items) != 0 {
		t.Fatalf("decoded undo = %+v metadata=%+v", decoded, metadata)
	}
}

func TestFeatureUndoCodecRefusesItemsOutsideItsMask(t *testing.T) {
	repeat := "RRULE:FREQ=DAILY"
	items := model.NewEditList([]model.Item{{Key: "local-key", Title: "item"}})
	action := UndoAction{
		Op:     OpTaskUpdate,
		TaskID: "t1",
		Before: &model.TaskEdit{RepeatFlag: &repeat, Items: items},
	}
	_, err := EncodeUndoAction(action, FeatureUndoMetadata{
		Version: FeaturePayloadVersion,
		Fields:  FeatureFields{RepeatFlag: true},
	})
	if err == nil {
		t.Fatal("unmarked keyed undo items were accepted")
	}
}

func TestFeatureSnapshotRequiresEveryWritableItemField(t *testing.T) {
	incomplete := []byte(`{"items":[{"Title":"desired title"}],"_tt":{"version":1,"fields":{"items":true},"item_keys":["local-key"],"phase":"armed","snapshot":{"items":[{"key":"local-key"}],"prior_bindings":[]}}}`)
	if _, _, err := DecodeTaskEditPayload(incomplete); err == nil {
		t.Fatal("snapshot with omitted writable item fields was accepted")
	}
	complete := []byte(`{"items":[{"Title":"desired title"}],"_tt":{"version":1,"fields":{"items":true},"item_keys":["local-key"],"phase":"armed","snapshot":{"items":[{"key":"local-key","title":"","status":0,"sort_order":0,"start_date":"","is_all_day":false,"time_zone":"","completed_time":""}],"prior_bindings":[]}}}`)
	if _, _, err := DecodeTaskEditPayload(complete); err != nil {
		t.Fatalf("snapshot with explicit zero writable fields was refused: %v", err)
	}
}

func TestFeatureCodecsRefuseMalformedOrUnknownMetadata(t *testing.T) {
	cases := []string{
		`{"title":"x","_tt":null}`,
		`{"title":"x","_tt":{"version":2,"fields":{"repeat_flag":true},"phase":"prepared"}}`,
		`{"title":"x","_tt":{"version":1,"fields":{"repeat_flag":true},"phase":"future"}}`,
		`{"title":"x","_tt":{"version":1,"fields":{"repeat_flag":true},"phase":"armed"}}`,
		`{"title":"x","_tt":{"version":1,"fields":{"repeat_flag":true},"phase":"prepared","future":1}}`,
		`{"items":[{"Title":"a"}],"_tt":{"version":1,"fields":{"items":true},"item_keys":[],"phase":"prepared"}}`,
		`{"repeat_flag":"","items":[],"_tt":{"version":1,"fields":{"repeat_flag":true},"phase":"prepared"}}`,
		`{"_tt":{"version":1,"fields":{"reminders":true},"phase":"prepared"}}`,
		`{"_tt":{"version":1,"fields":{"kind":true},"phase":"prepared"}}`,
	}
	if _, _, err := DecodeTaskEditPayload([]byte(`{"repeat_flag":"","_tt":{"version":1,"fields":{"repeat_flag":true},"phase":"prepared"}}`)); err != nil {
		t.Fatalf("valid repeat-only payload refused: %v", err)
	}
	for _, payload := range cases {
		if _, _, err := DecodeTaskEditPayload([]byte(payload)); err == nil {
			t.Errorf("malformed payload accepted: %s", payload)
		}
	}
	if _, _, err := DecodeUndoAction([]byte(`{"op":"task.update","task_id":"t1","_tt":{"version":9,"fields":{"repeat_flag":true}}}`)); err == nil {
		t.Error("unknown undo metadata version was accepted")
	}
}

func TestLegacyFeatureCodecDecodeKeepsOldPayloadSemantics(t *testing.T) {
	legacy := []byte(`{"title":"x","items":null}`)
	edit, metadata, err := DecodeTaskEditPayload(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if metadata != nil || edit.Title == nil || *edit.Title != "x" || edit.Items != nil {
		t.Fatalf("legacy decode = %+v metadata=%+v", edit, metadata)
	}
}
