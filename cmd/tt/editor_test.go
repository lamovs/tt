package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/editor"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func documentWithFields(t *testing.T, task model.Task, change func(*editorFields), body string) []byte {
	t.Helper()
	fields := taskEditorFields(task)
	if change != nil {
		change(&fields)
	}
	var out bytes.Buffer
	out.WriteString("+++\n")
	if err := toml.NewEncoder(&out).Encode(fields); err != nil {
		t.Fatal(err)
	}
	out.WriteString("+++\n")
	out.WriteString(body)
	return out.Bytes()
}

func TestEditorDocumentExactBodyAndNoop(t *testing.T) {
	for _, body := range []string{"", "\n", "# Title\n\n  indent \n\n", "+++ inside\n", "line\r\n\r\n", "Привет\n", "no final newline"} {
		task := model.Task{Id: "id", ProjectId: "p", Title: "title", Kind: "NOTE", Content: body}
		for _, hints := range []model.EditorHints{model.HintsFull, model.HintsShort, model.HintsNone} {
			doc, err := editorDocument(task, hints)
			if err != nil {
				t.Fatal(err)
			}
			next, edit, err := editedTaskDocument(task, doc, false)
			if err != nil || !edit.IsEmpty() || !reflect.DeepEqual(next, task) {
				t.Fatalf("roundtrip body %q hints %v: %#v %+v %v", body, hints, next, edit, err)
			}
			hasLongHint := bytes.Contains(doc, []byte("# priority:"))
			hasShortHint := bytes.Contains(doc, []byte("# Edit fields"))
			if hasLongHint != (hints == model.HintsFull) || hasShortHint != (hints != model.HintsNone) {
				t.Fatal("hints mismatch")
			}
		}
	}
}

func TestEditorDocumentUsesDayMonthYearAndShowsExamples(t *testing.T) {
	task := model.Task{Id: "id", ProjectId: "p", Title: "title", Kind: "TEXT",
		DueDate: model.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.Local))}
	doc, err := editorDocument(task, model.HintsFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`due = "21.10.2026 09:30"`,
		`# Example values: due = "21.10.2026 09:30"`,
		`# Example arrays: reminders = ["TRIGGER:-PT10M"], tags = ["work", "urgent"]`,
	} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("editor document does not contain %q:\n%s", want, doc)
		}
	}
}

func TestEditorDocumentMinimalPatchPreservesChecklistAndDate(t *testing.T) {
	stamp := model.NewTime(time.Date(2026, 9, 9, 12, 13, 14, 123000000, time.FixedZone("original", 9*3600)))
	task := model.Task{Id: "id", ProjectId: "p", Title: "old", Kind: "CHECKLIST", DueDate: stamp,
		TimeZone: "Asia/Tokyo", Reminders: []string{"TRIGGER:PT0S", "TRIGGER:PT0S"},
		Items: []model.Item{{Id: "server", Key: "stable", Title: "keep", SortOrder: 19, Status: model.ItemDone}}}
	doc := documentWithFields(t, task, func(f *editorFields) { f.Title = "new" }, "\n# Notes\n  exact \n\n")
	next, edit, err := editedTaskDocument(task, doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if edit.Title == nil || edit.Content == nil || edit.DueDate != nil || edit.TimeZone != nil || edit.Items != nil || edit.Kind != nil || edit.Reminders != nil {
		t.Fatalf("unexpected patch %+v", edit)
	}
	if !reflect.DeepEqual(next.Items, task.Items) || !reflect.DeepEqual(next.DueDate, task.DueDate) || next.Content != "\n# Notes\n  exact \n\n" {
		t.Fatal("uneditable data or exact body lost")
	}
}

func TestEditorDocumentRejectsInvalidMetadata(t *testing.T) {
	task := model.Task{Id: "id", ProjectId: "p", Title: "title", Kind: "TEXT"}
	for name, change := range map[string]func(*editorFields){
		"id":        func(f *editorFields) { f.ID = "other" },
		"project":   func(f *editorFields) { f.ProjectID = "other" },
		"kind":      func(f *editorFields) { f.Kind = "NOTE" },
		"title":     func(f *editorFields) { f.Title = " " },
		"control":   func(f *editorFields) { f.Title = "title\nspoof" },
		"priority":  func(f *editorFields) { f.Priority = "urgent" },
		"due":       func(f *editorFields) { f.Due = "invalid date" },
		"repeat":    func(f *editorFields) { f.Repeat = "yesterday" },
		"reminder":  func(f *editorFields) { f.Reminders = []string{"invalid"} },
		"duplicate": func(f *editorFields) { f.Reminders = []string{"TRIGGER:PT0S", "TRIGGER:PT0S"} },
		"tag":       func(f *editorFields) { f.Tags = []string{""} },
	} {
		t.Run(name, func(t *testing.T) {
			doc := documentWithFields(t, task, change, "body")
			if _, _, err := editedTaskDocument(task, doc, false); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
	valid := documentWithFields(t, task, nil, "")
	for _, data := range [][]byte{
		[]byte("no frontmatter"),
		bytes.Replace(valid, []byte("+++\n"), []byte("+++\nunknown = 'x'\n"), 1),
		bytes.Replace(valid, []byte("+++\n"), []byte("+++\nTITLE = 'hidden'\n"), 1),
		bytes.Replace(valid, []byte("title = \"title\"\n"), nil, 1),
		bytes.Replace(valid, []byte("title = \"title\""), []byte("title = 7"), 1),
		bytes.Replace(valid, []byte("title = \"title\""), []byte("title = \"title\"\ntitle = \"duplicate\""), 1),
	} {
		if _, _, err := editedTaskDocument(task, data, false); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestEditorDocumentCreateNoteAndFieldClears(t *testing.T) {
	seed := model.Task{ProjectId: "p", Title: "draft", Kind: "TEXT"}
	doc := documentWithFields(t, seed, func(f *editorFields) {
		f.Kind = "NOTE"
		f.Priority = "high"
		f.Due = "25.09.2026 9:00"
		f.Repeat = "weekly"
		f.Reminders = []string{"TRIGGER:PT0S"}
		f.Tags = []string{"one", "two"}
	}, "# body\n\n")
	created, _, err := editedTaskDocument(seed, doc, true)
	if err != nil || created.Kind != "NOTE" || created.Priority != model.PriorityHigh || created.DueDate.IsZero() || created.IsAllDay || created.RepeatFlag == "" {
		t.Fatalf("create %+v %v", created, err)
	}
	created.Id = "id"
	doc = documentWithFields(t, created, func(f *editorFields) {
		f.Due = "none"
		f.Repeat = "none"
		f.Reminders = []string{}
		f.Tags = []string{}
	}, "")
	next, edit, err := editedTaskDocument(created, doc, false)
	if err != nil || !next.DueDate.IsZero() || next.RepeatFlag != "" || len(next.Reminders) != 0 || len(next.Tags) != 0 || next.Content != "" {
		t.Fatalf("clear %+v %v", next, err)
	}
	if edit.DueDate == nil || edit.StartDate == nil || edit.IsAllDay == nil || *edit.IsAllDay || edit.RepeatFlag == nil || edit.Reminders == nil || edit.Tags == nil || edit.Content == nil {
		t.Fatalf("clear pointers %+v", edit)
	}
	seed.Items = []model.Item{{Title: "checklist"}}
	if _, _, err := editedTaskDocument(seed, documentWithFields(t, seed, func(f *editorFields) { f.Kind = "NOTE" }, ""), true); err == nil {
		t.Fatal("NOTE with checklist accepted")
	}
}

func TestEditorDocumentCommentOnlyBlankCreateDoesNothing(t *testing.T) {
	task := model.Task{ProjectId: "p", Kind: "TEXT"}
	doc := documentWithFields(t, task, nil, "")
	next, edit, err := editedTaskDocument(task, doc, true)
	if err != nil || !edit.IsEmpty() || !reflect.DeepEqual(next, task) {
		t.Fatal("comment-only blank document was not a no-op")
	}
}

func TestCmdAddMarkdownNoInitialTitle(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject})
	seed := model.Task{ProjectId: "p1", Kind: "TEXT"}
	replacement := documentWithFields(t, seed, func(f *editorFields) { f.Title = "typed in editor" }, "body\n")
	setupMarkdownEditor(t, replacement)
	var out, diagnostic bytes.Buffer
	if code := run(context.Background(), []string{"add", "-e", "markdown"}, strings.NewReader(""), &out, &diagnostic); code != exitOK {
		t.Fatalf("code %d: %s", code, &diagnostic)
	}
	tasks := cachedTasks(t)
	if len(tasks) != 1 || tasks[0].Title != "typed in editor" || tasks[0].Content != "body\n" {
		t.Fatalf("tasks %+v", tasks)
	}
}

func TestMarkdownEditorProcess(t *testing.T) {
	if os.Getenv("TT_MARKDOWN_HELPER") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	if capture := os.Getenv("TT_MARKDOWN_CAPTURE"); capture != "" {
		initial, err := os.ReadFile(path)
		if err != nil || os.WriteFile(capture, initial, 0600) != nil || os.WriteFile(capture+".path", []byte(path), 0600) != nil {
			os.Exit(20)
		}
	}
	if target := os.Getenv("TT_MARKDOWN_CONCURRENT"); target != "" {
		st, err := store.Open(context.Background(), "")
		if err != nil {
			os.Exit(21)
		}
		_, err = st.UpdateTask(context.Background(), target, model.TaskEdit{Title: model.Ptr("concurrent client")})
		_ = st.Close()
		if err != nil {
			os.Exit(22)
		}
	}
	if replacement := os.Getenv("TT_MARKDOWN_REPLACEMENT"); replacement != "" {
		data, err := os.ReadFile(replacement)
		if err != nil || os.WriteFile(path, data, 0600) != nil {
			os.Exit(23)
		}
	}
	if os.Getenv("TT_MARKDOWN_FAIL") == "1" {
		os.Exit(4)
	}
	os.Exit(0)
}

func setupMarkdownEditor(t *testing.T, replacement []byte) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TT_MARKDOWN_HELPER", "1")
	t.Setenv("VISUAL", strconv.Quote(os.Args[0])+" -test.run=^TestMarkdownEditorProcess$ --")
	t.Setenv("TT_MARKDOWN_CAPTURE", filepath.Join(root, "capture.md"))
	t.Setenv("TT_MARKDOWN_CONCURRENT", "")
	t.Setenv("TT_MARKDOWN_FAIL", "")
	t.Setenv("TT_MARKDOWN_REPLACEMENT", "")
	if replacement != nil {
		path := filepath.Join(root, "replacement.md")
		if err := os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TT_MARKDOWN_REPLACEMENT", path)
	}
	return filepath.Join(root, "capture.md")
}

func TestCmdAddMarkdownPrefillAndExactNote(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: "Personal", Kind: "TASK"})
	seed := model.Task{ProjectId: "p1", Title: "initial", Kind: "TEXT", Priority: model.PriorityHigh}
	body := "# Meeting\n\n  notes \n\n"
	replacement := documentWithFields(t, seed, func(f *editorFields) { f.Kind = "NOTE"; f.Title = "saved note" }, body)
	capture := setupMarkdownEditor(t, replacement)
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), []string{"add", "initial", "-P", "Personal", "-p", "high", "-e", "markdown"}, strings.NewReader(""), &out, &diagnostic)
	if code != exitOK {
		t.Fatalf("code %d stderr %s", code, &diagnostic)
	}
	data, _ := os.ReadFile(capture)
	fields, content, err := parseEditorDocument(data)
	if err != nil || fields.Title != "initial" || fields.Priority != "high" || content != "" {
		t.Fatal("add flags not prefilled")
	}
	tasks := cachedTasks(t)
	if len(tasks) != 1 || tasks[0].Kind != "NOTE" || tasks[0].Title != "saved note" || tasks[0].Content != body {
		t.Fatalf("tasks %+v", tasks)
	}
	draftPath, _ := os.ReadFile(capture + ".path")
	if _, err := os.Stat(string(draftPath)); !os.IsNotExist(err) {
		t.Fatal("successful draft not removed")
	}
}

func TestCmdMarkdownNoopFailureAndConflict(t *testing.T) {
	for _, mode := range []string{"unchanged-add", "comments-only-add", "invalid", "editor-fail", "conflict", "edit", "unchanged-edit"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})
			ctx := context.Background()
			st, err := store.Open(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			original := model.Task{ProjectId: "p1", Title: "original", Kind: "TEXT"}
			args := []string{"add", "original", "-e", "markdown"}
			if !strings.HasSuffix(mode, "-add") {
				original, err = st.CreateTask(ctx, original)
				if err != nil {
					t.Fatal(err)
				}
				original, err = st.Task(ctx, original.Id)
				if err != nil {
					t.Fatal(err)
				}
				args = []string{"edit", original.Id, "-e", "markdown"}
			}
			var replacement []byte
			switch mode {
			case "comments-only-add":
				replacement = documentWithFields(t, original, nil, "")
			case "invalid":
				replacement = []byte("invalid")
			case "edit", "conflict", "editor-fail":
				replacement = documentWithFields(t, original, func(f *editorFields) { f.Title = "editor title" }, "\nbody\n\n")
			}
			capture := setupMarkdownEditor(t, replacement)
			if mode == "editor-fail" {
				t.Setenv("TT_MARKDOWN_FAIL", "1")
			}
			if mode == "conflict" {
				t.Setenv("TT_MARKDOWN_CONCURRENT", original.Id)
			}
			var countBefore int
			if err := st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&countBefore); err != nil {
				t.Fatal(err)
			}
			var out, diagnostic bytes.Buffer
			code := run(ctx, args, strings.NewReader(""), &out, &diagnostic)
			wantFail := mode == "invalid" || mode == "editor-fail" || mode == "conflict"
			if (code != exitOK) != wantFail {
				t.Fatalf("code %d: %s", code, &diagnostic)
			}
			var countAfter int
			if err := st.DB().QueryRow("SELECT count(*) FROM outbox").Scan(&countAfter); err != nil {
				t.Fatal(err)
			}
			delta := 0
			if mode == "edit" || mode == "conflict" {
				delta = 1
			}
			if countAfter != countBefore+delta {
				t.Fatalf("queue grew by %d instead of %d", countAfter-countBefore, delta)
			}
			if !strings.HasSuffix(mode, "-add") {
				task, err := st.Task(ctx, original.Id)
				if err != nil {
					t.Fatal(err)
				}
				wantTitle := "original"
				if mode == "edit" {
					wantTitle = "editor title"
				}
				if mode == "conflict" {
					wantTitle = "concurrent client"
				}
				if task.Title != wantTitle {
					t.Fatalf("title %q want %q", task.Title, wantTitle)
				}
			} else if tasks := cachedTasks(t); len(tasks) != 0 {
				t.Fatal("noop created task")
			}
			draftPath, _ := os.ReadFile(capture + ".path")
			if wantFail {
				if !strings.Contains(diagnostic.String(), "draft kept at") {
					t.Fatal("missing recovery path")
				}
				data, err := os.ReadFile(string(draftPath))
				if err != nil || !bytes.Equal(data, replacement) {
					t.Fatal("recovery draft differs")
				}
			} else if _, err := os.Stat(string(draftPath)); !os.IsNotExist(err) {
				t.Fatal("successful draft not removed")
			}
		})
	}
}

func TestEditorRecoveryPathFinalOutputKeepsExactBytes(t *testing.T) {
	path := "/tmp/with  repeated   spaces/" + strings.Repeat("long-segment", 30) + "/tab\tnewline\ncontrol\x1b/task.md"
	var diagnostic bytes.Buffer
	inv := &invocation{verb: "edit", ctx: context.Background(), stderr: &diagnostic}
	if code := inv.editorFailure(&editor.Draft{Path: path}, errors.New("invalid document")); code != exitError {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSuffix(diagnostic.String(), "\n"), "\n")
	if len(lines) != 3 || lines[1] != "draft kept at" {
		t.Fatalf("diagnostic shape %q", diagnostic.String())
	}
	decoded, err := strconv.Unquote(lines[2])
	if err != nil || decoded != path {
		t.Fatalf("decoded path %q, %v", decoded, err)
	}
	if strings.ContainsAny(lines[2], "\t\x1b") {
		t.Fatal("raw controls in path output")
	}
}

func TestEditorRemovalFailurePathFinalOutputKeepsExactBytes(t *testing.T) {
	isolate(t)
	setupMarkdownEditor(t, nil)
	parent := filepath.Join(t.TempDir(), "repeated  spaces  "+strings.Repeat("long", 25))
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", parent)
	var out, diagnostic bytes.Buffer
	inv := &invocation{verb: "edit", ctx: context.Background(), stdin: strings.NewReader(""), stdout: &out, stderr: &diagnostic}
	draft, err := inv.openTaskEditor(model.Task{Id: "id", ProjectId: "p", Title: "title"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700); _ = draft.Discard() })
	if code := inv.editorDone(draft, "unchanged"); code != exitOK {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSuffix(diagnostic.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "could not remove editor draft:" {
		t.Fatalf("diagnostic shape %q", diagnostic.String())
	}
	decoded, err := strconv.Unquote(lines[1])
	if err != nil || decoded != draft.Path {
		t.Fatalf("decoded path %q, %v", decoded, err)
	}
}

func TestEditorFailureBoundsAndEscapesFinalError(t *testing.T) {
	for _, verb := range []string{"add", "edit"} {
		for _, message := range []string{
			"parser rejected \x1b[31mred\r\nforged\tlabel\x00\xff",
			"invalid \"quoted\" \\ path with  spaces",
			strings.Repeat("unbroken", 100),
			strings.Repeat("界", 200),
		} {
			var out, diagnostic bytes.Buffer
			inv := &invocation{verb: verb, ctx: context.Background(), stdout: &out, stderr: &diagnostic}
			if code := inv.editorFailure(nil, errors.New(message)); code != exitError {
				t.Fatal(code)
			}
			printed := strings.TrimSuffix(diagnostic.String(), "\n")
			if out.Len() != 0 || strings.ContainsAny(printed, "\r\n\t\x00\x1b\xff") {
				t.Fatalf("unsafe final output %q", printed)
			}
			if width := cli.DisplayWidth(printed); width > cli.Width {
				t.Fatalf("error line is %d columns: %q", width, printed)
			}
			prefix := "tt: " + verb + ": "
			if !strings.HasPrefix(printed, prefix) {
				t.Fatalf("prefix missing: %q", printed)
			}
			decoded, err := strconv.Unquote(strings.TrimPrefix(printed, prefix))
			if err != nil {
				t.Fatalf("invalid final quoted error %q: %v", printed, err)
			}
			if cli.DisplayWidth(strconv.Quote(message)) <= cli.Width-len(prefix) && decoded != message {
				t.Fatalf("short error bytes changed: %q != %q", decoded, message)
			}
			if decoded == "" {
				t.Fatal("error text was swallowed")
			}
		}
	}
}

func TestEditorReportsBoundTitlesAndProjectNames(t *testing.T) {
	for _, mode := range []string{"ordinary-add", "markdown-add", "markdown-edit"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			project := "Project\x1b[31m\r\n" + strings.Repeat("列表", 150)
			title := "Title\"\\" + strings.Repeat("界", 150)
			seedProjects(t, model.Project{Id: "p1", Name: project})
			args := []string{"add", title, "-P", project}
			if mode != "ordinary-add" {
				original := model.Task{ProjectId: "p1", Title: "original", Kind: "TEXT"}
				if mode == "markdown-edit" {
					st, err := store.Open(context.Background(), "")
					if err != nil {
						t.Fatal(err)
					}
					original, err = st.CreateTask(context.Background(), original)
					_ = st.Close()
					if err != nil {
						t.Fatal(err)
					}
					args = []string{"edit", original.Id, "-e", "markdown"}
				} else {
					args = []string{"add", "original", "-P", project, "-e", "markdown"}
				}
				setupMarkdownEditor(t, documentWithFields(t, original, func(f *editorFields) { f.Title = title }, "body\n"))
			}
			var out, diagnostic bytes.Buffer
			if code := run(context.Background(), args, strings.NewReader(""), &out, &diagnostic); code != exitOK {
				t.Fatalf("code %d: %s", code, &diagnostic)
			}
			if diagnostic.Len() != 0 {
				t.Fatalf("stderr %q", diagnostic.String())
			}
			printed := strings.TrimSuffix(out.String(), "\n")
			if strings.ContainsAny(printed, "\r\n\t\x00\x1b") {
				t.Fatalf("unsafe success output %q", printed)
			}
			if width := cli.DisplayWidth(printed); width > cli.Width {
				t.Fatalf("success is %d columns: %q", width, printed)
			}
			if !strings.Contains(printed, "Title") {
				t.Fatalf("task title swallowed: %q", printed)
			}
			if mode != "markdown-edit" && (!strings.Contains(printed, " to ") || !strings.Contains(printed, "Project")) {
				t.Fatalf("project name swallowed: %q", printed)
			}
			tasks := cachedTasks(t)
			if len(tasks) != 1 || tasks[0].Title != title {
				t.Fatal("display bound changed the stored title")
			}
		})
	}
}
