package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const (
	stage1CProjectID = "stage1c-project"
	stage1CTaskID    = "stage1c-task"
)

func stage1CHolderItem(id, title string, rank int64) map[string]any {
	return map[string]any{
		"id": id, "title": title, "status": 0, "sortOrder": rank,
		"startDate": "", "isAllDay": false, "timeZone": "", "completedTime": "",
	}
}

func stage1CHolderTask(taskID, title string, items []map[string]any) map[string]any {
	body := map[string]any{
		"id": taskID, "projectId": stage1CProjectID, "title": title,
		"status": 0, "kind": "CHECKLIST", "items": items,
	}
	return body
}

func stage1COpenStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func stage1CSeedFromServer(t *testing.T, st *store.Store, fake *stage1CWireFake, raw map[string]any) *Syncer {
	t.Helper()
	fake.SeedProject(stage1CProjectID, "Stage 1C")
	fake.SeedTask(stage1CProjectID, raw)
	sy := testSyncer(t, st, fake.Server())
	res, err := sy.Pull(context.Background())
	if err != nil {
		t.Fatalf("initial Pull: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("initial Pull = %+v, want one task", res)
	}
	return sy
}

func stage1CTask(t *testing.T, st *store.Store, taskID string) model.Task {
	t.Helper()
	task, err := st.Task(context.Background(), taskID)
	if err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return task
}

func stage1CRaw(t *testing.T, st *store.Store, taskID string) string {
	t.Helper()
	var raw string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT raw FROM tasks WHERE id = ?`, taskID).Scan(&raw); err != nil {
		t.Fatalf("read raw of %s: %v", taskID, err)
	}
	return raw
}

func stage1CEpoch(t *testing.T, st *store.Store) int64 {
	t.Helper()
	value, ok, err := st.Meta(context.Background(), "item_identity_epoch")
	if err != nil || !ok {
		t.Fatalf("read item identity epoch: value=%q ok=%v err=%v", value, ok, err)
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse item identity epoch %q: %v", value, err)
	}
	return epoch
}

func stage1CRegistry(t *testing.T, st *store.Store, taskID string) [][3]string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(),
		`SELECT item_key, ifnull(server_id, ''), state
		   FROM item_identities WHERE task_id = ? ORDER BY item_key`, taskID)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var row [3]string
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatalf("scan registry: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("finish registry: %v", err)
	}
	return out
}

func stage1CControl(t *testing.T, st *store.Store, taskID string) string {
	t.Helper()
	var state string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT state FROM item_identities WHERE task_id = ? AND item_key = ''`, taskID).Scan(&state); err != nil {
		t.Fatalf("read recovery control: %v", err)
	}
	return state
}

func stage1COutboxCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM outbox`).Scan(&count); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return count
}

func stage1CForceNextConfirmationMismatch(fake *stage1CWireFake) {
	var used atomic.Bool
	fake.SetResponseHook(func(req stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
		if req.Method != http.MethodGet || req.Path != "/open/v1/project/"+stage1CProjectID+"/task/"+stage1CTaskID || !used.CompareAndSwap(false, true) {
			return response
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body, &body); err != nil {
			return stage1CWireRawResponse(http.StatusInternalServerError, []byte(`{"error":"fixture decode"}`))
		}
		items, _ := body["items"].([]any)
		if len(items) > 0 {
			items[0].(map[string]any)["title"] = "confirmation did not match"
		}
		return stage1CWireJSONResponse(http.StatusOK, body)
	})
}

func stage1CAmbiguousDiscard(t *testing.T, st *store.Store, sy *Syncer, fake *stage1CWireFake) (store.UndoEntry, int64) {
	t.Helper()
	ctx := context.Background()
	remoteBefore, ok := fake.Task(stage1CProjectID, stage1CTaskID)
	if !ok {
		t.Fatal("remote task disappeared before ambiguous update")
	}
	itemsBefore, ok := remoteBefore["items"].([]any)
	if !ok {
		t.Fatalf("remote task has no complete item array before ambiguous update: %#v", remoteBefore)
	}
	if _, err := st.AddTaskItem(ctx, stage1CTaskID, "allocated while the answer is lost"); err != nil {
		t.Fatalf("queue allocating item update: %v", err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil || undo.Feature == nil || !undo.Feature.Fields.Items {
		t.Fatalf("pre-discard undo = %+v, err=%v; want keyed version 1 undo", undo, err)
	}
	stage1CForceNextConfirmationMismatch(fake)
	res, err := sy.Push(ctx)
	if err != nil {
		t.Fatalf("Push ambiguous item update: %v", err)
	}
	if res.Failed != 1 || stage1COutboxCount(t, st) != 1 {
		t.Fatalf("ambiguous Push = %+v, outbox=%d; want one parked entry", res, stage1COutboxCount(t, st))
	}
	remote, ok := fake.Task(stage1CProjectID, stage1CTaskID)
	remoteItems, itemsOK := remote["items"].([]any)
	if !ok || !itemsOK || len(remoteItems) != len(itemsBefore)+1 || remoteItems[len(remoteItems)-1].(map[string]any)["id"] == "" {
		t.Fatalf("ambiguous POST did not allocate and retain the remote item: %#v", remote)
	}
	legacyPayload := fmt.Sprintf(`{"op":%q,"task_id":%q,"project_id":%q,"before":{"items":[{"Id":"remote-a","Title":"alpha","Status":"open","SortOrder":1}]}}`,
		store.OpTaskUpdate, stage1CTaskID, stage1CProjectID)
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO undo_log(at, kind, payload) VALUES (?, ?, ?)`, time.Now().Unix(), store.OpTaskUpdate, legacyPayload); err != nil {
		t.Fatalf("insert pre-discard ID-only undo fixture: %v", err)
	}
	var legacySeq int64
	if err := st.DB().QueryRowContext(ctx, `SELECT max(seq) FROM undo_log`).Scan(&legacySeq); err != nil {
		t.Fatal(err)
	}
	fake.SetResponseHook(nil)
	dropped, err := st.DropParkedConfirmed(ctx, func(int) error { return nil })
	if err != nil || len(dropped) != 1 {
		t.Fatalf("DropParkedConfirmed = %+v, %v; want one explicit discard", dropped, err)
	}
	if state := stage1CControl(t, st, stage1CTaskID); state != string(store.RecoveryPending) {
		t.Fatalf("control after discard = %q, want recovery_pending", state)
	}
	if count := stage1COutboxCount(t, st); count != 0 {
		t.Fatalf("outbox after discard = %d, want empty", count)
	}
	return undo, legacySeq
}

func stage1CAdopt(t *testing.T, st *store.Store, sy *Syncer) model.Task {
	t.Helper()
	res, err := sy.Pull(context.Background())
	if err != nil {
		t.Fatalf("recovery Pull: %v", err)
	}
	if res.Pulled != 1 {
		t.Fatalf("recovery Pull = %+v, want adopted task", res)
	}
	if state := stage1CControl(t, st, stage1CTaskID); state != string(store.RecoveryIdle) {
		t.Fatalf("control after adoption = %q, want recovery_idle", state)
	}
	return stage1CTask(t, st, stage1CTaskID)
}

func stage1CKeys(task model.Task) []string {
	keys := make([]string, len(task.Items))
	for i := range task.Items {
		keys[i] = task.Items[i].Key
	}
	return keys
}

func TestStage1CRC1ExplicitDiscardRecoveryLifecycle(t *testing.T) {
	t.Run("adopt edit pull restart and undo", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "cache.db")
		st := stage1COpenStore(t, path)
		fake := newStage1CWireFake(t)
		sy := stage1CSeedFromServer(t, st, fake,
			stage1CHolderTask(stage1CTaskID, "recovery", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)}))

		oldKeyedUndo, oldLegacySeq := stage1CAmbiguousDiscard(t, st, sy, fake)
		oldKeys := stage1CKeys(stage1CTask(t, st, stage1CTaskID))
		for _, row := range stage1CRegistry(t, st, stage1CTaskID) {
			if row[0] != "" && row[2] != string(store.ItemAbandoned) {
				t.Fatalf("registry after discard = %+v; old key %q was not abandoned", stage1CRegistry(t, st, stage1CTaskID), row[0])
			}
		}

		adopted := stage1CAdopt(t, st, sy)
		if len(adopted.Items) != 2 {
			t.Fatalf("adopted items = %+v, want complete remote snapshot", adopted.Items)
		}
		for _, key := range stage1CKeys(adopted) {
			for _, oldKey := range oldKeys {
				if key == oldKey {
					t.Fatalf("adoption reused pre-discard key %q", key)
				}
			}
		}

		beforeEdit := adopted
		if _, err := st.RenameTaskItem(ctx, stage1CTaskID, 1, "alpha after recovery"); err != nil {
			t.Fatalf("queue post-recovery edit: %v", err)
		}
		currentUndo, err := st.LastUndo(ctx)
		if err != nil || currentUndo.Feature == nil || currentUndo.Seq <= oldLegacySeq {
			t.Fatalf("current v1 undo = %+v, err=%v", currentUndo, err)
		}
		if res, err := sy.Push(ctx); err != nil || res.Pushed != 1 || stage1COutboxCount(t, st) != 0 {
			t.Fatalf("confirmed post-recovery Push = %+v, %v", res, err)
		}

		stableKeys := stage1CKeys(stage1CTask(t, st, stage1CTaskID))
		stableRegistry := stage1CRegistry(t, st, stage1CTaskID)
		for pass := 1; pass <= 2; pass++ {
			if _, err := sy.Pull(ctx); err != nil {
				t.Fatalf("ordinary Pull %d: %v", pass, err)
			}
			if got := stage1CKeys(stage1CTask(t, st, stage1CTaskID)); !reflect.DeepEqual(got, stableKeys) {
				t.Fatalf("ordinary Pull %d changed adopted keys: %v -> %v", pass, stableKeys, got)
			}
			if got := stage1CRegistry(t, st, stage1CTaskID); !reflect.DeepEqual(got, stableRegistry) {
				t.Fatalf("ordinary Pull %d changed registry: %v -> %v", pass, stableRegistry, got)
			}
		}

		if err := st.Close(); err != nil {
			t.Fatalf("close recovered cache: %v", err)
		}
		st = stage1COpenStore(t, path)
		t.Cleanup(func() { st.Close() })
		gotUndo, err := st.LastUndo(ctx)
		if err != nil || gotUndo.Seq != currentUndo.Seq {
			t.Fatalf("v1 undo after reopen = %+v, %v", gotUndo, err)
		}
		undone, err := st.ApplyUndo(ctx, gotUndo)
		if err != nil || !reflect.DeepEqual(undone.Items, beforeEdit.Items) {
			t.Fatalf("apply current v1 undo = %+v, %v; want %+v", undone.Items, err, beforeEdit.Items)
		}

		legacy, err := st.LastUndo(ctx)
		if err != nil || legacy.Seq != oldLegacySeq {
			t.Fatalf("old ID-only undo = %+v, %v", legacy, err)
		}
		beforeRefusal := stage1CHolderState(t, st)
		if _, err := st.ApplyUndo(ctx, legacy); !errors.Is(err, store.ErrUnsafeChecklist) {
			t.Fatalf("old ID-only undo = %v, want unsafe-checklist refusal", err)
		}
		if after := stage1CHolderState(t, st); after != beforeRefusal {
			t.Fatalf("ID-only undo refusal changed state\nbefore: %s\nafter:  %s", beforeRefusal, after)
		}
		if _, err := st.DB().ExecContext(ctx, `DELETE FROM undo_log WHERE seq = ?`, oldLegacySeq); err != nil {
			t.Fatalf("remove synthetic ID-only fixture: %v", err)
		}
		keyed, err := st.LastUndo(ctx)
		if err != nil || keyed.Seq != oldKeyedUndo.Seq {
			t.Fatalf("old keyed undo = %+v, %v", keyed, err)
		}
		beforeRefusal = stage1CHolderState(t, st)
		if _, err := st.ApplyUndo(ctx, keyed); !errors.Is(err, store.ErrUnsafeChecklist) {
			t.Fatalf("old keyed undo = %v, want unsafe-checklist refusal", err)
		}
		if after := stage1CHolderState(t, st); after != beforeRefusal {
			t.Fatalf("keyed undo refusal changed state\nbefore: %s\nafter:  %s", beforeRefusal, after)
		}
	})

	t.Run("empty adoption and rollback retain authorization", func(t *testing.T) {
		ctx := context.Background()
		st := stage1COpenStore(t, filepath.Join(t.TempDir(), "cache.db"))
		t.Cleanup(func() { st.Close() })
		fake := newStage1CWireFake(t)
		sy := stage1CSeedFromServer(t, st, fake,
			stage1CHolderTask(stage1CTaskID, "empty recovery", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)}))
		_, _ = stage1CAmbiguousDiscard(t, st, sy, fake)
		fake.SetTask(stage1CProjectID, stage1CTaskID,
			stage1CHolderTask(stage1CTaskID, "empty recovery", []map[string]any{}))

		before := stage1CHolderState(t, st)
		if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER stage1c_abort_adoption
			BEFORE UPDATE ON tasks WHEN NEW.id = '`+stage1CTaskID+`'
			BEGIN SELECT RAISE(ABORT, 'stage1c adoption rollback'); END`); err != nil {
			t.Fatalf("install adoption rollback trigger: %v", err)
		}
		if _, err := sy.Pull(ctx); err == nil {
			t.Fatal("recovery Pull succeeded through forced late transaction failure")
		}
		if after := stage1CHolderState(t, st); after != before {
			t.Fatalf("failed adoption consumed or changed state\nbefore: %s\nafter:  %s", before, after)
		}
		if state := stage1CControl(t, st, stage1CTaskID); state != string(store.RecoveryPending) {
			t.Fatalf("failed adoption control = %q, want recovery_pending", state)
		}
		if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER stage1c_abort_adoption`); err != nil {
			t.Fatalf("drop adoption rollback trigger: %v", err)
		}
		adopted := stage1CAdopt(t, st, sy)
		if len(adopted.Items) != 0 {
			t.Fatalf("empty adoption items = %#v, want empty checklist", adopted.Items)
		}
		stable := stage1CHolderState(t, st)
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("ordinary Pull after empty adoption: %v", err)
		}
		if after := stage1CHolderState(t, st); after != stable {
			t.Fatalf("consumed empty adoption ran again\nbefore: %s\nafter:  %s", stable, after)
		}
	})

	t.Run("each explicit discard authorizes exactly one adoption", func(t *testing.T) {
		ctx := context.Background()
		st := stage1COpenStore(t, filepath.Join(t.TempDir(), "cache.db"))
		t.Cleanup(func() { st.Close() })
		fake := newStage1CWireFake(t)
		sy := stage1CSeedFromServer(t, st, fake,
			stage1CHolderTask(stage1CTaskID, "two discards", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)}))

		_, _ = stage1CAmbiguousDiscard(t, st, sy, fake)
		first := stage1CAdopt(t, st, sy)
		firstKeys := stage1CKeys(first)
		firstRegistry := stage1CRegistry(t, st, stage1CTaskID)
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("Pull after first adoption: %v", err)
		}
		if got := stage1CKeys(stage1CTask(t, st, stage1CTaskID)); !reflect.DeepEqual(got, firstKeys) {
			t.Fatalf("abandoned history retriggered adoption: %v -> %v", firstKeys, got)
		}
		if got := stage1CRegistry(t, st, stage1CTaskID); !reflect.DeepEqual(got, firstRegistry) {
			t.Fatalf("registry changed after consumed first authorization: %v -> %v", firstRegistry, got)
		}

		_, _ = stage1CAmbiguousDiscard(t, st, sy, fake)
		second := stage1CAdopt(t, st, sy)
		secondKeys := stage1CKeys(second)
		for _, secondKey := range secondKeys {
			for _, firstKey := range firstKeys {
				if secondKey == firstKey {
					t.Fatalf("second explicit adoption reused first history key %q", secondKey)
				}
			}
		}
		stable := stage1CHolderState(t, st)
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("Pull after second adoption: %v", err)
		}
		if after := stage1CHolderState(t, st); after != stable {
			t.Fatalf("second authorization was not consumed exactly once\nbefore: %s\nafter:  %s", stable, after)
		}
		if _, err := st.RenameTaskItem(ctx, stage1CTaskID, 1, "fresh history remains editable"); err != nil {
			t.Fatalf("abandoned history permanently blocked fresh history: %v", err)
		}
		if res, err := sy.Push(ctx); err != nil || res.Pushed != 1 {
			t.Fatalf("confirm edit in fresh history = %+v, %v", res, err)
		}
	})
}

func stage1CHolderState(t *testing.T, st *store.Store) string {
	t.Helper()
	queries := []string{
		`SELECT * FROM tasks ORDER BY id`,
		`SELECT * FROM items ORDER BY task_id, position, id`,
		`SELECT * FROM item_identities ORDER BY task_id, item_key`,
		`SELECT * FROM outbox ORDER BY seq`,
		`SELECT * FROM undo_log ORDER BY seq`,
		`SELECT * FROM meta ORDER BY key`,
	}
	all := make([][][]any, 0, len(queries))
	for _, query := range queries {
		rows, err := st.DB().QueryContext(context.Background(), query)
		if err != nil {
			t.Fatalf("snapshot %q: %v", query, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("snapshot columns: %v", err)
		}
		var table [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatalf("snapshot row: %v", err)
			}
			for i, value := range values {
				if bytes, ok := value.([]byte); ok {
					values[i] = string(bytes)
				}
			}
			table = append(table, values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("snapshot rows: %v", err)
		}
		rows.Close()
		all = append(all, table)
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("encode holder state: %v", err)
	}
	return string(encoded)
}
