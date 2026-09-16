package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const undoGroupID = "group-cli-sample"

func undoGroupAdd(t *testing.T, st *store.Store, ctx context.Context, title string) model.Task {
	t.Helper()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title, Status: model.TaskOpen})
	if err != nil {
		t.Fatalf("add %q: %v", title, err)
	}
	return task
}

func undoGroupLinesFit(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if n := len([]rune(line)); n > cli.Width {
			t.Errorf("a line is %d columns wide:\n%q", n, line)
		}
	}
}

func undoGroupState(t *testing.T, st *store.Store) string {
	t.Helper()
	return stage1BUndoRows(t, st,
		`SELECT * FROM tasks ORDER BY id`,
		`SELECT * FROM items ORDER BY task_id, id`,
		`SELECT * FROM outbox ORDER BY seq`,
		`SELECT * FROM events ORDER BY seq`,
		`SELECT * FROM undo_log ORDER BY seq`,
	)
}

type undoGroupFixture struct {
	base, edited, added model.Task
	before              store.UndoEntry
}

func undoGroupMixed(t *testing.T, st *store.Store) undoGroupFixture {
	t.Helper()
	ctx := context.Background()
	var f undoGroupFixture
	f.base = undoGroupAdd(t, st, ctx, "Sample base")
	f.edited = undoGroupAdd(t, st, ctx, "Sample edited")
	f.before, _ = undoTop(t, st)

	grouped := store.WithUndoGroup(ctx, undoGroupID)
	f.added = undoGroupAdd(t, st, grouped, "Sample added")
	if _, err := st.UpdateTask(grouped, f.edited.Id, model.TaskEdit{Title: model.Ptr("Sample edited again")}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := st.CompleteTask(grouped, f.base.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return f
}

func TestUndoOfAGroupReversesEveryChangeInIt(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	f := undoGroupMixed(t, st)

	code, stdout, stderr := undoRun(t)
	if code != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
	want := []string{
		"undid 3 changes made together, newest first:",
		`undid completing "Sample base": it is open again`,
		`undid the last change to "Sample edited"`,
		`undid adding "Sample added": the task is gone again`,
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("stdout lines:\n got %q\nwant %q", lines, want)
	}
	undoGroupLinesFit(t, stdout)

	if got := undoTask(t, st, f.base.Id); got.Status.Done() {
		t.Error("the completion in the group is still there")
	}
	if got := undoTask(t, st, f.edited.Id); got.Title != "Sample edited" {
		t.Errorf("edited task is %q, want the title the group replaced", got.Title)
	}
	if _, err := st.Task(ctx, f.added.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the add in the group is still cached: %v", err)
	}
	if top, ok := undoTop(t, st); !ok || top.Seq != f.before.Seq {
		t.Errorf("the stack is at %+v, want the record before the group", top)
	}
}

func TestUndoOfAGroupReportsEveryChangeInJSON(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	f := undoGroupMixed(t, st)

	result, code := runMachine(t, ctx, "undo")
	if code != exitOK {
		t.Fatalf("undo: %d %s", code, result.Data)
	}
	var data struct {
		Changed    bool   `json:"changed"`
		GroupID    string `json:"group_id"`
		Count      int    `json:"count"`
		Operation  string `json:"operation"`
		Operations []struct {
			Operation string     `json:"operation"`
			Task      model.Task `json:"task"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Changed || data.GroupID != undoGroupID || data.Count != 3 || data.Operation != "group" || len(data.Operations) != 3 {
		t.Fatalf("group undo data = %s", result.Data)
	}
	wantOps := []string{store.OpTaskComplete, store.OpTaskUpdate, store.OpTaskCreate}
	wantIDs := []string{f.base.Id, f.edited.Id, f.added.Id}
	for i, op := range data.Operations {
		if op.Operation != wantOps[i] || op.Task.Id != wantIDs[i] {
			t.Errorf("operation %d = %s on %s, want %s on %s", i, op.Operation, op.Task.Id, wantOps[i], wantIDs[i])
		}
	}

	grouped := store.WithUndoGroup(ctx, "group-cli-skip")
	first := undoGroupAdd(t, st, grouped, "Sample skip one")
	second := undoGroupAdd(t, st, grouped, "Sample skip two")
	group, err := st.LastUndoGroup(ctx)
	if err != nil || len(group) != 2 {
		t.Fatalf("group = %+v, %v", group, err)
	}
	result, code = runMachine(t, ctx, "undo", undoSkipOption)
	if code != exitOK {
		t.Fatalf("undo --skip: %d %s", code, result.Data)
	}
	var skipped struct {
		Changed    bool   `json:"changed"`
		Reversed   *bool  `json:"reversed"`
		Operation  string `json:"operation"`
		GroupID    string `json:"group_id"`
		Count      int    `json:"count"`
		Operations []struct {
			DroppedRecord int64  `json:"dropped_record"`
			Operation     string `json:"operation"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(result.Data, &skipped); err != nil {
		t.Fatal(err)
	}
	if !skipped.Changed || skipped.Reversed == nil || *skipped.Reversed || skipped.Operation != "group" || skipped.GroupID != "group-cli-skip" || skipped.Count != 2 || len(skipped.Operations) != 2 ||
		skipped.Operations[0].DroppedRecord != group[0].Seq || skipped.Operations[1].DroppedRecord != group[1].Seq || skipped.Operations[0].Operation != store.OpTaskCreate {
		t.Fatalf("group skip data = %s", result.Data)
	}
	for _, task := range []model.Task{first, second} {
		undoTask(t, st, task.Id)
	}
	if top, ok := undoTop(t, st); !ok || top.Seq != f.before.Seq {
		t.Errorf("the stack is at %+v, want the record before both groups", top)
	}
}

func TestUndoOfAnUngroupedChangeOnTopOfAGroupStaysAsItWas(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	grouped := store.WithUndoGroup(ctx, undoGroupID)
	first := undoGroupAdd(t, st, grouped, "Sample one")
	second := undoGroupAdd(t, st, grouped, "Sample two")
	loose := undoAdd(t, st, "Sample loose")

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" {
		t.Fatalf("tt undo = %d (stderr: %s)", code, stderr)
	}
	if want := "undid adding \"Sample loose\": the task is gone again\n"; stdout != want {
		t.Errorf("stdout = %q, want the ungrouped report %q", stdout, want)
	}
	if _, err := st.Task(ctx, loose.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the ungrouped add is still cached: %v", err)
	}
	for _, task := range []model.Task{first, second} {
		undoTask(t, st, task.Id)
	}
	group, err := st.LastUndoGroup(ctx)
	if err != nil || len(group) != 2 {
		t.Fatalf("the group under it = %+v, %v; want both records kept", group, err)
	}

	third := undoAdd(t, st, "Sample loose again")
	result, code := runMachine(t, ctx, "undo")
	if code != exitOK || !strings.Contains(string(result.Data), `"operation":"task.create"`) || strings.Contains(string(result.Data), "group_id") {
		t.Fatalf("ungrouped JSON undo: %d %s", code, result.Data)
	}
	if _, err := st.Task(ctx, third.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the second ungrouped add is still cached: %v", err)
	}

	code, stdout, stderr = undoRun(t)
	if code != exitOK || !strings.HasPrefix(stdout, "undid 2 changes made together") {
		t.Fatalf("tt undo = %d, stdout %q (stderr: %s); want the group", code, stdout, stderr)
	}
	if _, ok := undoTop(t, st); ok {
		t.Error("the stack still holds a record")
	}
}

func TestUndoOfAGroupThatCannotBeReversedChangesNothing(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	f := undoGroupMixed(t, st)
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, f.edited.Id); err != nil {
		t.Fatal(err)
	}
	before := undoGroupState(t, st)

	code, stdout, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d (stdout: %s)", code, exitError, stdout)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, want := range []string{
		"tt: undo: the last change cannot be undone",
		"3 changes made together",
		"the change to",
		`"` + f.edited.Id + `"`,
		"not in the cache",
		undoSkipOption,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
	undoGroupLinesFit(t, stderr)
	if after := undoGroupState(t, st); after != before {
		t.Fatalf("a refused group undo changed the cache:\nbefore %s\nafter  %s", before, after)
	}

	code, stdout, stderr = undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	for _, want := range []string{
		"dropped the records of 3 changes made together",
		`dropped the record of completing "Sample base"`,
		`dropped the record of adding "Sample added"`,
		"nothing was reversed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to carry %q", stdout, want)
		}
	}
	undoGroupLinesFit(t, stdout)
	if top, ok := undoTop(t, st); !ok || top.Seq != f.before.Seq {
		t.Errorf("the stack is at %+v, want the record before the group", top)
	}
	if got := undoTask(t, st, f.base.Id); !got.Status.Done() {
		t.Error("skipping the group reversed the completion")
	}
	undoTask(t, st, f.added.Id)
}

func TestUndoOfAGroupWithAForeignOpStaysInsideTheWidth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	before := undoAdd(t, st, "Sample before")
	grouped := store.WithUndoGroup(ctx, undoGroupID)
	first := undoGroupAdd(t, st, grouped, strings.Repeat("sample-long-title-", 20))
	op := strings.Repeat("z", 200)
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload, group_id) VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), op, `{"op":"`+op+`","task_id":"`+first.Id+`"}`, undoGroupID); err != nil {
		t.Fatalf("write a record from another build: %v", err)
	}
	last := undoGroupAdd(t, st, grouped, "Sample two")
	state := undoGroupState(t, st)

	code, _, stderr := undoRun(t)
	if code != exitError {
		t.Fatalf("tt undo = %d, want %d", code, exitError)
	}
	for _, want := range []string{"3 changes made together", `"zzz`, undoSkipOption} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
	undoGroupLinesFit(t, stderr)
	if after := undoGroupState(t, st); after != state {
		t.Fatal("a refused group undo changed the cache")
	}

	code, stdout, stderr := undoRun(t, undoSkipOption)
	if code != exitOK {
		t.Fatalf("tt undo --skip = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"dropped the records of 3 changes made together", `"zzz`, `"sample-long-title-`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to carry %q", stdout, want)
		}
	}
	undoGroupLinesFit(t, stdout)
	for _, task := range []model.Task{first, last} {
		undoTask(t, st, task.Id)
	}
	if top, ok := undoTop(t, st); !ok || top.Action.TaskID != before.Id || top.Group != "" {
		t.Errorf("the stack is at %+v, want the ungrouped add before the group", top)
	}
}

func TestUndoOfAGroupOfOneChangeReadsLikeASingleUndo(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	before := undoAdd(t, st, "Sample before")
	grouped := store.WithUndoGroup(ctx, undoGroupID)

	undoGroupAdd(t, st, grouped, "Sample alone")
	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" {
		t.Fatalf("tt undo = %d (stderr: %s)", code, stderr)
	}
	if want := "undid adding \"Sample alone\": the task is gone again\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if top, ok := undoTop(t, st); !ok || top.Action.TaskID != before.Id {
		t.Errorf("the stack is at %+v, want the add before the group", top)
	}

	skipped := undoGroupAdd(t, st, grouped, "Sample skipped")
	code, stdout, stderr = undoRun(t, undoSkipOption)
	if code != exitOK || stderr != "" {
		t.Fatalf("tt undo --skip = %d (stderr: %s)", code, stderr)
	}
	if want := "dropped the record of adding \"Sample skipped\"\nnothing was reversed; \"tt undo\" now reaches the change before it\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	undoTask(t, st, skipped.Id)

	if _, err := st.UpdateTask(grouped, before.Id, model.TaskEdit{Title: model.Ptr("Sample renamed")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, before.Id); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = undoRun(t)
	if code != exitError || stdout != "" {
		t.Fatalf("tt undo of a lost task = %d, stdout %q", code, stdout)
	}
	if flat := strings.Join(strings.Fields(stderr), " "); strings.Contains(flat, "made together") ||
		!strings.Contains(flat, "not in the cache") || !strings.Contains(flat, "drop this record") {
		t.Errorf("stderr = %q, want the refusal of a single change", stderr)
	}
	undoGroupLinesFit(t, stderr)

	result, code := runMachine(t, ctx, "undo", undoSkipOption)
	var data struct {
		Operation string `json:"operation"`
		Count     int    `json:"count"`
	}
	if code != exitOK || json.Unmarshal(result.Data, &data) != nil || data.Operation != "group" || data.Count != 1 {
		t.Errorf("json skip of a group of one = %d %s", code, result.Data)
	}
}
