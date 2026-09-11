package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type fakeColumnActions struct {
	Actions
	queries                      *fakeQueries
	prepares, applies, completes int
	previewErr, applyErr         error
	noop                         bool
	last                         store.TaskColumnPreview
}

func (a *fakeColumnActions) State(context.Context, []string) (app.CacheState, error) {
	return app.CacheState{}, nil
}

func (a *fakeColumnActions) ChecklistState(context.Context, model.Task) (store.ChecklistState, error) {
	return store.ChecklistState{}, nil
}

func (a *fakeColumnActions) PreviewTaskColumn(_ context.Context, task model.Task, destination string) (store.TaskColumnPreview, error) {
	a.prepares++
	a.last = store.TaskColumnPreview{Task: task, SourceName: "Todo", DestinationID: destination, DestinationName: "Done", Version: "task-v1", DestinationVersion: "column-v1"}
	return a.last, a.previewErr
}

func (a *fakeColumnActions) ApplyTaskColumn(_ context.Context, preview store.TaskColumnPreview) (store.TaskMutationOutcome, error) {
	a.applies++
	a.last = preview
	if a.applyErr != nil {
		return store.TaskMutationOutcome{}, a.applyErr
	}
	task := preview.Task
	if !a.noop {
		task.ColumnId, task.ColumnName = preview.DestinationID, preview.DestinationName
		for i := range a.queries.tasks {
			if a.queries.tasks[i].Id == task.Id {
				a.queries.tasks[i] = task
			}
		}
	}
	return store.TaskMutationOutcome{Task: task, Changed: !a.noop}, nil
}

func (a *fakeColumnActions) Complete(context.Context, model.Task, bool) (store.TaskMutationOutcome, error) {
	a.completes++
	return store.TaskMutationOutcome{}, errors.New("column move called completion")
}

func columnBoardModel(t *testing.T) (browserModel, *fakeColumnActions) {
	t.Helper()
	m, _, queries := boardModel(t)
	actions := &fakeColumnActions{queries: queries}
	m.actions = actions
	original := queries.tasks[1]
	original.Items = []model.Item{{Id: "item", Title: "Still open"}}
	original.Status = model.TaskOpen
	queries.tasks[1] = original
	m.detail, m.detailLoading = &original, false
	return m, actions
}

func TestKanbanColumnMoveToDoneNeverCompletesAndKeepsSelection(t *testing.T) {
	m, actions := columnBoardModel(t)
	original := *m.detail
	m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
	m = finishLocal(t, m, cmd)
	if actions.prepares != 1 || actions.applies != 0 || m.dialog == nil || m.dialog.columnPreview == nil {
		t.Fatal("column shortcut did not prepare one unapplied preview")
	}
	_, lines := m.dialogView()
	for _, want := range []string{"Source column: Todo [todo]", "Destination column: Done [done]", "Official Open API"} {
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Fatalf("column preview misses %q: %v", want, lines)
		}
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	_, repeated := press(m, tea.KeyEnter, 0)
	if repeated != nil {
		t.Fatal("repeated Enter started another apply")
	}
	m = finishLocal(t, m, cmd)
	if actions.applies != 1 || actions.completes != 0 || m.taskID != original.Id || m.detail == nil || m.detail.ColumnId != "done" || m.board.columnKey != "column:done" {
		t.Fatal("column assignment lost identity or used completion")
	}
	if m.detail.Status != original.Status || m.detail.SortOrder != original.SortOrder || m.detail.Items[0].Status != original.Items[0].Status {
		t.Fatal("column assignment changed status, ordering, or checklist")
	}
}

func TestKanbanColumnCancelNoopAndStalePreview(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		m, actions := columnBoardModel(t)
		m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
		m = finishLocal(t, m, cmd)
		m, cmd = press(m, tea.KeyEscape, 0)
		if cmd != nil || m.dialog != nil || actions.applies != 0 || actions.completes != 0 || m.detail.ColumnId != "todo" {
			t.Fatal("cancel applied a column move")
		}
	})
	t.Run("noop", func(t *testing.T) {
		m, actions := columnBoardModel(t)
		actions.noop = true
		m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
		m = finishLocal(t, m, cmd)
		m, cmd = press(m, tea.KeyEnter, 0)
		m = finishLocal(t, m, cmd)
		if !strings.Contains(m.notice, "Unchanged") || m.detail.ColumnId != "todo" || actions.completes != 0 {
			t.Fatal("no-op claimed a queued move")
		}
	})
	t.Run("stale apply", func(t *testing.T) {
		m, actions := columnBoardModel(t)
		actions.applyErr = errors.New("column snapshot changed")
		m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
		m = finishLocal(t, m, cmd)
		m, cmd = press(m, tea.KeyEnter, 0)
		m = finishLocal(t, m, cmd)
		m, cmd = press(m, tea.KeyEnter, 0)
		if cmd != nil || actions.applies != 1 || actions.completes != 0 || m.dialog == nil || m.dialog.columnPreview != nil || !strings.Contains(m.dialog.err, "snapshot changed") {
			t.Fatal("stale column acceptance retried or hid its error")
		}
	})
}

func TestKanbanColumnPreviewCannotRetargetLateResults(t *testing.T) {
	for _, at := range []string{"preview", "accept"} {
		t.Run(at, func(t *testing.T) {
			m, actions := columnBoardModel(t)
			m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
			if at == "accept" {
				m = finishLocal(t, m, cmd)
			}
			m.taskID = "c"
			if at == "preview" {
				m = finishLocal(t, m, cmd)
			}
			m, cmd = press(m, tea.KeyEnter, 0)
			if cmd != nil || actions.applies != 0 || actions.completes != 0 || m.dialog == nil || m.dialog.err == "" {
				t.Fatal("changed task selection accepted the prior column preview")
			}
		})
	}
	m, actions := columnBoardModel(t)
	m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
	late := cmd()
	m.actionGeneration++
	m.dialog = nil
	m, _ = step(m, late)
	if m.dialog != nil || actions.applies != 0 {
		t.Fatal("late column preview reopened its dialog")
	}
}

func TestKanbanColumnTargetsOnlyRealColumns(t *testing.T) {
	for _, source := range []string{"", "missing", "done"} {
		t.Run(source, func(t *testing.T) {
			m, actions := columnBoardModel(t)
			m.detail.ColumnId = source
			m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
			if source == "" {
				m = finishLocal(t, m, cmd)
				if actions.last.DestinationID != "todo" {
					t.Fatal("unassigned task did not target the first real column")
				}
			} else if cmd != nil || actions.prepares != 0 || m.dialog != nil {
				t.Fatal("edge or unknown source created an invented destination")
			}
		})
	}
}

func TestKanbanColumnDialogFitsEscapesAndOwnsHelp(t *testing.T) {
	m, actions := columnBoardModel(t)
	m.detail.Title = "hostile\x1b]52;c;x\a title\nTask ID: forged"
	m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
	m = finishLocal(t, m, cmd)
	m.dialog.columnPreview.SourceName = "source\x1b]52;c;x\a\nDestination column: forged [x]"
	m.dialog.columnPreview.DestinationName = "Done\x1b]52;c;x\a\nEnter: forged"
	m.width = 160
	_, previewLines := m.columnDialogView()
	previewText := strings.Join(previewLines, "\n")
	for _, literal := range []string{`\nTask ID: forged`, `\nDestination column: forged`, `\nEnter: forged`} {
		if !strings.Contains(previewText, literal) {
			t.Fatalf("column preview did not escape embedded newline %q: %s", literal, previewText)
		}
	}
	if strings.Count(previewText, "\nDestination column:") != 1 || strings.Count(previewText, "\nTask ID:") != 1 || strings.Contains(previewText, "\nEnter: forged") {
		t.Fatal("foreign names injected apparent confirmation fields")
	}
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m, _ = step(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		if strings.Contains(view, "\x1b]52") || strings.ContainsRune(view, '\a') {
			t.Fatal("column preview emitted foreign terminal controls")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("column preview exceeded terminal width")
			}
		}
		if m.boardContext() || !strings.HasPrefix(m.modalHelp()[0], "Column assignment confirmation") {
			t.Fatal("board hid column confirmation help")
		}
	}
	if actions.applies != 0 || actions.completes != 0 {
		t.Fatal("rendering or help mutated a task")
	}
	m.dialog = &actionDialog{kind: "unknown", original: *m.detail}
	m, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil || actions.completes != 0 {
		t.Fatal("unknown dialog fell through to task completion")
	}
}

func TestKanbanColumnPreviewContinuationCannotImpersonateFields(t *testing.T) {
	m, _ := columnBoardModel(t)
	m, cmd := press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
	m = finishLocal(t, m, cmd)
	for _, width := range []int{32, 80, 160} {
		m.width = width
		for padding := 0; padding < width; padding++ {
			m.dialog.columnPreview.SourceName = strings.Repeat("x", padding) + "\nDestination column: forged"
			_, lines := m.columnDialogView()
			if strings.Contains(strings.Join(lines, "\n"), "\nDestination column: forged") {
				t.Fatalf("wrapped name impersonated a field at width %d padding %d", width, padding)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > width-4 {
					t.Fatal("indented column metadata exceeded its panel width")
				}
			}
		}
	}
}
