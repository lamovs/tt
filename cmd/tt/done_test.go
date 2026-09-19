package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func openDoneStore(t *testing.T) *store.Store {
	t.Helper()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return st
}

func seedDone(t *testing.T, projects []model.Project, tasks ...model.Task) []string {
	t.Helper()
	ctx := context.Background()
	st := openDoneStore(t)
	defer st.Close()
	if len(projects) > 0 {
		if err := st.ReplaceProjects(ctx, projects); err != nil {
			t.Fatalf("replace projects: %v", err)
		}
	}
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		created, err := st.CreateTask(ctx, task)
		if err != nil {
			t.Fatalf("create task %q: %v", task.Title, err)
		}
		ids[i] = created.Id
	}
	return ids
}

func setDoneListing(t *testing.T, ids []string) {
	t.Helper()
	st := openDoneStore(t)
	defer st.Close()
	if err := st.SetListing(context.Background(), ids); err != nil {
		t.Fatalf("set listing: %v", err)
	}
}

func readDoneTask(t *testing.T, id string) model.Task {
	t.Helper()
	st := openDoneStore(t)
	defer st.Close()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return task
}

var doneProjects = []model.Project{{Id: "p1", Name: "Personal"}}

func TestDoneNoArgsIsMisuse(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"done"}, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
		t.Fatalf("run(done) = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
	}
	if want := "tt: done: " + noTaskGivenFor("done") + "\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to carry %q - the sentence that says how a task is named", stderr.String(), want)
	}
}

func TestDoneRejectsUnknownOption(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	args := []string{"done", "--bogus"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--bogus") {
		t.Errorf("stderr = %q, want it to name the option it refused", stderr.String())
	}
}

func TestDoneByNumber(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects, model.Task{Title: "remont", ProjectId: "p1"})
	setDoneListing(t, ids)

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"done", "1"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(done 1) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if want := "done: \"remont\"\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if task := readDoneTask(t, ids[0]); !task.Status.Done() {
		t.Errorf("task status = %v, want done", task.Status)
	}
}

func TestDoneClosesChecklistByDefault(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects, model.Task{
		Title:     "remont",
		ProjectId: "p1",
		Items: []model.Item{
			{Title: "paint"},
			{Title: "tiles"},
			{Title: "already done", Status: model.ItemDone},
		},
	})
	setDoneListing(t, ids)

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"done", "1"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(done 1) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if want := "done: \"remont\" (+2 subtasks)\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	task := readDoneTask(t, ids[0])
	if !task.Status.Done() {
		t.Fatal("task not completed")
	}
	for _, it := range task.Items {
		if !it.Status.Done() {
			t.Errorf("item %q left open", it.Title)
		}
	}
}

func TestDoneKeepSubsLeavesChecklistOpen(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects, model.Task{
		Title:     "remont",
		ProjectId: "p1",
		Items:     []model.Item{{Title: "paint"}},
	})
	setDoneListing(t, ids)

	var stdout, stderr bytes.Buffer
	args := []string{"done", "1", "--keep-subs"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
	}
	if want := "done: \"remont\"\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	task := readDoneTask(t, ids[0])
	if !task.Status.Done() {
		t.Fatal("task not completed")
	}
	if len(task.Items) != 1 || task.Items[0].Status.Done() {
		t.Fatalf("items = %+v, want the checklist left open", task.Items)
	}
}

func TestDoneTextSearchNoMatch(t *testing.T) {
	isolate(t)
	seedDone(t, doneProjects, model.Task{Title: "buy milk", ProjectId: "p1"})

	var stdout, stderr bytes.Buffer
	args := []string{"done", "nothing like it"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run(%v) = %d, want %d", args, got, exitError)
	}
	if !strings.Contains(stderr.String(), "no task matches") || !strings.Contains(stderr.String(), "open") {
		t.Errorf("stderr = %q, want it to name what was searched", stderr.String())
	}
}

func TestDoneTextSearchAmbiguousWithoutAll(t *testing.T) {
	isolate(t)
	seedDone(t, doneProjects,
		model.Task{Title: "renew domain", ProjectId: "p1"},
		model.Task{Title: "renew insurance", ProjectId: "p1"},
	)

	var stdout, stderr bytes.Buffer
	args := []string{"done", "renew"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run(%v) = %d, want %d (stdout: %s)", args, got, exitError, stdout.String())
	}
	if !strings.Contains(stderr.String(), "matches 2 tasks") {
		t.Errorf("stderr = %q, want it to say the query matched 2 tasks", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a task was completed without -A: %q", stdout.String())
	}
}

func TestDoneAllCompletesEveryMatch(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects,
		model.Task{Title: "renew domain", ProjectId: "p1"},
		model.Task{Title: "renew insurance", ProjectId: "p1"},
	)

	var stdout, stderr bytes.Buffer
	args := []string{"done", "renew", "-A"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want one line per completed task", stdout.String())
	}
	for _, id := range ids {
		if task := readDoneTask(t, id); !task.Status.Done() {
			t.Errorf("task %s status = %v, want done", id, task.Status)
		}
	}
}

func TestDoneLeavesAnAlreadyCompletedTaskAlone(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	ids := seedDone(t, doneProjects, model.Task{Title: "buy milk", ProjectId: "p1"})
	setDoneListing(t, ids)

	st := openDoneStore(t)
	if _, err := st.CompleteTask(ctx, ids[0], store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	before := readDoneTask(t, ids[0])
	top, ok := undoTop(t, st)
	if !ok {
		t.Fatal("completing the task recorded nothing to undo")
	}
	st.Close()

	var stdout, stderr bytes.Buffer

	if got := run(ctx, []string{"done", "1"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(done 1) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if want := "already done: \"buy milk\"\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	after := readDoneTask(t, ids[0])
	if !after.CompletedTime.Time.Equal(before.CompletedTime.Time) {
		t.Errorf("completed at %v, want the moment it was finished at: %v", after.CompletedTime, before.CompletedTime)
	}
	st = openDoneStore(t)
	defer st.Close()
	now, ok := undoTop(t, st)
	if !ok || now.Seq != top.Seq {
		t.Errorf("the undo stack is at %+v, want the completion record it already carried (%+v)", now, top)
	}
}

func TestDoneReportsEveryTaskOfAMixedBatch(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	ids := seedDone(t, doneProjects,
		model.Task{Title: "buy milk", ProjectId: "p1"},
		model.Task{Title: "pay rent", ProjectId: "p1"},
	)
	setDoneListing(t, ids)

	st := openDoneStore(t)
	if _, err := st.CompleteTask(ctx, ids[0], store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"done", "1-2"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(done 1-2) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	want := "already done: \"buy milk\"\ndone: \"pay rent\"\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if task := readDoneTask(t, ids[1]); !task.Status.Done() {
		t.Errorf("task %s status = %v, want the open one closed", ids[1], task.Status)
	}
}

func TestDoneClosesNoneOfThemWhenOneReferenceIsStale(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	ids := seedDone(t, doneProjects,
		model.Task{Title: "one", ProjectId: "p1"},
		model.Task{Title: "two", ProjectId: "p1"},
		model.Task{Title: "three", ProjectId: "p1"},
	)
	setDoneListing(t, ids)

	st := openDoneStore(t)
	if err := st.DeleteTask(ctx, ids[1]); err != nil {
		t.Fatalf("delete out of band: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"done", "1-3"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run(done 1-3) = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: the run has to fail before it closes anything", stdout.String())
	}
	for _, id := range []string{ids[0], ids[2]} {
		if task := readDoneTask(t, id); task.Status.Done() {
			t.Errorf("task %s was completed by a batch that failed", id)
		}
	}
}

func TestDoneClosesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects, model.Task{ProjectId: "p1", Title: "buy milk", Status: model.TaskOpen})
	setDoneListing(t, ids)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"done", "1"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	if task := readDoneTask(t, ids[0]); task.Status.Done() {
		t.Error("a run the user stopped completed the task anyway")
	}
}

type doneStopOnWrite struct {
	cancel context.CancelFunc
	to     io.Writer
}

func (w *doneStopOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return w.to.Write(p)
}

func TestDoneClosesNoMoreTasksOnceTheSignalLands(t *testing.T) {
	isolate(t)
	ids := seedDone(t, doneProjects,
		model.Task{ProjectId: "p1", Title: "buy milk", Status: model.TaskOpen},
		model.Task{ProjectId: "p1", Title: "buy bread", Status: model.TaskOpen})
	setDoneListing(t, ids)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer

	got := run(ctx, []string{"done", "1-2"}, strings.NewReader(""), &doneStopOnWrite{cancel: cancel, to: &stdout}, &stderr)
	if got != exitInterrupted {
		t.Fatalf("run(done 1-2) = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing: ttMain says the run was interrupted", stderr.String())
	}
	if !strings.Contains(stdout.String(), "buy milk") {
		t.Errorf("stdout = %q, want the task that did close reported", stdout.String())
	}
	if task := readDoneTask(t, ids[0]); !task.Status.Done() {
		t.Error("the task closed before the signal came back open")
	}
	if task := readDoneTask(t, ids[1]); task.Status.Done() {
		t.Error("the batch closed a task after the user stopped the run")
	}
}

// tt done is one of the four ways a task is closed, and the refusal the
// store words for a child relationship that has not been sent yet reaches
// the reader whole, with the sync it asks for.
func TestDoneRefusesAParentWhoseChildLinkIsUnsent(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	ids := seedDone(t, doneProjects,
		model.Task{Title: "trip", ProjectId: "p1"},
		model.Task{Title: "book tickets", ProjectId: "p1"},
	)
	setDoneListing(t, ids)

	st := openDoneStore(t)
	if _, err := st.UpdateTask(ctx, ids[1], model.TaskEdit{ParentId: model.Ptr(ids[0])}); err != nil {
		t.Fatalf("hang the child under the queued parent: %v", err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"done", "1"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run(done 1) = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "run tt sync first") {
		t.Errorf("stderr = %q, want it to send the reader to tt sync", stderr.String())
	}
	if task := readDoneTask(t, ids[0]); task.Status.Done() {
		t.Errorf("parent %s was closed while the relationship of its child was still queued", ids[0])
	}
	// The child hangs nothing under itself, so closing it is untouched.
	stdout.Reset()
	stderr.Reset()
	if got := run(ctx, []string{"done", "2"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(done 2) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if task := readDoneTask(t, ids[1]); !task.Status.Done() {
		t.Errorf("child %s = %+v, want tt done to close it", ids[1], task)
	}
}
