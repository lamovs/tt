package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func priStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedTask(t *testing.T, s *store.Store, title string) model.Task {
	t.Helper()
	got, err := s.CreateTask(context.Background(), model.Task{ProjectId: "p1", Title: title})
	if err != nil {
		t.Fatalf("create task %q: %v", title, err)
	}
	return got
}

func TestPriSetsThePriority(t *testing.T) {
	isolate(t)
	s := priStore(t)
	task := seedTask(t, s, "buy milk")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "milk", "high"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "buy milk") || !strings.Contains(stdout.String(), "high") {
		t.Errorf("stdout = %q, want it to name the task and the level", stdout.String())
	}

	updated, err := s.Task(context.Background(), task.Id)
	if err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if updated.Priority != model.PriorityHigh {
		t.Errorf("priority = %v, want %v", updated.Priority, model.PriorityHigh)
	}
	if updated.Title != "buy milk" {
		t.Errorf("title = %q, want it untouched", updated.Title)
	}
}

func TestPriByListingPosition(t *testing.T) {
	isolate(t)
	s := priStore(t)
	task := seedTask(t, s, "call mom")
	if err := s.SetListing(context.Background(), []string{task.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "1", "none"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}

	updated, err := s.Task(context.Background(), task.Id)
	if err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if updated.Priority != model.PriorityNone {
		t.Errorf("priority = %v, want %v", updated.Priority, model.PriorityNone)
	}
}

func TestPriSetsSeveralTasksAtOnce(t *testing.T) {
	isolate(t)
	s := priStore(t)
	taskA := seedTask(t, s, "call mom")
	taskB := seedTask(t, s, "buy milk")
	if err := s.SetListing(context.Background(), []string{taskA.Id, taskB.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "1-2", "high"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
	if want := "priority set to high on 2 tasks\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	for _, id := range []string{taskA.Id, taskB.Id} {
		updated, err := s.Task(context.Background(), id)
		if err != nil {
			t.Fatalf("read back task %s: %v", id, err)
		}
		if updated.Priority != model.PriorityHigh {
			t.Errorf("task %s priority = %v, want %v", id, updated.Priority, model.PriorityHigh)
		}
	}
}

func TestPriNoArgs(t *testing.T) {
	isolate(t)
	priStore(t)

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
}

func TestPriMissingLevel(t *testing.T) {
	isolate(t)
	priStore(t)

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "milk"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run() = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "task and a priority") {
		t.Errorf("stderr = %q, want it to say a task and a level are both required", stderr.String())
	}
}

func TestPriUnparsableLevel(t *testing.T) {
	isolate(t)
	priStore(t)

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "milk", "urgent"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not read a priority at the end of") {
		t.Errorf("stderr = %q, want it to say no priority was found at the end of the line", stderr.String())
	}
}

func TestPriUnknownOption(t *testing.T) {
	isolate(t)
	priStore(t)

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "--bogus"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitUsage {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitUsage, stderr.String())
	}
}

func TestPriNoMatchingTask(t *testing.T) {
	isolate(t)
	s := priStore(t)
	seedTask(t, s, "buy milk")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "nosuchtask", "high"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no task matches") {
		t.Errorf("stderr = %q, want it to say no task matches", stderr.String())
	}
}

func TestPriIgnoresDoneTasks(t *testing.T) {
	isolate(t)
	s := priStore(t)
	task := seedTask(t, s, "buy milk")
	if _, err := s.CompleteTask(context.Background(), task.Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"pri", "milk", "high"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (done tasks are out of scope)\nstderr: %s", got, exitError, stderr.String())
	}
}

func TestPriWritesNoneOfThemWhenOneReferenceIsStale(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := priStore(t)
	one := seedTask(t, s, "one")
	two := seedTask(t, s, "two")
	three := seedTask(t, s, "three")
	if err := s.SetListing(ctx, []string{one.Id, two.Id, three.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	if err := s.DeleteTask(ctx, two.Id); err != nil {
		t.Fatalf("delete out of band: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run(ctx, []string{"pri", "1-3", "high"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitError, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: the run has to fail before it writes anything", stdout.String())
	}
	for _, id := range []string{one.Id, three.Id} {
		updated, err := s.Task(ctx, id)
		if err != nil {
			t.Fatalf("read back task %s: %v", id, err)
		}
		if updated.Priority != model.PriorityNone {
			t.Errorf("task %s priority = %v, want it untouched by a batch that failed", id, updated.Priority)
		}
	}
}

func TestPriWritesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	s := priStore(t)
	task := seedTask(t, s, "buy milk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "milk", "high"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	updated, err := s.Task(context.Background(), task.Id)
	if err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if updated.Priority != model.PriorityNone {
		t.Errorf("priority = %v, want it untouched by a run the user stopped", updated.Priority)
	}
}

func TestPriLeavesATaskThatAlreadyHasTheLevel(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := priStore(t)
	task := seedTask(t, s, "buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "milk", "high"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("setting the level: run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if got := run(ctx, []string{"pri", "milk", "high"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	if want := "\"buy milk\": priority already high\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	if got := run(ctx, []string{"undo"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("tt undo = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	updated, err := s.Task(ctx, task.Id)
	if err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if updated.Priority != model.PriorityNone {
		t.Errorf("priority after one undo = %v, want %v: the second run left a record of its own",
			updated.Priority, model.PriorityNone)
	}
}

func TestPriReportsEveryTaskOfAMixedBatch(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := priStore(t)
	already := seedTask(t, s, "call mom")
	fresh := seedTask(t, s, "buy milk")
	if _, err := s.UpdateTask(ctx, already.Id, model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)}); err != nil {
		t.Fatalf("seed the level: %v", err)
	}
	if err := s.SetListing(ctx, []string{already.Id, fresh.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "1-2", "high"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	want := "\"call mom\": priority already high\n\"buy milk\": priority set to high\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestPriSaysNothingMoreForABatchItSkippedWhole(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := priStore(t)
	one := seedTask(t, s, "call mom")
	two := seedTask(t, s, "buy milk")
	for _, id := range []string{one.Id, two.Id} {
		if _, err := s.UpdateTask(ctx, id, model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)}); err != nil {
			t.Fatalf("seed the level: %v", err)
		}
	}
	if err := s.SetListing(ctx, []string{one.Id, two.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "1-2", "high"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	want := "\"call mom\": priority already high\n\"buy milk\": priority already high\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

type priCancellingWriter struct {
	cancel context.CancelFunc
	out    bytes.Buffer
}

func (w *priCancellingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "priority already") {
		w.cancel()
	}
	return w.out.Write(p)
}

func TestPriWritesNothingAfterASignalLandsMidBatch(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := priStore(t)
	already := seedTask(t, s, "call mom")
	fresh := seedTask(t, s, "buy milk")
	if _, err := s.UpdateTask(ctx, already.Id, model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)}); err != nil {
		t.Fatalf("seed the level: %v", err)
	}
	if err := s.SetListing(ctx, []string{already.Id, fresh.Id}); err != nil {
		t.Fatalf("set listing: %v", err)
	}

	stdout := &priCancellingWriter{cancel: cancel}
	var stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "1-2", "high"}, strings.NewReader(""), stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	out := stdout.out.String()
	if want := "\"call mom\": priority already high\n"; out != want {
		t.Errorf("stdout = %q, want %q and nothing after it", out, want)
	}

	updated, err := s.Task(context.Background(), fresh.Id)
	if err != nil {
		t.Fatalf("read back task: %v", err)
	}
	if updated.Priority != model.PriorityNone {
		t.Errorf("priority = %v, want it untouched: the signal landed before this task was reached", updated.Priority)
	}
}

func TestPriSetsNothingWhenTheSignalLandsOnTheQuestion(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)
	s := priStore(t)
	milk := seedTask(t, s, "buy milk")
	bread := seedTask(t, s, "buy bread")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mvInterruptingReader{cancel: cancel, answer: strings.NewReader("1\n")}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"pri", "buy", "high"}, stdin, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run(pri buy high) = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if !strings.Contains(stderr.String(), "which one?") {
		t.Fatalf("stderr = %q, want the question the signal landed on", stderr.String())
	}
	if strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr = %q, want the database's complaint kept off the screen", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no level was set to report", stdout.String())
	}
	for _, seeded := range []model.Task{milk, bread} {
		got, err := s.Task(context.Background(), seeded.Id)
		if err != nil {
			t.Fatalf("read back %q: %v", seeded.Title, err)
		}
		if got.Priority != model.PriorityNone {
			t.Errorf("%q was given a priority by a run the user stopped", seeded.Title)
		}
	}
}
