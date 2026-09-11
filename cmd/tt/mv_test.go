package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const (
	mvParcel   = "68a1f2c0e4b0a10000000001"
	mvPassport = "68a1f2c0e4b0a10000000002"
	mvPaid     = "68a1f2c0e4b0a10000000003"
	mvUnknown  = "68a1f2c0e4b0a1000000ffff"
)

const (
	mvPersonal = "Личное"
	mvWork     = "Работа"
	mvChores   = "Домашние дела"
)

func mvSeed(t *testing.T, tasks ...model.Task) {
	t.Helper()
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer st.Close()

	projects := []model.Project{
		{Id: "p1", Name: mvPersonal},
		{Id: "p2", Name: mvWork},
		{Id: "p3", Name: mvChores},
	}
	if err := st.ReplaceProjects(ctx, projects); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	byProject := map[string][]store.ServerTask{}
	for _, task := range tasks {
		raw, err := json.Marshal(map[string]string{"id": task.Id, "title": task.Title})
		if err != nil {
			t.Fatal(err)
		}
		byProject[task.ProjectId] = append(byProject[task.ProjectId], store.ServerTask{Task: task, Raw: raw})
	}
	for projectID, list := range byProject {
		if _, err := st.SyncProject(ctx, projectID, list); err != nil {
			t.Fatalf("seed %s: %v", projectID, err)
		}
	}
}

func mvTasksIn(t *testing.T, projectID string) []model.Task {
	t.Helper()
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer st.Close()
	found, err := st.Tasks(ctx, store.TaskFilter{ProjectID: projectID, Status: store.StatusAll})
	if err != nil {
		t.Fatalf("read %s: %v", projectID, err)
	}
	return found
}

func mvSetting(t *testing.T, value config.MoveByRecreate) {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[sync]\nmove_by_recreate = '" + string(value) + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mvNumbering(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer st.Close()
	if err := st.SetListing(ctx, ids); err != nil {
		t.Fatalf("set listing: %v", err)
	}
}

func mvOpen(id, projectID, title string) model.Task {
	return model.Task{Id: id, ProjectId: projectID, Title: title, Status: model.TaskOpen}
}

func mvRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return mvAnswer(t, "", args...)
}

func mvAnswer(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	// These fixtures retain the explicit legacy recreate workflow contract.
	if len(args) > 0 && args[0] == "mv" {
		args = append(append([]string(nil), args...), "--recreate")
	}
	var out, errs bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errs)
	return code, out.String(), errs.String()
}

func mvAtATerminal(t *testing.T) {
	t.Helper()
	saved := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = saved })
}

func TestMvRefusesACommandLineItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"nothing at all", []string{"mv"}, []string{"name a task and the list"}},
		{"a task and no list", []string{"mv", "3"}, []string{"name a task and the list"}},
		{"an option it does not have", []string{"mv", "3", mvWork, "--force"}, []string{"--force"}},
		{"an empty task reference", []string{"mv", "", mvWork}, []string{"name a task and a list"}},
		{"a list nothing is called", []string{"mv", mvParcel, "Хобби"}, []string{"could not read a list at the end", "Хобби"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))
			code, _, stderr := mvRun(t, c.args...)
			if code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "tt help mv") {
				t.Errorf("stderr = %q, want it to send them to the examples", stderr)
			}
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to mention %q", stderr, want)
				}
			}
			if got := mvTasksIn(t, "p2"); len(got) != 0 {
				t.Errorf("a refused command line moved %d tasks", len(got))
			}
		})
	}
}

func TestMvMovesTheTaskWhenTheSettingSaysAlways(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, stdout, stderr := mvRun(t, "mv", mvParcel, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	moved := mvTasksIn(t, "p2")
	if len(moved) != 1 || moved[0].Title != "Забрать посылку" {
		t.Fatalf("the Work list holds %+v", moved)
	}
	if moved[0].Id == mvParcel {
		t.Errorf("the copy kept the original id %q", moved[0].Id)
	}
	if left := mvTasksIn(t, "p1"); len(left) != 0 {
		t.Errorf("the original is still in its old list: %+v", left)
	}

	if !strings.Contains(stdout, "Забрать посылку") || !strings.Contains(stdout, mvWork) {
		t.Errorf("stdout = %q, want the moved task listed under its new list", stdout)
	}
}

func TestMvMovesEveryTaskItWasGiven(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t,
		mvOpen(mvParcel, "p1", "Забрать посылку"),
		mvOpen(mvPassport, "p1", "Поменять паспорт"),
	)

	code, stdout, stderr := mvRun(t, "mv", mvParcel, mvPassport, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if got := mvTasksIn(t, "p2"); len(got) != 2 {
		t.Fatalf("the Work list holds %d tasks, want 2", len(got))
	}
	if got := mvTasksIn(t, "p1"); len(got) != 0 {
		t.Fatalf("the old list still holds %d tasks", len(got))
	}
	for _, title := range []string{"Забрать посылку", "Поменять паспорт"} {
		if !strings.Contains(stdout, title) {
			t.Errorf("stdout = %q, want it to list %q", stdout, title)
		}
	}
}

func TestMvLeavesATaskAlreadyInTheListAlone(t *testing.T) {
	isolate(t)
	mvSeed(t, mvOpen(mvParcel, "p2", "Забрать посылку"))

	code, stdout, stderr := mvRun(t, "mv", mvParcel, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	got := mvTasksIn(t, "p2")
	if len(got) != 1 || got[0].Id != mvParcel {
		t.Fatalf("the Work list holds %+v, want the task under its own id", got)
	}
	if !strings.Contains(stdout, "Забрать посылку") {
		t.Errorf("stdout = %q, want the task listed", stdout)
	}
}

func TestMvRefusesWhenThereIsNobodyToAsk(t *testing.T) {
	isolate(t)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, _, stderr := mvRun(t, "mv", mvParcel, mvWork)
	if code != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	for _, want := range []string{"not a terminal", "move_by_recreate", "always"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
	if got := mvTasksIn(t, "p1"); len(got) != 1 {
		t.Errorf("the task did not stay where it was: %+v", got)
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a refused move still wrote to the target list: %+v", got)
	}
}

func TestMvRefusesWhenTheSettingSaysNever(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveNever)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, _, stderr := mvRun(t, "mv", mvParcel, mvWork)
	if code != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "'never'") {
		t.Errorf("stderr = %q, want it to name the setting that refused", stderr)
	}
	if got := mvTasksIn(t, "p1"); len(got) != 1 {
		t.Errorf("the task did not stay where it was: %+v", got)
	}
}

func TestMvTakesATaskByItsNumber(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t,
		mvOpen(mvParcel, "p1", "Забрать посылку"),
		mvOpen(mvPassport, "p1", "Поменять паспорт"),
	)
	mvNumbering(t, mvParcel, mvPassport)

	code, stdout, stderr := mvRun(t, "mv", "2", mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	moved := mvTasksIn(t, "p2")
	if len(moved) != 1 || moved[0].Title != "Поменять паспорт" {
		t.Fatalf("the Work list holds %+v, want the second line of the listing", moved)
	}
	left := mvTasksIn(t, "p1")
	if len(left) != 1 || left[0].Id != mvParcel {
		t.Errorf("the old list holds %+v, want the task nobody pointed at", left)
	}
	if !strings.Contains(stdout, "Поменять паспорт") {
		t.Errorf("stdout = %q, want the moved task listed", stdout)
	}
}

func TestMvTakesAListNamedInTwoWords(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"quoted, and the shell kept them together", []string{"mv", "посылку", mvChores}},
		{"unquoted, and tt puts them back together", []string{"mv", "посылку", "Домашние", "дела"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			mvSetting(t, config.MoveAlways)
			mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

			code, stdout, stderr := mvRun(t, c.args...)
			if code != exitOK {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, code, exitOK, stderr)
			}
			moved := mvTasksIn(t, "p3")
			if len(moved) != 1 || moved[0].Title != "Забрать посылку" {
				t.Fatalf("the chores list holds %+v", moved)
			}
			if left := mvTasksIn(t, "p1"); len(left) != 0 {
				t.Errorf("the original is still in its old list: %+v", left)
			}
			if !strings.Contains(stdout, mvChores) {
				t.Errorf("stdout = %q, want the task listed under its new list", stdout)
			}
		})
	}
}

func TestMvAsksOnceForTheWholeRun(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)
	mvSeed(t,
		mvOpen(mvParcel, "p1", "Забрать посылку"),
		mvOpen(mvPassport, "p1", "Поменять паспорт"),
	)

	code, stdout, stderr := mvAnswer(t, "y\n", "mv", mvParcel, mvPassport, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if got := strings.Count(stderr, "[y/N]"); got != 1 {
		t.Errorf("the question was put %d times, want once (stderr: %s)", got, stderr)
	}
	if !strings.Contains(stderr, "2 tasks") {
		t.Errorf("stderr = %q, want the question to say how much the answer covers", stderr)
	}
	if got := mvTasksIn(t, "p2"); len(got) != 2 {
		t.Fatalf("the Work list holds %d tasks, want both (stderr: %s)", len(got), stderr)
	}
	if got := mvTasksIn(t, "p1"); len(got) != 0 {
		t.Errorf("the old list still holds %+v", got)
	}
	for _, title := range []string{"Забрать посылку", "Поменять паспорт"} {
		if !strings.Contains(stdout, title) {
			t.Errorf("stdout = %q, want it to list %q", stdout, title)
		}
	}
}

func TestMvStopsWhenTheAnswerIsNo(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, stdout, stderr := mvAnswer(t, "n\n", "mv", mvParcel, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}

	if !strings.Contains(stderr, "nothing moved") {
		t.Errorf("stderr = %q, want it to say that nothing happened", stderr)
	}
	if strings.Contains(stdout, "nothing moved") {
		t.Errorf("stdout = %q, want the reply where the question was", stdout)
	}
	if strings.Contains(stdout, "[y/N]") {
		t.Errorf("stdout = %q, want the question on stderr and out of what a user redirects", stdout)
	}
	if !strings.Contains(stderr, "[y/N]") {
		t.Errorf("stderr = %q, want the question the answer answered", stderr)
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a declined move wrote to the target list anyway: %+v", got)
	}
	got := mvTasksIn(t, "p1")
	if len(got) != 1 || got[0].Id != mvParcel {
		t.Errorf("the task did not stay where it was under its own id: %+v", got)
	}
}

func TestMvReportsWhatMovedBeforeItSaysWhatStopped(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	done := mvOpen(mvPaid, "p1", "Оплатить счёт")
	done.Status = model.TaskDone
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"), done)

	code, stdout, stderr := mvRun(t, "mv", mvParcel, mvPaid, mvWork)
	if code != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	if !strings.Contains(stdout, "Забрать посылку") {
		t.Errorf("stdout = %q, want the task that did move listed", stdout)
	}
	if !strings.Contains(stderr, "Оплатить счёт") || !strings.Contains(stderr, "is done") {
		t.Errorf("stderr = %q, want it to say which task stopped the run and why", stderr)
	}

	if got := mvTasksIn(t, "p2"); len(got) != 1 {
		t.Fatalf("the Work list holds %+v, want the one task that moved", got)
	}
}

func TestMoveReportPrintsAfterTheSignal(t *testing.T) {
	isolate(t)
	mvSeed(t, mvOpen(mvParcel, "p2", "Забрать посылку"))

	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer st.Close()

	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	inv := &invocation{ctx: stopped, verb: "mv", stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	moveReport(inv, st, []model.Task{mvOpen(mvParcel, "p2", "Забрать посылку")})

	if !strings.Contains(stdout.String(), "Забрать посылку") {
		t.Errorf("stdout = %q, want the moved task listed (stderr: %s)", stdout.String(), stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want the report to have printed without complaint", stderr.String())
	}

	listing, err := st.Listing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 || listing[0] != mvParcel {
		t.Errorf("the numbering is %v, want the task the report listed", listing)
	}
}

type mvInterruptingReader struct {
	cancel context.CancelFunc
	answer io.Reader
}

func (r *mvInterruptingReader) Read(p []byte) (int, error) {
	r.cancel()
	return r.answer.Read(p)
}

func TestMvBoundsEveryNameItPutsOnScreen(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)

	mvSeed(t, mvOpen(mvParcel, "p1", strings.Repeat("Ж", 300)+"\nи ещё"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mvInterruptingReader{cancel: cancel, answer: strings.NewReader("y\n")}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"mv", mvParcel, mvWork, "--recreate"}, stdin, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if !strings.Contains(stderr.String(), "was left where it is") {
		t.Fatalf("stderr = %q, want the word owed to a user who answered yes", stderr.String())
	}

	measured := strings.Replace(stderr.String(), "[y/N] ", "[y/N]\n", 1)
	for _, line := range strings.Split(strings.TrimRight(measured, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("a line is %d columns wide:\n%q", n, line)
		}
		for _, r := range line {
			if unicode.IsControl(r) {
				t.Errorf("a line carries %U raw:\n%q", r, line)
			}
		}
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a move the user stopped took %d tasks across", len(got))
	}
}

func TestMvOpensNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"mv", mvParcel, mvWork, "--recreate"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a run the user stopped moved %d tasks anyway", len(got))
	}
}

func TestMvReadsNothingWhenTheSignalLandsOnTheQuestion(t *testing.T) {
	isolate(t)
	mvAtATerminal(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t,
		mvOpen(mvParcel, "p1", "Забрать посылку"),
		mvOpen(mvPassport, "p1", "Забрать паспорт"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mvInterruptingReader{cancel: cancel, answer: strings.NewReader("1\n")}

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"mv", "Забрать", mvWork, "--recreate"}, stdin, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if !strings.Contains(stderr.String(), "which one?") {
		t.Fatalf("stderr = %q, want the question the signal landed on", stderr.String())
	}
	if strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr = %q, want the database's complaint kept off the screen", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no task was moved to report", stdout.String())
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a run the user stopped moved %d tasks anyway", len(got))
	}
}

func TestMvSaysAMissingIdOnce(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, _, stderr := mvRun(t, "mv", mvUnknown, mvWork)
	if code != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	if got := strings.Count(stderr, mvUnknown); got != 1 {
		t.Errorf("stderr = %q names the id %d times, want once", stderr, got)
	}
}

func TestMvRefusesACompletedTask(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	done := mvOpen(mvPaid, "p1", "Оплатить счёт")
	done.Status = model.TaskDone
	mvSeed(t, done)

	code, _, stderr := mvRun(t, "mv", mvPaid, mvWork)
	if code != exitError {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitError, stderr)
	}
	if !strings.Contains(stderr, "Оплатить счёт") || !strings.Contains(stderr, "done") {
		t.Errorf("stderr = %q, want it to name the task and say it is done", stderr)
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("the completed task was copied anyway: %+v", got)
	}
}

func TestMvSaysWhatItCouldNotFind(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"no task matches the words", []string{"mv", "хлеб", mvWork}, []string{"no task matches", "хлеб"}},
		{"no task under that id", []string{"mv", mvUnknown, mvWork}, []string{mvUnknown, "cache"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			mvSetting(t, config.MoveAlways)
			mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))
			code, _, stderr := mvRun(t, c.args...)
			if code != exitError {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, code, exitError, stderr)
			}
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to mention %q", stderr, want)
				}
			}
		})
	}
}

func TestMoveConsentPutsTheQuestion(t *testing.T) {
	pending := []model.Task{mvOpen(mvParcel, "p1", "Забрать посылку")}
	project := model.Project{Id: "p2", Name: mvWork}
	cases := []struct {
		name    string
		answer  string
		granted bool
	}{
		{"y", "y\n", true},
		{"yes", "yes\n", true},
		{"an empty line", "\n", false},
		{"n", "n\n", false},
		{"a closed stream", "", false},
		{"anything else", "later\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			inv := &invocation{verb: "mv", stdin: strings.NewReader(c.answer), stdout: &stdout, stderr: &stderr}
			granted, err := moveConsent(inv, config.MoveAsk, true, pending, project)
			if err != nil {
				t.Fatalf("moveConsent: %v", err)
			}
			if granted != c.granted {
				t.Errorf("answering %q granted = %v, want %v", c.answer, granted, c.granted)
			}
			for _, want := range []string{"Забрать посылку", mvWork, "id of its own", "[y/N]"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the question was %q, want it to mention %q", stderr.String(), want)
				}
			}

			if stdout.String() != "" {
				t.Errorf("the question put %q on stdout", stdout.String())
			}
		})
	}
}

func TestMoveConsentAsksAboutEveryTaskItCovers(t *testing.T) {
	pending := []model.Task{
		mvOpen(mvParcel, "p1", "Забрать посылку"),
		mvOpen(mvPassport, "p1", "Поменять паспорт"),
	}
	var stdout, stderr bytes.Buffer
	inv := &invocation{verb: "mv", stdin: strings.NewReader("y\n"), stdout: &stdout, stderr: &stderr}
	granted, err := moveConsent(inv, config.MoveAsk, true, pending, model.Project{Id: "p2", Name: mvWork})
	if err != nil || !granted {
		t.Fatalf("moveConsent = %v, %v; want true, nil", granted, err)
	}
	if !strings.Contains(stderr.String(), "2 tasks") {
		t.Errorf("the question was %q, want it to say how many tasks it covers", stderr.String())
	}
}

func TestMoveConsentRefusesWithoutAsking(t *testing.T) {
	pending := []model.Task{mvOpen(mvParcel, "p1", "Забрать посылку")}
	project := model.Project{Id: "p2", Name: mvWork}
	cases := []struct {
		name string
		mode config.MoveByRecreate
		ask  bool
		want string
	}{
		{"the setting says never", config.MoveNever, true, "'never'"},
		{"nobody to ask", config.MoveAsk, false, "not a terminal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			inv := &invocation{verb: "mv", stdin: strings.NewReader("y\n"), stdout: &stdout, stderr: &stderr}
			granted, err := moveConsent(inv, c.mode, c.ask, pending, project)
			if granted {
				t.Fatal("the move was allowed with no answer to allow it")
			}
			if err == nil {
				t.Fatal("no error to tell the user why not")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}

			if !strings.Contains(err.Error(), "creating a copy of the task") {
				t.Errorf("error = %q, want it to say what a move would cost", err)
			}
			if stdout.String() != "" || stderr.String() != "" {
				t.Errorf("it wrote %q and %q for a question it never put", stdout.String(), stderr.String())
			}
		})
	}
}

func TestMoveFailureNamesTheTask(t *testing.T) {
	task := mvOpen(mvParcel, "p1", "Забрать посылку")
	project := model.Project{Id: "p2", Name: mvWork}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"completed", store.ErrMoveCompleted, "is done"},
		{"chained", store.ErrMoveChained, "tt sync"},
		{"gone from the cache", store.ErrNotFound, "no longer in the local cache"},
		{"anything else", errors.New("database is locked"), "database is locked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := moveFailure(task, project, c.err)
			if !strings.Contains(got.Error(), "Забрать посылку") {
				t.Errorf("message = %q, want it to name the task", got)
			}
			if !strings.Contains(got.Error(), c.want) {
				t.Errorf("message = %q, want it to say %q", got, c.want)
			}
		})
	}
}

func TestMoveFailureWrapsEveryBranch(t *testing.T) {
	task := mvOpen(mvParcel, "p1", strings.TrimSpace(strings.Repeat("Забрать посылку из отделения ", 4)))
	project := model.Project{Id: "p2", Name: mvWork}
	for _, err := range []error{
		store.ErrMoveCompleted,
		store.ErrMoveChained,
		store.ErrNotFound,
		errors.New("database is locked"),
	} {
		for _, line := range strings.Split(moveFailure(task, project, err).Error(), "\n") {
			if got := utf8.RuneCountInString(line); got > moveMessageWidth {
				t.Errorf("%v: a line of %d columns, want at most %d: %q", err, got, moveMessageWidth, line)
			}
		}
	}
}

func TestMoveFailureBoundsANameTheWrapCannotBreak(t *testing.T) {
	task := mvOpen(mvParcel, "p1", strings.Repeat("Ж", 300))
	project := model.Project{Id: "p2", Name: strings.Repeat("Р", 300)}
	for _, err := range []error{
		store.ErrMoveCompleted,
		store.ErrMoveChained,
		store.ErrNotFound,
		errors.New("database is locked"),
	} {
		for _, line := range strings.Split(moveFailure(task, project, err).Error(), "\n") {
			if got := utf8.RuneCountInString(line); got > moveMessageWidth {
				t.Errorf("%v: a line of %d columns, want at most %d: %q", err, got, moveMessageWidth, line)
			}
		}
	}
}

func TestMoveFailureKeepsTheErrorItPassesOn(t *testing.T) {
	locked := errors.New("database is locked")
	got := moveFailure(mvOpen(mvParcel, "p1", "Забрать посылку"), model.Project{Id: "p2", Name: mvWork}, locked)
	if !errors.Is(got, locked) {
		t.Errorf("moveFailure(%v) = %v, which no longer wraps what it was given", locked, got)
	}
}

func TestMvHelpShowsWhatAMoveCosts(t *testing.T) {
	got := strings.Join(strings.Fields(helpText(t, "mv")), " ")
	for _, want := range []string{"unchanged task ID", "--recreate", "creates a new ID", "move_by_recreate", "tt mv"} {
		if !strings.Contains(got, want) {
			t.Errorf("tt help mv does not mention %q:\n%s", want, got)
		}
	}
}

func TestMvSaysNothingWhenTheSignalLandsOnTheListLookup(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saved := canAsk
	canAsk = func(*invocation) bool { cancel(); return false }
	t.Cleanup(func() { canAsk = saved })

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"mv", mvParcel, mvWork, "--recreate"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
	if got := mvTasksIn(t, "p2"); len(got) != 0 {
		t.Errorf("a run the user stopped moved %d tasks anyway", len(got))
	}
}

func TestMvNumbersWhatItMoved(t *testing.T) {
	isolate(t)
	mvSetting(t, config.MoveAlways)
	mvSeed(t, mvOpen(mvParcel, "p1", "Забрать посылку"))

	code, stdout, stderr := mvRun(t, "mv", mvParcel, mvWork)
	if code != exitOK {
		t.Fatalf("run = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "Забрать посылку") {
		t.Errorf("stdout = %q, want the task that moved listed", stdout)
	}

	moved := mvTasksIn(t, "p2")
	if len(moved) != 1 {
		t.Fatalf("the Work list holds %+v, want the one task that was moved", moved)
	}
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer st.Close()
	listing, err := st.Listing(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(listing) != 1 || listing[0] != moved[0].Id {
		t.Errorf("the numbering is %v, want the task the listing showed (%s)", listing, moved[0].Id)
	}
}
