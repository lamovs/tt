package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func keyedChecklist(t *testing.T) (*Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Work"})
	task, err := st.createTask(ctx, model.Task{ProjectId: "p1", Title: "Trip", Content: "  body\r\n- [ ] text\n", Items: []model.Item{
		{Title: "same", SortOrder: 17, TimeZone: "Asia/Tokyo", IsAllDay: true, StartDate: model.NewTime(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))},
		{Title: "same", SortOrder: 91},
	}}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.Task(ctx, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	return st, task
}

func TestKeyedChecklistUsesSharedOperationsAndUndo(t *testing.T) {
	ctx := context.Background()
	st, task := keyedChecklist(t)
	first, second := task.Items[0].Key, task.Items[1].Key
	original := task
	changes := []ItemChange{
		{Action: ItemRename, Key: second, Title: "  renamed  "},
		{Action: ItemSetDone, Key: first, Done: true},
		{Action: ItemSetDone, Key: first, Done: false},
		{Action: ItemMove, Key: second, DestinationKey: first},
		{Action: ItemRemove, Key: second},
		{Action: ItemRemove, Key: first},
		{Action: ItemAdd, Title: "same"},
	}
	for _, change := range changes {
		before := stage1BStateOf(t, st, task.Id)
		out, err := st.ChangeTaskItemIfUnchanged(ctx, task, change)
		if err != nil || !out.Changed {
			t.Fatalf("%+v: %+v %v", change, out, err)
		}
		after := stage1BStateOf(t, st, task.Id)
		if after.Outbox != before.Outbox+1 || after.Undo != before.Undo+1 || after.Events != before.Events+1 || out.Task.Content != original.Content {
			t.Fatalf("non-atomic or lossy change: %+v", change)
		}
		for _, item := range out.Task.Items {
			if item.Key == first && (item.TimeZone != original.Items[0].TimeZone || item.StartDate != original.Items[0].StartDate || !item.IsAllDay) {
				t.Fatal("untouched fields changed")
			}
		}
		if len(out.Task.Items) == 0 && out.Task.Kind != "CHECKLIST" {
			t.Fatal("last removal changed kind")
		}
		if change.Action == ItemAdd && (out.Task.Items[0].Key == first || out.Task.Items[0].Key == second) {
			t.Fatal("re-add reused retired key")
		}
		entry, err := st.LastUndo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ApplyUndo(ctx, entry); err != nil {
			t.Fatal(err)
		}
		reverted, err := st.Task(ctx, task.Id)
		if err != nil || !sameItems(reverted.Items, task.Items) {
			t.Fatalf("undo lost identity/fields/order: %v", err)
		}
		out, err = st.ChangeTaskItemIfUnchanged(ctx, reverted, change)
		if err != nil {
			t.Fatal(err)
		}
		task, err = st.Task(ctx, out.Task.Id)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeyedChecklistRejectsStaleSnapshotAndMissingKeys(t *testing.T) {
	for _, action := range []ItemAction{ItemAdd, ItemRename, ItemSetDone, ItemRemove, ItemMove} {
		t.Run(string(action), func(t *testing.T) {
			ctx := context.Background()
			st, original := keyedChecklist(t)
			if _, err := st.MoveTaskItem(ctx, original.Id, 1, 2); err != nil {
				t.Fatal(err)
			}
			before := stage1BStateOf(t, st, original.Id)
			change := ItemChange{Action: action, Key: original.Items[0].Key, DestinationKey: original.Items[1].Key, Title: "new", Done: true}
			if _, err := st.ChangeTaskItemIfUnchanged(ctx, original, change); !errors.Is(err, ErrTaskChanged) {
				t.Fatalf("stale operation: %v", err)
			}
			if after := stage1BStateOf(t, st, original.Id); !reflect.DeepEqual(before, after) {
				t.Fatal("stale operation wrote data")
			}
			if action != ItemAdd {
				change.Key = "missing"
				if _, err := st.ChangeTaskItemIfUnchanged(ctx, before.Task, change); err == nil {
					t.Fatal("missing key selected a row")
				}
				if after := stage1BStateOf(t, st, original.Id); !reflect.DeepEqual(before, after) {
					t.Fatal("missing key wrote data")
				}
			}
		})
	}
}

func TestChecklistInspectionNoopAndRawRefusalDoNotWrite(t *testing.T) {
	ctx := context.Background()
	st, task := keyedChecklist(t)
	before := stage1BStateOf(t, st, task.Id)
	state, err := st.ChecklistStateFor(ctx, task)
	if err != nil || state.Refusal != "" {
		t.Fatalf("inspection: %+v %v", state, err)
	}
	out, err := st.ChangeTaskItemIfUnchanged(ctx, task, ItemChange{Action: ItemRename, Key: task.Items[0].Key, Title: task.Items[0].Title})
	if err != nil || out.Changed {
		t.Fatalf("no-op: %+v %v", out, err)
	}
	if after := stage1BStateOf(t, st, task.Id); !reflect.DeepEqual(before, after) {
		t.Fatal("inspection or no-op wrote data")
	}
	raw := `{"items":[{"id":"server","title":"same","status":0,"sortOrder":17,"future":{"exact":"  value  "}}]}`
	if _, err := st.DB().Exec("UPDATE tasks SET raw=? WHERE id=?", raw, task.Id); err != nil {
		t.Fatal(err)
	}
	state, err = st.ChecklistStateFor(ctx, task)
	if err != nil || state.Refusal == "" {
		t.Fatalf("unsafe raw not reported: %+v %v", state, err)
	}
	if _, err := st.ChangeTaskItemIfUnchanged(ctx, task, ItemChange{Action: ItemRemove, Key: task.Items[0].Key}); !errors.Is(err, ErrUnsafeChecklist) {
		t.Fatalf("raw changed: %v", err)
	}
	var kept string
	if err := st.DB().QueryRow("SELECT raw FROM tasks WHERE id=?", task.Id).Scan(&kept); err != nil || kept != raw {
		t.Fatal("raw was rewritten")
	}
	if after := stage1BStateOf(t, st, task.Id); !reflect.DeepEqual(before, after) {
		t.Fatal("refused operation wrote data")
	}
}

func TestChecklistInspectionReportsUncertaintyAndRefusesUnsupportedKinds(t *testing.T) {
	ctx := context.Background()
	st, task := keyedChecklist(t)
	if err := setItemIdentity(ctx, st.DB(), ItemIdentity{TaskID: task.Id, ItemKey: task.Items[0].Key, State: ItemUncertain}); err != nil {
		t.Fatal(err)
	}
	state, err := st.ChecklistStateFor(ctx, task)
	if err != nil || !state.Uncertain {
		t.Fatalf("uncertainty missing: %+v %v", state, err)
	}
	for _, kind := range []string{"NOTE", "FUTURE"} {
		if _, err := st.DB().Exec("UPDATE tasks SET kind=? WHERE id=?", kind, task.Id); err != nil {
			t.Fatal(err)
		}
		task, err = st.Task(ctx, task.Id)
		if err != nil {
			t.Fatal(err)
		}
		state, err = st.ChecklistStateFor(ctx, task)
		if err != nil || state.Refusal == "" {
			t.Fatalf("kind refusal missing: %+v %v", state, err)
		}
	}
}
