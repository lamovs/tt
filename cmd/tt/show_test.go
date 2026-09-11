package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedForShow(t *testing.T, fill func(ctx context.Context, s *store.Store)) {
	t.Helper()
	s, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fill(context.Background(), s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCmdShowNoArguments(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr: %s", got, exitUsage, stderr.String())
	}
}

func TestCmdShowByNumberDoesNotRenumber(t *testing.T) {
	isolate(t)
	var ids []string
	seedForShow(t, func(ctx context.Context, s *store.Store) {
		if err := s.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal"}}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
		first, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "buy milk"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		second, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "pay rent"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = []string{first.Id, second.Id}
		if err := s.SetListing(ctx, ids); err != nil {
			t.Fatalf("set listing: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show", "2"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pay rent") {
		t.Errorf("card missing the title:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Personal") {
		t.Errorf("card missing the project:\n%s", stdout.String())
	}

	s, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	listing, err := s.Listing(context.Background())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if strings.Join(listing, ",") != strings.Join(ids, ",") {
		t.Fatalf("listing changed to %v", listing)
	}
}

func TestCmdShowPrintsStoredScheduleMetadata(t *testing.T) {
	isolate(t)
	var ids []string
	wantReminders := []string{"TRIGGER:-PT10M", "TRIGGER:-PT1H", "TRIGGER:-PT10M"}
	seedForShow(t, func(ctx context.Context, s *store.Store) {
		if err := s.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal"}}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
		plain, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "buy milk"})
		if err != nil {
			t.Fatalf("create plain task: %v", err)
		}
		scheduled, err := s.CreateTask(ctx, model.Task{
			ProjectId:  "p1",
			Title:      "send report",
			RepeatFlag: "RRULE:FREQ=WEEKLY;INTERVAL=1",
			Reminders:  wantReminders,
		})
		if err != nil {
			t.Fatalf("create scheduled task: %v", err)
		}
		ids = []string{plain.Id, scheduled.Id}
		if err := s.SetListing(ctx, ids); err != nil {
			t.Fatalf("set listing: %v", err)
		}
	})

	s, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("reopen seeded cache: %v", err)
	}
	stored, err := s.Task(context.Background(), ids[1])
	if err != nil {
		t.Fatalf("read scheduled task: %v", err)
	}
	if stored.RepeatFlag != "RRULE:FREQ=WEEKLY;INTERVAL=1" {
		t.Errorf("stored repeat = %q", stored.RepeatFlag)
	}
	if !slices.Equal(stored.Reminders, wantReminders) {
		t.Errorf("stored reminders = %q, want %q", stored.Reminders, wantReminders)
	}
	listing, err := s.Listing(context.Background())
	if err != nil {
		t.Fatalf("read seeded listing: %v", err)
	}
	if !slices.Equal(listing, ids) {
		t.Fatalf("seeded listing = %v, want %v", listing, ids)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close reopened cache: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show", "2"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("scheduled show exit = %d, want %d; stderr: %s", got, exitOK, stderr.String())
	}
	wantScheduled := "[ ] send report\n" +
		"    list      Personal\n" +
		"    due       --\n" +
		"    priority  none\n" +
		"    repeat    RRULE:FREQ=WEEKLY;INTERVAL=1\n" +
		"    reminder  TRIGGER:-PT10M\n" +
		"    reminder  TRIGGER:-PT1H\n" +
		"    reminder  TRIGGER:-PT10M\n"
	if stdout.String() != wantScheduled {
		t.Errorf("scheduled show stdout:\n%s\nwant:\n%s", stdout.String(), wantScheduled)
	}

	stdout.Reset()
	stderr.Reset()
	got = run(context.Background(), []string{"show", "1"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("plain show exit = %d, want %d; stderr: %s", got, exitOK, stderr.String())
	}
	wantPlain := "[ ] buy milk\n" +
		"    list      Personal\n" +
		"    due       --\n" +
		"    priority  none\n"
	if stdout.String() != wantPlain {
		t.Errorf("plain show stdout:\n%s\nwant:\n%s", stdout.String(), wantPlain)
	}

	s, err = store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("reopen after show: %v", err)
	}
	defer s.Close()
	listing, err = s.Listing(context.Background())
	if err != nil {
		t.Fatalf("listing after show: %v", err)
	}
	if !slices.Equal(listing, ids) {
		t.Fatalf("listing after show = %v, want %v", listing, ids)
	}
}

func TestCmdShowFindsADoneTask(t *testing.T) {
	isolate(t)
	seedForShow(t, func(ctx context.Context, s *store.Store) {
		if err := s.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal"}}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
		created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "renew passport"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.CompleteTask(ctx, created.Id, store.CompleteOptions{}); err != nil {
			t.Fatalf("complete: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer

	got := run(context.Background(), []string{"show", "passport"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", got, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "[x] renew passport") {
		t.Errorf("card does not show as done:\n%s", stdout.String())
	}
}

func TestCmdShowNoMatch(t *testing.T) {
	isolate(t)
	seedForShow(t, func(ctx context.Context, s *store.Store) {
		if err := s.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal"}}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
		if _, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "buy milk"}); err != nil {
			t.Fatalf("create: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show", "nothing-like-that"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("exit = %d, want %d; stdout: %s stderr: %s", got, exitError, stdout.String(), stderr.String())
	}
}

func TestCmdShowRejectsMoreThanOneReference(t *testing.T) {
	isolate(t)
	seedForShow(t, func(ctx context.Context, s *store.Store) {
		if err := s.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal"}}); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
		first, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "buy milk"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		second, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "pay rent"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := s.SetListing(ctx, []string{first.Id, second.Id}); err != nil {
			t.Fatalf("set listing: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show", "1", "2"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr: %s", got, exitUsage, stderr.String())
	}
}

func TestCmdShowRejectsUnknownOption(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"show", "1", "--bogus"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr: %s", got, exitUsage, stderr.String())
	}
}
