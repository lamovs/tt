package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedSearchStore(t *testing.T, project model.Project, tasks []model.Task) {
	t.Helper()
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if project.Id != "" {
		if err := st.ReplaceProjects(context.Background(), []model.Project{project}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
	}
	for _, tk := range tasks {
		if _, err := st.CreateTask(context.Background(), tk); err != nil {
			t.Fatalf("create task %q: %v", tk.Title, err)
		}
	}
}

func TestSearchRequiresAWord(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"s"}, nil, &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run(s) = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tt: s:") {
		t.Errorf("stderr = %q, want it to name the command", stderr.String())
	}
}

func TestSearchRejectsBlankQuery(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take out the trash"},
	})

	for _, args := range [][]string{{"s", ""}, {"s", "   "}} {
		var stdout, stderr bytes.Buffer
		got := run(context.Background(), args, nil, &stdout, &stderr)
		if got != exitUsage {
			t.Fatalf("run(%v) = %d, want %d (stdout: %s stderr: %s)", args, got, exitUsage, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "search needs at least one word") {
			t.Errorf("run(%v): stderr = %q, want it to say a word is needed", args, stderr.String())
		}
		if stdout.String() != "" {
			t.Errorf("run(%v): stdout = %q, want nothing printed on a refused query", args, stdout.String())
		}
	}
}

func TestSearchFlagsBeforeWordsNamesTheMistake(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"s", "-a", "posylku"}, nil, &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run(s -a posylku) = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "words come before the options") {
		t.Errorf("stderr = %q, want it to name the ordering mistake", stderr.String())
	}
}

func TestSearchRejectsUnknownOption(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"s", "posylku", "--bogus"}, nil, &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run(s posylku --bogus) = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"--bogus"`) {
		t.Errorf("stderr = %q, want it to name the option it refused", stderr.String())
	}
}

func TestSearchMatchesAcrossTitleContentTagsAndProject(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take the posylku from the office"},
		{ProjectId: "p1", Title: "Call the courier", Content: "ask about the posylku"},
		{ProjectId: "p1", Title: "Sign for the delivery", Tags: []string{"posylku"}},
		{ProjectId: "p1", Title: "Pick something up"},
	})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"s", "posylku"}, nil, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(s posylku) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Take the posylku", "Call the courier", "Sign for the delivery"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Pick something up") {
		t.Errorf("output matched a task with no posylku anywhere:\n%s", out)
	}

	stdout.Reset()
	stderr.Reset()
	got = run(context.Background(), []string{"s", "errands"}, nil, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(s errands) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Pick something up") {
		t.Errorf("a search for the project name did not find its task:\n%s", stdout.String())
	}
}

func TestSearchIsAPhraseNotAWordSet(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Kitchen"}, []model.Task{
		{ProjectId: "p1", Title: "Buy milk and bread"},
	})

	cases := []struct {
		words     []string
		wantFound bool
	}{
		{[]string{"milk", "and"}, true},
		{[]string{"milk", "bread"}, false},
		{[]string{"bread", "milk"}, false},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		args := append([]string{"s"}, c.words...)
		got := run(context.Background(), args, nil, &stdout, &stderr)
		if got != exitOK {
			t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
		}
		found := strings.Contains(stdout.String(), "Buy milk and bread")
		if found != c.wantFound {
			t.Errorf("run(%v): found = %v, want %v\n%s", args, found, c.wantFound, stdout.String())
		}
	}
}

func TestSearchOpenOnlyUnlessDashA(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Ship the posylku", Status: model.TaskOpen},
		{ProjectId: "p1", Title: "Shipped the posylku already", Status: model.TaskDone},
	})

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"s", "posylku"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s posylku) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if strings.Contains(stdout.String(), "Shipped the posylku already") {
		t.Errorf("a finished task showed up without -a:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Ship the posylku") {
		t.Errorf("the open task did not show up:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if got := run(context.Background(), []string{"s", "posylku", "-a"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s posylku -a) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Shipped the posylku already") {
		t.Errorf("-a did not bring back the finished task:\n%s", stdout.String())
	}
}

func TestSearchWithNoMatchPrintsOneLineAndExitsClean(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take out the trash"},
	})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"s", "nonexistentword"}, nil, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(s nonexistentword) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("a search with no results wrote to stderr: %q", stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "nonexistentword") {
		t.Errorf("stdout = %q, want exactly one line naming the query", stdout.String())
	}
}

func TestSearchNumbersResultsForFollowUpCommands(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take the posylku from the office"},
		{ProjectId: "p1", Title: "Nothing to do with it"},
	})

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"s", "posylku"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s posylku) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	resolved, err := st.ResolveRefs(context.Background(), []string{"1"})
	if err != nil {
		t.Fatalf("resolve 1 after a search: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved %v, want exactly one task", resolved)
	}
	task, err := st.Task(context.Background(), resolved[0])
	if err != nil {
		t.Fatalf("read resolved task: %v", err)
	}
	if task.Title != "Take the posylku from the office" {
		t.Errorf("number 1 after the search resolved to %q, want the matching task", task.Title)
	}
}

func TestSearchResetsTheListingWhenNothingMatches(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take the posylku from the office"},
	})

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"s", "posylku"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s posylku) = %d, want %d", got, exitOK)
	}
	stdout.Reset()
	stderr.Reset()
	if got := run(context.Background(), []string{"s", "nonexistentword"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s nonexistentword) = %d, want %d", got, exitOK)
	}

	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := st.ResolveRefs(context.Background(), []string{"1"}); err == nil {
		t.Errorf("resolving 1 after an empty search still resolved, want an error about no such number")
	}
}

func TestSearchBoundsThePhraseItReportsBack(t *testing.T) {
	isolate(t)
	seedSearchStore(t, model.Project{Id: "p1", Name: "Errands"}, []model.Task{
		{ProjectId: "p1", Title: "Take the posylku from the office"},
	})

	long := strings.Repeat("z", 200)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"s", long}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(s <200 z>) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	if !strings.Contains(stdout.String(), `no tasks match "zzz`) {
		t.Errorf("stdout = %q, want it to quote back the phrase searched for", stdout.String())
	}
	for i, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
}
