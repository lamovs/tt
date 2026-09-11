package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedDueTask(t *testing.T, title string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task.Id
}

func TestCmdDueSetsAnAllDayDate(t *testing.T) {
	isolate(t)
	seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"due", "milk", "+3d"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	task := loadDueTaskByTitle(t, "Buy milk")
	if task.DueDate.IsZero() {
		t.Fatal("due date was not set")
	}
	if !task.IsAllDay {
		t.Error("a date with no time of day should leave the task all-day")
	}

	want := "\"Buy milk\": due " + dates.FormatNow(task.DueDate, task.IsAllDay, dates.Zone(task.TimeZone)) + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestCmdDueSyncsTimeZoneWithTheNewDate(t *testing.T) {
	isolate(t)
	id := seedDueTask(t, "Buy milk")

	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if _, err := st.UpdateTask(ctx, id, model.TaskEdit{
		DueDate:  model.NewEditTime(model.NewTime(time.Now())),
		IsAllDay: model.Ptr(true),
		TimeZone: model.Ptr("Pacific/Kiritimati"),
	}); err != nil {
		t.Fatalf("seed stale time zone: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	got := run(ctx, []string{"due", "milk", "fri"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	task := loadDueTaskByTitle(t, "Buy milk")
	if task.TimeZone != "" {
		t.Errorf("time zone = %q, want it cleared so the new all-day date reads back in time.Local, not the stale zone", task.TimeZone)
	}
	if dates.Zone(task.TimeZone) != time.Local {
		t.Errorf("dates.Zone(%q) did not resolve to time.Local", task.TimeZone)
	}
}

func TestCmdDueSetsATimeOfDay(t *testing.T) {
	isolate(t)
	seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"due", "milk", "+3d", "18:00"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	task := loadDueTaskByTitle(t, "Buy milk")
	if task.DueDate.IsZero() {
		t.Fatal("due date was not set")
	}
	if task.IsAllDay {
		t.Error("a date with a time of day should not leave the task all-day")
	}
	local := task.DueDate.In(time.Local)
	if local.Hour() != 18 || local.Minute() != 0 {
		t.Errorf("due date = %v, want 18:00 local", local)
	}
}

func TestCmdDueNoneClearsTheDate(t *testing.T) {
	isolate(t)
	seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"due", "milk", "+3d"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("setting the date: run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	got := run(context.Background(), []string{"due", "milk", "none"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	task := loadDueTaskByTitle(t, "Buy milk")
	if !task.DueDate.IsZero() {
		t.Errorf("due date = %v, want cleared", task.DueDate)
	}
	if want := "\"Buy milk\": due --\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestCmdDueMisuse(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"nothing at all", []string{"due"}, "task and a date"},
		{"task with no date", []string{"due", "milk"}, "task and a date"},
		{"date that will not parse", []string{"due", "milk", "sometime"}, "could not read a date at the end of"},
		{"unknown option", []string{"due", "milk", "+3d", "--loud"}, "unknown option"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			seedDueTask(t, "Buy milk")

			var stdout, stderr bytes.Buffer
			got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr)
			if got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.wantStderr) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), c.wantStderr)
			}
		})
	}
}

func TestCmdDueNoMatch(t *testing.T) {
	isolate(t)
	seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"due", "nonexistent", "+3d"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no task matches") {
		t.Errorf("stderr = %q, want it to say no task matches", stderr.String())
	}
}

func TestCmdDueIsScopedToOpenTasks(t *testing.T) {
	isolate(t)
	id := seedDueTask(t, "Buy milk")

	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CompleteTask(ctx, id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	got := run(ctx, []string{"due", "milk", "+3d"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no task matches") {
		t.Errorf("stderr = %q, want it to say no task matches", stderr.String())
	}
}

func TestCmdDueRejectsARange(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	idA := seedDueTask(t, "Task A")
	idB := seedDueTask(t, "Task B")
	idC := seedDueTask(t, "Task C")

	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetListing(ctx, []string{idA, idB, idC}); err != nil {
		t.Fatalf("set listing: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	got := run(ctx, []string{"due", "2-3", "fri"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "names 2") {
		t.Errorf("stderr = %q, want it to name the count", stderr.String())
	}
	if !strings.Contains(stderr.String(), `run "tt help due" for examples`) {
		t.Errorf("stderr = %q, want the pointer to the examples a misuse carries", stderr.String())
	}

	taskB := loadDueTaskByTitle(t, "Task B")
	taskC := loadDueTaskByTitle(t, "Task C")
	if !taskB.DueDate.IsZero() || !taskC.DueDate.IsZero() {
		t.Error("neither task in the rejected range should have been written")
	}
}

func loadDueTaskByTitle(t *testing.T, title string) model.Task {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	found, err := st.Tasks(ctx, store.TaskFilter{Search: title})
	if err != nil {
		t.Fatalf("find task: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d tasks titled %q, want 1", len(found), title)
	}
	return found[0]
}

func TestCmdDueWritesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	seedDueTask(t, "Buy milk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"due", "milk", "fri"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	if task := loadDueTaskByTitle(t, "Buy milk"); !task.DueDate.IsZero() {
		t.Errorf("due date = %v, want it untouched by a run the user stopped", task.DueDate)
	}
}

func TestCmdDueLeavesADateThatIsAlreadySet(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"due", "milk", "+3d"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("setting the date: run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	task := loadDueTaskByTitle(t, "Buy milk")

	stdout.Reset()
	stderr.Reset()
	if got := run(ctx, []string{"due", "milk", "+3d"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	want := "\"Buy milk\": due already " +
		dates.FormatNow(task.DueDate, task.IsAllDay, dates.Zone(task.TimeZone)) + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	if got := run(ctx, []string{"undo"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if got := loadDueTaskByTitle(t, "Buy milk"); !got.DueDate.IsZero() {
		t.Errorf("due date after one undo = %v, want it cleared: the second run left a record of its own", got.DueDate)
	}
}

func TestCmdDueLeavesAnUndatedTaskAlone(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	id := seedDueTask(t, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"due", "milk", "none"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	if want := "\"Buy milk\": already has no due date\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	e, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatalf("read the undo stack: %v", err)
	}
	if e.Action.Op != store.OpTaskCreate || e.Action.TaskID != id {
		t.Errorf("the record on top is %+v, want the creation of %s", e.Action, id)
	}
}

func TestCmdDueRewritesADateThatIsRightInEveryWayButOne(t *testing.T) {
	cases := []struct {
		name   string
		allDay bool
		zone   string
	}{
		{"a zone another client left on it", true, "Pacific/Kiritimati"},
		{"the same instant with a time of day", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			id := seedDueTask(t, "Buy milk")

			ctx := context.Background()

			want, err := dates.ParseNow("fri")
			if err != nil {
				t.Fatalf("parse the date the command will parse: %v", err)
			}
			st, err := store.Open(ctx, "")
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if _, err := st.UpdateTask(ctx, id, model.TaskEdit{
				DueDate:  model.NewEditTime(want.Time),
				IsAllDay: model.Ptr(c.allDay),
				TimeZone: model.Ptr(c.zone),
			}); err != nil {
				st.Close()
				t.Fatalf("seed the task: %v", err)
			}
			st.Close()

			var stdout, stderr bytes.Buffer
			if got := run(ctx, []string{"due", "milk", "fri"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
				t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
			}
			if strings.Contains(stdout.String(), "already") {
				t.Fatalf("stdout = %q, want the write reported: the instant matches and the rest of the edit does not",
					stdout.String())
			}

			task := loadDueTaskByTitle(t, "Buy milk")
			if task.TimeZone != "" {
				t.Errorf("time zone = %q, want it cleared so the date reads back where it was computed", task.TimeZone)
			}
			if !task.IsAllDay {
				t.Error("the task should have come out all-day, which is what a date with no time of day means")
			}
			if !task.DueDate.Equal(want.Time.Time) {
				t.Errorf("due date = %v, want it left at %v", task.DueDate, want.Time.Time)
			}
		})
	}
}

func TestDueSetsNothingWhenTheSignalLandsOnTheQuestion(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)
	seedDueTask(t, "buy milk")
	seedDueTask(t, "buy bread")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mvInterruptingReader{cancel: cancel, answer: strings.NewReader("1\n")}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"due", "buy", "fri"}, stdin, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run(due buy fri) = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if !strings.Contains(stderr.String(), "which one?") {
		t.Fatalf("stderr = %q, want the question the signal landed on", stderr.String())
	}
	if strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr = %q, want the database's complaint kept off the screen", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no date was set to report", stdout.String())
	}
	for _, title := range []string{"buy milk", "buy bread"} {
		if task := loadDueTaskByTitle(t, title); !task.DueDate.IsZero() {
			t.Errorf("%q was given a due date by a run the user stopped", title)
		}
	}
}
