package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestStage1BUndoItemUpdateRestoresKeysValuesOrderAndStack(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	created := undoAdd(t, st, "Stage 1B keyed undo",
		model.Item{Title: "first", Status: model.ItemOpen, TimeZone: "Europe/Moscow"},
		model.Item{Title: "second", Status: model.ItemDone, IsAllDay: true},
		model.Item{Title: "third", Status: model.ItemOpen})
	want := append([]model.Item(nil), created.Items...)
	createEntry, ok := undoTop(t, st)
	if !ok || createEntry.Action.Op != store.OpTaskCreate {
		t.Fatalf("create left undo entry %+v, want task.create", createEntry.Action)
	}

	changed := []model.Item{want[2], want[0], want[1]}
	changed[0].Title = "third changed"
	changed[0].Status = model.ItemDone
	changed[0].SortOrder = 31
	changed[1].SortOrder = 11
	changed[2].SortOrder = 21
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Items: model.NewEditList(changed)}); err != nil {
		t.Fatalf("update checklist: %v", err)
	}
	changedEntry, ok := undoTop(t, st)
	if !ok || changedEntry.Action.Op != store.OpTaskUpdate || changedEntry.Seq <= createEntry.Seq {
		t.Fatalf("update left undo entry %+v, want a newer task.update", changedEntry)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close before command: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "undid the last change") {
		t.Fatalf("tt undo = %d, stdout %q, stderr %q; want a successful item-edit reversal", code, stdout, stderr)
	}

	reopened := undoCache(t)
	got := undoTask(t, reopened, created.Id)
	if !reflect.DeepEqual(got.Items, want) {
		t.Fatalf("items after reopen = %+v, want original keys, values, order, and ranks %+v", got.Items, want)
	}
	top, ok := undoTop(t, reopened)
	if !ok || top.Seq != createEntry.Seq || top.Action.Op != store.OpTaskCreate {
		t.Fatalf("stack after undo = %+v, want original create entry at seq %d", top, createEntry.Seq)
	}
}

type stage1BUndoState struct {
	Modeled  string
	Queue    string
	Journal  string
	Registry string
	Undo     string
	Meta     string
}

func stage1BUndoSnapshot(t *testing.T, st *store.Store) stage1BUndoState {
	t.Helper()
	return stage1BUndoState{
		Modeled: stage1BUndoRows(t, st,
			`SELECT * FROM tasks ORDER BY id`,
			`SELECT * FROM items ORDER BY task_id, position, id`),
		Queue:    stage1BUndoRows(t, st, `SELECT * FROM outbox ORDER BY seq`),
		Journal:  stage1BUndoRows(t, st, `SELECT * FROM events ORDER BY seq`),
		Registry: stage1BUndoRows(t, st, `SELECT * FROM item_identities ORDER BY task_id, item_key`),
		Undo:     stage1BUndoRows(t, st, `SELECT * FROM undo_log ORDER BY seq`),
		Meta:     stage1BUndoRows(t, st, `SELECT * FROM meta ORDER BY key`),
	}
}

func stage1BUndoRows(t *testing.T, st *store.Store, queries ...string) string {
	t.Helper()
	all := make([][][]any, 0, len(queries))
	for _, query := range queries {
		rows, err := st.DB().QueryContext(context.Background(), query)
		if err != nil {
			t.Fatalf("snapshot %q: %v", query, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("snapshot columns for %q: %v", query, err)
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
				t.Fatalf("snapshot row for %q: %v", query, err)
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
			t.Fatalf("finish snapshot %q: %v", query, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close snapshot %q: %v", query, err)
		}
		all = append(all, table)
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	return string(encoded)
}

func TestStage1BUndoRefusesInvalidKeyWithoutChangingState(t *testing.T) {
	cases := []struct {
		name        string
		makeInvalid func(context.Context, *store.Store, model.Task) error
		want        string
	}{
		{
			name: "missing registry provenance",
			makeInvalid: func(ctx context.Context, st *store.Store, task model.Task) error {
				_, err := st.DB().ExecContext(ctx,
					`DELETE FROM item_identities WHERE task_id = ? AND item_key = ?`, task.Id, task.Items[0].Key)
				return err
			},
			want: "no registry provenance",
		},
		{
			name: "abandoned tombstone",
			makeInvalid: func(ctx context.Context, st *store.Store, task model.Task) error {
				_, err := st.DB().ExecContext(ctx,
					`UPDATE item_identities SET state = 'abandoned' WHERE task_id = ? AND item_key = ?`,
					task.Id, task.Items[0].Key)
				return err
			},
			want: "abandoned item identity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			st := undoCache(t)
			undoProjects(t, st)
			created := undoAdd(t, st, "Stage 1B refused undo",
				model.Item{Title: "first"}, model.Item{Title: "second"})
			changed := append([]model.Item(nil), created.Items...)
			changed[0].Title = "changed"
			if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Items: model.NewEditList(changed)}); err != nil {
				t.Fatalf("update checklist: %v", err)
			}
			top, ok := undoTop(t, st)
			if !ok || top.Action.Op != store.OpTaskUpdate {
				t.Fatalf("update left undo entry %+v, want task.update", top.Action)
			}
			if err := tc.makeInvalid(ctx, st, created); err != nil {
				t.Fatalf("make synthetic %s fixture: %v", tc.name, err)
			}
			before := stage1BUndoSnapshot(t, st)
			if err := st.Close(); err != nil {
				t.Fatalf("close before command: %v", err)
			}

			code, stdout, stderr := undoRun(t)
			if code != exitError {
				t.Fatalf("tt undo = %d, stdout %q, stderr %q; want refusal", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want no success report", stdout)
			}
			normalizedStderr := strings.Join(strings.Fields(stderr), " ")
			for _, fragment := range []string{"checklist cannot be replaced losslessly", tc.want, undoSkipOption} {
				if !strings.Contains(normalizedStderr, fragment) {
					t.Errorf("stderr = %q, want it to contain %q", stderr, fragment)
				}
			}

			reopened := undoCache(t)
			after := stage1BUndoSnapshot(t, reopened)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("state changed across refused undo:\nbefore: %+v\nafter:  %+v", before, after)
			}
			retained, ok := undoTop(t, reopened)
			if !ok || retained.Seq != top.Seq || retained.Action.Op != store.OpTaskUpdate {
				t.Fatalf("stack after refusal = %+v, want original update entry at seq %d", retained, top.Seq)
			}
		})
	}
}

func TestStage1BUndoContinuesBehindAnUncertainItemWrite(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	const taskID = "srv-stage1b-uncertain"
	remote := model.Task{
		Id: taskID, ProjectId: "p1", Title: "Stage 1B uncertain predecessor", Status: model.TaskOpen,
		Kind: "CHECKLIST",
		Items: []model.Item{
			{Id: "srv-uncertain-a", Title: "alpha", Status: model.ItemOpen, SortOrder: 41},
			{Id: "srv-uncertain-b", Title: "beta", Status: model.ItemDone, SortOrder: 17},
		},
	}
	raw := json.RawMessage(`{"id":"srv-stage1b-uncertain","projectId":"p1","title":"Stage 1B uncertain predecessor","kind":"CHECKLIST","items":[{"id":"srv-uncertain-a","title":"alpha","status":0,"sortOrder":41},{"id":"srv-uncertain-b","title":"beta","status":1,"sortOrder":17}]}`)
	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{Task: remote, Raw: raw}}); err != nil {
		t.Fatalf("seed remote checklist: %v", err)
	}
	want := undoTask(t, st, taskID)
	changed := []model.Item{want.Items[1], want.Items[0]}
	changed[0].Title = "beta changed"
	changed[0].SortOrder = 71
	changed[1].SortOrder = 81
	if _, err := st.UpdateTask(ctx, taskID, model.TaskEdit{Items: model.NewEditList(changed)}); err != nil {
		t.Fatalf("queue predecessor item update: %v", err)
	}
	updateEntry, ok := undoTop(t, st)
	if !ok || updateEntry.Action.Op != store.OpTaskUpdate {
		t.Fatalf("update left undo entry %+v, want task.update", updateEntry.Action)
	}
	uncertainKey := want.Items[0].Key
	const uncertainServerID = "srv-uncertain-a"
	res, err := st.DB().ExecContext(ctx,
		`UPDATE item_identities SET state = 'uncertain' WHERE task_id = ? AND item_key = ? AND server_id = ?`,
		taskID, uncertainKey, uncertainServerID)
	if err != nil {
		t.Fatalf("mark synthetic allocating predecessor uncertain: %v", err)
	}
	if changed, err := res.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("mark uncertain changed %d row(s): %v", changed, err)
	}

	beforeRegistry := stage1BUndoRows(t, st,
		`SELECT * FROM item_identities ORDER BY task_id, item_key`)
	beforeQueue := stage1BUndoRows(t, st, `SELECT * FROM outbox ORDER BY seq`)
	var beforeCount int
	var beforeMax int64
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*), max(seq) FROM outbox`).Scan(&beforeCount, &beforeMax); err != nil {
		t.Fatalf("read predecessor queue extent: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close before command: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "undid the last change") {
		t.Fatalf("tt undo = %d, stdout %q, stderr %q; want a successful queued reversal", code, stdout, stderr)
	}

	reopened := undoCache(t)
	got := undoTask(t, reopened, taskID)
	wantAfterReopen := append([]model.Item(nil), want.Items...)
	wantAfterReopen[0].Id = ""
	if !reflect.DeepEqual(got.Items, wantAfterReopen) {
		t.Fatalf("items after reopen = %+v, want stable keys and restored values %+v", got.Items, wantAfterReopen)
	}
	afterRegistry := stage1BUndoRows(t, reopened,
		`SELECT * FROM item_identities ORDER BY task_id, item_key`)
	if afterRegistry != beforeRegistry {
		t.Fatalf("registry changed across local undo:\nbefore: %s\nafter:  %s", beforeRegistry, afterRegistry)
	}
	var serverID, state string
	if err := reopened.DB().QueryRowContext(ctx,
		`SELECT server_id, state FROM item_identities WHERE task_id = ? AND item_key = ?`,
		taskID, uncertainKey).Scan(&serverID, &state); err != nil {
		t.Fatalf("read uncertain identity after undo: %v", err)
	}
	if serverID != uncertainServerID || state != "uncertain" {
		t.Fatalf("uncertain identity after undo = server_id %q, state %q", serverID, state)
	}
	priorQueueAfter := stage1BUndoRows(t, reopened,
		fmt.Sprintf(`SELECT * FROM outbox WHERE seq <= %d ORDER BY seq`, beforeMax))
	if priorQueueAfter != beforeQueue {
		t.Fatalf("predecessor queue evidence changed:\nbefore: %s\nafter:  %s", beforeQueue, priorQueueAfter)
	}
	var afterCount int
	var afterMax int64
	if err := reopened.DB().QueryRowContext(ctx, `SELECT count(*), max(seq) FROM outbox`).Scan(&afterCount, &afterMax); err != nil {
		t.Fatalf("read queue after undo: %v", err)
	}
	if afterCount != beforeCount+1 || afterMax <= beforeMax {
		t.Fatalf("queue after undo has count %d and max seq %d, want %d and a seq after %d",
			afterCount, afterMax, beforeCount+1, beforeMax)
	}
	var op, queuedTaskID, payload string
	if err := reopened.DB().QueryRowContext(ctx,
		`SELECT op, task_id, payload FROM outbox WHERE seq = ?`, afterMax).
		Scan(&op, &queuedTaskID, &payload); err != nil {
		t.Fatalf("read queued reversal: %v", err)
	}
	if op != store.OpTaskUpdate || queuedTaskID != taskID {
		t.Fatalf("queued reversal = op %q task %q, want task.update for %s", op, queuedTaskID, taskID)
	}
	for _, item := range want.Items {
		if !strings.Contains(payload, item.Key) {
			t.Errorf("queued reversal payload lost item key %q: %s", item.Key, payload)
		}
	}
	if top, ok := undoTop(t, reopened); ok {
		t.Fatalf("undo stack retained %+v, want the sole update record consumed", top)
	}
}

func TestStage1BUndoParentDeleteAllocatesFreshKeysAndClearsServerItemIDs(t *testing.T) {
	t.Run("offline local parent", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		st := undoCache(t)
		undoProjects(t, st)
		deleted := undoAdd(t, st, "Stage 1B local parent delete",
			model.Item{Title: "alpha", Status: model.ItemOpen, SortOrder: 41},
			model.Item{Title: "beta", Status: model.ItemDone, SortOrder: 17})
		createEntry, ok := undoTop(t, st)
		if !ok || createEntry.Action.Op != store.OpTaskCreate {
			t.Fatalf("create left undo entry %+v, want task.create", createEntry.Action)
		}
		oldKeys := make(map[string]bool, len(deleted.Items))
		for _, item := range deleted.Items {
			if item.Key == "" || !store.IsLocalID(item.Key) || item.Id != "" {
				t.Fatalf("offline item has unexpected identity: %+v", item)
			}
			oldKeys[item.Key] = true
		}
		if err := st.DeleteTask(ctx, deleted.Id); err != nil {
			t.Fatalf("delete local parent: %v", err)
		}
		deleteEntry, ok := undoTop(t, st)
		if !ok || deleteEntry.Action.Op != store.OpTaskDelete || deleteEntry.Seq <= createEntry.Seq {
			t.Fatalf("delete left undo entry %+v, want a newer task.delete", deleteEntry)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close before command: %v", err)
		}

		code, stdout, stderr := undoRun(t)
		if code != exitOK || stderr != "" || !strings.Contains(stdout, "the task is back") ||
			!strings.Contains(stdout, "new task") {
			t.Fatalf("tt undo = %d, stdout %q, stderr %q; want a fresh local-parent report", code, stdout, stderr)
		}

		reopened := undoCache(t)
		if _, err := reopened.Task(ctx, deleted.Id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted local parent id %q was reused: %v", deleted.Id, err)
		}
		tasks, err := reopened.Tasks(ctx, store.TaskFilter{Search: "Stage 1B local parent delete"})
		if err != nil {
			t.Fatalf("find restored local parent: %v", err)
		}
		if len(tasks) != 1 || !store.IsLocalID(tasks[0].Id) || tasks[0].Id == deleted.Id {
			t.Fatalf("restored local parents = %+v, want one fresh local id distinct from %s", tasks, deleted.Id)
		}
		restored := undoTask(t, reopened, tasks[0].Id)
		if len(restored.Items) != len(deleted.Items) {
			t.Fatalf("restored items = %+v, want %d", restored.Items, len(deleted.Items))
		}
		for i, item := range restored.Items {
			want := deleted.Items[i]
			if item.Id != "" {
				t.Errorf("restored item %d kept server id %q", i+1, item.Id)
			}
			if item.Key == "" || !store.IsLocalID(item.Key) || oldKeys[item.Key] {
				t.Errorf("restored item %d key = %q, want a fresh local key", i+1, item.Key)
			}
			if item.Title != want.Title || item.Status != want.Status || item.SortOrder != want.SortOrder {
				t.Errorf("restored item %d = %+v, want copied values/order rank %+v", i+1, item, want)
			}
		}
		top, ok := undoTop(t, reopened)
		if !ok || top.Seq != createEntry.Seq || top.Action.Op != store.OpTaskCreate {
			t.Fatalf("stack after local-parent undo = %+v, want original create entry at seq %d", top, createEntry.Seq)
		}
	})

	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	const oldTaskID = "srv-stage1b-parent"
	remote := model.Task{
		Id: oldTaskID, ProjectId: "p1", Title: "Stage 1B parent delete", Status: model.TaskOpen,
		Kind: "CHECKLIST",
		Items: []model.Item{
			{Id: "srv-item-a", Title: "alpha", Status: model.ItemOpen, SortOrder: 41},
			{Id: "srv-item-b", Title: "beta", Status: model.ItemDone, SortOrder: 17},
		},
	}
	raw := json.RawMessage(`{"id":"srv-stage1b-parent","projectId":"p1","title":"Stage 1B parent delete","kind":"CHECKLIST","items":[{"id":"srv-item-a","title":"alpha","status":0,"sortOrder":41},{"id":"srv-item-b","title":"beta","status":1,"sortOrder":17}]}`)
	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{Task: remote, Raw: raw}}); err != nil {
		t.Fatalf("seed remote checklist: %v", err)
	}
	registered := undoTask(t, st, oldTaskID)
	registered.Items[0].Title = "alpha edited"
	registered, err := st.UpdateTask(ctx, oldTaskID,
		model.TaskEdit{Items: model.NewEditList(append([]model.Item(nil), registered.Items...))})
	if err != nil {
		t.Fatalf("register checklist identity: %v", err)
	}
	oldKeys := make(map[string]bool, len(registered.Items))
	for _, item := range registered.Items {
		if item.Key == "" || item.Id == "" {
			t.Fatalf("registered item lacks old identity: %+v", item)
		}
		oldKeys[item.Key] = true
	}
	if err := st.DeleteTask(ctx, oldTaskID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	deleteEntry, ok := undoTop(t, st)
	if !ok || deleteEntry.Action.Op != store.OpTaskDelete {
		t.Fatalf("delete left undo entry %+v, want task.delete", deleteEntry.Action)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close before command: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "the task is back") ||
		!strings.Contains(stdout, "new task") {
		t.Fatalf("tt undo = %d, stdout %q, stderr %q; want a remade-parent report", code, stdout, stderr)
	}

	reopened := undoCache(t)
	if _, err := reopened.Task(ctx, oldTaskID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old parent id was resurrected: %v", err)
	}
	tasks, err := reopened.Tasks(ctx, store.TaskFilter{Search: "Stage 1B parent delete"})
	if err != nil {
		t.Fatalf("find restored parent: %v", err)
	}
	if len(tasks) != 1 || !store.IsLocalID(tasks[0].Id) || tasks[0].Id == oldTaskID {
		t.Fatalf("restored parents = %+v, want one fresh local parent", tasks)
	}
	restored := undoTask(t, reopened, tasks[0].Id)
	if len(restored.Items) != len(registered.Items) {
		t.Fatalf("restored items = %+v, want %d", restored.Items, len(registered.Items))
	}
	for i, item := range restored.Items {
		want := registered.Items[i]
		if item.Id != "" {
			t.Errorf("restored item %d kept server id %q", i+1, item.Id)
		}
		if item.Key == "" || !store.IsLocalID(item.Key) || oldKeys[item.Key] {
			t.Errorf("restored item %d key = %q, want a fresh local key", i+1, item.Key)
		}
		if item.Title != want.Title || item.Status != want.Status || item.SortOrder != want.SortOrder {
			t.Errorf("restored item %d = %+v, want copied values/order rank %+v", i+1, item, want)
		}
	}
	if top, ok := undoTop(t, reopened); ok && top.Seq >= deleteEntry.Seq {
		t.Fatalf("delete undo entry was not consumed: top %+v, reversed seq %d", top, deleteEntry.Seq)
	}
}
