package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedProjects(t *testing.T, projects ...model.Project) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, projects); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
}

func cachedTasks(t *testing.T) []model.Task {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	return tasks
}

func TestCmdAddDefaultProject(t *testing.T) {
	isolate(t)

	defaultName := config.Default().DefaultProject
	seedProjects(t, model.Project{Id: "p1", Name: defaultName, Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"add", "Buy", "milk"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Buy milk") || !strings.Contains(stdout.String(), defaultName) {
		t.Fatalf("stdout = %q, want the title and the default project named", stdout.String())
	}

	tasks := cachedTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Title != "Buy milk" || tasks[0].ProjectId != "p1" {
		t.Fatalf("task = %+v, want title %q in project p1", tasks[0], "Buy milk")
	}
	if tasks[0].Priority != model.PriorityNone {
		t.Fatalf("priority = %v, want none", tasks[0].Priority)
	}
	if !tasks[0].DueDate.IsZero() {
		t.Fatalf("due date = %v, want none", tasks[0].DueDate)
	}
}

func TestCmdAddProjectDateAndPriority(t *testing.T) {
	isolate(t)
	seedProjects(t,
		model.Project{Id: "p1", Name: "Personal", Kind: "TASK"},
		model.Project{Id: "p2", Name: "Work", Kind: "TASK"},
	)

	var stdout, stderr bytes.Buffer
	args := []string{"add", "pay", "rent", "-P", "work", "-d", "+3d", "-p", "high"}
	got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Work") {
		t.Fatalf("stdout = %q, want the project it landed in", stdout.String())
	}

	tasks := cachedTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	got1 := tasks[0]
	if got1.Title != "pay rent" {
		t.Errorf("title = %q, want %q", got1.Title, "pay rent")
	}
	if got1.ProjectId != "p2" {
		t.Errorf("project id = %q, want p2 (Work)", got1.ProjectId)
	}
	if got1.Priority != model.PriorityHigh {
		t.Errorf("priority = %v, want high", got1.Priority)
	}
	if got1.DueDate.IsZero() {
		t.Errorf("due date is zero, want +3d to have set one")
	}
	if !got1.IsAllDay {
		t.Errorf("all day = false, want true for a date with no time of day")
	}
}

func TestCmdAddDateWithTimeIsMultipleWords(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: "Personal", Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	args := []string{"add", "stand-up", "-P", "personal", "-d", "fri", "18:00"}
	got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	tasks := cachedTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Title != "stand-up" {
		t.Errorf("title = %q, want %q", tasks[0].Title, "stand-up")
	}
	if tasks[0].IsAllDay {
		t.Errorf("all day = true, want false: a time of day was given")
	}
	if tasks[0].DueDate.IsZero() {
		t.Errorf("due date is zero, want fri 18:00 to have set one")
	}
}

func TestCmdAddNotesExistingDuplicateButCreatesAnyway(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal", Kind: "TASK"}}); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Buy milk"}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	got := run(ctx, []string{"add", "buy", "milk", "-P", "personal"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "note:") {
		t.Fatalf("stdout = %q, want a note about the existing task", stdout.String())
	}

	tasks := cachedTasks(t)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (the duplicate is created, not refused)", len(tasks))
	}
}

func TestCmdAddNoProjectsYet(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"add", "Buy", "milk"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if len(cachedTasks(t)) != 0 {
		t.Fatalf("a task was written despite there being no project to put it in")
	}
}

func TestCmdAddUnknownProjectFails(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: "Personal", Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"add", "Buy", "milk", "-P", "nosuchlist"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
}

func TestCmdAddMisuse(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no title", []string{"add"}},
		{"title is only whitespace flags aside", []string{"add", "-p", "high"}},
		{"unknown option", []string{"add", "milk", "-x"}},
		{"-P with no value", []string{"add", "milk", "-P"}},
		{"-d with no value", []string{"add", "milk", "-d"}},
		{"-p with no value", []string{"add", "milk", "-p"}},
		{"bad priority word", []string{"add", "milk", "-p", "urgent"}},
		{"bad date expression", []string{"add", "milk", "-d", "whenever"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr)
			if got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitUsage, stderr.String())
			}
			if len(cachedTasks(t)) != 0 {
				t.Fatalf("run(%v) wrote a task despite being a misuse", c.args)
			}
		})
	}
}

func TestCmdAddTakesATitleAfterTheMarker(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	args := []string{"add", "--", "-5 min plank", "-d"}
	got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
	}

	tasks := cachedTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}

	if want := "-5 min plank -d"; tasks[0].Title != want {
		t.Errorf("title = %q, want %q", tasks[0].Title, want)
	}
}

func TestCmdAddOffersTheMarker(t *testing.T) {
	if got := helpText(t, "add"); !strings.Contains(got, endOfOptions) {
		t.Errorf("tt help add does not say how to give a title that starts with a dash:\n%s", got)
	}
}

func TestCmdAddBoundsTheOptionItRefuses(t *testing.T) {
	isolate(t)
	long := "--" + strings.Repeat("z", 200)
	var stdout, stderr bytes.Buffer
	args := []string{"add", "hello", long}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
		t.Fatalf("run(add hello <200 z>) = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}

	if !strings.Contains(stderr.String(), "zzz") {
		t.Errorf("stderr = %q, want it to quote back what was typed", stderr.String())
	}
	for i, line := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
	if len(cachedTasks(t)) != 0 {
		t.Fatalf("run(%v) wrote a task despite being a misuse", args)
	}
}
