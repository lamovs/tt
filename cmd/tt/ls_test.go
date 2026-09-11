package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestLsRejectsArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"a project name", []string{"ls", "Work"}},
		{"an unknown option", []string{"ls", "--bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr)
			if got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d\nstderr: %s", c.args, got, exitUsage, stderr.String())
			}
		})
	}
}

func TestLsOfNothingIsAFriendlyLineNotAnEmptyScreen(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"ls"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(ls) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatal("ls of an empty cache printed nothing at all")
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatal("ls of an empty cache printed only blank lines")
	}
}

func TestLsOfNothingReadsAsAClauseLikeItsSiblings(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"ls"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run(ls) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	const want = "no open tasks: everything is done, or nothing was ever added\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestLsReplacesAStandingListingWithAnEmptyOne(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Work"}}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "finish the report"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	var first bytes.Buffer
	if got := run(ctx, []string{"ls"}, strings.NewReader(""), &first, &bytes.Buffer{}); got != exitOK {
		t.Fatalf("first ls = %d, want %d", got, exitOK)
	}
	if listing, err := st.Listing(ctx); err != nil || len(listing) != 1 || listing[0] != created.Id {
		t.Fatalf("listing after first ls = %v, %v; want [%s]", listing, err, created.Id)
	}

	if _, err := st.CompleteTask(ctx, created.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	var second bytes.Buffer
	if got := run(ctx, []string{"ls"}, strings.NewReader(""), &second, &bytes.Buffer{}); got != exitOK {
		t.Fatalf("second ls = %d, want %d", got, exitOK)
	}
	listing, err := st.Listing(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listing) != 0 {
		t.Fatalf("listing after ls found nothing open = %v, want it cleared", listing)
	}
}

func TestLsOrdersByDueDateAcrossProjects(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.ReplaceProjects(ctx, []model.Project{
		{Id: "p1", Name: "Work"},
		{Id: "p2", Name: "Home"},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}

	day := func(y int, m time.Month, d int) model.Time {
		return model.NewTime(time.Date(y, m, d, 0, 0, 0, 0, time.Local))
	}
	tasks := []model.Task{
		{ProjectId: "p2", Title: "someday maybe"},
		{ProjectId: "p1", Title: "due later", DueDate: day(2026, 9, 20), IsAllDay: true},
		{ProjectId: "p2", Title: "due soonest", DueDate: day(2026, 9, 7), IsAllDay: true},
		{ProjectId: "p1", Title: "a closed task, not shown", Status: model.TaskDone},
	}
	for _, task := range tasks {
		if _, err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("create task %q: %v", task.Title, err)
		}
	}

	var stdout bytes.Buffer
	if got := run(ctx, []string{"ls"}, strings.NewReader(""), &stdout, &bytes.Buffer{}); got != exitOK {
		t.Fatalf("run(ls) = %d, want %d", got, exitOK)
	}
	out := stdout.String()

	if strings.Contains(out, "a closed task, not shown") {
		t.Fatalf("ls printed a done task:\n%s", out)
	}
	posSoonest := strings.Index(out, "due soonest")
	posLater := strings.Index(out, "due later")
	posSomeday := strings.Index(out, "someday maybe")
	if posSoonest == -1 || posLater == -1 || posSomeday == -1 {
		t.Fatalf("not all three open tasks were printed:\n%s", out)
	}
	if !(posSoonest < posLater && posLater < posSomeday) {
		t.Fatalf("ls did not order by due date with undated last:\n%s", out)
	}
}
