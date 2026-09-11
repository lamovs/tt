package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func stage1CVersionOneFixture(t *testing.T, st *store.Store, taskID string) {
	t.Helper()
	var seq int64
	var payload []byte
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT seq, payload FROM outbox WHERE task_id = ? ORDER BY seq LIMIT 1`, taskID).Scan(&seq, &payload); err != nil {
		t.Fatal(err)
	}
	edit, metadata, err := store.DecodeTaskEditPayload(payload)
	if err != nil || metadata == nil || metadata.Version != 2 || metadata.Phase != store.FeaturePrepared || !metadata.Fields.Items {
		t.Fatalf("version-1 fixture requires a new prepared checklist update: %+v, %v", metadata, err)
	}
	metadata.Version = 1
	payload, err = store.EncodeTaskEditPayload(edit, *metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE outbox SET payload = ? WHERE seq = ?`, string(payload), seq); err != nil {
		t.Fatal(err)
	}
}

func TestChecklistReplacementV2LifecyclePreservesLogicalKeys(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "replacement", Items: []model.Item{{Title: "same"}, {Title: "same"}}})
	if err != nil {
		t.Fatal(err)
	}
	if metadata := stage1CBoundaryFeature(t, st); metadata.Version != 2 {
		t.Fatalf("new checklist create version = %d", metadata.Version)
	}
	if result := stage1CPush(t, st, fake); result.Pushed != 1 {
		t.Fatalf("create = %+v", result)
	}
	current := stage1CTask(t, st, "remote-task-1")
	if !reflect.DeepEqual(stage1CKeys(current), stage1CKeys(created)) {
		t.Fatal("create replaced private keys")
	}
	steps := []struct {
		name string
		run  func() (model.Task, error)
	}{
		{"rename", func() (model.Task, error) { return st.RenameTaskItem(ctx, current.Id, 1, "renamed") }},
		{"done", func() (model.Task, error) { return st.SetTaskItemDone(ctx, current.Id, 2, true) }},
		{"undo done", func() (model.Task, error) { return st.ApplyUndo(ctx, stage1CLastUndo(t, st)) }},
		{"move", func() (model.Task, error) { return st.MoveTaskItem(ctx, current.Id, 2, 1) }},
		{"remove", func() (model.Task, error) { return st.RemoveTaskItem(ctx, current.Id, 2) }},
		{"undo remove", func() (model.Task, error) { return st.ApplyUndo(ctx, stage1CLastUndo(t, st)) }},
		{"add", func() (model.Task, error) { return st.AddTaskItem(ctx, current.Id, "same") }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			priorIDs := map[string]bool{}
			for _, item := range current.Items {
				priorIDs[item.Id] = true
			}
			want, err := step.run()
			if err != nil {
				t.Fatal(err)
			}

			want = stage1CTask(t, st, want.Id)
			if metadata := stage1CBoundaryFeature(t, st); metadata.Version != 2 {
				t.Fatalf("new update version = %d", metadata.Version)
			}
			if undo := stage1CLastUndo(t, st); undo.Feature == nil || undo.Feature.Version != 1 {
				t.Fatalf("undo metadata = %+v", undo.Feature)
			}
			if result := stage1CPush(t, st, fake); result.Pushed != 1 {
				t.Fatalf("replacement = %+v", result)
			}
			current = stage1CTask(t, st, current.Id)
			if !reflect.DeepEqual(stage1CKeys(current), stage1CKeys(want)) || !stage1CWritableItemsEqual(current.Items, want.Items) {
				t.Fatalf("replacement changed logical state: got=%+v want=%+v", current.Items, want.Items)
			}
			for _, item := range current.Items {
				if item.Id == "" || priorIDs[item.Id] {
					t.Fatalf("replacement retained prior ID %q", item.Id)
				}
			}
		})
	}
	for _, post := range stage1CPostRequests(fake) {
		for _, item := range stage1CDecodeItems(t, stage1CDecodeObject(t, post.Body)["items"]) {
			if id, present := item["id"]; present && string(id) != `""` {
				t.Fatalf("allocating request carried an item ID: %s", post.Body)
			}
		}
	}
}

func TestChecklistReplacementV2ArmRetainsPriorIDsAndRejectRestores(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	seeded := stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, ""), stage1CItem("two", "old-2", 0, 2, "")))
	changed, err := st.AddTaskItem(ctx, "t1", "three")
	if err != nil {
		t.Fatal(err)
	}
	claimed := stage1CBoundaryClaim(t, st)
	send, versioned, post, err := st.PrepareFeatureSend(ctx, claimed)
	if err != nil || !versioned || !post || send.Metadata.Version != 2 {
		t.Fatalf("arm = %+v, %v/%v/%v", send, versioned, post, err)
	}
	frozen, _ := json.Marshal(send.Metadata.Snapshot)
	for i, item := range *send.Metadata.Snapshot.Items {
		if item.ID != "" {
			t.Fatalf("frozen item %d has ID %q", i, item.ID)
		}
		id, state := stage1CIdentity(t, st, "t1", item.Key)
		wantID := ""
		if i < len(seeded.Items) {
			wantID = seeded.Items[i].Id
		}
		if id != wantID || state != store.ItemUncertain {
			t.Fatalf("uncertain item %d = %q/%s want %q", i, id, state, wantID)
		}
	}
	if err := st.RejectFeature(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	for i, item := range changed.Items {
		id, state := stage1CIdentity(t, st, "t1", item.Key)
		if i < 2 && (id != seeded.Items[i].Id || state != store.ItemBound) || i == 2 && (id != "" || state != store.ItemUnbound) {
			t.Fatalf("rejected identity %d = %q/%s", i, id, state)
		}
	}
	again, _, post, err := st.PrepareFeatureSend(ctx, claimed)
	againFrozen, _ := json.Marshal(again.Metadata.Snapshot)
	if err != nil || !post || !bytes.Equal(frozen, againFrozen) {
		t.Fatalf("rejected retry changed frozen body: %s -> %s, post=%v err=%v", frozen, againFrozen, post, err)
	}
	_, _, post, err = st.PrepareFeatureSend(ctx, claimed)
	if err != nil || post {
		t.Fatalf("armed retry authorized another POST: %v, %v", post, err)
	}
}

func TestChecklistReplacementV1KnownIDsRemainStrict(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
	if _, err := st.RenameTaskItem(ctx, "t1", 1, "renamed"); err != nil {
		t.Fatal(err)
	}
	stage1CVersionOneFixture(t, st, "t1")
	claimed := stage1CBoundaryClaim(t, st)
	send, _, post, err := st.PrepareFeatureSend(ctx, claimed)
	if err != nil || !post || send.Metadata.Version != 1 || (*send.Metadata.Snapshot.Items)[0].ID != "old-1" {
		t.Fatalf("version-1 frozen meaning changed: %+v, %v", send, err)
	}
	proof := stage1CBoundaryConfirmation(t, send, "t1", "p1")
	changed := bytes.ReplaceAll(proof.Raw, []byte(`"old-1"`), []byte(`"fresh-1"`))
	if _, err := store.ConfirmFeatureResponse(send, "t1", "p1", changed); err == nil {
		t.Fatal("version 1 accepted replacement of its known ID")
	}
	before, _ := json.Marshal(send.Metadata.Snapshot)
	again, _, post, err := st.PrepareFeatureSend(ctx, claimed)
	after, _ := json.Marshal(again.Metadata.Snapshot)
	if err != nil || post || again.Metadata.Version != 1 || !bytes.Equal(before, after) {
		t.Fatal("armed version 1 was rewritten or authorized another POST")
	}
}

func TestChecklistReplacementV2ConfirmationRejectsUnsafeCorrespondence(t *testing.T) {
	for _, mutation := range []string{"none", "old IDs", "mixed IDs", "duplicate IDs", "collapsed ranks", "reordered", "missing", "extra", "unknown field", "ambiguous"} {
		t.Run(mutation, func(t *testing.T) {
			want := []store.FeatureItemSnapshot{{Key: "key-1", Title: "same", SortOrder: 1}, {Key: "key-2", Title: "same", SortOrder: 2}}
			prior := []store.FeatureItemBinding{{Key: "key-1", ID: "old-1"}, {Key: "key-2", ID: "old-2"}}
			send := store.FeatureSend{Metadata: store.FeaturePayloadMetadata{Version: 2, Fields: store.FeatureFields{Items: true}, Snapshot: &store.FeatureSnapshot{Items: &want, PriorBindings: &prior}}}
			items := []any{stage1CItem("same", "fresh-1", 0, 1, ""), stage1CItem("same", "fresh-2", 0, 2, "")}
			switch mutation {
			case "old IDs":
				for i := range items {
					items[i].(map[string]any)["id"] = fmt.Sprintf("old-%d", i+1)
				}
			case "mixed IDs":
				items[1].(map[string]any)["id"] = "old-1"
			case "duplicate IDs":
				items[1].(map[string]any)["id"] = "fresh-1"
			case "collapsed ranks":
				items[1].(map[string]any)["sortOrder"] = 1
			case "reordered":
				items[0], items[1] = items[1], items[0]
			case "missing":
				items = items[:1]
			case "extra":
				items = append(items, stage1CItem("extra", "fresh-3", 0, 3, ""))
			case "unknown field":
				items[0].(map[string]any)["future"] = true
			case "ambiguous":
				want[1].SortOrder = 1
				items[1].(map[string]any)["sortOrder"] = 1
			}
			raw, err := json.Marshal(map[string]any{"id": "t1", "projectId": "p1", "items": items})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(send)
			confirmation, err := store.ConfirmFeatureResponse(send, "t1", "p1", raw)
			if (err == nil) != (mutation == "none") {
				t.Fatalf("confirmation = %+v, %v", confirmation, err)
			}
			after, _ := json.Marshal(send)
			if !bytes.Equal(before, after) || !bytes.Equal(confirmation.Raw, raw) {
				t.Fatal("confirmation changed snapshot or raw evidence")
			}
		})
	}
}

func TestChecklistReplacementV2LostReplyRecoversWithReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	st := stage1COpenStore(t, path)
	t.Cleanup(func() { st.Close() })
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1", stage1CItem("one", "old-1", 0, 1, "")))
	if _, err := st.RenameTaskItem(ctx, "t1", 1, "renamed"); err != nil {
		t.Fatal(err)
	}
	fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
		if request.Method == http.MethodPost {
			return stage1CWireRawResponse(http.StatusInternalServerError, []byte(`{"error":"reply lost"}`))
		}
		return response
	})
	first, _ := testSyncer(t, st, fake.Server()).Push(ctx)
	if first.Pushed != 0 || stage1CBoundaryFeature(t, st).Version != 2 {
		t.Fatalf("lost reply = %+v", first)
	}
	if id, state := stage1CIdentity(t, st, "t1", "old-1"); id != "old-1" || state != store.ItemUncertain {
		t.Fatalf("uncertain provenance = %q/%s", id, state)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = stage1COpenStore(t, path)
	fake.SetResponseHook(nil)
	stage1CBoundaryRetryIfFailed(t, st)
	second, err := testSyncer(t, st, fake.Server()).Push(ctx)
	if err != nil || second.Pushed != 1 || fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 {
		t.Fatalf("read-only recovery = %+v, %v, requests=%+v", second, err, fake.Requests())
	}
}
