package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const stage1CProjectDataPath = "/open/v1/project/" + stage1CProjectID + "/data"

type stage1CHolderPullResult struct {
	result Result
	err    error
}

func stage1CHolderHoldPull(t *testing.T, sy *Syncer, fake *stage1CWireFake, method, path string) (func(), <-chan stage1CHolderPullResult) {
	t.Helper()
	reached, release := fake.HoldNext(method, path)
	done := make(chan stage1CHolderPullResult, 1)
	go func() {
		result, err := sy.Pull(context.Background())
		done <- stage1CHolderPullResult{result: result, err: err}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("held pull did not reach the requested HTTP exchange")
	}
	return release, done
}

func stage1CHolderFinishPull(t *testing.T, release func(), done <-chan stage1CHolderPullResult) Result {
	t.Helper()
	release()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("held Pull: %v", outcome.err)
		}
		return outcome.result
	case <-time.After(5 * time.Second):
		t.Fatal("held Pull did not finish after release")
		return Result{}
	}
}

func stage1CHolderProtectedState(t *testing.T, st *store.Store) string {
	t.Helper()
	queries := []string{
		`SELECT * FROM tasks ORDER BY id`,
		`SELECT * FROM items ORDER BY task_id, position, id`,
		`SELECT * FROM item_identities ORDER BY task_id, item_key`,
		`SELECT * FROM outbox ORDER BY seq`,
		`SELECT * FROM undo_log ORDER BY seq`,
		`SELECT * FROM events ORDER BY seq`,
		`SELECT key, value FROM meta WHERE key = 'item_identity_epoch'`,
	}
	all := make([][][]any, 0, len(queries))
	for _, query := range queries {
		rows, err := st.DB().QueryContext(context.Background(), query)
		if err != nil {
			t.Fatalf("protected snapshot %q: %v", query, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("protected snapshot columns: %v", err)
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
				t.Fatalf("protected snapshot row: %v", err)
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
			t.Fatalf("protected snapshot rows: %v", err)
		}
		rows.Close()
		all = append(all, table)
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("encode protected snapshot: %v", err)
	}
	return string(encoded)
}

func stage1CHolderSetup(t *testing.T, tasks ...map[string]any) (*store.Store, *stage1CWireFake, *Syncer) {
	t.Helper()
	st := stage1COpenStore(t, filepath.Join(t.TempDir(), "cache.db"))
	t.Cleanup(func() { st.Close() })
	fake := newStage1CWireFake(t)
	fake.SeedProject(stage1CProjectID, "Stage 1C")
	for _, task := range tasks {
		fake.SeedTask(stage1CProjectID, task)
	}
	sy := testSyncer(t, st, fake.Server())
	if _, err := sy.Pull(context.Background()); err != nil {
		t.Fatalf("initial Pull: %v", err)
	}
	return st, fake, sy
}

func stage1CHolderRegister(t *testing.T, st *store.Store, sy *Syncer, taskID string) {
	t.Helper()
	if _, err := st.RenameTaskItem(context.Background(), taskID, 1, "registered "+taskID); err != nil {
		t.Fatalf("register %s: %v", taskID, err)
	}
	if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
		t.Fatalf("confirm registration of %s = %+v, %v", taskID, result, err)
	}
}

func stage1CHolderAssertMissing(t *testing.T, st *store.Store, taskID string) {
	t.Helper()
	if task, err := st.Task(context.Background(), taskID); err == nil {
		t.Fatalf("task %s was recreated: %+v", taskID, task)
	}
}

func stage1CHolderSetRemoteTitle(fake *stage1CWireFake, taskID, title string) {
	fake.MutateTask(stage1CProjectID, taskID, func(task map[string]any) { task["title"] = title })
}

func TestStage1CRC2PrefetchEpochFence(t *testing.T) {
	t.Run("old project response skips upsert and absence deletion as one unit", func(t *testing.T) {
		one := stage1CHolderTask("one", "one", []map[string]any{stage1CHolderItem("one-item", "one item", 1)})
		two := stage1CHolderTask("two", "two", []map[string]any{stage1CHolderItem("two-item", "two item", 1)})
		st, fake, sy := stage1CHolderSetup(t, one, two)
		stage1CHolderRegister(t, st, sy, "one")
		stage1CHolderRegister(t, st, sy, "two")
		fake.RemoveTask(stage1CProjectID, "two")
		stage1CHolderSetRemoteTitle(fake, "one", "stale captured title")

		epochAtCapture := stage1CEpoch(t, st)
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		if _, err := st.UpdateTask(context.Background(), "one", model.TaskEdit{Title: model.Ptr("new confirmed title")}); err != nil {
			t.Fatalf("queue edit while old GET waits: %v", err)
		}
		if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
			t.Fatalf("fully settle edit while old GET waits = %+v, %v", result, err)
		}
		if stage1COutboxCount(t, st) != 0 {
			t.Fatal("settled edit left an outbox row")
		}
		if epoch := stage1CEpoch(t, st); epoch <= epochAtCapture {
			t.Fatalf("epoch after edit/settlement = %d, want > %d despite clean/no outbox", epoch, epochAtCapture)
		}
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("old project response changed protected state\nbefore: %s\nafter:  %s", before, after)
		}
		if got := stage1CTask(t, st, "one").Title; got != "new confirmed title" {
			t.Fatalf("old upsert replaced newer model with %q", got)
		}
		if _, err := st.Task(context.Background(), "two"); err != nil {
			t.Fatalf("old absence deletion ran despite stale unit: %v", err)
		}
		if result, err := sy.Pull(context.Background()); err != nil || result.Deleted != 1 {
			t.Fatalf("fresh Pull did not apply upsert and absence set: %+v, %v", result, err)
		}
		stage1CHolderAssertMissing(t, st, "two")
	})

	t.Run("overlapping valid pulls keep the newer raw and registry", func(t *testing.T) {
		task := stage1CHolderTask(stage1CTaskID, "overlap", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)})
		st, fake, sy := stage1CHolderSetup(t, task)
		stage1CHolderRegister(t, st, sy, stage1CTaskID)
		stage1CHolderSetRemoteTitle(fake, stage1CTaskID, "first response")
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		stage1CHolderSetRemoteTitle(fake, stage1CTaskID, "second response")
		fake.MutateTask(stage1CProjectID, stage1CTaskID, func(task map[string]any) {
			task["serverMarker"] = "newer"
			task["items"] = append(task["items"].([]any), stage1CHolderItem("remote-b", "beta", 2))
		})
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("overlapping newer Pull: %v", err)
		}
		newer := stage1CHolderProtectedState(t, st)
		newerRaw := stage1CRaw(t, st, stage1CTaskID)
		newerRegistry := stage1CRegistry(t, st, stage1CTaskID)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != newer {
			t.Fatalf("older overlapping Pull changed newer state\nbefore: %s\nafter:  %s", newer, after)
		}
		if stage1CRaw(t, st, stage1CTaskID) != newerRaw || !reflect.DeepEqual(stage1CRegistry(t, st, stage1CTaskID), newerRegistry) {
			t.Fatal("older overlapping Pull replaced newer raw or registry")
		}
		stage1CHolderSetRemoteTitle(fake, stage1CTaskID, "third fresh response")
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("fresh Pull after stale overlap: %v", err)
		}
		if got := stage1CTask(t, st, stage1CTaskID).Title; got != "third fresh response" {
			t.Fatalf("fresh Pull did not progress, title=%q", got)
		}
	})

	t.Run("held at capture stays skipped after the write settles clean", func(t *testing.T) {
		task := stage1CHolderTask(stage1CTaskID, "held", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)})
		st, fake, sy := stage1CHolderSetup(t, task)
		stage1CHolderRegister(t, st, sy, stage1CTaskID)
		if _, err := st.UpdateTask(context.Background(), stage1CTaskID, model.TaskEdit{Title: model.Ptr("queued before capture")}); err != nil {
			t.Fatal(err)
		}
		stage1CHolderSetRemoteTitle(fake, stage1CTaskID, "old while held")
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
			t.Fatalf("settle held edit = %+v, %v", result, err)
		}
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("response captured while held applied after clean settlement\nbefore: %s\nafter:  %s", before, after)
		}
		if got := stage1CTask(t, st, stage1CTaskID).Title; got != "queued before capture" {
			t.Fatalf("held-at-capture response won with title %q", got)
		}
	})

	t.Run("first repeat-only registration fences unsupported item raw", func(t *testing.T) {
		unsupported := stage1CHolderTask(stage1CTaskID, "partial", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)})
		unsupported["items"].([]map[string]any)[0]["future"] = true
		st, fake, sy := stage1CHolderSetup(t, unsupported)
		oldRaw := stage1CRaw(t, st, stage1CTaskID)
		epochAtCapture := int64(0)
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		if _, err := st.SetTaskRepeat(context.Background(), stage1CTaskID, "RRULE:FREQ=WEEKLY;INTERVAL=1"); err != nil {
			t.Fatalf("queue repeat-only registration: %v", err)
		}
		fake.SetResponseHook(func(req stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if req.Method == http.MethodGet && req.Path == "/open/v1/project/"+stage1CProjectID+"/task/"+stage1CTaskID {
				return stage1CWireJSONResponse(http.StatusOK, map[string]any{
					"id": stage1CTaskID, "projectId": stage1CProjectID,
					"repeatFlag": "RRULE:FREQ=WEEKLY;INTERVAL=1",
				})
			}
			return response
		})
		if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
			t.Fatalf("confirm partial repeat-only registration = %+v, %v", result, err)
		}
		if stage1CRaw(t, st, stage1CTaskID) != oldRaw {
			t.Fatal("partial confirmation replaced unsupported item raw")
		}
		if epoch := stage1CEpoch(t, st); epoch <= epochAtCapture {
			t.Fatalf("repeat-only registration epoch = %d, want > %d", epoch, epochAtCapture)
		}
		registry := stage1CRegistry(t, st, stage1CTaskID)
		if len(registry) != 1 || registry[0][0] != "" || registry[0][2] != string(store.RecoveryIdle) {
			t.Fatalf("repeat-only registration adopted item identities: %+v", registry)
		}
		for _, request := range fake.Requests() {
			if request.Method == http.MethodPost && request.Path == "/open/v1/task/"+stage1CTaskID {
				var body map[string]json.RawMessage
				if err := json.Unmarshal(request.Body, &body); err != nil {
					t.Fatal(err)
				}
				if _, ok := body["items"]; ok {
					t.Fatalf("repeat-only request sent Items: %s", request.Body)
				}
			}
		}
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("pre-registration body changed post-registration state\nbefore: %s\nafter:  %s", before, after)
		}

		noOpBefore := stage1CHolderProtectedState(t, st)
		if _, err := st.SetTaskRepeat(context.Background(), stage1CTaskID, "RRULE:FREQ=WEEKLY;INTERVAL=1"); err != nil {
			t.Fatalf("repeat true no-op: %v", err)
		}
		if after := stage1CHolderProtectedState(t, st); after != noOpBefore {
			t.Fatalf("repeat true no-op wrote state\nbefore: %s\nafter:  %s", noOpBefore, after)
		}
		if _, err := st.RenameTaskItem(context.Background(), stage1CTaskID, 1, "refused"); !errors.Is(err, store.ErrUnsafeChecklist) {
			t.Fatalf("item mutation over unsupported raw = %v", err)
		}
		fake.SetResponseHook(nil)
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("fresh Pull after registration fence: %v", err)
		}
		if fresh := stage1CRaw(t, st, stage1CTaskID); fresh == oldRaw || !strings.Contains(fresh, "WEEKLY") {
			t.Fatalf("fresh Pull did not progress raw evidence: %s", fresh)
		}
	})

	t.Run("registered parent deletion cannot be recreated", func(t *testing.T) {
		parent := stage1CHolderTask(stage1CTaskID, "parent", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)})
		spectator := stage1CHolderTask("spectator", "before", nil)
		st, fake, sy := stage1CHolderSetup(t, parent, spectator)
		stage1CHolderRegister(t, st, sy, stage1CTaskID)
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		if err := st.DeleteTask(context.Background(), stage1CTaskID); err != nil {
			t.Fatalf("queue registered parent deletion: %v", err)
		}
		if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
			t.Fatalf("fully settle parent deletion = %+v, %v", result, err)
		}
		stage1CHolderSetRemoteTitle(fake, "spectator", "after")
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("old response recreated registered parent or changed state\nbefore: %s\nafter:  %s", before, after)
		}
		stage1CHolderAssertMissing(t, st, stage1CTaskID)
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("fresh Pull after parent deletion: %v", err)
		}
		if got := stage1CTask(t, st, "spectator").Title; got != "after" {
			t.Fatalf("fresh Pull did not progress spectator, title=%q", got)
		}
	})

	t.Run("absent registration and deletion invalidates the old response", func(t *testing.T) {
		spectator := stage1CHolderTask("spectator", "before", nil)
		st, fake, sy := stage1CHolderSetup(t, spectator)
		stage1CHolderSetRemoteTitle(fake, "spectator", "captured old")
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
		created, err := st.CreateTask(context.Background(), model.Task{
			ProjectId: stage1CProjectID, Title: "ephemeral", Kind: "CHECKLIST",
			Items: []model.Item{{Title: "registered then deleted"}},
		})
		if err != nil {
			t.Fatalf("register absent task: %v", err)
		}
		if err := st.DeleteTask(context.Background(), created.Id); err != nil {
			t.Fatalf("delete newly registered task: %v", err)
		}
		stage1CHolderSetRemoteTitle(fake, "spectator", "fresh after cycle")
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("absent-registration-deletion cycle did not fence old response\nbefore: %s\nafter:  %s", before, after)
		}
		stage1CHolderAssertMissing(t, st, created.Id)
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("fresh Pull after absent cycle: %v", err)
		}
		if got := stage1CTask(t, st, "spectator").Title; got != "fresh after cycle" {
			t.Fatalf("fresh Pull did not progress after absent cycle, title=%q", got)
		}
	})

	t.Run("completed recovery GET is fenced after edit settles", func(t *testing.T) {
		task := stage1CHolderTask(stage1CTaskID, "completed recovery", []map[string]any{stage1CHolderItem("remote-a", "alpha", 1)})
		task["status"] = 2
		task["completedTime"] = "2026-09-09T12:00:00.000+0000"
		st, fake, sy := stage1CHolderSetup(t, task)
		_, _ = stage1CAmbiguousDiscard(t, st, sy, fake)
		fake.SetResponseHook(func(req stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if req.Method == http.MethodGet && req.Path == stage1CProjectDataPath {
				return stage1CWireJSONResponse(http.StatusOK, map[string]any{
					"project": map[string]any{"id": stage1CProjectID, "name": "Stage 1C"},
					"tasks":   []any{}, "columns": []any{},
				})
			}
			return response
		})
		addressedPath := "/open/v1/project/" + stage1CProjectID + "/task/" + stage1CTaskID
		release, done := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, addressedPath)
		if _, err := st.SetTaskRepeat(context.Background(), stage1CTaskID, "RRULE:FREQ=DAILY;INTERVAL=2"); err != nil {
			t.Fatalf("edit completed task while recovery GET waits: %v", err)
		}
		if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
			t.Fatalf("fully settle completed-task edit = %+v, %v", result, err)
		}
		if stage1COutboxCount(t, st) != 0 || stage1CControl(t, st, stage1CTaskID) != string(store.RecoveryPending) {
			t.Fatal("completed task did not return to clean recovery-pending state")
		}
		before := stage1CHolderProtectedState(t, st)
		stage1CHolderFinishPull(t, release, done)
		if after := stage1CHolderProtectedState(t, st); after != before {
			t.Fatalf("old completed recovery GET changed state\nbefore: %s\nafter:  %s", before, after)
		}
		if stage1CControl(t, st, stage1CTaskID) != string(store.RecoveryPending) {
			t.Fatal("stale completed recovery GET consumed authorization")
		}
		if _, err := sy.Pull(context.Background()); err != nil {
			t.Fatalf("fresh completed recovery Pull: %v", err)
		}
		if stage1CControl(t, st, stage1CTaskID) != string(store.RecoveryIdle) {
			t.Fatal("fresh completed recovery GET did not adopt and consume authorization")
		}
	})
}
