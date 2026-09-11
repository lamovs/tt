package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

type stage1BState struct {
	Task     model.Task
	Dirty    int64
	Modified string
	Outbox   int
	Events   int
	Undo     int
	Registry int
	Epoch    string
}

func stage1BStateOf(t *testing.T, store *Store, taskID string) stage1BState {
	t.Helper()
	ctx := context.Background()
	var state stage1BState
	var err error
	state.Task, err = store.Task(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT dirty, ifnull(modified_time, '') FROM tasks WHERE id = ?`, taskID).Scan(&state.Dirty, &state.Modified); err != nil {
		t.Fatal(err)
	}
	for query, destination := range map[string]*int{
		`SELECT count(*) FROM outbox`:          &state.Outbox,
		`SELECT count(*) FROM events`:          &state.Events,
		`SELECT count(*) FROM undo_log`:        &state.Undo,
		`SELECT count(*) FROM item_identities`: &state.Registry,
	} {
		if err := store.DB().QueryRowContext(ctx, query).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	state.Epoch, _, err = store.Meta(ctx, "item_identity_epoch")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func stage1BRawTask(t *testing.T, task model.Task, raw json.RawMessage) ServerTask {
	t.Helper()
	return ServerTask{Task: task, Raw: raw}
}

func TestStage1BCreateCompletionAndAtomicUndoKeepKeys(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "Trip", Kind: "TEXT",
		Items:      []model.Item{{Title: "passport"}, {Title: "charger"}},
		RepeatFlag: "RRULE:FREQ=WEEKLY;INTERVAL=1",
		Reminders:  []string{"TRIGGER:PT0S"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != "CHECKLIST" || len(created.Items) != 2 {
		t.Fatalf("created task = %+v", created)
	}
	for i, item := range created.Items {
		if !IsLocalID(item.Key) || item.Id != "" || item.SortOrder != int64(i+1) {
			t.Fatalf("created item %d = %+v", i+1, item)
		}
	}
	var createPayload []byte
	if err := store.DB().QueryRowContext(ctx,
		`SELECT payload FROM outbox WHERE op = ? ORDER BY seq LIMIT 1`, OpTaskCreate).Scan(&createPayload); err != nil {
		t.Fatal(err)
	}
	decoded, metadata, err := DecodeTaskPayload(createPayload)
	if err != nil || metadata == nil || !metadata.Fields.Items || !metadata.Fields.Kind {
		t.Fatalf("create payload metadata = %+v, task = %+v, err = %v", metadata, decoded, err)
	}
	if decoded.Items[0].Key != created.Items[0].Key || metadata.ItemKeys[1] != created.Items[1].Key {
		t.Fatalf("create keys = %+v / %+v", decoded.Items, metadata.ItemKeys)
	}
	before := created
	completed, err := store.CompleteTask(ctx, created.Id, CompleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range completed.Items {
		if !item.Status.Done() || item.CompletedTime.IsZero() {
			t.Fatalf("completed item = %+v", item)
		}
	}
	top, err := store.LastUndo(ctx)
	if err != nil || top.Feature == nil || !top.Feature.Fields.Items {
		t.Fatalf("completion undo = %+v, err = %v", top, err)
	}
	if _, err := store.ApplyUndo(ctx, top); err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	got, err := store.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if !sameItems(got.Items, before.Items) || got.Status != before.Status {
		t.Fatalf("after undo and reopen = %+v, want %+v", got, before)
	}
	if latest, err := store.LastUndo(ctx); err != nil || latest.Action.Op != OpTaskCreate {
		t.Fatalf("stack after atomic undo = %+v, %v", latest, err)
	}
}

func TestStage1BFeatureNoopsWriteNothing(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Trip", Items: []model.Item{{Title: "passport"}}})
	if err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, store, created.Id)
	if _, err := store.RenameTaskItem(ctx, created.Id, 1, "passport"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MoveTaskItem(ctx, created.Id, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskItemDone(ctx, created.Id, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskRepeat(ctx, created.Id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskReminders(ctx, created.Id, nil); err != nil {
		t.Fatal(err)
	}
	after := stage1BStateOf(t, store, created.Id)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("no-op changed state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestStage1BControlRegistrationIsIndependentFromItemAdoption(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Items = []model.Item{{Id: "i1", Title: "legacy"}}
	raw := json.RawMessage(`{"id":"t1","items":[{"id":"i1","title":"legacy","status":0,"future":true}]}`)
	if _, err := store.SyncProject(ctx, "p1", []ServerTask{stage1BRawTask(t, task, raw)}); err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, store, "t1")
	if _, err := store.SetTaskRepeat(ctx, "t1", ""); err != nil {
		t.Fatal(err)
	}
	if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("repeat no-op registered control: %+v -> %+v", before, after)
	}
	if _, err := store.SetTaskRepeat(ctx, "t1", "RRULE:FREQ=DAILY;INTERVAL=1"); err != nil {
		t.Fatal(err)
	}
	var control, itemRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM item_identities WHERE task_id = 't1' AND item_key = ''`).Scan(&control); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM item_identities WHERE task_id = 't1' AND item_key <> ''`).Scan(&itemRows); err != nil {
		t.Fatal(err)
	}
	if control != 1 || itemRows != 0 {
		t.Fatalf("control/item registration = %d/%d", control, itemRows)
	}
	stable := stage1BStateOf(t, store, "t1")
	if _, err := store.RenameTaskItem(ctx, "t1", 1, "changed"); !errors.Is(err, ErrUnsafeChecklist) {
		t.Fatalf("item edit over unsupported raw = %v", err)
	}
	if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, stable) {
		t.Fatalf("refusal changed state:\nbefore: %+v\nafter:  %+v", stable, after)
	}
}

func TestStage1BRawRefusalsAreAtomicAndPartialFeaturesStillWork(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"missing", `{"id":"t1"}`},
		{"duplicate ids", `{"items":[{"id":"i1","title":"a","status":0},{"id":"i1","title":"b","status":0}]}`},
		{"unknown field", `{"items":[{"id":"i1","title":"a","status":0,"future":1},{"id":"i2","title":"b","status":0}]}`},
		{"wrong type", `{"items":[{"id":"i1","title":"a","status":"0"},{"id":"i2","title":"b","status":0}]}`},
		{"unknown status", `{"items":[{"id":"i1","title":"a","status":2},{"id":"i2","title":"b","status":0}]}`},
		{"invalid date", `{"items":[{"id":"i1","title":"a","status":0,"startDate":"tomorrow"},{"id":"i2","title":"b","status":0}]}`},
		{"null boolean", `{"items":[{"id":"i1","title":"a","status":0,"isAllDay":null},{"id":"i2","title":"b","status":0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
			task := openTask("t1", "p1", "Imported")
			task.Items = []model.Item{{Id: "i1", Title: "a"}, {Id: "i2", Title: "b"}}
			if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: json.RawMessage(tc.raw)}}); err != nil {
				t.Fatal(err)
			}
			before := stage1BStateOf(t, store, "t1")
			if _, err := store.RenameTaskItem(ctx, "t1", 1, "changed"); !errors.Is(err, ErrUnsafeChecklist) {
				t.Fatalf("item edit = %v", err)
			}
			if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed state: %+v -> %+v", before, after)
			}
			if _, err := store.SetTaskReminders(ctx, "t1", []string{"TRIGGER:PT0S"}); err != nil {
				t.Fatalf("unrelated partial feature edit: %v", err)
			}
		})
	}
}

func TestStage1BRawBooleanAbsentAndFalseRemainLossless(t *testing.T) {
	for name, raw := range map[string]string{
		"absent": `{"items":[{"id":"i1","title":"a","status":0}]}`,
		"false":  `{"items":[{"id":"i1","title":"a","status":0,"isAllDay":false}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
			task := openTask("t1", "p1", "Imported")
			task.Items = []model.Item{{Id: "i1", Title: "a"}}
			if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: json.RawMessage(raw)}}); err != nil {
				t.Fatal(err)
			}
			got, err := store.RenameTaskItem(ctx, "t1", 1, "changed")
			if err != nil || got.Items[0].Title != "changed" || got.Items[0].Key != "i1" {
				t.Fatalf("valid boolean representation: task=%+v, err=%v", got, err)
			}
		})
	}
}

func TestStage1BRepeatedOfflineEditsUseRegistryProvenance(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Kind = "CHECKLIST"
	task.Items = []model.Item{{Id: "i1", Title: "old", SortOrder: 9}}
	raw := json.RawMessage(`{"id":"t1","items":[{"id":"i1","title":"old","status":0,"sortOrder":9,"startDate":"","isAllDay":false,"timeZone":"","completedTime":""}]}`)
	if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.RenameTaskItem(ctx, "t1", 1, "first local")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RenameTaskItem(ctx, "t1", 1, "second local")
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.AddTaskItem(ctx, "t1", "new local")
	if err != nil {
		t.Fatal(err)
	}
	if first.Items[0].Key != "i1" || second.Items[0].Key != "i1" || third.Items[0].Key != "i1" || !IsLocalID(third.Items[1].Key) {
		t.Fatalf("keys across repeated edits = %+v / %+v / %+v", first.Items, second.Items, third.Items)
	}
	var keptRaw []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = 't1'`).Scan(&keptRaw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keptRaw, []byte(raw)) {
		t.Fatalf("local edits rewrote raw: %s", keptRaw)
	}
}

func TestStage1BUnknownRawStatusRefusesModeledNoop(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Items = []model.Item{{Id: "i1", Title: "a", Status: model.ItemDone}}
	raw := json.RawMessage(`{"items":[{"id":"i1","title":"a","status":7}]}`)
	if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, store, "t1")
	if _, err := store.SetTaskItemDone(ctx, "t1", 1, true); !errors.Is(err, ErrUnsafeChecklist) {
		t.Fatalf("modeled no-op over unknown raw status = %v", err)
	}
	if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused modeled no-op changed state: %+v -> %+v", before, after)
	}
}

func TestStage1BRawNullBooleanRefusesModeledNoop(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Items = []model.Item{{Id: "i1", Title: "a"}}
	raw := json.RawMessage(`{"items":[{"id":"i1","title":"a","status":0,"isAllDay":null}]}`)
	if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, store, "t1")
	if _, err := store.RenameTaskItem(ctx, "t1", 1, "a"); !errors.Is(err, ErrUnsafeChecklist) {
		t.Fatalf("modeled no-op over null boolean = %v", err)
	}
	if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused modeled no-op changed state: %+v -> %+v", before, after)
	}
}

func TestStage1BLegacyKnownIDUndoAdoptsExactKey(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Kind = "CHECKLIST"
	task.Items = []model.Item{{Id: "i1", Title: "current"}}
	raw := json.RawMessage(`{"items":[{"id":"i1","title":"current","status":0}]}`)
	if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	before := model.TaskEdit{Items: model.NewEditList([]model.Item{{Id: "i1", Title: "original"}})}
	if err := store.Tx(ctx, func(tx *sql.Tx) error {
		return pushUndo(ctx, tx, UndoAction{Op: OpTaskUpdate, TaskID: "t1", ProjectID: "p1", Before: &before})
	}); err != nil {
		t.Fatal(err)
	}
	top, err := store.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ApplyUndo(ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Key != "i1" || got.Items[0].Title != "original" {
		t.Fatalf("legacy undo restored %+v", got.Items)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state FROM item_identities WHERE task_id = 't1' AND item_key = 'i1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(ItemBound) {
		t.Fatalf("adopted identity state = %q", state)
	}
}

func TestStage1BLegacyItemUndoRefusalsKeepWholeState(t *testing.T) {
	valid := `"Id":"i1","Title":"original","Status":"open","SortOrder":0,"StartDate":null,"IsAllDay":false,"TimeZone":"","CompletedTime":null`
	missing := `"Id":"i2","Title":"original","Status":"open","SortOrder":0,"StartDate":null,"IsAllDay":false,"TimeZone":"","CompletedTime":null`
	cases := map[string]string{
		"duplicate id":         `[{` + valid + `},{` + valid + `}]`,
		"unknown field":        `[{` + valid + `,"Future":1}]`,
		"null boolean":         `[{"Id":"i1","Title":"original","Status":"open","SortOrder":0,"StartDate":null,"IsAllDay":null,"TimeZone":"","CompletedTime":null}]`,
		"invalid date":         `[{"Id":"i1","Title":"original","Status":"open","SortOrder":0,"StartDate":"tomorrow","IsAllDay":false,"TimeZone":"","CompletedTime":null}]`,
		"missing provenance":   `[{` + missing + `}]`,
		"abandoned provenance": `[{` + valid + `}]`,
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
			task := openTask("t1", "p1", "Imported")
			task.Kind = "CHECKLIST"
			task.Items = []model.Item{{Id: "i1", Title: "current"}}
			raw := json.RawMessage(`{"items":[{"id":"i1","title":"current","status":0}]}`)
			if _, err := store.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing provenance":
				if err := setItemIdentity(ctx, store.DB(), ItemIdentity{
					TaskID: "t1", ItemKey: "i1", ServerID: "i1", State: ItemBound,
				}); err != nil {
					t.Fatal(err)
				}
			case "abandoned provenance":
				if err := store.Tx(ctx, func(tx *sql.Tx) error {
					if err := setItemIdentity(ctx, tx, ItemIdentity{
						TaskID: "t1", ItemKey: "i1", ServerID: "i1", State: ItemAbandoned,
					}); err != nil {
						return err
					}
					key, err := newItemKey(ctx, tx, "t1")
					if err != nil {
						return err
					}
					if err := setItemIdentity(ctx, tx, ItemIdentity{
						TaskID: "t1", ItemKey: key, ServerID: "i1", State: ItemBound,
					}); err != nil {
						return err
					}
					return replaceItems(ctx, tx, "t1", []model.Item{{Key: key, Id: "i1", Title: "current"}})
				}); err != nil {
					t.Fatal(err)
				}
			}
			payload := `{"op":"task.update","task_id":"t1","project_id":"p1","before":{"items":` + items + `}}`
			result, err := store.DB().ExecContext(ctx,
				`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`, time.Now().Unix(), OpTaskUpdate, payload)
			if err != nil {
				t.Fatal(err)
			}
			seq, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			before := stage1BStateOf(t, store, "t1")
			if _, err := store.ApplyUndo(ctx, UndoEntry{Seq: seq}); err == nil {
				t.Fatal("degraded legacy undo succeeded")
			}
			if after := stage1BStateOf(t, store, "t1"); !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed state: %+v -> %+v", before, after)
			}
			var kept int
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM undo_log WHERE seq = ?`, seq).Scan(&kept); err != nil || kept != 1 {
				t.Fatalf("undo entry retained = %d, err=%v", kept, err)
			}
		})
	}
}

func TestStage1BDeletedLocalParentGetsFreshIdentity(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "Local", Items: []model.Item{{Title: "a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	top, err := store.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.ApplyUndo(ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Id == created.Id || !IsLocalID(restored.Id) {
		t.Fatalf("restored parent id = %q, deleted %q", restored.Id, created.Id)
	}
	if len(restored.Items) != 1 || restored.Items[0].Key == created.Items[0].Key ||
		!IsLocalID(restored.Items[0].Key) || restored.Items[0].Id != "" {
		t.Fatalf("restored items = %+v, deleted %+v", restored.Items, created.Items)
	}
}

func TestStage1BUncertainKeysAllowLaterLocalEditAndUndo(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Offline", Items: []model.Item{{Title: "old"}}})
	if err != nil {
		t.Fatal(err)
	}
	key := created.Items[0].Key
	if err := setItemIdentity(ctx, store.DB(), ItemIdentity{
		TaskID: created.Id, ItemKey: key, ServerID: "server-item", State: ItemUncertain,
	}); err != nil {
		t.Fatal(err)
	}
	var firstPayload []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT payload FROM outbox ORDER BY seq LIMIT 1`).Scan(&firstPayload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenameTaskItem(ctx, created.Id, 1, "newer"); err != nil {
		t.Fatalf("edit over uncertain key: %v", err)
	}
	top, err := store.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyUndo(ctx, top); err != nil {
		t.Fatalf("undo over uncertain key: %v", err)
	}
	var state, serverID string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state, server_id FROM item_identities WHERE task_id = ? AND item_key = ?`, created.Id, key).Scan(&state, &serverID); err != nil {
		t.Fatal(err)
	}
	if state != string(ItemUncertain) || serverID != "server-item" {
		t.Fatalf("uncertain binding changed to %q/%q", state, serverID)
	}
	var keptPayload []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT payload FROM outbox ORDER BY seq LIMIT 1`).Scan(&keptPayload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keptPayload, firstPayload) {
		t.Fatalf("later local work changed earlier payload:\n%s\n%s", firstPayload, keptPayload)
	}
	var updates int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE task_id = ? AND op = ?`, created.Id, OpTaskUpdate).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("queued updates = %d, want edit and undo", updates)
	}
	got, err := store.Task(ctx, created.Id)
	if err != nil || got.Items[0].Title != "old" || got.Items[0].Key != key {
		t.Fatalf("undo restored %+v, %v", got.Items, err)
	}
}

func TestStage1BParentMoveCreatesFreshItemKeysAndKeepsRanks(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store,
		model.Project{Id: "p1", Name: "Personal"},
		model.Project{Id: "p2", Name: "Work"})
	created, err := store.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "Move", Items: []model.Item{{Title: "a"}, {Title: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := append([]model.Item(nil), created.Items...)
	items[0].SortOrder, items[1].SortOrder = 41, 99
	created, err = store.UpdateTask(ctx, created.Id, model.TaskEdit{Items: model.NewEditList(items)})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.MoveTask(ctx, created.Id, "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range moved.Items {
		if moved.Items[i].Id != "" || moved.Items[i].Key == created.Items[i].Key ||
			!IsLocalID(moved.Items[i].Key) || moved.Items[i].SortOrder != created.Items[i].SortOrder {
			t.Fatalf("moved item %d = %+v, source %+v", i+1, moved.Items[i], created.Items[i])
		}
	}
}

func TestStage1BRanksOverflowMoveAndUndoExactly(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Ranks", Items: []model.Item{{Title: "a"}, {Title: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	items := append([]model.Item(nil), created.Items...)
	items[0].SortOrder = 17
	items[1].SortOrder = math.MaxInt64
	if _, err := store.UpdateTask(ctx, created.Id, model.TaskEdit{Items: model.NewEditList(items)}); err != nil {
		t.Fatal(err)
	}
	added, err := store.AddTaskItem(ctx, created.Id, "c")
	if err != nil {
		t.Fatal(err)
	}
	if got := []int64{added.Items[0].SortOrder, added.Items[1].SortOrder, added.Items[2].SortOrder}; !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("overflow ranks = %v", got)
	}
	moved, err := store.MoveTaskItem(ctx, created.Id, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if titles := []string{moved.Items[0].Title, moved.Items[1].Title, moved.Items[2].Title}; !reflect.DeepEqual(titles, []string{"b", "c", "a"}) {
		t.Fatalf("move order = %v", titles)
	}
	top, _ := store.LastUndo(ctx)
	if _, err := store.ApplyUndo(ctx, top); err != nil {
		t.Fatal(err)
	}
	restored, _ := store.Task(ctx, created.Id)
	if !sameItems(restored.Items, added.Items) {
		t.Fatalf("undo ranks/items = %+v, want %+v", restored.Items, added.Items)
	}
}

func TestStage1BConcurrentWritersSerializeFeatureMutations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	seedProjects(t, first, model.Project{Id: "p1", Name: "Personal"})
	created, err := first.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Concurrent", Items: []model.Item{}})
	if err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- first.Tx(ctx, func(tx *sql.Tx) error {
			_, err := editTaskTx(ctx, tx, created.Id, OpTaskUpdate, TaskUpdatedKind,
				func(ctx context.Context, tx *sql.Tx, cur model.Task) (model.TaskEdit, []string, error) {
					key, err := newItemKey(ctx, tx, cur.Id)
					if err != nil {
						return model.TaskEdit{}, nil, err
					}
					items := append([]model.Item(nil), cur.Items...)
					items = append(items, model.Item{Key: key, Title: "first", SortOrder: 1})
					return model.TaskEdit{Items: model.NewEditList(items)}, []string{key}, nil
				}, true)
			if err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	started := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(started)
		_, err := second.AddTaskItem(ctx, created.Id, "second")
		secondDone <- err
	}()
	<-started
	select {
	case err := <-secondDone:
		t.Fatalf("second writer passed the held BEGIN IMMEDIATE transaction: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	got, err := first.Task(ctx, created.Id)
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("concurrent additions = %+v, %v", got.Items, err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{first, second} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, err := store.SetTaskItemDone(ctx, created.Id, 1, true)
			errs <- err
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var statusMutations int
	if err := first.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE task_id = ? AND op = ?`, created.Id, OpTaskUpdate).Scan(&statusMutations); err != nil {
		t.Fatal(err)
	}

	if statusMutations != 3 {
		t.Fatalf("update mutations = %d, want 3", statusMutations)
	}
}

func TestStage1BFeatureMutationRollsBackEveryStoreEffect(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedProjects(t, store, model.Project{Id: "p1", Name: "Personal"})
	created, err := store.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Atomic"})
	if err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, store, created.Id)
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE outbox RENAME TO outbox_hidden`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskRepeat(ctx, created.Id, "RRULE:FREQ=DAILY;INTERVAL=1"); err == nil {
		t.Fatal("feature mutation succeeded without an outbox table")
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE outbox_hidden RENAME TO outbox`); err != nil {
		t.Fatal(err)
	}
	after := stage1BStateOf(t, store, created.Id)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rollback left partial state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}
