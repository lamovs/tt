package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type documentActions struct {
	Actions
	calls    int
	document func(context.Context, app.DocumentRequest) (app.DocumentResult, error)
}

func (a *documentActions) Document(ctx context.Context, request app.DocumentRequest, _ io.Reader, _, _ io.Writer) (app.DocumentResult, error) {
	a.calls++
	return a.document(ctx, request)
}

func TestNoteFormAndEditorRequestSnapshots(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'n', 0)
	if m.form == nil || m.form.kind != "NOTE" || !strings.Contains(m.View().Content, "Create note (NOTE)") {
		t.Fatal("NOTE form missing")
	}
	m.form.title.set("New note")
	body := "\r\n  exact body\n- [x] body checkbox\n\n"
	m.form.body.set(body)
	var cmd tea.Cmd
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	tasks, err := st.Tasks(m.ctx, store.TaskFilter{Search: "New note"})
	if err != nil || len(tasks) != 1 || tasks[0].Kind != "NOTE" || tasks[0].Content != body || len(tasks[0].Items) != 0 {
		t.Fatalf("note %+v %v", tasks, err)
	}
	m.openEdit(tasks[0])
	m.form.title.set("Unsaved title")
	a := &documentActions{Actions: m.actions}
	a.document = func(_ context.Context, request app.DocumentRequest) (app.DocumentResult, error) {
		if request.Original.Id != tasks[0].Id || request.Original.Title != "New note" || request.Seed.Title != "Unsaved title" || request.Seed.Content != body || request.Seed.Kind != "NOTE" {
			t.Fatalf("request %+v", request)
		}
		return app.DocumentResult{Canceled: true}, nil
	}
	m.actions = a
	oldGeneration, oldDetail := m.generation, m.detailGeneration
	m, cmd = press(m, 'e', tea.ModCtrl)
	if cmd == nil || m.editor == nil || !m.busy {
		t.Fatal("handoff missing")
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'e', Mod: tea.ModCtrl}, {Code: 'E'}, {Code: 's', Mod: tea.ModCtrl}} {
		var duplicate tea.Cmd
		m, duplicate = step(m, key)
		if duplicate != nil {
			t.Fatal("duplicate handoff")
		}
	}
	pending := m.editor
	before := m.View().Content
	m.View()
	m.View()
	if a.calls != 0 || before != m.View().Content {
		t.Fatal("View ran editor or changed state")
	}
	m, _ = step(m, snapshotLoaded{generation: oldGeneration, query: m.query, tasks: []model.Task{{Id: "wrong"}}})
	m, _ = step(m, detailLoaded{generation: oldDetail, snapshot: oldGeneration, id: m.taskID, task: model.Task{Id: "wrong"}})
	if err = pending.Run(); err != nil {
		t.Fatal(err)
	}
	pending.Run()
	if a.calls != 1 {
		t.Fatal("editor ran more than once")
	}
	m = finishLocal(t, m, func() tea.Msg { return pending.finished })
	if m.busy || m.form == nil || string(m.form.title.value) != "Unsaved title" || !m.form.dirty() {
		t.Fatal("cancel lost form")
	}

	m, _ = press(m, 'e', tea.ModCtrl)
	newer := m.editor
	m, _ = step(m, pending.finished)
	if !m.busy || m.editor != newer {
		t.Fatal("late editor response accepted")
	}
}

func TestEditorRecoveryAndShutdownDrain(t *testing.T) {
	m, _ := writableModel(t)
	ctx, cancel := context.WithCancel(m.ctx)
	gate, journal := &commandGate{}, &draftJournal{}
	a := &documentActions{Actions: m.actions}
	started, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	path := "/private/draft  two spaces/\x1b]52;c;attack\a.md"
	a.document = func(ctx context.Context, _ app.DocumentRequest) (app.DocumentResult, error) {
		close(started)
		<-ctx.Done()
		<-release
		close(stopped)
		return app.DocumentResult{DraftPath: path}, errors.New("invalid document\x1b]52;c;attack\a")
	}
	c := &editorCommand{gate: gate, drafts: journal, actions: a, ctx: ctx, finished: editorFinished{generation: 1}}
	done := make(chan struct{})
	go func() { c.Run(); close(done) }()
	<-started
	closed := make(chan struct{})
	go func() { gate.close(cancel); close(closed) }()
	select {
	case <-closed:
		t.Fatal("gate closed before editor completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-closed
	<-done
	<-stopped
	m.busy, m.actionGeneration, m.editor = true, 1, c
	m, _ = m.finishEditor(c.finished)
	for _, size := range [][2]int{{140, 32}, {60, 20}, {32, 10}} {
		m.width, m.height = size[0], size[1]
		v := m.View().Content
		if strings.Contains(v, "\x1b]52;") || !strings.Contains(v, "Editor result") {
			t.Fatal("unsafe/missing recovery")
		}
	}
	var out bytes.Buffer
	journal.write(&out)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("path split: %q", out.String())
	}
	decoded, err := strconv.Unquote(lines[1])
	if err != nil || decoded != path {
		t.Fatalf("path bytes lost: %q %v", out.String(), err)
	}
	blocked := &editorCommand{gate: gate, actions: a, ctx: ctx}
	blocked.Run()
	if blocked.finished.ran || a.calls != 1 {
		t.Fatal("closed gate started editor")
	}
}
