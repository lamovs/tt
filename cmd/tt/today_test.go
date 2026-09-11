package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func fromServerTasks(tasks ...model.Task) []store.ServerTask {
	out := make([]store.ServerTask, 0, len(tasks))
	for _, t := range tasks {
		raw, _ := json.Marshal(map[string]string{"id": t.Id, "title": t.Title})
		out = append(out, store.ServerTask{Task: t, Raw: raw})
	}
	return out
}

func seedTodayStore(t *testing.T, tasks ...model.Task) {
	t.Helper()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.ReplaceProjects(context.Background(), []model.Project{{Id: "p1", Name: "Inbox"}}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	if _, err := s.SyncProject(context.Background(), "p1", fromServerTasks(tasks...)); err != nil {
		t.Fatalf("sync project: %v", err)
	}
}

func openTask(id, title string) model.Task {
	return model.Task{Id: id, ProjectId: "p1", Title: title, Status: model.TaskOpen}
}

func TestTodayRejectsArguments(t *testing.T) {
	for _, args := range [][]string{
		{"today", "inbox"},
		{"today", "--bogus"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d\nstderr: %s", args, got, exitUsage, stderr.String())
			}
		})
	}
}

func TestTodayEmpty(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"today"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(today) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatal("an empty day printed nothing at all")
	}
	if strings.Contains(stdout.String(), "[") {
		t.Errorf("an empty day printed what looks like a listing: %q", stdout.String())
	}
}

func TestTodayReplacesAStandingListingWithAnEmptyOne(t *testing.T) {
	isolate(t)
	ctx := context.Background()

	seedTodayStore(t, openTask("t1", "No due date"))

	var listed bytes.Buffer
	if got := run(ctx, []string{"ls"}, strings.NewReader(""), &listed, &bytes.Buffer{}); got != exitOK {
		t.Fatalf("run(ls) = %d, want %d", got, exitOK)
	}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"today"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(today) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}

	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	listing, err := st.Listing(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listing) != 0 {
		t.Fatalf("listing after a today that found nothing = %v, want it cleared", listing)
	}
}

func TestTodayOverdueThenToday(t *testing.T) {
	isolate(t)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	overdue := openTask("t1", "Overdue task")
	overdue.DueDate = model.NewTime(today.AddDate(0, 0, -3).Add(9 * time.Hour))

	dueToday := openTask("t2", "All-day today task")
	dueToday.DueDate = model.NewTime(today)
	dueToday.IsAllDay = true

	future := openTask("t3", "Future task")
	future.DueDate = model.NewTime(today.AddDate(0, 0, 3))
	future.IsAllDay = true

	doneToday := openTask("t4", "Done task")
	doneToday.DueDate = model.NewTime(today)
	doneToday.IsAllDay = true
	doneToday.Status = model.TaskDone
	doneToday.CompletedTime = model.NewTime(today)

	undated := openTask("t5", "No due date")

	seedTodayStore(t, overdue, dueToday, future, doneToday, undated)

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"today"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(today) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	out := stdout.String()

	overduePos := strings.Index(out, "Overdue task")
	todayPos := strings.Index(out, "All-day today task")
	if overduePos < 0 || todayPos < 0 {
		t.Fatalf("both tasks should be listed:\n%s", out)
	}
	if overduePos > todayPos {
		t.Errorf("overdue did not come first:\n%s", out)
	}
	for _, absent := range []string{"Future task", "Done task", "No due date"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q should not be in today's list:\n%s", absent, out)
		}
	}
	if strings.Contains(out, "overdue") == false {
		t.Errorf("the overdue task should be marked overdue:\n%s", out)
	}

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "All-day today task") && strings.Contains(line, "overdue") {
			t.Errorf("an all-day task due today was marked overdue: %q", line)
		}
	}
}

func TestTodayListsNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"today"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run(today) = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
}

func TestTodayReadsTheClockOnce(t *testing.T) {
	isolate(t)

	fixed := time.Date(2031, time.March, 12, 15, 0, 0, 0, time.Local)
	reads := 0
	saved := todayNow
	todayNow = func() time.Time {
		reads++
		return fixed
	}
	t.Cleanup(func() { todayNow = saved })

	day := time.Date(2031, time.March, 12, 0, 0, 0, 0, time.Local)
	late := openTask("t1", "Late task")
	late.DueDate = model.NewTime(day.AddDate(0, 0, -3).Add(9 * time.Hour))
	dueToday := openTask("t2", "Today task")
	dueToday.DueDate = model.NewTime(day)
	dueToday.IsAllDay = true
	tomorrow := openTask("t3", "Tomorrow task")
	tomorrow.DueDate = model.NewTime(day.AddDate(0, 0, 1))
	tomorrow.IsAllDay = true

	seedTodayStore(t, late, dueToday, tomorrow)

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"today"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(today) = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	if reads != 1 {
		t.Errorf("the clock was read %d times, want once", reads)
	}
	out := stdout.String()

	if strings.Contains(out, "Tomorrow task") {
		t.Errorf("a task due the day after the fixed instant is in the view:\n%s", out)
	}
	for _, want := range []string{"Late task", "Today task"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from the view:\n%s", want, out)
		}
	}

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Late task") && !strings.Contains(line, "overdue") {
			t.Errorf("a task three days past the fixed instant is not marked overdue: %q", line)
		}
		if strings.Contains(line, "Today task") && strings.Contains(line, "overdue") {
			t.Errorf("a task due on the fixed instant's own day is marked overdue: %q", line)
		}
	}
}
