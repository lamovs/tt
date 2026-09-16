package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func undoGroupDump(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	names, err := s.DB().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			names.Close()
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := names.Close(); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, table := range tables {
		rows, err := s.DB().QueryContext(ctx, fmt.Sprintf(`SELECT * FROM %q ORDER BY rowid`, table))
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s:", table)
			for _, value := range values {
				if raw, ok := value.([]byte); ok {
					value = string(raw)
				}
				fmt.Fprintf(&b, " %T=%v", value, value)
			}
			b.WriteString("\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func undoGroupTask(t *testing.T, s *Store, ctx context.Context, title string) model.Task {
	t.Helper()
	task, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title, Status: model.TaskOpen})
	if err != nil {
		t.Fatalf("add %q: %v", title, err)
	}
	return task
}

func undoGroupSeqs(entries []UndoEntry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.Seq
	}
	return out
}

func TestUndoGroupMigrationKeepsExistingRecordsUngrouped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	const beforeUndoGroups = 16
	db := openAtVersion(t, path, beforeUndoGroups)
	if tableColumns(t, db, "undo_log")["group_id"] {
		t.Fatalf("migration %d already creates undo_log.group_id: a cache written before it would never get the column", beforeUndoGroups)
	}
	for _, q := range []string{
		`INSERT INTO undo_log (at, kind, payload) VALUES (101, 'task.create', '{"op":"task.create","task_id":"local-00000000000000a1","project_id":"p1"}')`,
		`INSERT INTO undo_log (at, kind, payload) VALUES (102, 'task.update', '{"op":"task.update","task_id":"sample-task-1","before":{"title":"Old sample"}}')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	rowsOf := func(q execer) [][4]string {
		rows, err := q.QueryContext(ctx, `SELECT seq, at, kind, payload FROM undo_log ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][4]string
		for rows.Next() {
			var row [4]string
			if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := rowsOf(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open a cache written before undo groups: %v", err)
	}
	defer s.Close()
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version <= beforeUndoGroups || version != latestVersion(t) {
		t.Fatalf("version %d, want the latest past %d", version, beforeUndoGroups)
	}
	if !tableColumns(t, s.DB(), "undo_log")["group_id"] {
		t.Fatal("undo_log.group_id is missing after the upgrade")
	}
	if after := rowsOf(s.DB()); !reflect.DeepEqual(after, before) {
		t.Fatalf("the upgrade rewrote undo records:\nbefore %v\nafter  %v", before, after)
	}
	var grouped int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM undo_log WHERE group_id IS NOT NULL`).Scan(&grouped); err != nil {
		t.Fatal(err)
	}
	if grouped != 0 {
		t.Fatalf("%d existing record(s) joined a group", grouped)
	}
	group, err := s.LastUndoGroup(ctx)
	if err != nil || len(group) != 1 || group[0].Group != "" || group[0].Action.TaskID != "sample-task-1" {
		t.Fatalf("existing top record read as %+v, %v; want it alone and ungrouped", group, err)
	}

	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	grouped2 := WithUndoGroup(ctx, "group-after-upgrade")
	first := undoGroupTask(t, s, grouped2, "Sample one")
	second := undoGroupTask(t, s, grouped2, "Sample two")
	group, err = s.LastUndoGroup(ctx)
	if err != nil || len(group) != 2 || group[0].Action.TaskID != second.Id || group[1].Action.TaskID != first.Id {
		t.Fatalf("group on the upgraded cache = %+v, %v; want the two new records and nothing older", group, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if again, err := reopened.LastUndoGroup(ctx); err != nil || !reflect.DeepEqual(undoGroupSeqs(again), undoGroupSeqs(group)) {
		t.Fatalf("group after reopening = %+v, %v", again, err)
	}
}

func TestUndoGroupRecordsFollowTheContext(t *testing.T) {
	ctx := context.Background()
	id, err := NewUndoGroupID()
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewUndoGroupID()
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || id == other {
		t.Fatalf("group ids %q and %q, want two distinct ones", id, other)
	}
	grouped := WithUndoGroup(ctx, id)
	if got := UndoGroupFrom(ctx); got != "" {
		t.Fatalf("plain context carries group %q", got)
	}
	if got := UndoGroupFrom(grouped); got != id {
		t.Fatalf("group context carries %q, want %q", got, id)
	}
	cleared := WithUndoGroup(grouped, "")
	if got := UndoGroupFrom(cleared); got != "" {
		t.Fatalf("cleared context carries %q", got)
	}

	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	task := undoGroupTask(t, s, grouped, "Sample alpha")
	if _, err := s.AddTaskItem(grouped, task.Id, "sample step"); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewEntityMutation(grouped, EntityMutation{Ref: EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Sample folder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEntityMutation(grouped, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(cleared, task.Id, CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, task.Id, model.TaskEdit{Title: model.Ptr("Sample alpha renamed")}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.DB().QueryContext(ctx, `SELECT kind, group_id, instr(payload, '"_tt"') > 0 FROM undo_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var (
			kind    string
			group   *string
			feature bool
		)
		if err := rows.Scan(&kind, &group, &feature); err != nil {
			t.Fatal(err)
		}
		name := "NULL"
		if group != nil {
			name = *group
		}
		got = append(got, fmt.Sprintf("%s %s %v", kind, name, feature))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		OpTaskCreate + " " + id + " false",
		OpTaskUpdate + " " + id + " true",
		OpEntityMutation + " " + id + " false",
		OpTaskComplete + " NULL true",
		OpTaskUpdate + " NULL false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("undo records:\n got %q\nwant %q", got, want)
	}
}

func TestUndoGroupReversesMixedChangesTogether(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	done := undoGroupTask(t, s, ctx, "Sample beta")
	listed := undoGroupTask(t, s, ctx, "Sample gamma")
	earlier, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	grouped := WithUndoGroup(ctx, "group-mixed")
	added := undoGroupTask(t, s, grouped, "Sample alpha")
	if _, err := s.CompleteTask(grouped, done.Id, CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskItem(grouped, listed.Id, "sample step"); err != nil {
		t.Fatal(err)
	}

	group, err := s.LastUndoGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range group {
		if e.Group != "group-mixed" {
			t.Fatalf("record %d carries group %q", e.Seq, e.Group)
		}
		ops = append(ops, e.Action.Op+" "+e.Action.TaskID)
	}
	wantOps := []string{OpTaskUpdate + " " + listed.Id, OpTaskComplete + " " + done.Id, OpTaskCreate + " " + added.Id}
	if !reflect.DeepEqual(ops, wantOps) {
		t.Fatalf("group = %q, want %q newest first", ops, wantOps)
	}

	tasks, err := s.ApplyUndoGroup(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 || tasks[0].Id != listed.Id || tasks[1].Id != done.Id || tasks[2].Id != added.Id {
		t.Fatalf("reversed tasks = %+v, want one per record in group order", tasks)
	}
	if _, err := s.Task(ctx, added.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the added task is still cached: %v", err)
	}
	if got, err := s.Task(ctx, done.Id); err != nil || got.Status.Done() || !got.CompletedTime.IsZero() {
		t.Errorf("completed task after undo = %+v, %v; want it open again", got, err)
	}
	if got, err := s.Task(ctx, listed.Id); err != nil || len(got.Items) != 0 {
		t.Errorf("checklist task after undo = %+v, %v; want the item gone", got, err)
	}
	top, err := s.LastUndoGroup(ctx)
	if err != nil || len(top) != 1 || top[0].Seq != earlier.Seq || top[0].Group != "" {
		t.Fatalf("stack after the group = %+v, %v; want the ungrouped record before it", top, err)
	}

	queued := map[string][]string{}
	rows, err := s.DB().QueryContext(ctx, `SELECT task_id, op FROM outbox ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, op string
		if err := rows.Scan(&taskID, &op); err != nil {
			t.Fatal(err)
		}
		queued[taskID] = append(queued[taskID], op)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		done.Id:   {OpTaskCreate, OpTaskComplete, OpTaskUpdate},
		listed.Id: {OpTaskCreate, OpTaskUpdate, OpTaskUpdate},
	}
	if !reflect.DeepEqual(queued, want) {
		t.Fatalf("queue = %v, want each reversal queued like any other change and nothing left of the unsent add", queued)
	}
}

func TestUndoGroupStopsAtARecordOutsideIt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	one := WithUndoGroup(ctx, "group-one")
	two := WithUndoGroup(ctx, "group-two")
	first := undoGroupTask(t, s, one, "Sample one")
	second := undoGroupTask(t, s, one, "Sample two")
	loose := undoGroupTask(t, s, ctx, "Sample loose")
	third := undoGroupTask(t, s, one, "Sample three")
	fourth := undoGroupTask(t, s, two, "Sample four")
	fifth := undoGroupTask(t, s, two, "Sample five")

	steps := []struct {
		name  string
		tasks []model.Task
		group string
	}{
		{"a group on top stops at another group", []model.Task{fifth, fourth}, "group-two"},
		{"a group does not reach across a record outside it", []model.Task{third}, "group-one"},
		{"an ungrouped record on top of a group undoes only itself", []model.Task{loose}, ""},
		{"the rest of the group comes last", []model.Task{second, first}, "group-one"},
	}
	remaining := []model.Task{first, second, loose, third, fourth, fifth}
	for _, step := range steps {
		group, err := s.LastUndoGroup(ctx)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if len(group) != len(step.tasks) {
			t.Fatalf("%s: group has %d record(s), want %d", step.name, len(group), len(step.tasks))
		}
		for i, e := range group {
			if e.Action.TaskID != step.tasks[i].Id || e.Group != step.group {
				t.Fatalf("%s: record %d is %s in group %q, want %s in %q", step.name, i, e.Action.TaskID, e.Group, step.tasks[i].Id, step.group)
			}
		}
		if step.group == "" {
			if _, err := s.ApplyUndo(ctx, group[0]); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
		} else if _, err := s.ApplyUndoGroup(ctx, group); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		undone := map[string]bool{}
		for _, task := range step.tasks {
			undone[task.Id] = true
		}
		var kept []model.Task
		for _, task := range remaining {
			_, err := s.Task(ctx, task.Id)
			switch {
			case undone[task.Id] && !errors.Is(err, ErrNotFound):
				t.Fatalf("%s: %q is still cached: %v", step.name, task.Title, err)
			case !undone[task.Id] && err != nil:
				t.Fatalf("%s: %q was reached: %v", step.name, task.Title, err)
			case !undone[task.Id]:
				kept = append(kept, task)
			}
		}
		remaining = kept
	}
	if _, err := s.LastUndoGroup(ctx); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("stack after every step: %v, want it empty", err)
	}
}

func TestUndoGroupFailureLeavesEverythingInPlace(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	edited := undoGroupTask(t, s, ctx, "Sample beta")
	done := undoGroupTask(t, s, ctx, "Sample gamma")
	earlier, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grouped := WithUndoGroup(ctx, "group-fail")
	added := undoGroupTask(t, s, grouped, "Sample alpha")
	if _, err := s.UpdateTask(grouped, edited.Id, model.TaskEdit{Title: model.Ptr("Sample beta renamed")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(grouped, done.Id, CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	group, err := s.LastUndoGroup(ctx)
	if err != nil || len(group) != 3 {
		t.Fatalf("group = %+v, %v", group, err)
	}

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, edited.Id); err != nil {
		t.Fatal(err)
	}
	before := undoGroupDump(t, s)
	_, err = s.ApplyUndoGroup(ctx, group)
	var failed *UndoGroupError
	if !errors.As(err, &failed) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("undo of a group whose middle record lost its task = %v, want an UndoGroupError over ErrNotFound", err)
	}
	if failed.Index != 1 || failed.Count != 3 || failed.Entry.Seq != group[1].Seq || failed.Group != "group-fail" {
		t.Fatalf("failure names %+v, want the middle record of three", failed)
	}
	if after := undoGroupDump(t, s); after != before {
		t.Fatalf("a failed group undo changed the cache:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got, err := s.Task(ctx, done.Id); err != nil || !got.Status.Done() {
		t.Fatalf("the reversal before the failure was kept: %+v, %v", got, err)
	}
	again, err := s.LastUndoGroup(ctx)
	if err != nil || !reflect.DeepEqual(undoGroupSeqs(again), undoGroupSeqs(group)) {
		t.Fatalf("group after the failure = %+v, %v; want every record still there", again, err)
	}

	if err := s.DropUndoGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	top, err := s.LastUndoGroup(ctx)
	if err != nil || len(top) != 1 || top[0].Seq != earlier.Seq {
		t.Fatalf("stack after dropping the group = %+v, %v; want the record before it", top, err)
	}
	if got, err := s.Task(ctx, done.Id); err != nil || !got.Status.Done() {
		t.Fatalf("dropping reversed the completion: %+v, %v", got, err)
	}
	if _, err := s.Task(ctx, added.Id); err != nil {
		t.Fatalf("dropping reversed the add: %v", err)
	}
}

func TestUndoGroupRefusesWhatOneUndoDoesNotReach(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	undoGroupTask(t, s, ctx, "Sample loose")
	grouped := WithUndoGroup(ctx, "group-stale")
	undoGroupTask(t, s, grouped, "Sample one")
	undoGroupTask(t, s, grouped, "Sample two")
	group, err := s.LastUndoGroup(ctx)
	if err != nil || len(group) != 2 {
		t.Fatalf("group = %+v, %v", group, err)
	}

	before := undoGroupDump(t, s)
	cases := []struct {
		name  string
		apply func() error
		want  error
	}{
		{"part of the group", func() error { _, err := s.ApplyUndoGroup(ctx, group[:1]); return err }, ErrUndoConflict},
		{"nothing", func() error { _, err := s.ApplyUndoGroup(ctx, nil); return err }, ErrUndoConflict},
		{"one record of the group on its own", func() error { _, err := s.ApplyUndo(ctx, group[0]); return err }, ErrUndoGrouped},
		{"dropping part of the group", func() error { return s.DropUndoGroup(ctx, group[1:]) }, ErrUndoConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.apply(); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if after := undoGroupDump(t, s); after != before {
				t.Fatalf("a refused undo changed the cache:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}

	undoGroupTask(t, s, grouped, "Sample late")
	before = undoGroupDump(t, s)
	if _, err := s.ApplyUndoGroup(ctx, group); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("undo of a group that grew since it was read = %v, want ErrUndoConflict", err)
	}
	if err := s.DropUndoGroup(ctx, group); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("drop of a group that grew since it was read = %v, want ErrUndoConflict", err)
	}
	if after := undoGroupDump(t, s); after != before {
		t.Fatalf("a stale group undo changed the cache:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	grown, err := s.LastUndoGroup(ctx)
	if err != nil || len(grown) != 3 {
		t.Fatalf("group after it grew = %+v, %v", grown, err)
	}
	if _, err := s.ApplyUndoGroup(ctx, grown); err != nil {
		t.Fatal(err)
	}
}

func TestUngroupedUndoKeepsItsOneRecordAtATime(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Inbox"})
	first := undoGroupTask(t, s, ctx, "Sample one")
	second := undoGroupTask(t, s, ctx, "Sample two")
	single := undoGroupTask(t, s, WithUndoGroup(ctx, "group-single"), "Sample single")
	other := undoGroupTask(t, s, WithUndoGroup(ctx, "group-other"), "Sample other")

	top, err := s.LastUndo(ctx)
	if err != nil || top.Group != "group-other" || top.Action.TaskID != other.Id {
		t.Fatalf("top = %+v, %v", top, err)
	}
	if _, err := s.ApplyUndo(ctx, top); err != nil {
		t.Fatalf("a group of one record whose neighbour is another group: %v", err)
	}
	top, err = s.LastUndo(ctx)
	if err != nil || top.Action.TaskID != single.Id {
		t.Fatalf("top = %+v, %v", top, err)
	}
	if _, err := s.ApplyUndo(ctx, top); err != nil {
		t.Fatalf("a group of one record on an ungrouped one: %v", err)
	}

	for _, want := range []model.Task{second, first} {
		group, err := s.LastUndoGroup(ctx)
		if err != nil || len(group) != 1 || group[0].Group != "" || group[0].Action.TaskID != want.Id {
			t.Fatalf("group = %+v, %v; want the ungrouped add of %q alone", group, err, want.Title)
		}
		top, err := s.LastUndo(ctx)
		if err != nil || top.Seq != group[0].Seq {
			t.Fatalf("LastUndo = %+v, %v; want the record LastUndoGroup returned", top, err)
		}
		if _, err := s.ApplyUndo(ctx, top); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Task(ctx, want.Id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q is still cached: %v", want.Title, err)
		}
	}
	if _, err := s.LastUndoGroup(ctx); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("empty stack = %v, want ErrNoUndo", err)
	}
	if _, err := s.ApplyUndoGroup(ctx, nil); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("group undo on an empty stack = %v, want ErrNoUndo", err)
	}
}
