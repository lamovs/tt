package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

// batchRun opens the editor stub on document and runs tt batch with args. It
// returns the exit code, what the command printed, and the file the stub saw.
func batchRun(t *testing.T, document string, args ...string) (int, string, string, string) {
	t.Helper()
	var replacement []byte
	if document != "\x00" {
		replacement = []byte(document)
	}
	capture := setupMarkdownEditor(t, replacement)
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), append([]string{"batch"}, args...), strings.NewReader(""), &out, &diagnostic)
	initial, _ := os.ReadFile(capture)
	return code, out.String(), diagnostic.String(), string(initial)
}

// batchDraftPath is the file the editor stub was pointed at, which is the
// draft tt keeps when it cannot use the document.
func batchDraftPath(t *testing.T) string {
	t.Helper()
	path, err := os.ReadFile(os.Getenv("TT_MARKDOWN_CAPTURE") + ".path")
	if err != nil {
		t.Fatalf("read draft path: %v", err)
	}
	return string(path)
}

// batchPreviewID reads the ID a preview printed off the line under the line
// that offers it.
func batchPreviewID(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if strings.Contains(line, "--accept and this ID:") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	t.Fatalf("no preview ID in:\n%s", text)
	return ""
}

func batchCachedTask(t *testing.T, id string) model.Task {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	task, err := st.Task(ctx, id)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	return task
}

func batchSeed(t *testing.T) {
	t.Helper()
	isolate(t)
	seedProjects(t,
		model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"},
		model.Project{Id: "p2", Name: "Work", Kind: "TASK"})
}

func TestCmdBatchPrefillsTheTemplateAndCreatesWhatItShowed(t *testing.T) {
	batchSeed(t)
	document := "# Work\nBuy milk\n\t[ ] two liters\n[x] Water plants\n"
	code, out, diagnostic, initial := batchRun(t, document, "-y")
	if code != exitOK {
		t.Fatalf("code %d, stderr %s", code, diagnostic)
	}
	if !strings.Contains(initial, "// One line is one task.") {
		t.Errorf("the editor was not prefilled with the template:\n%s", initial)
	}
	for _, want := range []string{
		"2 tasks in 1 list, 1 checklist item",
		` 1. add "Buy milk"`,
		`to list "Work"`,
		`item "two liters"`,
		` 2. add "Water plants" (done)`,
		"created 2 tasks together",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout does not say %q:\n%s", want, out)
		}
	}
	tasks := cachedTasks(t)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want two", tasks)
	}
	byTitle := map[string]model.Task{}
	for _, task := range tasks {
		byTitle[task.Title] = task
	}
	milk, ok := byTitle["Buy milk"]
	if !ok || milk.ProjectId != "p2" || milk.Status.Done() {
		t.Errorf("Buy milk = %+v, want it open in Work", milk)
	}
	stored := batchCachedTask(t, milk.Id)
	if len(stored.Items) != 1 || stored.Items[0].Title != "two liters" {
		t.Errorf("Buy milk checklist = %+v, want the one item", stored.Items)
	}
	if plants := byTitle["Water plants"]; !plants.Status.Done() {
		t.Errorf("Water plants = %+v, want it completed right away", plants)
	}
	if _, err := os.Stat(batchDraftPath(t)); !os.IsNotExist(err) {
		t.Errorf("the draft of a batch that went through was kept: %v", err)
	}
}

func TestCmdBatchWithoutAListHeadingUsesTheDefaultList(t *testing.T) {
	batchSeed(t)
	code, out, diagnostic, _ := batchRun(t, "Loose task\n", "-y")
	if code != exitOK {
		t.Fatalf("code %d, stderr %s", code, diagnostic)
	}
	if !strings.Contains(out, `to list "`+config.Default().DefaultProject+`"`) {
		t.Errorf("stdout does not name the default list:\n%s", out)
	}
	tasks := cachedTasks(t)
	if len(tasks) != 1 || tasks[0].ProjectId != "p1" {
		t.Fatalf("tasks = %+v, want the one task in the default list", tasks)
	}
}

func TestCmdBatchPreviewSavesAnIDThatIsAcceptedOnce(t *testing.T) {
	batchSeed(t)
	code, out, diagnostic, _ := batchRun(t, "# Work\nBuy milk\n", "--preview")
	if code != exitOK {
		t.Fatalf("preview code %d, stderr %s", code, diagnostic)
	}
	if tasks := cachedTasks(t); len(tasks) != 0 {
		t.Fatalf("a preview created %d tasks", len(tasks))
	}
	id := batchPreviewID(t, out)
	if len(id) != 64 {
		t.Fatalf("preview ID = %q, want a 64 character hash", id)
	}

	var accepted, problems bytes.Buffer
	if got := run(context.Background(), []string{"batch", "--accept", id}, strings.NewReader(""), &accepted, &problems); got != exitOK {
		t.Fatalf("accept code %d, stderr %s", got, &problems)
	}
	if !strings.Contains(accepted.String(), "created 1 task") {
		t.Errorf("accept said %q, want it to report the one task", accepted.String())
	}
	if tasks := cachedTasks(t); len(tasks) != 1 || tasks[0].Title != "Buy milk" {
		t.Fatalf("tasks = %+v, want the previewed task", tasks)
	}

	accepted.Reset()
	problems.Reset()
	if got := run(context.Background(), []string{"batch", "--accept", id}, strings.NewReader(""), &accepted, &problems); got != exitUsage {
		t.Fatalf("second accept = %d, want %d (stderr %s)", got, exitUsage, &problems)
	}
	if !strings.Contains(problems.String(), "already accepted") {
		t.Errorf("second accept said %q, want it to say the preview is gone", problems.String())
	}
	if tasks := cachedTasks(t); len(tasks) != 1 {
		t.Errorf("a second accept created %d tasks in all", len(tasks))
	}
}

func TestCmdBatchRefusesToAskWhenNobodyCanAnswer(t *testing.T) {
	batchSeed(t)
	var out, diagnostic bytes.Buffer
	if got := run(context.Background(), []string{"batch"}, strings.NewReader(""), &out, &diagnostic); got != exitUsage {
		t.Fatalf("code %d, want %d", got, exitUsage)
	}
	for _, want := range []string{"--preview", "-y"} {
		if !strings.Contains(diagnostic.String(), want) {
			t.Errorf("stderr does not offer %s:\n%s", want, diagnostic.String())
		}
	}
	if tasks := cachedTasks(t); len(tasks) != 0 {
		t.Errorf("a refused run created %d tasks", len(tasks))
	}
}

func TestCmdBatchKeepsTheDraftWhenTheDocumentCannotBeUsed(t *testing.T) {
	for _, c := range []struct {
		name, document, want string
	}{
		{"unknown list", "# Nowhere\nBuy milk\n", `no list matches "Nowhere"`},
		{"heading close to a list", "# Wor\nBuy milk\n", "there is: Work"},
		{"unreadable line", "\tindented first\n", "line 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			batchSeed(t)
			code, _, diagnostic, _ := batchRun(t, c.document, "-y")
			if code != exitError {
				t.Fatalf("code %d, want %d (stderr %s)", code, exitError, diagnostic)
			}
			if !strings.Contains(diagnostic, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, diagnostic)
			}
			if !strings.Contains(diagnostic, "draft kept at") {
				t.Errorf("stderr does not name the kept draft:\n%s", diagnostic)
			}
			if _, err := os.Stat(batchDraftPath(t)); err != nil {
				t.Errorf("the draft was not kept: %v", err)
			}
			if tasks := cachedTasks(t); len(tasks) != 0 {
				t.Errorf("a refused document created %d tasks", len(tasks))
			}
		})
	}
}

// The name of a heading nothing matches comes from the document, so it is as
// long as whoever wrote it liked, and the refusal that quotes it goes out
// behind the "tt: batch: " every failure of the command carries.
func TestCmdBatchFitsARefusalAboutAnUnknownListOnTheTerminal(t *testing.T) {
	batchSeed(t)
	code, _, diagnostic, _ := batchRun(t, "# "+strings.Repeat("Nowhere", 20)+"\nBuy milk\n", "-y")
	if code != exitError {
		t.Fatalf("code %d, want %d (stderr %s)", code, exitError, diagnostic)
	}
	for _, want := range []string{"no list matches", "line 1 of the document", "tt project add"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("stderr does not say %q:\n%s", want, diagnostic)
		}
	}
	for _, line := range strings.Split(strings.TrimRight(diagnostic, "\n"), "\n") {
		if strings.HasPrefix(line, `"`) {
			// The path of the draft goes out whole, however long it is.
			continue
		}
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a refusal line is %d columns wide, want at most %d:\n%s", n, cli.Width, line)
		}
	}
}

// The hours a preview may be accepted in are the reader's to spend, and the
// editor session is the reader's own time: a session longer than the window
// used to leave a preview that had expired before it was ever shown.
func TestCmdBatchDoesNotSpendThePreviewWindowInsideTheEditor(t *testing.T) {
	batchSeed(t)
	start := time.Now()
	previous := batchNow
	batchNow = func() time.Time {
		// The clock is read before the editor opens and again after it
		// closes; between the two the reader wrote for longer than a preview
		// lives.
		if batchEditorHasRun() {
			return start.Add(app.BatchPlanLifetime + time.Hour)
		}
		return start
	}
	t.Cleanup(func() { batchNow = previous })

	code, out, diagnostic, _ := batchRun(t, "# Work\nBuy milk\n", "-y")
	if code != exitOK {
		t.Fatalf("code %d, want %d (stderr %s)", code, exitOK, diagnostic)
	}
	if !strings.Contains(out, "created 1 task") {
		t.Errorf("stdout does not report the task:\n%s", out)
	}
	if tasks := cachedTasks(t); len(tasks) != 1 {
		t.Errorf("tasks = %+v, want the one task of a document written slowly", tasks)
	}
}

// A preview that is gone by the time the run comes back for it takes the only
// copy of the document with it unless the draft is still on disk, so the
// draft outlives the preview and the refusal names the file.
func TestCmdBatchKeepsTheDraftWhenThePreviewIsGoneBeforeItIsApplied(t *testing.T) {
	batchSeed(t)
	start := time.Now()
	steps := 0
	previous := batchNow
	batchNow = func() time.Time {
		if !batchEditorHasRun() {
			return start
		}
		// Every step taken after the document was read takes longer than a
		// preview lives, so the preview saved for it is gone by the time this
		// same run claims it.
		steps++
		return start.Add(time.Duration(steps) * (app.BatchPlanLifetime + time.Hour))
	}
	t.Cleanup(func() { batchNow = previous })

	code, _, diagnostic, _ := batchRun(t, "# Work\nBuy milk\n", "-y")
	if code != exitError {
		t.Fatalf("code %d, want %d (stderr %s)", code, exitError, diagnostic)
	}
	for _, want := range []string{"expired", "draft kept at"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("stderr does not say %q:\n%s", want, diagnostic)
		}
	}
	if _, err := os.Stat(batchDraftPath(t)); err != nil {
		t.Errorf("the draft went with the preview, so the document is nowhere: %v", err)
	}
	if tasks := cachedTasks(t); len(tasks) != 0 {
		t.Errorf("a preview nobody could claim created %d tasks", len(tasks))
	}
}

// batchEditorHasRun reports whether the editor stub has already written the
// document it was given, which is how a test tells the time before the editor
// from the time after it.
func batchEditorHasRun() bool {
	_, err := os.Stat(os.Getenv("TT_MARKDOWN_CAPTURE"))
	return err == nil
}

func TestCmdBatchCreatesNothingForADocumentWithNoTasks(t *testing.T) {
	for _, c := range []struct {
		name, document, want string
	}{
		{"unchanged", "\x00", "unchanged; no tasks created"},
		{"emptied", "", "the document is empty; no tasks created"},
		{"hints only", "// nothing but a comment\n", "holds no tasks"},
	} {
		t.Run(c.name, func(t *testing.T) {
			batchSeed(t)
			code, out, diagnostic, _ := batchRun(t, c.document, "-y")
			if code != exitOK {
				t.Fatalf("code %d, want %d (stderr %s)", code, exitOK, diagnostic)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("stdout does not say %q:\n%s", c.want, out)
			}
			if tasks := cachedTasks(t); len(tasks) != 0 {
				t.Errorf("%d tasks were created from a document with none", len(tasks))
			}
			if _, err := os.Stat(batchDraftPath(t)); !os.IsNotExist(err) {
				t.Errorf("the draft of a cancelled batch was kept: %v", err)
			}
		})
	}
}

func TestCmdBatchCreatesAChildUnderTheTaskAboveIt(t *testing.T) {
	batchSeed(t)
	code, out, diagnostic, _ := batchRun(t, "# Work\nTrip\n\tBook tickets\n", "-y")
	if code != exitOK {
		t.Fatalf("code %d, want %d (stderr %s)", code, exitOK, diagnostic)
	}
	for _, want := range []string{"2 tasks in 1 list", " 1. add \"Trip\"", " 2. add \"Book tickets\"", "under task 1", "created 2 tasks together"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout does not say %q:\n%s", want, out)
		}
	}
	tasks := cachedTasks(t)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v, want the parent and its child", tasks)
	}
	byTitle := map[string]model.Task{}
	for _, task := range tasks {
		byTitle[task.Title] = batchCachedTask(t, task.Id)
	}
	parent, child := byTitle["Trip"], byTitle["Book tickets"]
	if parent.Id == "" || parent.ParentId != "" {
		t.Errorf("Trip = %+v, want a top-level task", parent)
	}
	if child.ParentId != parent.Id {
		t.Errorf("Book tickets = %+v, want it hanging under %s", child, parent.Id)
	}
	if _, err := os.Stat(batchDraftPath(t)); !os.IsNotExist(err) {
		t.Errorf("the draft of a batch that went through was kept: %v", err)
	}
}

func TestCmdBatchRefusesADoneTaskThatHasAChildOfItsOwn(t *testing.T) {
	batchSeed(t)
	code, _, diagnostic, _ := batchRun(t, "# Work\n[x] Trip\n\tBook tickets\n", "-y")
	if code != exitError {
		t.Fatalf("code %d, want %d (stderr %s)", code, exitError, diagnostic)
	}
	for _, want := range []string{"the document could not be used", "line 2", "cannot have child tasks", "remove the [x]"} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("stderr does not say %q:\n%s", want, diagnostic)
		}
	}
	if tasks := cachedTasks(t); len(tasks) != 0 {
		t.Errorf("a refused document created %d tasks: %+v", len(tasks), tasks)
	}
	// The whole document is refused at once, so the text has to survive it.
	if _, err := os.Stat(batchDraftPath(t)); err != nil {
		t.Errorf("the draft of a refused document was not kept: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(diagnostic, "\n"), "\n") {
		if strings.HasPrefix(line, `"`) {
			// The path of the draft goes out whole, however long it is:
			// half a path is of no use to anybody.
			continue
		}
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a refusal line is %d columns wide:\n%s", n, line)
		}
	}
}

func TestCmdBatchMisuseIsRefusedBeforeAnyEditor(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"words", []string{"batch", "some", "tasks"}, "takes no words"},
		{"both modes", []string{"batch", "--preview", "-y"}, "not both"},
		{"accept with more", []string{"batch", "--accept", strings.Repeat("a", 64), "-y"}, "takes nothing else"},
		{"repeated", []string{"batch", "--preview", "--preview"}, "only once"},
		{"unknown option", []string{"batch", "--frobnicate"}, "unknown option"},
		{"missing value", []string{"batch", "-P"}, "needs a value"},
		{"structured", []string{"batch", "--json"}, "external editor"},
	} {
		t.Run(c.name, func(t *testing.T) {
			batchSeed(t)
			var out, diagnostic bytes.Buffer
			got := run(context.Background(), c.args, strings.NewReader(""), &out, &diagnostic)
			if got != exitUsage {
				t.Fatalf("code %d, want %d (stdout %s stderr %s)", got, exitUsage, &out, &diagnostic)
			}
			printed := out.String() + diagnostic.String()
			if !strings.Contains(printed, c.want) {
				t.Errorf("output does not say %q:\n%s", c.want, printed)
			}
			if tasks := cachedTasks(t); len(tasks) != 0 {
				t.Errorf("a refused invocation created %d tasks", len(tasks))
			}
		})
	}
}

func TestCmdBatchReportsWhatAFailureWhileApplyingLeftBehind(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want []string
		gone []string
	}{
		{
			name: "reversed whole",
			err:  &app.BatchApplyError{Step: 2, Line: 3, Err: errors.New("the disk is on fire")},
			want: []string{"task 2 of the batch failed", "every task of the batch was removed",
				"the disk is on fire", "the preview is kept"},
		},
		{
			// A sync of another run took the parent while the batch was
			// being applied. Nothing is wrong with the batch, so the line
			// says so and the preview is there to be accepted again.
			name: "a parent another run took",
			err:  &app.BatchApplyError{Step: 2, Line: 3, Err: app.ErrBatchParentTaken},
			want: []string{"every task of the batch was removed", "another tt run changed the queued create",
				"the preview is kept"},
			gone: []string{"sync the parent task"},
		},
		{
			name: "reversal blocked",
			err: &app.BatchApplyError{Step: 2, Line: 3, Err: errors.New("the disk is on fire"),
				Rollback: &app.BatchRollbackBlockedError{Remaining: 1}},
			want: []string{"another change was recorded in between", "still there",
				"why removing them failed"},
			gone: []string{"the preview is kept"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			batchSeed(t)
			previous := batchApplyPlan
			batchApplyPlan = func(context.Context, *store.Store, app.BatchPlan) (app.BatchApplied, error) {
				return app.BatchApplied{}, c.err
			}
			t.Cleanup(func() { batchApplyPlan = previous })

			code, _, diagnostic, _ := batchRun(t, "# Work\nBuy milk\nFix the sink\n", "-y")
			if code != exitError {
				t.Fatalf("code %d, want %d (stderr %s)", code, exitError, diagnostic)
			}
			for _, want := range c.want {
				if !strings.Contains(diagnostic, want) {
					t.Errorf("stderr does not say %q:\n%s", want, diagnostic)
				}
			}
			for _, gone := range c.gone {
				if strings.Contains(diagnostic, gone) {
					t.Errorf("stderr still says %q, and the preview cannot be accepted again:\n%s", gone, diagnostic)
				}
			}
			for _, line := range strings.Split(strings.TrimRight(diagnostic, "\n"), "\n") {
				if n := cli.DisplayWidth(line); n > cli.Width {
					t.Errorf("a failure line is %d columns wide:\n%s", n, line)
				}
			}
		})
	}
}

// A title comes from the document and goes straight into the lines a preview
// prints, so the preview holds it the way every other report does: escaped,
// and cut to what one terminal line takes.
func TestBatchPreviewHoldsTheTitlesItReadsBack(t *testing.T) {
	const hostile = "Trip\u200bto\u202eMoscow"
	plan := app.BatchPlan{Steps: []app.BatchStep{
		{Task: model.Task{Title: hostile, ProjectId: "p2", Items: []model.Item{{Title: hostile}}},
			List: "Work", Line: 2, Parent: -1},
		{Task: model.Task{Title: strings.Repeat("wide ", 40), ProjectId: "p2"}, List: "Work", Line: 3, Parent: 0},
	}}

	lines := batchPreviewLines(plan)
	text := strings.Join(lines, "\n")
	for _, rune := range []string{"\u200b", "\u202e"} {
		if strings.Contains(text, rune) {
			t.Errorf("the preview prints %q as it stands:\n%s", rune, text)
		}
	}
	if !strings.Contains(text, `\u200b`) || !strings.Contains(text, `\u202e`) {
		t.Errorf("the preview does not show the invisible runes of the title at all:\n%s", text)
	}
	for _, line := range lines {
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a preview line is %d columns wide, want at most %d:\n%s", n, cli.Width, line)
		}
	}
}
