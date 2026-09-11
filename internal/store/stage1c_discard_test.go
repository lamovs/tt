package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestStage1CDiscardPreparedChecklistAuthorizesFreshAdoption(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	task := openTask("t1", "p1", "Imported")
	task.Kind = "CHECKLIST"
	task.Items = []model.Item{{Id: "i1", Title: "old", SortOrder: 9}}
	raw := json.RawMessage(`{"id":"t1","projectId":"p1","kind":"CHECKLIST","items":[{"id":"i1","title":"old","status":0,"sortOrder":9,"startDate":"","isAllDay":false,"timeZone":"","completedTime":""}]}`)
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	updated, err := st.AddTaskItem(ctx, "t1", "new local")
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	var payload []byte
	if err := st.DB().QueryRowContext(ctx, `SELECT seq, payload FROM outbox WHERE task_id = 't1'`).Scan(&seq, &payload); err != nil {
		t.Fatal(err)
	}
	_, metadata, err := DecodeTaskEditPayload(payload)
	if err != nil || metadata == nil || !metadata.Fields.Items || metadata.Snapshot != nil {
		t.Fatalf("prepared payload metadata = %+v, err = %v", metadata, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET state = 'failed', last_error = 'explicit fixture' WHERE seq = ?`, seq); err != nil {
		t.Fatal(err)
	}

	dropped, err := st.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || !dropped[0].ChecklistRecoveryPending {
		t.Fatalf("dropped = %+v, want one checklist recovery authorization", dropped)
	}
	control, ok, err := itemIdentity(ctx, st.DB(), "t1", "")
	if err != nil || !ok || control.State != RecoveryPending {
		t.Fatalf("control = %+v, ok=%v, err=%v", control, ok, err)
	}
	identities, err := itemIdentities(ctx, st.DB(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != len(updated.Items) {
		t.Fatalf("identities = %+v, want one row per discarded item", identities)
	}
	for _, identity := range identities {
		if identity.State != ItemAbandoned {
			t.Fatalf("identity survived prepared discard as %+v", identity)
		}
	}
	var dirty int64
	if err := st.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 't1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 0 {
		t.Fatalf("dirty = %d, want released discarded revision", dirty)
	}
}

func TestStage1CDiscardMalformedLegacyPayloadRemainsAvailable(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seq, err := st.Enqueue(ctx, OutboxEntry{
		Target:  TargetOpenAPI,
		Op:      OpTaskUpdate,
		TaskID:  "missing",
		Payload: []byte(`{"title":`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET state = 'failed', last_error = 'malformed fixture' WHERE seq = ?`, seq); err != nil {
		t.Fatal(err)
	}
	dropped, err := st.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Seq != seq || dropped[0].ChecklistRecoveryPending {
		t.Fatalf("dropped = %+v, want malformed legacy row discarded without identity recovery", dropped)
	}
}
