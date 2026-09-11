package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func undoCache(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func undoProjects(t *testing.T, st *store.Store) {
	t.Helper()
	err := st.ReplaceProjects(context.Background(), []model.Project{
		{Id: "p1", Name: "Личное"},
		{Id: "p2", Name: "Работа"},
	})
	if err != nil {
		t.Fatalf("seed projects: %v", err)
	}
}

func undoAdd(t *testing.T, st *store.Store, title string, items ...model.Item) model.Task {
	t.Helper()
	task, err := st.CreateTask(context.Background(), model.Task{
		ProjectId: "p1", Title: title, Status: model.TaskOpen, Items: items,
	})
	if err != nil {
		t.Fatalf("add %q: %v", title, err)
	}
	return task
}

func undoRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"undo"}, args...),
		strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func undoTop(t *testing.T, st *store.Store) (store.UndoEntry, bool) {
	t.Helper()
	e, err := st.LastUndo(context.Background())
	switch {
	case errors.Is(err, store.ErrNoUndo):
		return store.UndoEntry{}, false
	case err != nil:
		t.Fatalf("read the undo stack: %v", err)
	}
	return e, true
}

func undoTask(t *testing.T, st *store.Store, id string) model.Task {
	t.Helper()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return task
}

func TestUndoOnAnEmptyStack(t *testing.T) {
	isolate(t)
	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stderr != "nothing to undo\n" {
		t.Errorf("stderr = %q, want the one line saying there is nothing to undo", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}

	code, stdout, stderr = undoRun(t, undoSkipOption)
	if code != exitOK || stderr != "nothing to undo\n" || stdout != "" {
		t.Errorf("tt undo --skip = %d, stdout %q, stderr %q; want the same answer on stderr and nothing on stdout",
			code, stdout, stderr)
	}
}

func TestUndoTableCoversWhatTheStoreRecords(t *testing.T) {
	for op, rev := range undoReversals {
		if rev.what == "" {
			t.Errorf("%q has no words to name it by in a report", op)
		}
		if rev.reversible == (rev.refuse != nil) {
			t.Errorf("%q must have either a reversal or a reason it has none, not both and not neither", op)
		}
		if rev.reversible && rev.say == nil {
			t.Errorf("%q is reversed without saying what that records or what the user is told", op)
		}
	}

	for _, op := range []string{
		store.OpTaskCreate, store.OpTaskUpdate, store.OpTaskComplete,
		store.OpTaskDelete, store.OpTaskMoveRecreate,
	} {
		if _, ok := undoReversals[op]; !ok {
			t.Errorf("the store records %q and the table has no row for it", op)
		}
	}
}

func TestUndoRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"a task reference", []string{"1"}},
		{"a title", []string{"Забрать посылку"}},
		{"an unknown option", []string{"--bogus"}},
		{"an unknown option beside the one it takes", []string{"--skip", "--bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			st := undoCache(t)
			undoProjects(t, st)
			undoAdd(t, st, "Забрать посылку")

			code, stdout, stderr := undoRun(t, c.args...)
			if code != exitUsage {
				t.Fatalf("tt undo %v = %d, want %d", c.args, code, exitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if !strings.Contains(stderr, `run "tt help undo" for examples`) {
				t.Errorf("stderr = %q, want it to send the user to the examples", stderr)
			}

			if _, ok := undoTop(t, st); !ok {
				t.Error("the refusal dropped the record it refused to act on")
			}
		})
	}
}

func TestUndoOfEachRecordedChange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string

		act func(t *testing.T, st *store.Store) model.Task

		want []string

		check func(t *testing.T, st *store.Store, task model.Task)
	}{
		{
			name: "adding a task is undone by deleting it",
			act: func(t *testing.T, st *store.Store) model.Task {
				return undoAdd(t, st, "Забрать посылку")
			},
			want: []string{"undid adding", `"Забрать посылку"`, "gone again"},
			check: func(t *testing.T, st *store.Store, task model.Task) {
				if _, err := st.Task(ctx, task.Id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("the task is still cached: %v", err)
				}
			},
		},
		{
			name: "an edit is undone by putting the old values back",
			act: func(t *testing.T, st *store.Store) model.Task {
				task := undoAdd(t, st, "Забрать посылку")
				if _, err := st.UpdateTask(ctx, task.Id, model.TaskEdit{
					Title:    model.Ptr("Забрать документы"),
					Priority: model.Ptr(model.PriorityHigh),
				}); err != nil {
					t.Fatalf("edit: %v", err)
				}
				return task
			},
			want: []string{"undid the last change to", `"Забрать посылку"`},
			check: func(t *testing.T, st *store.Store, task model.Task) {
				got := undoTask(t, st, task.Id)
				if got.Title != "Забрать посылку" || got.Priority != model.PriorityNone {
					t.Errorf("task is %q at priority %v, want the values the edit replaced",
						got.Title, got.Priority)
				}
			},
		},
		{
			name: "a completion is undone with its checklist",
			act: func(t *testing.T, st *store.Store) model.Task {
				task := undoAdd(t, st, "Забрать посылку", model.Item{Title: "паспорт"})
				if _, err := st.CompleteTask(ctx, task.Id, store.CompleteOptions{}); err != nil {
					t.Fatalf("complete: %v", err)
				}
				return task
			},
			want: []string{"undid completing", `"Забрать посылку"`, "it is open again"},
			check: func(t *testing.T, st *store.Store, task model.Task) {
				got := undoTask(t, st, task.Id)
				if got.Status.Done() || !got.CompletedTime.IsZero() {
					t.Errorf("task is %v, completed at %v; want it open with no completion time",
						got.Status, got.CompletedTime)
				}
				if len(got.Items) != 1 || got.Items[0].Status.Done() {
					t.Errorf("checklist is %+v, want the one item open again", got.Items)
				}
			},
		},
		{
			name: "a reopen is undone by closing the task again",
			act: func(t *testing.T, st *store.Store) model.Task {
				task := undoAdd(t, st, "Забрать посылку")
				if _, err := st.CompleteTask(ctx, task.Id, store.CompleteOptions{}); err != nil {
					t.Fatalf("complete: %v", err)
				}
				if _, err := st.ReopenTask(ctx, task.Id); err != nil {
					t.Fatalf("reopen: %v", err)
				}
				return task
			},

			want: []string{"undid reopening", `"Забрать посылку"`, "it is done again"},
			check: func(t *testing.T, st *store.Store, task model.Task) {
				got := undoTask(t, st, task.Id)
				if !got.Status.Done() || got.CompletedTime.IsZero() {
					t.Errorf("task is %v, completed at %v; want it done again",
						got.Status, got.CompletedTime)
				}
			},
		},
		{
			name: "a delete is undone by making the task again",
			act: func(t *testing.T, st *store.Store) model.Task {
				task := undoAdd(t, st, "Забрать посылку", model.Item{Title: "паспорт"})
				if err := st.DeleteTask(ctx, task.Id); err != nil {
					t.Fatalf("delete: %v", err)
				}
				return task
			},
			want: []string{"undid deleting", `"Забрать посылку"`, "the task is back"},
			check: func(t *testing.T, st *store.Store, task model.Task) {
				if _, err := st.Task(ctx, task.Id); err == nil {
					t.Errorf("deleted parent id %q was reused", task.Id)
				}
				found, err := st.Tasks(ctx, store.TaskFilter{Search: "Забрать посылку"})
				if err != nil {
					t.Fatalf("find restored task: %v", err)
				}
				if len(found) != 1 {
					t.Fatalf("restored tasks = %+v, want one", found)
				}
				got := undoTask(t, st, found[0].Id)
				if got.Id == task.Id || !store.IsLocalID(got.Id) || got.Title != "Забрать посылку" || len(got.Items) != 1 {
					t.Fatalf("task came back as %+v, want the row the record kept", got)
				}
				if got.Items[0].Id != "" || got.Items[0].Key == task.Items[0].Key || !store.IsLocalID(got.Items[0].Key) {
					t.Errorf("item came back as %+v, want a fresh local incarnation", got.Items[0])
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			st := undoCache(t)
			undoProjects(t, st)
			task := c.act(t, st)

			before, ok := undoTop(t, st)
			if !ok {
				t.Fatal("the change recorded nothing to undo")
			}

			code, stdout, stderr := undoRun(t)
			if code != exitOK {
				t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
			}
			for _, want := range c.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout = %q, want it to carry %q", stdout, want)
				}
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			c.check(t, st, task)

			if top, ok := undoTop(t, st); ok && top.Seq >= before.Seq {
				t.Errorf("the stack is at %d (%s), want it back past %d",
					top.Seq, top.Action.Op, before.Seq)
			}
		})
	}
}

func TestUndoWalksBackAndDoesNotToggle(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")
	if _, err := st.CompleteTask(ctx, task.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if code, stdout, stderr := undoRun(t); code != exitOK || !strings.Contains(stdout, "undid completing") {
		t.Fatalf("first undo = %d, %q (stderr: %s); want it to undo the completion", code, stdout, stderr)
	}
	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("second undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "undid adding") {
		t.Errorf("second undo said %q, want it to reach the add before the completion", stdout)
	}
	if _, err := st.Task(ctx, task.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the task is still cached after both changes were undone: %v", err)
	}
	if _, ok := undoTop(t, st); ok {
		t.Error("the stack still holds a record, want it empty after undoing both changes")
	}
	if code, stdout, stderr := undoRun(t); code != exitOK || stderr != "nothing to undo\n" || stdout != "" {
		t.Errorf("third undo = %d, stdout %q, stderr %q; want the empty-stack answer on stderr", code, stdout, stderr)
	}
}

func TestUndoQueuesTheReversalLikeAnyOtherChange(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")
	if _, err := st.CompleteTask(ctx, task.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if code, _, stderr := undoRun(t); code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}

	var op string
	var payload []byte
	err := st.DB().QueryRowContext(ctx,
		`SELECT op, payload FROM outbox WHERE task_id = ? ORDER BY seq DESC LIMIT 1`, task.Id).
		Scan(&op, &payload)
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	if op != store.OpTaskUpdate {
		t.Errorf("the queue ends with %s, want the reversal queued as an update", op)
	}
	var edit model.TaskEdit
	if err := json.Unmarshal(payload, &edit); err != nil {
		t.Fatalf("decode the queued reversal: %v", err)
	}
	if edit.Status == nil || edit.Status.Done() {
		t.Errorf("the queued reversal is %+v, want it to reopen the task", edit)
	}
}

func TestUndoRefusesAMoveAndKeepsTheRecord(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")
	moved, err := st.MoveTask(ctx, task.Id, "p2", store.MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatalf("move: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d (stdout: %s)", code, exitError, stdout)
	}
	for _, want := range []string{
		"tt: undo: the last change cannot be undone",
		"the move of",
		`"Забрать посылку"`,
		undoSkipOption,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing said on the stream a report goes to", stdout)
	}
	top, ok := undoTop(t, st)
	if !ok || top.Action.Op != store.OpTaskMoveRecreate {
		t.Fatalf("the stack is at %+v, want the move still on it", top.Action)
	}
	if _, err := st.Task(ctx, moved.Id); err != nil {
		t.Errorf("the moved task is not where the move left it: %v", err)
	}

	code, stdout, stderr = undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "dropped the record of the move of") ||
		!strings.Contains(stdout, "nothing was reversed") {
		t.Errorf("stdout = %q, want it to say what was dropped and that nothing was reversed", stdout)
	}
	if _, err := st.Task(ctx, moved.Id); err != nil {
		t.Errorf("--skip changed the cache: %v", err)
	}
	top, ok = undoTop(t, st)
	if !ok || top.Action.Op != store.OpTaskCreate {
		t.Fatalf("the stack is at %+v, want the add that came before the move", top.Action)
	}

	code, _, stderr = undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo over a record of a task that is gone = %d, want %d", code, exitError)
	}

	if !strings.Contains(stderr, "not in the cache") {
		t.Errorf("stderr = %q, want it to say the task the record names has gone", stderr)
	}
	if _, ok := undoTop(t, st); !ok {
		t.Error("the refusal dropped the record it could not act on")
	}
}

func TestUndoRefusalOfAMoveStaysInsideTheWidth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)

	task := undoAdd(t, st, strings.Repeat("a", cli.Width))
	if _, err := st.MoveTask(ctx, task.Id, "p2", store.MoveOptions{ByRecreate: true}); err != nil {
		t.Fatalf("move: %v", err)
	}

	code, _, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "the move of") {
		t.Errorf("stderr = %q, want the refusal of the move", stderr)
	}
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if n := len([]rune(line)); n > cli.Width {
			t.Errorf("a refusal line is %d columns wide:\n%q", n, line)
		}
	}
}

func TestUndoOfADeleteOfATaskTheServerKnew(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	_, err := st.SyncProject(ctx, "p1", []store.ServerTask{{
		Task: model.Task{Id: "srv-1", ProjectId: "p1", Title: "Продлить домен", Status: model.TaskOpen},
		Raw:  json.RawMessage(`{"id":"srv-1"}`),
	}})
	if err != nil {
		t.Fatalf("seed a server task: %v", err)
	}
	if err := st.DeleteTask(ctx, "srv-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "the task is back") || !strings.Contains(stdout, "new task") {
		t.Errorf("stdout = %q, want it to say the task is back and that it is a new one", stdout)
	}
	if _, err := st.Task(ctx, "srv-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old id is in the cache again: %v", err)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Search: "домен"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Id == "srv-1" || !store.IsLocalID(tasks[0].Id) {
		t.Fatalf("cache holds %+v, want the task back under a local id", tasks)
	}
}

func TestUndoOfADeleteOfADoneTaskSaysItComesBackOpen(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Продлить домен")
	if _, err := st.CompleteTask(ctx, task.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := st.DeleteTask(ctx, task.Id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "the task is back") {
		t.Errorf("stdout = %q, want it to say the task is back", stdout)
	}
	for _, want := range []string{"was done", "work to do", "sync"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to carry %q: the completion does not come back", stdout, want)
		}
	}

	open := undoAdd(t, st, "Забрать посылку")
	if err := st.DeleteTask(ctx, open.Id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	code, stdout, stderr = undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if strings.Contains(stdout, "work to do") {
		t.Errorf("stdout = %q, want nothing said about a completion the task never had", stdout)
	}
}

func TestUndoOfADeleteDoesNotPutServerItemIdsBack(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	_, err := st.SyncProject(ctx, "p1", []store.ServerTask{{
		Task: model.Task{Id: "srv-1", ProjectId: "p1", Title: "Продлить домен", Status: model.TaskOpen,
			Items: []model.Item{{Id: "item-9", Title: "паспорт"}}},
		Raw: json.RawMessage(`{"id":"srv-1","items":[{"id":"item-9","title":"паспорт","status":0,"sortOrder":0,"startDate":"","isAllDay":false,"timeZone":"","completedTime":""}]}`),
	}})
	if err != nil {
		t.Fatalf("seed a server task: %v", err)
	}
	if err := st.DeleteTask(ctx, "srv-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if code, _, stderr := undoRun(t); code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	found, err := st.Tasks(ctx, store.TaskFilter{Search: "домен"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("cache holds %+v, want the one task back", found)
	}

	back := undoTask(t, st, found[0].Id)
	if len(back.Items) != 1 || back.Items[0].Title != "паспорт" {
		t.Fatalf("the task came back as %+v, want its one checklist item with it", back)
	}
	if id := back.Items[0].Id; id != "" {
		t.Errorf("the item came back under id %q, want none: the server's number was retired with the task", id)
	}

	var op string
	var payload []byte
	if err := st.DB().QueryRowContext(ctx,
		`SELECT op, payload FROM outbox ORDER BY seq DESC LIMIT 1`).Scan(&op, &payload); err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	if op != store.OpTaskCreate {
		t.Fatalf("the queue ends with %s, want the reversal queued as a create", op)
	}
	if strings.Contains(string(payload), "item-9") {
		t.Errorf("the queued create is %s, want it to name no server item id", payload)
	}
}

func TestUndoRefusesARecordThatDoesNotCarryWhatItNeeds(t *testing.T) {
	cases := []struct {
		name    string
		record  string
		subject string
	}{
		{"a delete with no task on it", `{"op":"task.delete","task_id":%q}`, "deleting"},
		{"an edit with no values on it", `{"op":"task.update","task_id":%q}`, "the change to"},
		{"a completion whose values are empty", `{"op":"task.complete","task_id":%q,"before":{}}`, "completing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			st := undoCache(t)
			undoProjects(t, st)
			task := undoAdd(t, st, "Забрать посылку")

			payload := fmt.Sprintf(c.record, task.Id)
			if _, err := st.DB().ExecContext(ctx,
				`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
				time.Now().Unix(), "task.malformed", payload); err != nil {
				t.Fatalf("write a record from another build: %v", err)
			}

			code, stdout, stderr := undoRun(t)
			if code != exitError {
				t.Fatalf("tt undo = %d, want %d (stdout: %s)", code, exitError, stdout)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing said on the stream a report goes to", stdout)
			}
			for _, want := range []string{
				"tt: undo: the last change cannot be undone",
				c.subject,
				`"Забрать посылку"`,
				undoSkipOption,
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to carry %q", stderr, want)
				}
			}

			if top, ok := undoTop(t, st); !ok || top.Action.Op == store.OpTaskCreate {
				t.Errorf("the stack is at %+v, want the record it refused still on it", top.Action)
			}
			if got := undoTask(t, st, task.Id); got.Title != "Забрать посылку" || got.Status.Done() {
				t.Errorf("the task is %+v, want it as the refusal found it", got)
			}
		})
	}
}

func TestUndoSkipStaysInsideTheWidth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)

	title := strings.Repeat("не-помещается-", 20)
	task := undoAdd(t, st, title)

	op := strings.Repeat("q", 200)
	_, err := st.DB().ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), op,
		`{"op":"`+op+`","task_id":"`+task.Id+`"}`)
	if err != nil {
		t.Fatalf("write a record from another build: %v", err)
	}

	code, stdout, stderr := undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}

	if !strings.Contains(stdout, `"qqq`) {
		t.Errorf("stdout = %q, want it to name the record it dropped", stdout)
	}
	if !strings.Contains(stdout, `"`+string([]rune(title)[:8])) {
		t.Errorf("stdout = %q, want it to name the task the record was against", stdout)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if n := len([]rune(line)); n > cli.Width {
			t.Errorf("a report line is %d columns wide:\n%q", n, line)
		}
	}
}

func TestUndoRefusesARecordItHasNoRowFor(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")

	_, err := st.DB().ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), "task.frobnicate",
		`{"op":"task.frobnicate","task_id":"`+task.Id+`"}`)
	if err != nil {
		t.Fatalf("write a record from another build: %v", err)
	}

	code, _, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d", code, exitError)
	}
	for _, want := range []string{`"task.frobnicate"`, undoSkipOption} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
	if top, _ := undoTop(t, st); top.Action.Op != "task.frobnicate" {
		t.Errorf("the stack is at %q, want the record it refused still on it", top.Action.Op)
	}

	code, stdout, stderr := undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, `"task.frobnicate"`) {
		t.Errorf("stdout = %q, want it to name the record it dropped", stdout)
	}
	if top, ok := undoTop(t, st); !ok || top.Action.Op != store.OpTaskCreate {
		t.Errorf("the stack is at %+v, want the add that came before it", top.Action)
	}

	undoTask(t, st, task.Id)
}

func TestUndoOfAForeignOpStaysInsideTheWidth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")

	op := strings.Repeat("z", 200)
	_, err := st.DB().ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), op, `{"op":"`+op+`","task_id":"`+task.Id+`"}`)
	if err != nil {
		t.Fatalf("write a record from another build: %v", err)
	}

	code, _, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d", code, exitError)
	}

	if !strings.Contains(stderr, `"zzz`) {
		t.Errorf("stderr = %q, want it to name the op it refused over", stderr)
	}
	for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		if n := len([]rune(line)); n > cli.Width {
			t.Errorf("a refusal line is %d columns wide:\n%q", n, line)
		}
	}

	code, stdout, stderr := undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, `"zzz`) {
		t.Errorf("stdout = %q, want it to name the record it dropped", stdout)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if n := len([]rune(line)); n > cli.Width {
			t.Errorf("a report line is %d columns wide:\n%q", n, line)
		}
	}
}

func TestUndoReportStaysInsideTheWidth(t *testing.T) {
	isolate(t)
	st := undoCache(t)
	undoProjects(t, st)
	undoAdd(t, st, strings.Repeat("Забрать посылку из отделения на углу ", 8))

	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("a report line is %d columns wide:\n%q", n, line)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("the report carried escape sequences to a buffer that is not a terminal:\n%q", stdout)
	}
}

func TestUndoSkipIsOffered(t *testing.T) {
	if got := helpText(t, "undo"); !strings.Contains(got, undoSkipOption) {
		t.Errorf("the help for undo does not offer %s:\n%s", undoSkipOption, got)
	}
}

func TestUndoReversesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	st := undoCache(t)
	undoProjects(t, st)
	task := undoAdd(t, st, "Забрать посылку")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"undo"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	top, ok := undoTop(t, st)
	if !ok || top.Action.Op != store.OpTaskCreate || top.Action.TaskID != task.Id {
		t.Errorf("the record on top is %+v (present: %v), want the creation the run did not reverse", top.Action, ok)
	}
	if got := undoTask(t, st, task.Id); got.Title != "Забрать посылку" {
		t.Errorf("the task is %+v, want it as the stopped run found it", got)
	}
}

func TestUndoSkipMeasuresTheTextAndNotTheStyle(t *testing.T) {
	byMode := map[string]string{}
	for _, mode := range []string{"never", "always"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			st := undoCache(t)
			undoProjects(t, st)

			undoAdd(t, st, strings.TrimSpace(strings.Repeat("Забрать посылку из отделения ", 3)))

			code, stdout, stderr := undoRun(t, undoSkipOption, "--color="+mode)
			if code != exitOK {
				t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
			}
			byMode[mode] = stdout
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				if n := len([]rune(line)); n > cli.Width {
					t.Errorf("a report line is %d columns wide:\n%q", n, line)
				}
			}
		})
	}
	if byMode["never"] != byMode["always"] {
		t.Errorf("the line was broken differently with colour on:\nalways: %q\nnever:  %q",
			byMode["always"], byMode["never"])
	}
}

func TestUndoDropTopRefusesWithoutClaimingAReversal(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	undoAdd(t, st, "Забрать посылку")

	err := undoDropTop(ctx, st, func(store.UndoEntry) bool { return false })
	if err == nil {
		t.Fatal("undoDropTop popped a record the caller did not mean")
	}
	if strings.Contains(err.Error(), "revers") {
		t.Errorf("undoDropTop = %q, want a refusal that claims nothing about reversing", err)
	}
	if !strings.Contains(err.Error(), "undo stack") {
		t.Errorf("undoDropTop = %q, want it to name what it found in the state it found it", err)
	}

	if top, ok := undoTop(t, st); !ok || top.Action.Op != store.OpTaskCreate {
		t.Errorf("the stack is at %+v, want the record the refusal left where it was", top.Action)
	}
}
