package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestConfirmedEmptyChecklistPullAcceptsOnlyRetiredIdentities(t *testing.T) {
	for _, kind := range []string{"", "TEXT", "CHECKLIST"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st, _ := scalarPullFixture(t, kind)
			for _, key := range []string{"retired-one", "retired-two"} {
				if err := setItemIdentity(ctx, st.DB(), ItemIdentity{TaskID: "t1", ItemKey: key, State: ItemUnbound}); err != nil {
					t.Fatal(err)
				}
			}
			identities, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			response := scalarPullResponse(t, "TEXT")
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			before := stage1BStateOf(t, st, "t1")
			result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, fence)
			if err != nil || result.Upserted != 1 || result.Skipped != 0 || result.Stale {
				t.Fatalf("confirmed empty pull: %+v %v", result, err)
			}
			after := stage1BStateOf(t, st, "t1")
			if !samePulledTask(after.Task, response.Task) || after.Registry != before.Registry || after.Outbox != before.Outbox ||
				after.Undo != before.Undo || after.Events != before.Events || after.Dirty != 0 {
				t.Fatalf("pull changed unrelated state: %+v -> %+v", before, after)
			}
			kept, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil || !reflect.DeepEqual(kept, identities) {
				t.Fatalf("retired identity history changed: %+v -> %+v (%v)", identities, kept, err)
			}
		})
	}
}

func TestConfirmedEmptyChecklistPullRejectsUnprovenKindAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name, currentKind, responseKind string
		state                           ItemIdentityState
		serverID                        string
	}{
		{"bound", "CHECKLIST", "TEXT", ItemBound, "old-id"},
		{"uncertain", "CHECKLIST", "TEXT", ItemUncertain, "old-id"},
		{"abandoned", "CHECKLIST", "TEXT", ItemAbandoned, "old-id"},
		{"abandoned empty id", "CHECKLIST", "TEXT", ItemAbandoned, ""},
		{"current note", "NOTE", "TEXT", ItemUnbound, ""},
		{"incoming note", "TEXT", "NOTE", ItemUnbound, ""},
		{"current unknown kind", "FUTURE", "TEXT", ItemUnbound, ""},
		{"incoming unknown kind", "CHECKLIST", "FUTURE", ItemUnbound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, _ := scalarPullFixture(t, tc.currentKind)
			for _, identity := range []ItemIdentity{
				{TaskID: "t1", ItemKey: "safe-unbound", State: ItemUnbound},
				{TaskID: "t1", ItemKey: "other", State: tc.state, ServerID: tc.serverID},
			} {
				if err := setItemIdentity(ctx, st.DB(), identity); err != nil {
					t.Fatal(err)
				}
			}
			before := stage1BStateOf(t, st, "t1")
			identities, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{scalarPullResponse(t, tc.responseKind)}, fence)
			if err != nil || result.Upserted != 0 || result.Skipped != 1 {
				t.Fatalf("unsafe empty pull: %+v %v", result, err)
			}
			after := stage1BStateOf(t, st, "t1")
			after.Epoch = before.Epoch
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("unsafe pull changed modeled state: %+v -> %+v", before, after)
			}
			kept, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil || !reflect.DeepEqual(kept, identities) {
				t.Fatalf("unsafe pull changed identities: %+v -> %+v (%v)", identities, kept, err)
			}
		})
	}
}

func clearLastChecklistItemThroughConfirmation(t *testing.T) (*Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	initial := openTask("t1", "p1", "Checklist")
	initial.Kind = "CHECKLIST"
	initial.Items = []model.Item{{Id: "i1", Title: "last", SortOrder: 17}}
	raw := json.RawMessage(`{"id":"t1","projectId":"p1","title":"Checklist","kind":"CHECKLIST","items":[{"id":"i1","title":"last","status":0,"sortOrder":17,"startDate":"","isAllDay":false,"timeZone":"","completedTime":""}]}`)
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: initial, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	removed, err := st.RemoveTaskItem(ctx, "t1", 1)
	if err != nil || len(removed.Items) != 0 || removed.Kind != "CHECKLIST" {
		t.Fatalf("remove last item: %+v %v", removed, err)
	}
	staleFence, err := st.CapturePullFence(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	claims, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim removal: %+v %v", claims, err)
	}
	send, present, post, err := st.PrepareFeatureSend(ctx, claims[0])
	if err != nil || !present || !post || send.Metadata.Version != FeatureReplacementPayloadVersion ||
		send.Edit.Kind != nil || send.Metadata.Fields.Kind || send.Metadata.Snapshot.Kind != nil ||
		send.Metadata.Snapshot.Items == nil || len(*send.Metadata.Snapshot.Items) != 0 {
		t.Fatalf("actual frozen last-removal intent: %+v %v", send, err)
	}
	frozen, err := json.Marshal(send.Metadata.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	response := ServerTask{Task: openTask("t1", "p1", "Server scalar after removal"),
		Raw: json.RawMessage(`{"id":"t1","projectId":"p1","kind":"TEXT","title":"Server scalar after removal"}`)}
	response.Task.Kind = "TEXT"
	confirmation, err := ConfirmFeatureResponse(send, "t1", "p1", response.Raw)
	if err != nil || len(confirmation.Bindings) != 0 {
		t.Fatalf("confirm actual nil-kind empty intent: %+v %v", confirmation, err)
	}
	afterConfirmation, _ := json.Marshal(send.Metadata.Snapshot)
	if string(afterConfirmation) != string(frozen) {
		t.Fatal("confirmation rewrote the frozen intent")
	}
	if err := st.RecordFeatureEvidence(ctx, claims[0], FeatureAccepted, confirmation); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleFeature(ctx, claims[0], confirmation); err != nil {
		t.Fatal(err)
	}
	settled := stage1BStateOf(t, st, "t1")
	if settled.Task.Kind != "CHECKLIST" || settled.Task.Title != "Checklist" || len(settled.Task.Items) != 0 || settled.Dirty != 0 || settled.Outbox != 0 {
		t.Fatalf("settle overwrote modeled fields or left queue: %+v", settled)
	}
	identities, err := itemIdentities(ctx, st.DB(), "t1")
	if err != nil || len(identities) != 1 || identities[0].State != ItemUnbound || identities[0].ServerID != "" || identities[0].ItemKey != "i1" {
		t.Fatalf("clear identity result: %+v %v", identities, err)
	}
	result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, staleFence)
	if err != nil || !result.Stale || result.Upserted != 0 {
		t.Fatalf("pre-clear fence accepted: %+v %v", result, err)
	}
	if current := stage1BStateOf(t, st, "t1"); !reflect.DeepEqual(current, settled) {
		t.Fatalf("stale pull changed settlement: %+v -> %+v", settled, current)
	}
	fence, err := st.CapturePullFence(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	result, err = st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, fence)
	if err != nil || result.Stale || result.Upserted != 1 || result.Skipped != 0 {
		t.Fatalf("fresh clear pull: %+v %v", result, err)
	}
	converged := stage1BStateOf(t, st, "t1")
	if !samePulledTask(converged.Task, response.Task) || converged.Outbox != settled.Outbox ||
		converged.Undo != settled.Undo || converged.Events != settled.Events || converged.Registry != settled.Registry {
		t.Fatalf("kind convergence changed unrelated state: %+v -> %+v", settled, converged)
	}
	return st, converged.Task
}

func TestConfirmedLastRemovalConvergesKindOnlyThroughFreshPull(t *testing.T) {
	clearLastChecklistItemThroughConfirmation(t)
}

func TestConfirmedLastRemovalAllowsSafeUndoAndReadd(t *testing.T) {
	for _, action := range []string{"undo", "readd"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			st, _ := clearLastChecklistItemThroughConfirmation(t)
			var next model.Task
			var err error
			if action == "undo" {
				top, err := st.LastUndo(ctx)
				if err != nil {
					t.Fatal(err)
				}
				next, err = st.ApplyUndo(ctx, top)
			} else {
				next, err = st.AddTaskItem(ctx, "t1", "new after clear")
			}
			if err != nil {
				t.Fatalf("%s after confirmed clear: %+v %v", action, next, err)
			}
			next, err = st.Task(ctx, "t1")
			if err != nil || len(next.Items) != 1 || next.Items[0].Id != "" {
				t.Fatalf("%s authoritative identity after reopen: %+v %v", action, next, err)
			}
			key := next.Items[0].Key
			if action == "undo" && (key != "i1" || next.Items[0].SortOrder != 17 || next.Kind != "TEXT") {
				t.Fatalf("undo lost original key/rank or forced kind: %+v", next)
			}
			if action == "readd" && (key == "i1" || !IsLocalID(key) || next.Kind != "CHECKLIST") {
				t.Fatalf("re-add reused a retired key or lost requested kind: %+v", next)
			}
			claims, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claim %s: %+v %v", action, claims, err)
			}
			send, present, post, err := st.PrepareFeatureSend(ctx, claims[0])
			if err != nil || !present || !post || len(*send.Metadata.Snapshot.Items) != 1 ||
				(*send.Metadata.Snapshot.Items)[0].Key != key || (*send.Metadata.Snapshot.Items)[0].ID != "" ||
				len(*send.Metadata.Snapshot.PriorBindings) != 0 {
				t.Fatalf("%s did not freeze allocating replacement: %+v %v", action, send, err)
			}
			if action == "undo" && (send.Metadata.Snapshot.Kind != nil || send.Metadata.Fields.Kind) {
				t.Fatal("undo gained a kind intent")
			}
			item := next.Items[0]
			raw, err := json.Marshal(map[string]any{
				"id": "t1", "projectId": "p1", "kind": "CHECKLIST", "title": next.Title,
				"items": []map[string]any{{"id": "new-server-id", "title": item.Title, "status": 0,
					"sortOrder": item.SortOrder, "startDate": "", "isAllDay": false, "timeZone": "", "completedTime": ""}},
			})
			if err != nil {
				t.Fatal(err)
			}
			confirmation, err := ConfirmFeatureResponse(send, "t1", "p1", raw)
			if err != nil || len(confirmation.Bindings) != 1 || confirmation.Bindings[0].Key != key || confirmation.Bindings[0].ID != "new-server-id" {
				t.Fatalf("%s confirmation: %+v %v", action, confirmation, err)
			}
			if err := st.RecordFeatureEvidence(ctx, claims[0], FeatureAccepted, confirmation); err != nil {
				t.Fatal(err)
			}
			if err := st.SettleFeature(ctx, claims[0], confirmation); err != nil {
				t.Fatal(err)
			}
			response := next
			response.Kind = "CHECKLIST"
			response.Items = append([]model.Item(nil), next.Items...)
			response.Items[0].Id = "new-server-id"
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			if result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{{Task: response, Raw: raw}}, fence); err != nil || result.Skipped != 0 || result.Stale {
				t.Fatalf("%s final pull: %+v %v", action, result, err)
			}
			got, err := st.Task(ctx, "t1")
			if err != nil || got.Kind != "CHECKLIST" || len(got.Items) != 1 || got.Items[0].Key != key || got.Items[0].Id != "new-server-id" {
				t.Fatalf("%s final task: %+v %v", action, got, err)
			}
		})
	}
}

func TestEmptyRetainedRawRefusesUnprovenChecklistMutation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		state     ItemIdentityState
		serverID  string
	}{
		{"no history", `{"id":"t1","projectId":"p1","kind":"TEXT"}`, "", ""},
		{"bound", `{"id":"t1","projectId":"p1","kind":"TEXT"}`, ItemBound, "old-id"},
		{"uncertain", `{"id":"t1","projectId":"p1","kind":"TEXT"}`, ItemUncertain, "old-id"},
		{"abandoned", `{"id":"t1","projectId":"p1","kind":"TEXT"}`, ItemAbandoned, "old-id"},
		{"null array", `{"id":"t1","projectId":"p1","kind":"TEXT","items":null}`, ItemUnbound, ""},
		{"missing task address", `{"projectId":"p1","kind":"TEXT"}`, ItemUnbound, ""},
		{"wrong task address", `{"id":"other","projectId":"p1","kind":"TEXT"}`, ItemUnbound, ""},
		{"wrong project address", `{"id":"t1","projectId":"other","kind":"TEXT"}`, ItemUnbound, ""},
		{"missing kind", `{"id":"t1","projectId":"p1"}`, ItemUnbound, ""},
		{"note kind", `{"id":"t1","projectId":"p1","kind":"NOTE"}`, ItemUnbound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, _ := scalarPullFixture(t, "TEXT")
			if tc.state != "" {
				if err := setItemIdentity(ctx, st.DB(), ItemIdentity{TaskID: "t1", ItemKey: "old", State: tc.state, ServerID: tc.serverID}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.DB().ExecContext(ctx, `UPDATE tasks SET raw=? WHERE id='t1'`, tc.raw); err != nil {
				t.Fatal(err)
			}
			before := stage1BStateOf(t, st, "t1")
			if _, err := st.AddTaskItem(ctx, "t1", "cannot infer an empty checklist"); !errors.Is(err, ErrUnsafeChecklist) {
				t.Fatalf("unproven mutation: %v", err)
			}
			if after := stage1BStateOf(t, st, "t1"); !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed state: %+v -> %+v", before, after)
			}
		})
	}
}

func TestConfirmedLastRemovalAllowsSeveralOfflineEdits(t *testing.T) {
	ctx := context.Background()
	st, _ := clearLastChecklistItemThroughConfirmation(t)
	first, err := st.AddTaskItem(ctx, "t1", "first new")
	if err != nil || len(first.Items) != 1 {
		t.Fatalf("first add: %+v %v", first, err)
	}
	second, err := st.AddTaskItem(ctx, "t1", "second new")
	if err != nil || len(second.Items) != 2 {
		t.Fatalf("second offline add: %+v %v", second, err)
	}
	renamed, err := st.RenameTaskItem(ctx, "t1", 1, "renamed offline")
	if err != nil || len(renamed.Items) != 2 || renamed.Items[0].Title != "renamed offline" ||
		renamed.Items[0].Key != first.Items[0].Key || renamed.Items[1].Key != second.Items[1].Key {
		t.Fatalf("offline rename: %+v %v", renamed, err)
	}
	identities, err := itemIdentities(ctx, st.DB(), "t1")
	if err != nil || len(identities) != 3 {
		t.Fatalf("offline identities: %+v %v", identities, err)
	}
	for _, identity := range identities {
		if identity.State != ItemUnbound || identity.ServerID != "" {
			t.Fatalf("offline edit acquired a server identity: %+v", identity)
		}
	}
	fence, err := st.CapturePullFence(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, st, "t1")
	result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{scalarPullResponse(t, "TEXT")}, fence)
	if err != nil || result.Upserted != 0 || result.Skipped != 1 {
		t.Fatalf("scalar pull accepted over new local items: %+v %v", result, err)
	}
	after := stage1BStateOf(t, st, "t1")
	after.Epoch = before.Epoch
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("pull changed offline edits: %+v -> %+v", before, after)
	}
}

func TestConfirmedEmptyRawProofRejectsInvalidIdentityAndUnknownLocalKey(t *testing.T) {
	raw := []byte(`{"id":"t1","projectId":"p1","kind":"TEXT"}`)
	current := model.Task{Id: "t1", ProjectId: "p1", Kind: "CHECKLIST", Items: []model.Item{{Key: "known"}}}
	identities := []ItemIdentity{{TaskID: "t1", ItemKey: "known", State: ItemUnbound}}
	if !confirmedEmptyChecklistRaw(current, raw, identities) {
		t.Fatal("known unbound local item lost confirmed remote-empty proof")
	}

	identities[0].ServerID = "invalid-retired-id"
	if confirmedEmptyChecklistRaw(current, raw, identities) {
		t.Fatal("unbound identity with server ID passed proof")
	}
	identities[0].ServerID = ""
	current.Items[0].Key = "unknown"
	if confirmedEmptyChecklistRaw(current, raw, identities) {
		t.Fatal("local item without registry provenance passed proof")
	}
	current.Items[0].Key = ""
	if confirmedEmptyChecklistRaw(current, raw, identities) {
		t.Fatal("empty local item key passed proof")
	}
}
