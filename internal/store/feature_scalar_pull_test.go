package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func scalarPullFixture(t *testing.T, kind string) (*Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	prior := openTask("t1", "p1", "before")
	prior.Kind = kind
	prior.Status = model.TaskDone
	prior.DueDate = mustTime(t, "2026-09-09T12:10:00.000+0000")
	prior.CompletedTime = mustTime(t, "2026-09-09T11:55:13.633+0000")
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: prior, Raw: json.RawMessage(`{"id":"t1","kind":"TEXT","status":2}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx *sql.Tx) error { return ensureItemControl(ctx, tx, "t1") }); err != nil {
		t.Fatal(err)
	}
	return st, prior
}

func scalarPullResponse(t *testing.T, kind string) ServerTask {
	t.Helper()
	next := openTask("t1", "p1", "after")
	next.Kind = kind
	next.DueDate = mustTime(t, "2026-09-10T12:10:00.000+0000")
	next.StartDate = next.DueDate
	next.ModifiedTime = mustTime(t, "2026-09-09T11:55:14.535+0000")
	next.RepeatFlag = "RRULE:FREQ=DAILY;INTERVAL=1"
	raw, err := json.Marshal(map[string]any{
		"id": next.Id, "projectId": next.ProjectId, "kind": kind, "title": next.Title,
		"status": 0, "dueDate": next.DueDate.String(), "startDate": next.StartDate.String(),
		"modifiedTime": next.ModifiedTime.String(), "repeatFlag": next.RepeatFlag,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ServerTask{Task: next, Raw: raw}
}

func TestRegisteredScalarPullAcceptsAbsentItems(t *testing.T) {
	for _, kinds := range [][2]string{{"", "TEXT"}, {"TEXT", "TEXT"}, {"", "NOTE"}, {"NOTE", "NOTE"}} {
		t.Run(kinds[0]+"_to_"+kinds[1], func(t *testing.T) {
			ctx := context.Background()
			st, _ := scalarPullFixture(t, kinds[0])
			response := scalarPullResponse(t, kinds[1])
			before := stage1BStateOf(t, st, "t1")
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, fence)
			if err != nil || result.Upserted != 1 || result.Skipped != 0 || result.Stale {
				t.Fatalf("scalar pull = %+v, %v", result, err)
			}
			after := stage1BStateOf(t, st, "t1")
			if !samePulledTask(after.Task, response.Task) || after.Registry != before.Registry || after.Outbox != before.Outbox ||
				after.Events != before.Events || after.Undo != before.Undo || after.Dirty != 0 || after.Epoch == before.Epoch {
				t.Fatalf("scalar pull state: before=%+v after=%+v", before, after)
			}
			var raw string
			if err := st.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id='t1'`).Scan(&raw); err != nil || raw != string(response.Raw) {
				t.Fatalf("retained raw = %s, %v", raw, err)
			}
			fence, err = st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, fence); err != nil {
				t.Fatal(err)
			}
			if repeated := stage1BStateOf(t, st, "t1"); !reflect.DeepEqual(repeated, after) {
				t.Fatalf("identical pull changed state: before=%+v after=%+v", after, repeated)
			}
		})
	}
}

func TestRegisteredScalarPullPreservesChecklistAndPendingState(t *testing.T) {
	for _, name := range []string{
		"checklist kind", "different kind", "unknown kind", "missing kind", "null items", "bad items", "invalid raw",
		"current items", "incoming items", "bound", "uncertain", "abandoned", "dirty", "local", "held", "recovery",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			kind := "TEXT"
			if name == "checklist kind" {
				kind = "CHECKLIST"
			}
			if name == "different kind" {
				kind = "NOTE"
			}
			st, _ := scalarPullFixture(t, kind)
			response := scalarPullResponse(t, "TEXT")
			switch name {
			case "unknown kind":
				response = scalarPullResponse(t, "FUTURE")
			case "missing kind":
				response.Raw = json.RawMessage(`{"id":"t1","projectId":"p1","status":0}`)
			case "null items":
				response.Raw = json.RawMessage(`{"id":"t1","kind":"TEXT","items":null}`)
			case "bad items":
				response.Raw = json.RawMessage(`{"id":"t1","kind":"TEXT","items":{}}`)
			case "invalid raw":
				response.Raw = json.RawMessage(`{`)
			case "current items":
				if err := replaceItems(ctx, st.DB(), "t1", []model.Item{{Id: "old-item", Title: "retain"}}); err != nil {
					t.Fatal(err)
				}
			case "incoming items":
				response.Task.Items = []model.Item{{Id: "new-item", Title: "unproven"}}
			case "bound", "uncertain", "abandoned":
				identity := ItemIdentity{TaskID: "t1", ItemKey: "old-key", State: ItemIdentityState(name)}
				identity.ServerID = "old-item"
				if err := setItemIdentity(ctx, st.DB(), identity); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("local edit")}); err != nil {
					t.Fatal(err)
				}
			case "local":
				if _, err := st.DB().ExecContext(ctx, `UPDATE tasks SET local=1 WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			case "held":
				if _, err := st.SetTaskRepeat(ctx, "t1", "RRULE:FREQ=WEEKLY;INTERVAL=1"); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().ExecContext(ctx, `UPDATE tasks SET dirty=0 WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			case "recovery":
				if err := setItemIdentity(ctx, st.DB(), ItemIdentity{TaskID: "t1", State: RecoveryPending}); err != nil {
					t.Fatal(err)
				}
			}
			before := stage1BStateOf(t, st, "t1")
			identities, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := itemIdentity(ctx, st.DB(), "t1", "")
			if err != nil {
				t.Fatal(err)
			}
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			result, err := st.SyncProjectFenced(ctx, "p1", []ServerTask{response}, fence)
			if err != nil || result.Upserted != 0 || result.Skipped != 1 {
				t.Fatalf("unsafe scalar pull = %+v, %v", result, err)
			}
			after := stage1BStateOf(t, st, "t1")
			after.Epoch = before.Epoch
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("unsafe scalar pull changed modeled state: before=%+v after=%+v", before, after)
			}
			afterIdentities, err := itemIdentities(ctx, st.DB(), "t1")
			if err != nil || !reflect.DeepEqual(identities, afterIdentities) {
				t.Fatalf("identities changed: before=%+v after=%+v err=%v", identities, afterIdentities, err)
			}
			afterControl, _, err := itemIdentity(ctx, st.DB(), "t1", "")
			if err != nil || control != afterControl {
				t.Fatalf("control changed: before=%+v after=%+v err=%v", control, afterControl, err)
			}
		})
	}
}
