package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
)

func workspaceStatus(t *testing.T, m browserModel) string {
	t.Helper()
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) < 2 {
		t.Fatal("missing status line")
	}
	return strings.TrimSpace(ansi.Strip(lines[1]))
}

func TestKanbanDoesNotHideActionRefusalValidationOrSyncStatus(t *testing.T) {
	m, _, _ := boardModel(t)
	m.width = 160
	m, _, _ = m.kanbanKey(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt | tea.ModShift})
	if !strings.Contains(workspaceStatus(t, m), "unavailable in this session") {
		t.Fatal("column move refusal is not visible")
	}
	m.notice = ""
	m.form = &taskForm{kind: "TEXT", project: -1, err: "exact validation failure"}
	if !strings.Contains(workspaceStatus(t, m), "Not saved: exact validation failure") {
		t.Fatal("board hid form validation")
	}
	m.form = nil
	m.busy = true
	if !strings.Contains(workspaceStatus(t, m), "Saving local operation") {
		t.Fatal("board hid busy state")
	}
	m.busy = false
	m.sync = &syncPanel{result: app.SyncOutcome{Resources: app.ResourceSyncResult{Failed: 1}}}
	if !strings.Contains(workspaceStatus(t, m), "Sync failed or partially completed") {
		t.Fatal("board hid partial sync status")
	}
	m.sync = nil
	m.notice, m.board.err = "", "column read failed"
	if !strings.Contains(workspaceStatus(t, m), "column read failed") {
		t.Fatal("board error disappeared without a higher-priority context")
	}
}

func TestKanbanHelpYieldsToEveryOpenedModal(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*browserModel)
		want      string
	}{
		{"task form", func(m *browserModel) { m.form = &taskForm{} }, "Task/note form"},
		{"dirty form", func(m *browserModel) { m.form = &taskForm{discard: true} }, "Unsaved changes"},
		{"checklist", func(m *browserModel) { m.checklist = &checklistPanel{form: &itemForm{}} }, "Checklist title input"},
		{"task confirmation", func(m *browserModel) { m.dialog = &actionDialog{} }, "Task confirmation"},
		{"task move", func(m *browserModel) { m.taskOperation = &taskOperationPanel{} }, "Delete/move confirmation"},
		{"sync", func(m *browserModel) { m.sync = &syncPanel{} }, "Explicit sync"},
		{"settings", func(m *browserModel) { m.systemPanel = &systemPanel{preview: &SystemPreview{}} }, "Settings and diagnostics"},
		{"search", func(m *browserModel) { m.searching = true }, "Search input"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			m, _, _ := boardModel(t)
			test.configure(&m)
			m, _ = press(m, tea.KeyF1, 0)
			lines := m.modalHelp()
			if len(lines) == 0 || !strings.HasPrefix(lines[0], test.want) {
				t.Fatalf("wrong help context: %v", lines)
			}
			if m.boardContext() {
				t.Fatal("modal retained board context")
			}
		})
	}
	m, _, _ := boardModel(t)
	m, _ = press(m, tea.KeyF1, 0)
	if lines := m.modalHelp(); len(lines) == 0 || lines[0] != "Kanban board" {
		t.Fatalf("board help itself was lost: %v", lines)
	}
}

func TestResourceActionNoticeIsVisibleUntilAnErrorOrActiveOperation(t *testing.T) {
	m, _ := resourceModel(t, "folder")
	m.width = 160
	m.notice = "Queued folder operation 17 locally. Sync sends it separately."
	if !strings.Contains(workspaceStatus(t, m), "Queued folder operation 17") {
		t.Fatal("resource status hid successful enqueue")
	}
	m.resource.err = "exact preview became stale"
	if !strings.Contains(workspaceStatus(t, m), "exact preview became stale") {
		t.Fatal("old notice hid resource error")
	}
	m.resource.err = ""
	m.resource.working = true
	if !strings.Contains(workspaceStatus(t, m), "Preparing or applying") {
		t.Fatal("old notice hid active resource operation")
	}

	focus, _ := focusRecordModel(t)
	focus.width = 160
	focus.notice = "Completed focus record saved locally: session. Open Local focus timer to upload, or use tt timer sync."
	if !strings.Contains(workspaceStatus(t, focus), "Local focus timer to upload") {
		t.Fatal("manual-focus upload guidance is not visible")
	}
}
