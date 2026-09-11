package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func queueModel(t *testing.T) (browserModel, *store.Store) {
	t.Helper()
	m, st := writableModel(t)
	if _, err := st.DB().Exec(`UPDATE outbox SET state='failed',op=?,payload='{}'`, store.OpTaskComplete); err != nil {
		t.Fatal(err)
	}
	var cmd tea.Cmd
	m, cmd = press(m, 'Q', 0)
	if m.queue == nil || cmd == nil {
		t.Fatal("queue did not open")
	}
	m, _ = step(m, cmd())
	return m, st
}

func TestQueueConfirmIsFrozenAndDoesNotSelectNewRows(t *testing.T) {
	m, st := queueModel(t)
	m, _ = press(m, 'f', 0)
	if m.queue.confirmation == nil {
		t.Fatal("missing recovery preview", m.notice)
	}
	expected := *m.queue.confirmation
	if _, err := st.DB().Exec(`UPDATE outbox SET last_error='concurrent' WHERE seq=?`, expected.Item.Seq); err != nil {
		t.Fatal(err)
	}
	m, cmd := m.loadQueue()
	m, _ = step(m, cmd())
	if m.queue.confirmation.Version != expected.Version {
		t.Fatal("refresh replaced confirmation")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	if !m.busy || cmd == nil {
		t.Fatal("confirmation did not start")
	}
	m, again := press(m, tea.KeyEnter, 0)
	if again != nil {
		t.Fatal("duplicate submission")
	}
	m, _ = step(m, cmd())
	if !strings.Contains(m.notice, "stale") {
		t.Fatal(m.notice)
	}
	if _, err := st.DB().Exec(`DELETE FROM outbox WHERE seq=?`, expected.Item.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(m.ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: store.OpTaskDelete, TaskID: "neighbor", ProjectID: "work"}); err != nil {
		t.Fatal(err)
	}
	m, cmd = m.loadQueue()
	m, _ = step(m, cmd())
	if m.queue.seq != 0 || !m.queue.lost {
		t.Fatal("missing operation selected neighbor")
	}
	m, _ = press(m, 'f', 0)
	if m.queue.confirmation != nil {
		t.Fatal("lost selection mutated")
	}
	m, _ = press(m, 'j', 0)
	if m.queue.seq == 0 {
		t.Fatal("explicit selection failed")
	}
}

func TestQueueFocusAndViewAreReadOnlyAndLateResultsFenced(t *testing.T) {
	m, st := queueModel(t)
	original := m.queue.data
	m.queue.data.Focus = []store.FocusQueueEntry{{SessionID: "session", TaskID: "task", Title: "\x1b]2;injected\a", Phase: "armed", LastError: "uncertain"}}
	m.queue.sessionID = "session"
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, 'f', 0)
	if m.queue.confirmation != nil || !strings.Contains(m.notice, "read-only") {
		t.Fatal("focus retry available")
	}
	m, _ = press(m, tea.KeyEnter, 0)
	for _, width := range []int{32, 60, 90, 140} {
		m.width, m.height = width, 24
		text := ansi.Strip(m.View().Content)
		if strings.Contains(text, "\x1b]2;") || !strings.Contains(text, "Focus") {
			t.Fatal("unsafe focus render", text)
		}
	}
	if rowsIn(t, st, "outbox") != 1 {
		t.Fatal("View wrote")
	}
	m, _ = press(m, tea.KeyEsc, 0)
	m, _ = press(m, tea.KeyEsc, 0)
	old := m.queueGeneration - 1
	m, _ = step(m, queueLoaded{generation: old, data: original})
	if m.queue != nil {
		t.Fatal("late queue result reopened panel")
	}
}

func TestTaskDeletePreviewConflictsAndMoveKeepsExactDestination(t *testing.T) {
	m, st := writableModel(t)
	m, cmd := press(m, 'd', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.taskOperation == nil || m.taskOperation.preview == nil {
		t.Fatal("missing deletion preview")
	}
	id := m.taskOperation.original.Id
	if _, err := st.UpdateTask(m.ctx, id, model.TaskEdit{Title: model.Ptr("Changed")}); err != nil {
		t.Fatal(err)
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if m.taskOperation == nil || !strings.Contains(m.taskOperation.err, "stale") {
		t.Fatal("stale delete applied")
	}
	if rowsIn(t, st, "tasks") != 1 {
		t.Fatal("conflicting delete removed task")
	}
	m, _ = press(m, tea.KeyEsc, 0)
	if err := st.DeleteTask(m.ctx, id); err != nil {
		t.Fatal(err)
	}
	id = "confirmed-task"
	confirmed := model.Task{Id: id, ProjectId: "work", Title: "Changed", Kind: "TEXT", Content: "original body"}
	if _, err := st.SyncProject(m.ctx, "work", []store.ServerTask{{Task: confirmed, Raw: json.RawMessage(`{"id":"confirmed-task","projectId":"work","title":"Changed","kind":"TEXT","content":"original body"}`)}}); err != nil {
		t.Fatal(err)
	}
	m.taskID = id
	m, cmd = m.load()
	m = settle(t, m, cmd)
	m, _ = press(m, 'm', 0)
	destination := m.taskOperation.destination
	m.projects = []model.Project{{Id: "surprise", Name: "new search match"}}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if m.taskOperation.preview.Destination.Id != destination {
		t.Fatal("new list joined selection")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m, again := press(m, tea.KeyEnter, 0)
	if again != nil {
		t.Fatal("duplicate move")
	}
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "tasks") != 1 || !strings.Contains(m.notice, "global undo") || m.taskID != id {
		t.Fatal("move result", m.notice, m.taskID)
	}
}

func TestRecoveryConfirmationCannotSubmitWhenHidden(t *testing.T) {
	m, st := queueModel(t)
	m, _ = press(m, 'f', 0)
	m.width = 20
	m, cmd := press(m, tea.KeyEnter, 0)
	if cmd != nil || m.busy {
		t.Fatal("hidden queue preview submitted")
	}
	m.width = 80
	m.queue = nil
	m, cmd = press(m, 'd', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	m.width = 20
	m, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil || rowsIn(t, st, "tasks") != 1 {
		t.Fatal("hidden delete preview submitted")
	}
}

func TestExternalMoveReconcilesParentAndClearsOldPrivateItemSelection(t *testing.T) {
	m, st := writableModel(t)
	if _, err := st.AddTaskItem(m.ctx, m.taskID, "same"); err != nil {
		t.Fatal(err)
	}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	m, _ = press(m, 'c', 0)
	oldID, oldKey := m.taskID, m.checklist.key
	if oldKey == "" {
		t.Fatal("missing initial key")
	}
	got, err := st.MoveTask(m.ctx, oldID, "home", store.MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd = m.load()
	m = settle(t, m, cmd)
	if m.taskID != got.Id || m.checklist.taskID != got.Id || m.checklist.key != "" {
		t.Fatal("move retargeted old item key", m.taskID, m.checklist)
	}
	m, cmd = press(m, ' ', 0)
	if cmd != nil || !strings.Contains(m.notice, "Select an item first") {
		t.Fatal("old selection edited copied item")
	}
}
