package main

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const rmProjectID = "p-personal"

func rmCache(t *testing.T) *store.Store {
	t.Helper()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ReplaceProjects(context.Background(), []model.Project{{Id: rmProjectID, Name: "Personal"}}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return st
}

func rmSeed(t *testing.T, st *store.Store, titles ...string) []model.Task {
	t.Helper()
	ctx := context.Background()
	tasks := make([]model.Task, 0, len(titles))
	for _, title := range titles {
		task, err := st.CreateTask(ctx, model.Task{ProjectId: rmProjectID, Title: title, Status: model.TaskOpen})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		tasks = append(tasks, task)
	}
	if err := st.SetListing(ctx, cli.TaskIDs(tasks)); err != nil {
		t.Fatalf("set listing: %v", err)
	}
	return tasks
}

func rmTitles(t *testing.T, st *store.Store) []string {
	t.Helper()
	tasks, err := st.Tasks(context.Background(), store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	titles := make([]string, len(tasks))
	for i, task := range tasks {
		titles[i] = task.Title
	}
	sort.Strings(titles)
	return titles
}

func rmWantTitles(t *testing.T, st *store.Store, want ...string) {
	t.Helper()
	got := rmTitles(t, st)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cache holds %v, want %v", got, want)
	}
}

func rmAnswering(t *testing.T, answer string) *strings.Reader {
	t.Helper()
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	return strings.NewReader(answer)
}

func TestRmMisuse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{

		{"no arguments", []string{"rm"}, noTaskGivenFor("rm")},
		{"nothing but options", []string{"rm", "-y"}, noTaskGivenFor("rm")},
		{"blank argument", []string{"rm", "   ", "-y"}, noTaskGivenFor("rm")},
		{"unknown option", []string{"rm", "1", "--force"}, "unknown option"},
		{"tasks after the options", []string{"rm", "-y", "1"}, "the tasks come before the options"},
		{"all without yes", []string{"rm", "milk", "-A"}, "only allowed with -y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			st := rmCache(t)
			rmSeed(t, st, "Buy milk")

			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), c.want)
			}
			rmWantTitles(t, st, "Buy milk")
		})
	}
}

func TestRmHintNamesTheCommandThatWasMeant(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a task after the option", []string{"rm", "3", "-y", "4"}, "tt rm 3 4 -y"},
		{"a whole phrase after the option", []string{"rm", "-y", "buy", "some", "milk"}, "tt rm buy some milk -y"},

		{"all without yes", []string{"rm", "-A", "milk"}, "tt rm milk -A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			st := rmCache(t)
			rmSeed(t, st, "Buy milk", "Buy tickets")

			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitUsage, stderr.String())
			}

			if want := "the tasks come before the options: " + c.want + "\n"; !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to offer %q", stderr.String(), want)
			}
			rmWantTitles(t, st, "Buy milk", "Buy tickets")
		})
	}
}

func TestRmDeletesWhatWasNamed(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk", "Renew the domain", "Call the bank")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "1", "3", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	want := "deleted \"Buy milk\"\ndeleted \"Call the bank\"\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	rmWantTitles(t, st, "Renew the domain")
}

func TestRmDeletesARange(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "One", "Two", "Three", "Four")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "2-3", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if lines := strings.Count(stdout.String(), "\n"); lines != 2 {
		t.Errorf("stdout = %q, want one line per deleted task", stdout.String())
	}
	rmWantTitles(t, st, "One", "Four")
}

func TestRmRefusesToDeleteWithNobodyToAsk(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "1"}, strings.NewReader("y\n"), &stdout, &stderr); got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a terminal") || !strings.Contains(stderr.String(), "-y") {
		t.Errorf("stderr = %q, want the reason and the flag that gets past it", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no question may be put", stdout.String())
	}
	rmWantTitles(t, st, "Buy milk")
}

func TestRmAsksBeforeDeleting(t *testing.T) {
	cases := []struct {
		name    string
		answer  string
		deleted bool
	}{
		{"yes", "y\n", true},
		{"the whole word", "yes\n", true},
		{"no", "n\n", false},
		{"an empty line", "\n", false},
		{"nothing at all", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			st := rmCache(t)
			rmSeed(t, st, "Buy milk", "Renew the domain")
			stdin := rmAnswering(t, c.answer)

			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), []string{"rm", "1"}, stdin, &stdout, &stderr); got != exitOK {
				t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
			}
			out, errs := stdout.String(), stderr.String()

			for _, want := range []string{"this deletes 1 task:", "Buy milk", "cannot be restored", "delete it? [y/N] "} {
				if !strings.Contains(errs, want) {
					t.Errorf("stderr = %q, want it to carry %q", errs, want)
				}
			}
			if strings.Contains(out, "[y/N]") {
				t.Errorf("stdout = %q, want the question nowhere in the output the user asked for", out)
			}
			if c.deleted {
				if !strings.Contains(out, `deleted "Buy milk"`) {
					t.Errorf("stdout = %q, want the deleted task reported", out)
				}
				rmWantTitles(t, st, "Renew the domain")
				return
			}

			if !strings.Contains(errs, "aborted, nothing deleted") {
				t.Errorf("stderr = %q, want the refusal said where the question was", errs)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing: stdout is the record of what went, and nothing went", out)
			}
			rmWantTitles(t, st, "Buy milk", "Renew the domain")
		})
	}
}

func TestRmAsksOnceForSeveralTasks(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "One", "Two", "Three")
	stdin := rmAnswering(t, "y\n")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "1-2"}, stdin, &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	errs := stderr.String()
	if !strings.Contains(errs, "this deletes 2 tasks:") || !strings.Contains(errs, "delete them? [y/N] ") {
		t.Errorf("stderr = %q, want one question naming both tasks", errs)
	}
	if n := strings.Count(errs, "[y/N]"); n != 1 {
		t.Errorf("asked %d times, want once:\n%s", n, errs)
	}
	rmWantTitles(t, st, "Three")
}

func TestRmWithoutAllRefusesAnAmbiguousPhrase(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Send the invoice", "Pay the invoice", "Call the bank")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "invoice", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "matches 2 tasks") {
		t.Errorf("stderr = %q, want the candidates named", stderr.String())
	}
	rmWantTitles(t, st, "Call the bank", "Pay the invoice", "Send the invoice")
}

func TestRmAllDeletesEveryOpenMatch(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	tasks := rmSeed(t, st, "Send the invoice", "Pay the invoice", "File the invoice", "Call the bank")
	if _, err := st.CompleteTask(context.Background(), tasks[2].Id, store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "invoice", "-y", "-A"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if n := strings.Count(stdout.String(), "deleted "); n != 2 {
		t.Errorf("stdout = %q, want one line for each of the two open matches", stdout.String())
	}
	rmWantTitles(t, st, "Call the bank", "File the invoice")
}

func TestRmAllOnASingleMatchIsAnOrdinaryDelete(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Send the invoice", "Call the bank")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "2", "-y", "-A"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if want := "deleted \"Call the bank\"\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	rmWantTitles(t, st, "Send the invoice")
}

func TestRmNoMatch(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "kefir", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), `no task matches "kefir"`) {
		t.Errorf("stderr = %q, want it to say what was searched for", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	rmWantTitles(t, st, "Buy milk")
}

func TestRmNumberOutsideTheListing(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk")

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "7", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no task numbered 7") {
		t.Errorf("stderr = %q, want it to name the number that missed", stderr.String())
	}
	rmWantTitles(t, st, "Buy milk")
}

func TestRmDeletesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"rm", "1", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted", stdout.String(), stderr.String())
	}
	rmWantTitles(t, st, "Buy milk")
}

type rmInterruptingReader struct {
	cancel context.CancelFunc
	answer io.Reader
}

func (r *rmInterruptingReader) Read(p []byte) (int, error) {
	r.cancel()
	return r.answer.Read(p)
}

func TestRmDeletesNothingAfterCtrlCAtThePrompt(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "Buy milk")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &rmInterruptingReader{cancel: cancel, answer: rmAnswering(t, "y\n")}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"rm", "1"}, stdin, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}

	if !strings.Contains(stderr.String(), "interrupted, nothing deleted") {
		t.Errorf("stderr = %q, want the answer to the question it had just asked", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no task went", stdout.String())
	}
	rmWantTitles(t, st, "Buy milk")
}

func TestRmDeletesNoneOfThemWhenOneReferenceIsStale(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	tasks := rmSeed(t, st, "One", "Two", "Three")

	if err := st.DeleteTask(context.Background(), tasks[1].Id); err != nil {
		t.Fatalf("delete out of band: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"rm", "1-3", "-y"}, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: the run has to fail before it deletes anything", stdout.String())
	}
	rmWantTitles(t, st, "One", "Three")
}

type rmCancellingWriter struct {
	cancel context.CancelFunc
	out    bytes.Buffer
}

func (w *rmCancellingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "deleted ") {
		w.cancel()
	}
	return w.out.Write(p)
}

func TestRmReportsWhatIsLeftWhenItIsStoppedMidway(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	rmSeed(t, st, "One", "Two", "Three")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &rmCancellingWriter{cancel: cancel}

	var stderr bytes.Buffer
	if got := run(ctx, []string{"rm", "1-3", "-y"}, strings.NewReader(""), stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}

	out := stdout.out.String()
	if !strings.Contains(out, `deleted "One"`) {
		t.Errorf("stdout = %q, want the task that did go reported", out)
	}
	if strings.Contains(out, "interrupted") {
		t.Errorf("stdout = %q, want the count of what is left out of the record", out)
	}
	if !strings.Contains(stderr.String(), "interrupted, 2 tasks not deleted") {
		t.Errorf("stderr = %q, want it to count the tasks that are still there", stderr.String())
	}
	rmWantTitles(t, st, "Three", "Two")
}

func TestRmReadStopsOnASignalBeforeItReadsAnything(t *testing.T) {
	isolate(t)
	st := rmCache(t)
	tasks := rmSeed(t, st, "Buy milk")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	inv := &invocation{ctx: ctx, verb: "rm", stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	got, code, ok := rmRead(inv, st, cli.TaskIDs(tasks))
	if ok || code != exitInterrupted || got != nil {
		t.Fatalf("rmRead() = %v, %d, %v; want no tasks and %d", got, code, ok, exitInterrupted)
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q; want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
}

func TestRmHelpOffersItsFlags(t *testing.T) {
	got := helpText(t, "rm")
	for _, flag := range []string{rmYesOption, rmAllOption} {
		if !strings.Contains(got, flag) {
			t.Errorf("tt help rm does not offer %s:\n%s", flag, got)
		}
	}
}

func TestRmPreviewIsOneLinePerTask(t *testing.T) {
	tasks := []model.Task{
		{Title: "pay\nthe rent"},
		{Title: strings.Repeat("and file the receipt ", 12)},
		{Title: "\t\x07"},
	}
	lines := rmPreview(tasks)

	if want := len(tasks) + 2; len(lines) != want {
		t.Fatalf("rmPreview gave %d lines, want %d:\n%s", len(lines), want, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "3 tasks") {
		t.Errorf("the count line is %q, want it to say how many are going", lines[0])
	}
	for _, line := range lines[1 : len(lines)-1] {
		if strings.Contains(line, "\n") {
			t.Errorf("a preview row is more than one line: %q", line)
		}
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("a preview row is %d columns wide: %q", n, line)
		}
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("a preview row lost its indent: %q", line)
		}
	}
}

func TestRmRefusalFitsTheWidth(t *testing.T) {
	for _, verb := range []string{"add", "done", "version"} {
		lines := strings.Split(rmNoTerminalRefusal(verb), "\n")
		lines[0] = "tt: " + verb + ": " + lines[0]
		for i, line := range lines {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("tt %s: line %d is %d columns, want at most %d: %q", verb, i+1, n, cli.Width, line)
			}
		}
	}
}
