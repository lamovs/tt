package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type ColumnActions interface {
	PreviewTaskColumn(context.Context, model.Task, string) (store.TaskColumnPreview, error)
	ApplyTaskColumn(context.Context, store.TaskColumnPreview) (store.TaskMutationOutcome, error)
}

func (m browserModel) beginColumnMove(direction int) (browserModel, tea.Cmd) {
	actions, ok := m.actions.(ColumnActions)
	if !ok {
		m.notice = "Column assignment is unavailable in this session."
		return m, nil
	}
	if m.board == nil || m.board.loading || m.board.err != "" {
		m.notice = "Refresh the project columns before moving a card."
		return m, nil
	}
	if m.detailLoading || m.detail == nil || m.detail.Id != m.taskID || m.detail.ProjectId != m.board.projectID {
		m.notice = "Wait for the selected task details before moving it."
		return m, nil
	}
	original := *m.detail
	index := -1
	for i, column := range m.board.columns {
		if column.id == original.ColumnId {
			index = i
			break
		}
	}
	if index < 0 && original.ColumnId != "" {
		m.notice = "The source column is unknown; refresh columns or choose an exact destination with tt edit --column."
		return m, nil
	}
	target := index + direction
	if original.ColumnId == "" {
		target = 0
		if direction < 0 {
			target = len(m.board.columns) - 1
		}
	}
	if target < 0 || target >= len(m.board.columns) {
		m.notice = "No adjacent destination column."
		return m, nil
	}
	destination := m.board.columns[target].id
	m.dialog = &actionDialog{kind: "column move", original: original, columnDestination: destination}
	ctx := m.ctx
	return m.actionCommand("preview column move", func() actionFinished {
		preview, err := actions.PreviewTaskColumn(ctx, original, destination)
		return actionFinished{columnPreview: preview, err: err}
	})
}

func (m browserModel) columnDialogCurrent() bool {
	d := m.dialog
	return d != nil && d.kind == "column move" && m.board != nil &&
		m.board.projectID == d.original.ProjectId && m.query.ProjectID == d.original.ProjectId &&
		m.taskID == d.original.Id
}

func (m browserModel) columnDialogView() (string, []string) {
	d := m.dialog
	var lines []string
	add := func(s string) {
		for i, line := range strings.Split(ansi.Hardwrap(display(s), max(1, m.width-6), true), "\n") {
			if i != 0 {
				line = "  " + line
			}
			lines = append(lines, line)
		}
	}
	add("Task: " + d.original.Title)
	add("Task ID: " + d.original.Id)
	if d.err != "" {
		add("Not applied: " + d.err)
		add("Esc closes. Reopen the action to prepare a current preview.")
		return "Move card to column", lines
	}
	if d.columnPreview == nil {
		add("Reading the exact column assignment preview...")
		return "Move card to column", lines
	}
	p := d.columnPreview
	add("Project ID: " + p.Task.ProjectId)
	add("Source column: " + p.SourceName + " [" + p.Task.ColumnId + "]")
	add("Destination column: " + p.DestinationName + " [" + p.DestinationID + "]")
	add("Official Open API (column write live-verified, undocumented)")
	add("Queues one column assignment. Task status and ordering stay unchanged.")
	add("Sync is separate. Global undo checks send state.")
	add("Enter applies this exact preview; Esc cancels. Scroll to read all details.")
	return "Move card to column", lines
}
