package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type stage1CBoundaryPushResult struct {
	result Result
	err    error
}

func stage1CBoundaryStartPushOne(ctx context.Context, sy *Syncer, item store.OutboxItem) <-chan stage1CBoundaryPushResult {
	done := make(chan stage1CBoundaryPushResult, 1)
	go func() {
		var result Result
		_, err := sy.pushOne(ctx, item, &result, requeued{})
		done <- stage1CBoundaryPushResult{result: result, err: err}
	}()
	return done
}

func stage1CBoundaryStartPush(ctx context.Context, sy *Syncer) <-chan stage1CBoundaryPushResult {
	done := make(chan stage1CBoundaryPushResult, 1)
	go func() {
		result, err := sy.Push(ctx)
		done <- stage1CBoundaryPushResult{result: result, err: err}
	}()
	return done
}

func stage1CBoundaryFinishPush(t *testing.T, done <-chan stage1CBoundaryPushResult) stage1CBoundaryPushResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("held Push did not finish")
		return stage1CBoundaryPushResult{}
	}
}

func stage1CBoundaryWait(t *testing.T, reached <-chan stage1CWireExchange) stage1CWireExchange {
	t.Helper()
	select {
	case exchange := <-reached:
		return exchange
	case <-time.After(5 * time.Second):
		t.Fatal("held HTTP exchange was not reached")
		return stage1CWireExchange{}
	}
}

func stage1CBoundaryFeature(t *testing.T, st *store.Store) store.FeaturePayloadMetadata {
	t.Helper()
	var op string
	var payload []byte
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT op, payload FROM outbox ORDER BY seq LIMIT 1`).Scan(&op, &payload); err != nil {
		t.Fatalf("read feature outbox: %v", err)
	}
	var metadata *store.FeaturePayloadMetadata
	var err error
	switch op {
	case store.OpTaskCreate:
		_, metadata, err = store.DecodeTaskPayload(payload)
	case store.OpTaskUpdate, store.OpTaskMove:
		_, metadata, err = store.DecodeTaskEditPayload(payload)
	default:
		t.Fatalf("outbox op %q is not versioned", op)
	}
	if err != nil || metadata == nil {
		t.Fatalf("decode feature outbox %s: metadata=%+v err=%v", op, metadata, err)
	}
	return *metadata
}

func stage1CBoundaryClaim(t *testing.T, st *store.Store) store.OutboxItem {
	t.Helper()
	items, _, err := st.Claim(context.Background(), 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim feature entry = %+v, %v", items, err)
	}
	return items[0]
}

func stage1CBoundaryConfirmation(t *testing.T, send store.FeatureSend, taskID, projectID string) store.FeatureConfirmation {
	t.Helper()
	body := map[string]any{"id": taskID, "projectId": projectID}
	if snapshot := send.Metadata.Snapshot; snapshot != nil {
		if snapshot.Items != nil {
			items := make([]map[string]any, len(*snapshot.Items))
			for i, item := range *snapshot.Items {
				id := item.ID
				if id == "" {
					id = fmt.Sprintf("boundary-allocated-%d", i+1)
				}
				items[i] = map[string]any{
					"id": id, "title": item.Title, "status": item.Status,
					"sortOrder": item.SortOrder, "startDate": item.StartDate,
					"isAllDay": item.IsAllDay, "timeZone": item.TimeZone,
					"completedTime": item.CompletedTime,
				}
			}
			body["items"] = items
		}
		if snapshot.RepeatFlag != nil {
			body["repeatFlag"] = *snapshot.RepeatFlag
		}
		if snapshot.Reminders != nil {
			body["reminders"] = *snapshot.Reminders
		}
		if snapshot.Kind != nil {
			body["kind"] = *snapshot.Kind
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := store.ConfirmFeatureResponse(send, taskID, projectID, raw)
	if err != nil {
		t.Fatalf("build feature confirmation: %v; raw=%s", err, raw)
	}
	return confirmation
}

func stage1CBoundaryRegister(t *testing.T, st *store.Store, fake *stage1CWireFake, taskID string) *Syncer {
	t.Helper()
	sy := testSyncer(t, st, fake.Server())
	if _, err := st.RenameTaskItem(context.Background(), taskID, 1, "registered item"); err != nil {
		t.Fatalf("register task %s: %v", taskID, err)
	}
	if result, err := sy.Push(context.Background()); err != nil || result.Pushed != 1 {
		t.Fatalf("confirm registration of %s = %+v, %v", taskID, result, err)
	}
	return sy
}

func stage1CBoundaryRetryIfFailed(t *testing.T, st *store.Store) {
	t.Helper()
	counts := outboxCounts(t, st)
	if counts.Failed == 0 {
		return
	}
	if n, err := st.RetryFailed(context.Background()); err != nil || n != int64(counts.Failed) {
		t.Fatalf("RetryFailed = %d, %v; want %d", n, err, counts.Failed)
	}
}

func TestStage1CBoundaryArmAndRestartNeverDuplicateAllocatingPost(t *testing.T) {
	t.Run("arm transaction failure sends nothing", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "arm failure", Items: []model.Item{{Title: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		before := stage1CHolderProtectedState(t, st)
		if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER stage1c_refuse_arm
			BEFORE UPDATE OF payload ON outbox
			WHEN NEW.payload LIKE '%"phase":"armed"%'
			BEGIN SELECT RAISE(ABORT, 'arm refused'); END`); err != nil {
			t.Fatal(err)
		}
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("Push after arm refusal: %v", err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task") != 0 {
			t.Fatalf("arm failure sent a create: %+v", fake.Requests())
		}
		if result.Failed != 1 {
			t.Fatalf("arm failure result = %+v, want parked entry", result)
		}
		metadata := stage1CBoundaryFeature(t, st)
		if metadata.Phase != store.FeaturePrepared || metadata.Snapshot != nil {
			t.Fatalf("failed arm persisted partial metadata: %+v", metadata)
		}
		_, state := stage1CIdentity(t, st, created.Id, created.Items[0].Key)
		if state != store.ItemUnbound {
			t.Fatalf("failed arm changed registry state to %s", state)
		}
		if strings.Contains(stage1CHolderProtectedState(t, st), `"armed"`) || before == "" {
			t.Fatal("failed arm left durable armed state")
		}
	})

	t.Run("restart after arm before send has no address and no POST", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "cache.db")
		st := stage1COpenStore(t, path)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "armed crash", Items: []model.Item{{Title: "one"}}}); err != nil {
			t.Fatal(err)
		}
		claimed := stage1CBoundaryClaim(t, st)
		_, versioned, post, err := st.PrepareFeatureSend(ctx, claimed)
		if err != nil || !versioned || !post {
			t.Fatalf("PrepareFeatureSend = versioned %v post %v err %v", versioned, post, err)
		}
		if metadata := stage1CBoundaryFeature(t, st); metadata.Phase != store.FeatureArmed || metadata.Snapshot == nil {
			t.Fatalf("arm was not durable before send: %+v", metadata)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET inflight_at = inflight_at - 86400`); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st = stage1COpenStore(t, path)
		t.Cleanup(func() { st.Close() })
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("reclaimed armed create Push: %v", err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task") != 0 || result.Failed != 1 {
			t.Fatalf("unaddressed armed recovery = %+v, requests %+v", result, fake.Requests())
		}
	})

	t.Run("server apply before response never repeats create", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "lost create response", Items: []model.Item{{Title: "one"}}}); err != nil {
			t.Fatal(err)
		}
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		reached, release := fake.HoldNext(http.MethodPost, "/open/v1/task")
		done := stage1CBoundaryStartPush(ctx, testSyncer(t, st, fake.Server()))
		stage1CBoundaryWait(t, reached)
		if metadata := stage1CBoundaryFeature(t, st); metadata.Phase != store.FeatureArmed || metadata.Snapshot == nil {
			t.Fatalf("request reached server before durable arm: %+v", metadata)
		}
		cancel()
		stage1CBoundaryFinishPush(t, done)
		release()
		stage1CBoundaryRetryIfFailed(t, st)
		result, err := testSyncer(t, st, fake.Server()).Push(context.Background())
		if err != nil {
			t.Fatalf("recovery Push: %v", err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task") != 1 || result.Failed != 1 {
			t.Fatalf("lost create response recovery = %+v, requests %+v", result, fake.Requests())
		}
	})

	t.Run("accepted create and update recover by addressed GET only", func(t *testing.T) {
		for _, create := range []bool{true, false} {
			name := "update"
			if create {
				name = "create"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				st := testStore(t)
				seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
				fake := newStage1CWireFake(t)
				fake.SeedProject("p1", "P1")
				postPath, confirmPath := "/open/v1/task", "/open/v1/project/p1/task/remote-task-1"
				if create {
					if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "accepted", Items: []model.Item{{Title: "one"}}}); err != nil {
						t.Fatal(err)
					}
				} else {
					stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
					if _, err := st.AddTaskItem(ctx, "t1", "two"); err != nil {
						t.Fatal(err)
					}
					postPath, confirmPath = "/open/v1/task/t1", "/open/v1/project/p1/task/t1"
				}
				reached, release := fake.HoldNext(http.MethodGet, confirmPath)
				done := stage1CBoundaryStartPush(ctx, testSyncer(t, st, fake.Server()))
				stage1CBoundaryWait(t, reached)
				metadata := stage1CBoundaryFeature(t, st)
				if create {
					if metadata.Phase != store.FeatureAccepted || metadata.AcceptedTaskID == nil || *metadata.AcceptedTaskID != "remote-task-1" {
						t.Fatalf("accepted create evidence was not durable before settlement: %+v", metadata)
					}
				} else if metadata.Phase != store.FeatureArmed {
					t.Fatalf("update was not armed before confirmation read: %+v", metadata)
				}
				cancel()
				stage1CBoundaryFinishPush(t, done)
				release()
				stage1CBoundaryRetryIfFailed(t, st)
				beforePosts := fake.Count(http.MethodPost, postPath)
				result, err := testSyncer(t, st, fake.Server()).Push(context.Background())
				if err != nil || result.Pushed != 1 {
					t.Fatalf("confirmation-only recovery = %+v, %v", result, err)
				}
				if beforePosts != 1 || fake.Count(http.MethodPost, postPath) != 1 || fake.Count(http.MethodGet, confirmPath) != 2 {
					t.Fatalf("recovery repeated POST or missed addressed GET: %+v", fake.Requests())
				}
			})
		}
	})
}

func TestStage1CBoundaryLeaseAndSettlementAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		name            string
		breakSettlement func(*testing.T, *store.Store, store.OutboxItem)
		wantLease       bool
	}{
		{name: "stale lease", wantLease: true, breakSettlement: func(t *testing.T, st *store.Store, claimed store.OutboxItem) {
			if _, err := st.DB().ExecContext(context.Background(), `UPDATE outbox SET inflight_at = inflight_at - 86400 WHERE seq = ?`, claimed.Seq); err != nil {
				t.Fatal(err)
			}
			items, _, err := st.Claim(context.Background(), 1, time.Minute)
			if err != nil || len(items) != 1 || items[0].LeaseToken == claimed.LeaseToken {
				t.Fatalf("take over stale lease = %+v, %v", items, err)
			}
		}},
		{name: "late SQL failure", breakSettlement: func(t *testing.T, st *store.Store, _ store.OutboxItem) {
			if _, err := st.DB().ExecContext(context.Background(), `CREATE TRIGGER stage1c_refuse_settlement
				BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT, 'late settlement refused'); END`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
			fake := newStage1CWireFake(t)
			fake.SeedProject("p1", "P1")
			stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
			if _, err := st.AddTaskItem(ctx, "t1", "two"); err != nil {
				t.Fatal(err)
			}
			claimed := stage1CBoundaryClaim(t, st)
			send, versioned, post, err := st.PrepareFeatureSend(ctx, claimed)
			if err != nil || !versioned || !post {
				t.Fatalf("prepare = %+v, %v/%v/%v", send, versioned, post, err)
			}
			confirmation := stage1CBoundaryConfirmation(t, send, "t1", "p1")
			if err := st.RecordFeatureEvidence(ctx, claimed, store.FeatureAccepted, confirmation); err != nil {
				t.Fatal(err)
			}
			tc.breakSettlement(t, st, claimed)
			before := stage1CHolderProtectedState(t, st)
			err = st.SettleFeature(ctx, claimed, confirmation)
			if err == nil {
				t.Fatal("settlement succeeded through forced boundary failure")
			}
			if tc.wantLease && !errors.Is(err, store.ErrLeaseLost) && !errors.Is(err, store.ErrNoOutboxRow) {
				t.Fatalf("stale settlement error = %v, want lease ownership refusal", err)
			}
			if after := stage1CHolderProtectedState(t, st); after != before {
				t.Fatalf("failed settlement partially changed registry/parent/queue/dirty/raw\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

func TestStage1CBoundaryRejectionAndReadFailureRetryRules(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("POST %d restores eligibility", status), func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
			fake := newStage1CWireFake(t)
			fake.SeedProject("p1", "P1")
			seeded := stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
			changed, err := st.AddTaskItem(ctx, "t1", "two")
			if err != nil {
				t.Fatal(err)
			}
			fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
				if request.Method == http.MethodPost && request.Path == "/open/v1/task/t1" {
					return stage1CWireRawResponse(status, []byte(`{"error":"refused"}`))
				}
				return response
			})
			_, _ = testSyncer(t, st, fake.Server()).Push(ctx)
			if metadata := stage1CBoundaryFeature(t, st); metadata.Phase != store.FeatureRejected {
				t.Fatalf("POST %d phase = %s, want rejected", status, metadata.Phase)
			}
			if id, state := stage1CIdentity(t, st, "t1", seeded.Items[0].Key); id != "old-1" || state != store.ItemBound {
				t.Fatalf("POST %d prior identity = (%q,%s), want bound old-1", status, id, state)
			}
			if id, state := stage1CIdentity(t, st, "t1", changed.Items[1].Key); id != "" || state != store.ItemUnbound {
				t.Fatalf("POST %d new identity = (%q,%s), want unbound", status, id, state)
			}
		})
	}

	t.Run("confirmation GET failure remains confirmation-only", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		if _, err := st.AddTaskItem(ctx, "t1", "two"); err != nil {
			t.Fatal(err)
		}
		confirmPath := "/open/v1/project/p1/task/t1"
		failed := true
		fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if failed && request.Method == http.MethodGet && request.Path == confirmPath {
				return stage1CWireRawResponse(http.StatusInternalServerError, []byte(`{"error":"read failed"}`))
			}
			return response
		})
		first, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("Push with failed confirmation GET: %v", err)
		}
		if first.Pushed != 0 || fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 {
			t.Fatalf("failed confirmation result = %+v, requests %+v", first, fake.Requests())
		}
		failed = false
		stage1CBoundaryRetryIfFailed(t, st)
		second, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil || second.Pushed != 1 {
			t.Fatalf("confirmation-only retry = %+v, %v", second, err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 || fake.Count(http.MethodGet, confirmPath) != 2 {
			t.Fatalf("failed read authorized another POST: %+v", fake.Requests())
		}
	})
}

func TestStage1CBoundaryPredecessorAndFrozenRevision(t *testing.T) {
	t.Run("failed Items predecessor blocks later body and pull", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		if _, err := st.AddTaskItem(ctx, "t1", "two"); err != nil {
			t.Fatal(err)
		}
		fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if request.Method == http.MethodGet && request.Path == "/open/v1/project/p1/task/t1" {
				return stage1CWireJSONResponse(http.StatusOK, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
			}
			return response
		})
		first, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil || first.Failed != 1 {
			t.Fatalf("mismatched predecessor Push = %+v, %v", first, err)
		}
		if _, err := st.AddTaskItem(ctx, "t1", "three"); err != nil {
			t.Fatal(err)
		}
		before, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		second, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("Push behind failed predecessor: %v", err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 || second.Pushed != 0 {
			t.Fatalf("later Items body crossed failed predecessor: result=%+v requests=%+v", second, fake.Requests())
		}
		fake.SetResponseHook(nil)
		if _, err := testSyncer(t, st, fake.Server()).Pull(ctx); err != nil {
			t.Fatalf("Pull with held local intent: %v", err)
		}
		after, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after.Items, before.Items) {
			t.Fatalf("Pull replaced intent held behind predecessor\nbefore=%+v\nafter=%+v", before.Items, after.Items)
		}
	})

	t.Run("older confirmation refreshes raw and preserves newer revision", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		if _, err := st.AddTaskItem(ctx, "t1", "two"); err != nil {
			t.Fatal(err)
		}
		reached, release := fake.HoldNext(http.MethodGet, "/open/v1/project/p1/task/t1")
		done := stage1CBoundaryStartPushOne(ctx, testSyncer(t, st, fake.Server()), stage1CBoundaryClaim(t, st))
		exchange := stage1CBoundaryWait(t, reached)
		if _, err := st.AddTaskItem(ctx, "t1", "three"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RenameTaskItem(ctx, "t1", 3, "temporary"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyUndo(ctx, stage1CLastUndo(t, st)); err != nil {
			t.Fatalf("undo rename while response held: %v", err)
		}
		want, err := st.MoveTaskItem(ctx, "t1", 3, 1)
		if err != nil {
			t.Fatal(err)
		}
		release()
		first := stage1CBoundaryFinishPush(t, done)
		if first.err != nil || first.result.Pushed != 1 {
			t.Fatalf("settle frozen response = %+v, %v", first.result, first.err)
		}
		got, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if !stage1CWritableItemsEqual(got.Items, want.Items) {
			t.Fatalf("older confirmation replaced newer checklist\nwant=%+v\ngot=%+v", want.Items, got.Items)
		}
		for i := range got.Items {
			if got.Items[i].Key != want.Items[i].Key {
				t.Fatalf("older confirmation changed newer key order\nwant=%+v\ngot=%+v", want.Items, got.Items)
			}
		}
		if raw := stage1CRaw(t, st, "t1"); raw != string(exchange.Response.Body) {
			t.Fatalf("settlement raw does not equal frozen remote proof\nwant=%s\ngot=%s", exchange.Response.Body, raw)
		}
		final, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil || final.Pushed < 1 {
			t.Fatalf("send newer revisions = %+v, %v", final, err)
		}
		settled, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if !stage1CWritableItemsEqual(settled.Items, want.Items) {
			t.Fatalf("newer send lost current checklist\nwant=%+v\ngot=%+v", want.Items, settled.Items)
		}
		for _, item := range settled.Items {
			id, state := stage1CIdentity(t, st, "t1", item.Key)
			if id == "" || id != item.Id || state != store.ItemBound {
				t.Fatalf("current key %q binding = (%q,%s), item id %q", item.Key, id, state, item.Id)
			}
		}
	})
}

func TestStage1CBoundaryParkedCurrentRevisionHoldsCompetingCompletePull(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
	if _, err := st.AddTaskItem(ctx, "t1", "held local"); err != nil {
		t.Fatal(err)
	}
	fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
		if request.Method == http.MethodGet && request.Path == "/open/v1/project/p1/task/t1" {
			return stage1CWireJSONResponse(http.StatusOK, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		}
		return response
	})
	result, err := testSyncer(t, st, fake.Server()).Push(ctx)
	if err != nil || result.Failed != 1 {
		t.Fatalf("park current revision = %+v, %v", result, err)
	}
	_, dirty, local := taskRow(t, st, "t1")
	if dirty != 0 || local != 0 {
		t.Fatalf("parked current revision left dirty=%d local=%d; dirty must already be released", dirty, local)
	}
	before, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw := stage1CRaw(t, st, "t1")
	beforeRegistry := stage1CRegistry(t, st, "t1")
	fake.SetResponseHook(nil)
	fake.MutateTask("p1", "t1", func(task map[string]any) {
		task["items"] = []any{stage1CItem("competing remote", "old-1", 0, 1, "")}
	})
	pull, err := testSyncer(t, st, fake.Server()).Pull(ctx)
	if err != nil {
		t.Fatalf("competing complete Pull: %v", err)
	}
	after, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Items, before.Items) || stage1CRaw(t, st, "t1") != beforeRaw ||
		!reflect.DeepEqual(stage1CRegistry(t, st, "t1"), beforeRegistry) {
		t.Fatalf("complete Pull crossed parked feature hold: result=%+v\nbefore=%+v\nafter=%+v", pull, before.Items, after.Items)
	}
}

func TestStage1CBoundaryParentIdentityEdges(t *testing.T) {
	t.Run("create renames local parent without collision", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "new", Items: []model.Item{{Title: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		key := created.Items[0].Key
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil || result.Pushed != 1 {
			t.Fatalf("create Push = %+v, %v", result, err)
		}
		if _, err := st.Task(ctx, created.Id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("local parent survived rename: %v", err)
		}
		got, err := st.Task(ctx, "remote-task-1")
		if err != nil || len(got.Items) != 1 || got.Items[0].Key != key {
			t.Fatalf("renamed parent = %+v, %v", got, err)
		}
		if id, state := stage1CIdentity(t, st, got.Id, key); id != got.Items[0].Id || state != store.ItemBound {
			t.Fatalf("renamed registry = (%q,%s), want (%q,bound)", id, state, got.Items[0].Id)
		}
	})

	t.Run("v1 twin collision preserves both parents and accepted id", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		twin := stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("remote-task-1", stage1CItem("twin", "twin-item", 0, 1, "")))
		stage1CBoundaryRegister(t, st, fake, twin.Id)
		created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "collision", Items: []model.Item{{Title: "new"}}})
		if err != nil {
			t.Fatal(err)
		}
		localRegistry := stage1CRegistry(t, st, created.Id)
		twinRegistry := stage1CRegistry(t, st, twin.Id)
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil || result.Failed != 1 {
			t.Fatalf("collision Push = %+v, %v", result, err)
		}
		if _, err := st.Task(ctx, twin.Id); err != nil {
			t.Fatalf("existing twin was lost: %v", err)
		}
		if _, err := st.Task(ctx, created.Id); err != nil {
			t.Fatalf("local create was lost: %v", err)
		}
		gotLocalRegistry := stage1CRegistry(t, st, created.Id)
		if len(gotLocalRegistry) != len(localRegistry) {
			t.Fatalf("local registry rows changed at collision: before=%+v after=%+v", localRegistry, gotLocalRegistry)
		}
		for i := range localRegistry {
			if gotLocalRegistry[i][0] != localRegistry[i][0] || gotLocalRegistry[i][1] != localRegistry[i][1] {
				t.Fatalf("local registry identity changed at collision: before=%+v after=%+v", localRegistry, gotLocalRegistry)
			}
		}
		if got := stage1CRegistry(t, st, twin.Id); !reflect.DeepEqual(got, twinRegistry) {
			t.Fatalf("existing twin registry changed at collision: before=%+v after=%+v", twinRegistry, got)
		}
		metadata := stage1CBoundaryFeature(t, st)
		if metadata.Phase != store.FeatureAccepted || metadata.AcceptedTaskID == nil || *metadata.AcceptedTaskID != twin.Id {
			t.Fatalf("collision lost accepted id evidence: %+v", metadata)
		}
	})

	t.Run("local deletion during create response never resurrects", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "gone", Items: []model.Item{{Title: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		reached, release := fake.HoldNext(http.MethodPost, "/open/v1/task")
		done := stage1CBoundaryStartPush(ctx, testSyncer(t, st, fake.Server()))
		stage1CBoundaryWait(t, reached)
		if err := st.DeleteTask(ctx, created.Id); err != nil {
			t.Fatalf("delete local parent while create held: %v", err)
		}
		release()
		stage1CBoundaryFinishPush(t, done)
		if _, err := st.Task(ctx, "remote-task-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("accepted id resurrected deleted parent: %v", err)
		}
		if _, err := st.Task(ctx, created.Id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("local parent resurrected after response: %v", err)
		}
	})

	t.Run("unverified versioned copy cannot authorize move drop", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		seedProject(t, st, model.Project{Id: "p2", Name: "P2"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		fake.SeedProject("p2", "P2")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		moved, err := st.MoveTask(ctx, "t1", "p2", store.MoveOptions{ByRecreate: true})
		if err != nil {
			t.Fatal(err)
		}
		fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if request.Method == http.MethodGet && request.Path == "/open/v1/project/p2/task/remote-task-1" {
				bad := stage1CRemoteTask("remote-task-1", stage1CItem("different", "remote-item-1", 0, 1, ""))
				bad["projectId"] = "p2"
				return stage1CWireJSONResponse(http.StatusOK, bad)
			}
			return response
		})
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("move Push: %v", err)
		}
		if result.Failed == 0 || fake.Count(http.MethodDelete, "/open/v1/project/p1/task/t1") != 0 {
			t.Fatalf("unverified copy authorized move drop: result=%+v requests=%+v", result, fake.Requests())
		}
		if _, ok := fake.Task("p1", "t1"); !ok {
			t.Fatal("move source was deleted remotely without a verified copy")
		}
		if _, err := st.Task(ctx, moved.Id); err != nil {
			t.Fatalf("unverified local copy disappeared: %v", err)
		}
	})
}

func TestStage1CBoundaryUnsafeParentAndLegacyPayloadNeverReachHTTP(t *testing.T) {
	t.Run("legacy ID-less checklist is refused before HTTP", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		if _, err := st.AddTaskItem(ctx, "t1", "idless"); err != nil {
			t.Fatal(err)
		}
		var seq int64
		var payload []byte
		if err := st.DB().QueryRowContext(ctx, `SELECT seq, payload FROM outbox WHERE task_id = 't1' ORDER BY seq LIMIT 1`).Scan(&seq, &payload); err != nil {
			t.Fatal(err)
		}
		var legacy map[string]json.RawMessage
		if err := json.Unmarshal(payload, &legacy); err != nil {
			t.Fatal(err)
		}
		delete(legacy, "_tt")
		payload, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET payload = ? WHERE seq = ?`, payload, seq); err != nil {
			t.Fatal(err)
		}
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("unsafe legacy Push: %v", err)
		}
		if fake.Count(http.MethodPost, "/open/v1/task/t1") != 0 || result.Failed != 1 {
			t.Fatalf("unsafe legacy checklist reached HTTP: result=%+v requests=%+v", result, fake.Requests())
		}
	})

	t.Run("failed local parent create blocks versioned update", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "local", Items: []model.Item{{Title: "one"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddTaskItem(ctx, created.Id, "two"); err != nil {
			t.Fatal(err)
		}
		fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if request.Method == http.MethodGet && request.Path == "/open/v1/project/p1/task/remote-task-1" {
				return stage1CWireJSONResponse(http.StatusOK, map[string]any{
					"id": "remote-task-1", "projectId": "p1", "title": "mismatch", "kind": "CHECKLIST",
					"items": []any{stage1CItem("different", "remote-item-1", 0, 1, "")},
				})
			}
			return response
		})
		result, err := testSyncer(t, st, fake.Server()).Push(ctx)
		if err != nil {
			t.Fatalf("failed local create Push: %v", err)
		}
		if result.Failed == 0 {
			t.Fatalf("failed create result = %+v", result)
		}
		if got := fake.Count(http.MethodPost, "/open/v1/task/"+created.Id); got != 0 {
			t.Fatalf("versioned update used unresolved local parent %q: requests=%+v", created.Id, fake.Requests())
		}
	})
}

func TestStage1CBoundaryRegisteredPullAndRawRecovery(t *testing.T) {
	t.Run("complete pull keeps surviving keys and retires absent bindings", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		seeded := stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
			stage1CItem("one", "old-1", 0, 1, ""), stage1CItem("two", "old-2", 0, 2, "")))
		if _, err := st.RenameTaskItem(ctx, "t1", 1, "registered item"); err != nil {
			t.Fatal(err)
		}
		stage1CVersionOneFixture(t, st, "t1")
		sy := testSyncer(t, st, fake.Server())
		if result, err := sy.Push(ctx); err != nil || result.Pushed != 1 {
			t.Fatalf("version-1 registration = %+v, %v", result, err)
		}
		firstKey, removedKey := seeded.Items[0].Key, seeded.Items[1].Key
		fake.MutateTask("p1", "t1", func(task map[string]any) {
			task["items"] = []any{stage1CItem("server one", "old-1", 0, 1, "")}
		})
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("complete Pull: %v", err)
		}
		got, err := st.Task(ctx, "t1")
		if err != nil || len(got.Items) != 1 || got.Items[0].Key != firstKey {
			t.Fatalf("complete pull task = %+v, %v", got, err)
		}
		if id, state := stage1CIdentity(t, st, "t1", removedKey); id != "" || state != store.ItemUnbound {
			t.Fatalf("absent binding = (%q,%s), want empty/unbound", id, state)
		}
	})

	t.Run("degraded raw retains model and later sound read recovers", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
			stage1CItem("one", "old-1", 0, 1, ""), stage1CItem("two", "old-2", 0, 2, "")))
		sy := stage1CBoundaryRegister(t, st, fake, "t1")
		before, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		registry := stage1CRegistry(t, st, "t1")
		fake.MutateTask("p1", "t1", func(task map[string]any) {
			items := task["items"].([]any)
			items[0].(map[string]any)["future"] = true
			items[0].(map[string]any)["title"] = "degraded server"
		})
		remote, _ := fake.Task("p1", "t1")
		wantRaw, err := json.Marshal(completeWireTask(remote))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("degraded Pull: %v", err)
		}
		degraded, err := st.Task(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(degraded.Items, before.Items) || !reflect.DeepEqual(stage1CRegistry(t, st, "t1"), registry) {
			t.Fatalf("degraded raw replaced modeled checklist or bindings\nbefore=%+v\nafter=%+v", before.Items, degraded.Items)
		}
		if raw := stage1CRaw(t, st, "t1"); raw != string(wantRaw) {
			t.Fatalf("degraded exact raw not retained\nwant=%s\ngot=%s", wantRaw, raw)
		}
		fake.MutateTask("p1", "t1", func(task map[string]any) {
			item := task["items"].([]any)[0].(map[string]any)
			delete(item, "future")
			item["title"] = "sound server"
		})
		if _, err := sy.Pull(ctx); err != nil {
			t.Fatalf("sound recovery Pull: %v", err)
		}
		recovered, err := st.Task(ctx, "t1")
		if err != nil || recovered.Items[0].Title != "sound server" || recovered.Items[0].Key != before.Items[0].Key {
			t.Fatalf("sound recovery = %+v, %v", recovered, err)
		}
	})

	t.Run("partial repeat confirmation retains prior sound item raw", func(t *testing.T) {
		ctx := context.Background()
		st := testStore(t)
		seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
		fake := newStage1CWireFake(t)
		fake.SeedProject("p1", "P1")
		stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
		sy := stage1CBoundaryRegister(t, st, fake, "t1")
		oldRaw := stage1CRaw(t, st, "t1")
		if _, err := st.SetTaskRepeat(ctx, "t1", "RRULE:FREQ=WEEKLY;INTERVAL=1"); err != nil {
			t.Fatal(err)
		}
		fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
			if request.Method == http.MethodGet && request.Path == "/open/v1/project/p1/task/t1" {
				return stage1CWireJSONResponse(http.StatusOK, map[string]any{
					"id": "t1", "projectId": "p1", "repeatFlag": "RRULE:FREQ=WEEKLY;INTERVAL=1",
				})
			}
			return response
		})
		result, err := sy.Push(ctx)
		if err != nil || result.Pushed != 1 {
			t.Fatalf("partial repeat confirmation = %+v, %v", result, err)
		}
		if raw := stage1CRaw(t, st, "t1"); raw != oldRaw {
			t.Fatalf("partial confirmation replaced sound item raw\nbefore=%s\nafter=%s", oldRaw, raw)
		}
		got, err := st.Task(ctx, "t1")
		if err != nil || got.RepeatFlag != "RRULE:FREQ=WEEKLY;INTERVAL=1" || len(got.Items) != 1 || got.Items[0].Key == "" {
			t.Fatalf("partial repeat settlement = %+v, %v", got, err)
		}
	})
}
