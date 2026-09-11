package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func entityFixture(t *testing.T, st *Store, kind, id, data string) EntityRef {
	t.Helper()
	ref := EntityRef{Kind: kind, Key: id}
	if err := st.MergeEntities(context.Background(), kind, "", []ResourceEntity{{Ref: ref, ServerID: id, Data: json.RawMessage(data)}}, false); err != nil {
		t.Fatal(err)
	}
	return ref
}

func entityMutationFixture(t *testing.T, st *Store, mutation EntityMutation) EntityMutationOutcome {
	t.Helper()
	preview, err := st.PreviewEntityMutation(context.Background(), mutation)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.ApplyEntityMutation(context.Background(), preview)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestEntityOperationsLegacyQueueIsolation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	outcome := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "project"}, Action: "create", Patch: json.RawMessage(`{"name":"new"}`)})
	if _, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	legacy, _, err := st.Claim(ctx, 10, time.Minute)
	if err != nil || len(legacy) != 1 || legacy[0].Op != OpTaskUpdate {
		t.Fatalf("legacy claim: %+v %v", legacy, err)
	}
	claimed, err := st.ClaimEntityOperations(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("entity claim: %+v %v", claimed, err)
	}
	op, err := st.ArmEntityOperation(ctx, outcome.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil || op.Phase != "armed" {
		t.Fatalf("arm: %+v %v", op, err)
	}
	if err := st.FailEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "connection lost", false); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RetryFailed(ctx); err != nil || n != 0 {
		t.Fatalf("legacy retry touched entities: %d %v", n, err)
	}
	if dropped, err := st.DropParked(ctx); err != nil || len(dropped) != 0 {
		t.Fatalf("legacy drop touched entities: %+v %v", dropped, err)
	}
	if next, err := st.ClaimEntityOperations(ctx, 10, time.Minute); err != nil || len(next) != 0 {
		t.Fatalf("auto retry uncertain: %+v %v", next, err)
	}
	if err := st.CancelEntityOperation(ctx, op.Item.Seq, op.Revision); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("cancel uncertain: %v", err)
	}
	if err := st.RetryEntityOperation(ctx, op.Item.Seq, op.Revision); err != nil {
		t.Fatal(err)
	}
	recovery, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(recovery) != 1 || recovery[0].Phase != "uncertain" {
		t.Fatalf("recovery claim: %+v %v", recovery, err)
	}
	if _, err := st.ArmEntityOperation(ctx, op.Item.Seq, recovery[0].Item.LeaseToken); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("uncertain POST rearmed: %v", err)
	}
}

func TestEntityOperationsDependenciesAndTypedBinding(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	parent := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "project"}, Action: "create", Patch: json.RawMessage(`{"name":"parent"}`)})
	text := `{"name":"literal ` + parent.Entity.Ref.Key + `"}`
	child := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "column"}, Action: "create", Patch: json.RawMessage(text),
		References: []EntityReference{{Field: "projectId", Ref: parent.Entity.Ref}}})
	if err := st.CancelEntityOperation(ctx, parent.OperationSeq, parent.Entity.Revision); !errors.Is(err, ErrEntityDependency) {
		t.Fatalf("parent canceled with child: %v", err)
	}
	claimed, err := st.ClaimEntityOperations(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Item.Seq != parent.OperationSeq {
		t.Fatalf("dependency order: %+v %v", claimed, err)
	}
	op, err := st.ArmEntityOperation(ctx, parent.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "server-project", json.RawMessage(`{"name":"parent"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "server-project", json.RawMessage(`{"name":"parent"}`)); err != nil {
		t.Fatal(err)
	}
	claimed, err = st.ClaimEntityOperations(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Item.Seq != child.OperationSeq {
		t.Fatalf("child claim: %+v %v", claimed, err)
	}
	op, err = st.ArmEntityOperation(ctx, child.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]string
	if err := json.Unmarshal(op.Request, &request); err != nil {
		t.Fatal(err)
	}
	if request["projectId"] != "server-project" || request["name"] != "literal "+parent.Entity.Ref.Key {
		t.Fatalf("typed remap corrupted data: %s", op.Request)
	}
}

func TestEntityOperationsDirtyMergeAndConflict(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ref := entityFixture(t, st, "project", "p1", `{"name":"before","color":"red"}`)
	preview, err := st.PreviewEntityMutation(ctx, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"local"}`)})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.ApplyEntityMutation(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyEntityMutation(ctx, preview); !errors.Is(err, ErrEntityChanged) {
		t.Fatalf("stale preview accepted: %v", err)
	}
	if err := st.MergeEntities(ctx, "project", "", []ResourceEntity{{ServerID: "p1", Data: json.RawMessage(`{"name":"remote","color":"blue"}`)}}, false); err != nil {
		t.Fatal(err)
	}
	entity, err := st.Entity(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !entity.Dirty || !entityJSONEqual(entity.Data, json.RawMessage(`{"name":"local","color":"blue"}`)) || !entityJSONEqual(entity.Base, json.RawMessage(`{"name":"remote","color":"blue"}`)) {
		t.Fatalf("dirty overlay lost: %+v", entity)
	}
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := st.ArmEntityOperation(ctx, outcome.OperationSeq, claimed[0].Item.LeaseToken); !errors.Is(err, ErrEntityChanged) {
		t.Fatalf("same-field conflict sent: %v", err)
	}
}

func TestEntityOperationsDistinctFieldMergeAndOrdering(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ref := entityFixture(t, st, "project", "p1", `{"name":"before","color":"red"}`)
	first := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"first"}`)})
	second := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"second"}`)})
	claimed, err := st.ClaimEntityOperations(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("serialized: %+v %v", claimed, err)
	}
	op, err := st.ArmEntityOperation(ctx, first.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "p1", json.RawMessage(`{"name":"first","color":"blue"}`)); err != nil {
		t.Fatal(err)
	}
	entity, _ := st.Entity(ctx, ref)
	if !entity.Dirty || !entityJSONEqual(entity.Data, json.RawMessage(`{"name":"second","color":"blue"}`)) {
		t.Fatalf("later overlay overwritten: %+v", entity)
	}
	claimed, err = st.ClaimEntityOperations(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("second claim: %+v %v", claimed, err)
	}
	op, err = st.ArmEntityOperation(ctx, second.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "p1", json.RawMessage(`{"name":"second","color":"blue"}`)); err != nil {
		t.Fatal(err)
	}
	entity, _ = st.Entity(ctx, ref)
	if entity.Dirty || !entityJSONEqual(entity.Data, json.RawMessage(`{"name":"second","color":"blue"}`)) {
		t.Fatalf("final merge failed: %+v", entity)
	}
}

func TestEntityOperationsRejectedRetryKeepsFrozenRequest(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	outcome := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "tag"}, Action: "create", Patch: json.RawMessage(`{"name":"one"}`)})
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.ArmEntityOperation(ctx, outcome.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "rejected", true); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryEntityOperation(ctx, op.Item.Seq, op.Revision); err != nil {
		t.Fatal(err)
	}
	claimed, err = st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := st.ArmEntityOperation(ctx, op.Item.Seq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if string(retry.Request) != string(op.Request) {
		t.Fatalf("frozen request changed: %s -> %s", op.Request, retry.Request)
	}
}

func TestEntityCollectionCompletenessAndFence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	first := entityFixture(t, st, "project", "p1", `{"name":"one"}`)
	second := entityFixture(t, st, "project", "p2", `{"name":"two"}`)
	if err := st.MergeEntities(ctx, "project", "", nil, true); err != nil {
		t.Fatal(err)
	}
	if entity, _ := st.Entity(ctx, first); entity.Deleted {
		t.Fatal("unfenced absence pruned entity")
	}
	fence, err := st.CaptureEntityCollectionFence(ctx, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	entityMutationFixture(t, st, EntityMutation{Ref: first, Action: "update", Patch: json.RawMessage(`{"name":"local"}`)})
	if err := st.MergeEntitiesFenced(ctx, nil, true, fence); err != nil {
		t.Fatal(err)
	}
	if entity, _ := st.Entity(ctx, first); entity.Deleted {
		t.Fatal("concurrently changed entity pruned")
	}
	if entity, _ := st.Entity(ctx, second); !entity.Deleted {
		t.Fatal("proven complete absence not recorded")
	}
}

func TestEntityCancelUnsentRestoresOverlay(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ref := entityFixture(t, st, "habit", "h1", `{"name":"before"}`)
	outcome := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"after"}`)})
	if err := st.CancelEntityOperation(ctx, outcome.OperationSeq, outcome.Entity.Revision); err != nil {
		t.Fatal(err)
	}
	entity, _ := st.Entity(ctx, ref)
	if entity.Dirty || !entityJSONEqual(entity.Data, json.RawMessage(`{"name":"before"}`)) {
		t.Fatalf("cancel failed: %+v", entity)
	}
	if summaries, err := st.EntityOperationSummaries(ctx); err != nil || len(summaries) != 0 {
		t.Fatalf("queue after cancel: %+v %v", summaries, err)
	}
}

func TestEntityLeaseExpiryAndConfirmationBinding(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	outcome := entityMutationFixture(t, st, EntityMutation{Ref: EntityRef{Kind: "project"}, Action: "create", Patch: json.RawMessage(`{"name":"one"}`)})
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	op, err := st.ArmEntityOperation(ctx, outcome.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "p1", json.RawMessage(`{"id":"p2","name":"one"}`)); err == nil {
		t.Fatal("accepted wrong response address")
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET inflight_at=0 WHERE seq=?`, op.Item.Seq); err != nil {
		t.Fatal(err)
	}
	if n, _, err := st.ReclaimStale(ctx, time.Second); err != nil || n != 0 {
		t.Fatalf("legacy reclaim: %d %v", n, err)
	}
	if next, err := st.ClaimEntityOperations(ctx, 1, time.Second); err != nil || len(next) != 0 {
		t.Fatalf("reclaimed uncertain operation: %+v %v", next, err)
	}
	if err := st.ConfirmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "p1", json.RawMessage(`{"name":"one"}`)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease settled: %v", err)
	}
	if err := st.RetryEntityOperation(ctx, op.Item.Seq, op.Revision); err != nil {
		t.Fatal(err)
	}
	recovery, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(recovery) != 1 {
		t.Fatalf("manual recovery: %+v %v", recovery, err)
	}
	if _, err := st.ArmEntityOperation(ctx, op.Item.Seq, recovery[0].Item.LeaseToken); !errors.Is(err, ErrEntityUncertain) {
		t.Fatalf("recovery armed another POST: %v", err)
	}
}

func TestEntityCancelAfterEarlierConfirmationKeepsRemoteData(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ref := entityFixture(t, st, "project", "p1", `{"name":"before","color":"red"}`)
	first := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"first"}`)})
	second := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{"name":"second"}`)})
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	op, err := st.ArmEntityOperation(ctx, first.OperationSeq, claimed[0].Item.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken, "p1", json.RawMessage(`{"name":"first","color":"blue"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.CancelEntityOperation(ctx, second.OperationSeq, second.Entity.Revision); err != nil {
		t.Fatal(err)
	}
	entity, err := st.Entity(ctx, ref)
	if err != nil || entity.Dirty || !entityJSONEqual(entity.Data, json.RawMessage(`{"name":"first","color":"blue"}`)) {
		t.Fatalf("cancel discarded remote fields: %+v %v", entity, err)
	}
}

func TestEntityTypedReferenceConflictRefusesSend(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	first := entityFixture(t, st, "folder", "f1", `{"name":"first"}`)
	second := entityFixture(t, st, "folder", "f2", `{"name":"second"}`)
	ref := entityFixture(t, st, "project", "p1", `{"name":"project","groupId":"f1"}`)
	mutation := entityMutationFixture(t, st, EntityMutation{Ref: ref, Action: "update", Patch: json.RawMessage(`{}`), References: []EntityReference{{Field: "groupId", Ref: second}}})
	if err := st.MergeEntities(ctx, "project", "", []ResourceEntity{{ServerID: "p1", Data: json.RawMessage(`{"name":"project","groupId":"f3"}`)}}, false); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimEntityOperations(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if _, err := st.ArmEntityOperation(ctx, mutation.OperationSeq, claimed[0].Item.LeaseToken); !errors.Is(err, ErrEntityChanged) {
		t.Fatalf("reference conflict sent: %v (old %s)", err, first.Key)
	}
}
