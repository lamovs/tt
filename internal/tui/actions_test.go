package tui

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	tasksync "github.com/movsar/tt/internal/sync"
)

func writableModel(t *testing.T) (browserModel, *store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "Home"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Selected", Kind: "TEXT", Content: "original body"}); err != nil {
		t.Fatal(err)
	}
	actions := app.NewActions(st, config.Default(), func() (*api.Client, error) { return nil, errors.New("no synthetic token") })
	m := newModel(ctx, app.NewBrowser(st), Options{Actions: actions, DefaultProject: "Home"})
	m.query = app.BrowseQuery{View: app.OpenView}
	return settle(t, m, m.Init()), st
}
func finishLocal(t *testing.T, m browserModel, cmd tea.Cmd) browserModel {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing action command")
	}
	m, next := step(m, cmd())
	if next != nil {
		m = settle(t, m, next)
	}
	return m
}

func TestTUIResourceUndoUsesTypedGlobalHistory(t *testing.T) {
	m, st := writableModel(t)
	preview, err := st.PreviewEntityMutation(m.ctx, store.EntityMutation{Ref: store.EntityRef{Kind: "folder"}, Action: "create", Patch: json.RawMessage(`{"name":"Global folder"}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := st.ApplyEntityMutation(m.ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	if m.dialog == nil || m.dialog.undo.Entry.Action.EntityRef == nil {
		t.Fatal("resource undo not previewed")
	}
	_, lines := m.dialogView()
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "Cancel unsent resource") || strings.Contains(text, "Task:") {
		t.Fatalf("misleading resource undo: %s", text)
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if _, err := st.Entity(m.ctx, out.Entity.Ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resource not canceled: %v", err)
	}
	if !strings.Contains(m.notice, "No remote request") || strings.Contains(m.notice, "requires sync") {
		t.Fatalf("misleading cancellation notice: %s", m.notice)
	}
}
func rowsIn(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFormSubmitOnceAndNoopConflictDrafts(t *testing.T) {
	m, st := writableModel(t)
	var cmd tea.Cmd
	m, _ = press(m, 'a', 0)
	if m.form == nil || m.form.draft().ProjectID != "home" {
		t.Fatal("create did not show configured target")
	}
	m, _ = press(m, 's', tea.ModCtrl)
	if m.form.err == "" || rowsIn(t, st, "tasks") != 1 {
		t.Fatal("invalid draft saved")
	}
	m, _ = step(m, tea.PasteMsg{Content: "New task"})
	m, _ = press(m, tea.KeyEnter, 0)
	body := "  exact body\nsecond line\n  "
	m, _ = step(m, tea.PasteMsg{Content: body})
	m, cmd = press(m, 's', tea.ModCtrl)
	if !m.busy {
		t.Fatal("submission was not fenced")
	}
	for _, key := range []tea.KeyPressMsg{{Code: 's', Mod: tea.ModCtrl}, {Code: tea.KeyEnter}, {Code: 'a'}, {Code: 'u'}} {
		var repeated tea.Cmd
		m, repeated = step(m, key)
		if repeated != nil {
			t.Fatal("repeated submission started work")
		}
	}
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "tasks") != 2 || rowsIn(t, st, "undo_log") != 2 || !strings.Contains(m.notice, "Saved locally") {
		t.Fatal("create outcome not atomic or truthful")
	}
	tasks, err := st.Tasks(m.ctx, store.TaskFilter{Search: "New task"})
	if err != nil || len(tasks) != 1 || tasks[0].Content != body {
		t.Fatalf("body round trip: %+v %v", tasks, err)
	}
	m, _ = press(m, 'e', 0)
	before := rowsIn(t, st, "events")
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "events") != before || !strings.Contains(m.notice, "Unchanged") {
		t.Fatal("unchanged form wrote a mutation")
	}
	m, _ = press(m, 'e', 0)
	original := *m.form.original
	m.form.title.set("Draft retained")
	_, err = st.UpdateTask(m.ctx, original.Id, model.TaskEdit{Content: model.Ptr("External CLI body")})
	if err != nil {
		t.Fatal(err)
	}

	m, cmd = m.load()
	m = settle(t, m, cmd)
	if m.form.original.Content != "original body" || string(m.form.title.value) != "Draft retained" {
		t.Fatal("refresh replaced dirty draft")
	}
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form == nil || m.form.err == "" || string(m.form.title.value) != "Draft retained" {
		t.Fatal("conflict lost the draft")
	}
	current, err := st.Task(m.ctx, original.Id)
	if err != nil || current.Content != "External CLI body" || current.Title == "Draft retained" {
		t.Fatal("conflict overwrote CLI edit")
	}
}

func TestModalKeysDirtyExitAndHostilePasteStayData(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'e', 0)
	original := m.taskID
	for _, r := range "qaeus+_123?/" {
		m, _ = press(m, r, 0)
	}
	m, _ = press(m, tea.KeyTab, 0)
	raw := "start\n\x1b]52;c;bad\a\nqsu_+"
	m, _ = step(m, tea.PasteMsg{Content: raw})
	if m.taskID != original || m.mode != normalMode || m.jump || m.searching {
		t.Fatal("input triggered shortcuts")
	}
	before := rowsIn(t, st, "events")
	for _, size := range [][2]int{{140, 32}, {80, 24}, {32, 10}} {
		m, _ = step(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for range 5 {
			view := m.View().Content
			if strings.Contains(view, "\x1b]52;") {
				t.Fatal("paste injected terminal controls")
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatal("form overflow")
				}
			}
		}
	}
	if rowsIn(t, st, "events") != before {
		t.Fatal("render wrote data")
	}
	m, _ = press(m, 'c', tea.ModCtrl)
	if !m.form.discard || !m.form.quitAfter {
		t.Fatal("dirty interrupt did not offer save/discard")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.form.discard || m.form.quitAfter {
		t.Fatal("continue did not cancel exit")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'd', 0)
	if m.form != nil || rowsIn(t, st, "events") != before {
		t.Fatal("discard saved changes")
	}
}

func TestCompletionUndoAndLateActionDoNotChangeOtherSelections(t *testing.T) {
	m, st := writableModel(t)
	selected := m.taskID
	m, _ = press(m, ' ', 0)
	if m.dialog == nil || m.dialog.original.Id != selected {
		t.Fatal("completion not bound to displayed ID")
	}
	m, cmd := press(m, tea.KeyEnter, 0)
	msg := cmd().(actionFinished)
	late := msg
	late.generation--
	m, _ = step(m, late)
	if !m.busy {
		t.Fatal("late result released current action")
	}
	m, cmd = step(m, msg)
	m = settle(t, m, cmd)
	if m.taskID != "" || !m.selectionLost {
		t.Fatal("completion selected a different task")
	}
	completed, err := st.Task(m.ctx, selected)
	if err != nil || !completed.Status.Done() {
		t.Fatal("not completed")
	}

	external, err := st.CreateTask(m.ctx, model.Task{ProjectId: "home", Title: "CLI global"})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd = press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	if m.dialog == nil || m.dialog.undo.Entry.Action.TaskID != external.Id {
		t.Fatal("undo used selected task")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if _, err = st.Task(m.ctx, external.Id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("global undo did not delete CLI create")
	}
	completed, err = st.Task(m.ctx, selected)
	if err != nil || !completed.Status.Done() {
		t.Fatal("global undo reopened selection")
	}
}

func TestSyncCancelPartialResultAndLateProgress(t *testing.T) {
	m, _ := writableModel(t)
	m, cmd := press(m, 's', 0)
	if cmd == nil || m.sync == nil || !m.sync.running {
		t.Fatal("sync not started")
	}
	m, repeated := press(m, 's', 0)
	if repeated != nil {
		t.Fatal("duplicate sync")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if !m.sync.canceling || m.sync.context.Err() == nil {
		t.Fatal("cancel did not reach operation")
	}
	generation := m.sync.generation
	out := app.SyncOutcome{Tasks: tasksync.Result{Pushed: 2, Failed: 1, Requeued: 3}, Canceled: true, Err: context.Canceled}
	m, refresh := step(m, syncFinished{generation: generation, result: out})
	if refresh == nil || m.sync.running {
		t.Fatal("completion not accepted")
	}
	m, _ = step(m, syncProgress{generation: generation, phase: "late success"})
	view := strings.Join(m.syncLines(), "\n")
	for _, want := range []string{"Sync canceled", "Remote confirmed: 2", "Newly held: 1", "Requeued: 3"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = step(m, syncFinished{generation: generation, result: app.SyncOutcome{}})
	if m.sync != nil {
		t.Fatal("late result reopened dismissed report")
	}
}

func TestIDPromotionPreservesSelectionAndDraftRefusesOldID(t *testing.T) {
	m, st := writableModel(t)
	old := m.taskID
	m, _ = press(m, 'e', 0)
	m.form.title.set("Draft")
	if err := st.ReplaceLocalID(m.ctx, old, "promoted"); err != nil {
		t.Fatal(err)
	}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	if m.taskID != "promoted" || m.form.original.Id != old || m.detail.Id != "promoted" {
		t.Fatal("promotion overwrote draft or lost selection")
	}
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form == nil || m.form.err == "" {
		t.Fatal("stale ID edit was silently redirected")
	}
}

func TestCommandGateDrainsAndRejectsUnstartedIO(t *testing.T) {
	gate := &commandGate{}
	ctx, cancel := context.WithCancel(context.Background())
	entered, exited, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cmd := gate.command(func() tea.Msg { close(entered); <-ctx.Done(); close(exited); return nil })
	go cmd()
	<-entered
	go func() { gate.close(cancel); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	select {
	case <-exited:
	default:
		t.Fatal("store could close before worker")
	}
	called := false
	late := gate.command(func() tea.Msg { called = true; return nil })
	late()
	if called {
		t.Fatal("unstarted command ran after store shutdown")
	}
}

func TestFailedRefreshDoesNotPresentOldQueueAsCurrent(t *testing.T) {
	m, _ := writableModel(t)
	if !m.stateKnown {
		t.Fatal("missing initial queue")
	}
	m, _ = step(m, snapshotLoaded{query: m.query, generation: m.generation, err: errors.New("cache unavailable")})
	if m.stateKnown || m.stateErr == nil || !strings.Contains(m.View().Content, "Queue unavailable") {
		t.Fatal("failed refresh advertised stale queue counts")
	}
}

func TestEmptyGlobalUndoIsAnExplicitNoop(t *testing.T) {
	m, st := writableModel(t)
	m, cmd := press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	before := rowsIn(t, st, "events")
	m, cmd = press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	if m.dialog != nil || m.busy || m.notice != "Nothing to undo." || rowsIn(t, st, "events") != before {
		t.Fatal("empty undo was reported as a failed mutation")
	}
}

func TestUndersizedTerminalCannotSubmitHiddenForms(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'e', 0)
	m.form.title.set("Hidden edit")
	before := rowsIn(t, st, "events")
	m, _ = step(m, tea.WindowSizeMsg{Width: 20, Height: 8})
	for _, key := range []tea.KeyPressMsg{{Code: 's', Mod: tea.ModCtrl}, {Code: tea.KeyEnter}, {Code: 'q'}} {
		var cmd tea.Cmd
		m, cmd = step(m, key)
		if cmd != nil || m.busy {
			t.Fatal("hidden form started work")
		}
	}
	if rowsIn(t, st, "events") != before || string(m.form.title.value) != "Hidden edit" {
		t.Fatal("undersized input changed draft or store")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form != nil || rowsIn(t, st, "events") != before+1 {
		t.Fatal("resize did not restore form")
	}
}
