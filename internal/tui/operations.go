package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

type Actions interface {
	State(context.Context, []string) (app.CacheState, error)
	ChecklistState(context.Context, model.Task) (store.ChecklistState, error)
	Checklist(context.Context, model.Task, store.ItemChange) (store.TaskMutationOutcome, error)
	Document(context.Context, app.DocumentRequest, io.Reader, io.Writer, io.Writer) (app.DocumentResult, error)
	Schedule(context.Context, model.Task, schedule.Change) (store.TaskMutationOutcome, error)
	Create(context.Context, app.Draft) (store.TaskMutationOutcome, error)
	Edit(context.Context, model.Task, app.Draft) (store.TaskMutationOutcome, error)
	Complete(context.Context, model.Task, bool) (store.TaskMutationOutcome, error)
	PreviewUndo(context.Context) (app.UndoPreview, error)
	Undo(context.Context, app.UndoPreview) (store.TaskMutationOutcome, error)
	Sync(context.Context, func(string)) app.SyncOutcome
}

type commandGate struct {
	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup
}

func (g *commandGate) command(fn tea.Cmd) tea.Cmd {
	if g == nil {
		return fn
	}
	return func() tea.Msg {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil
		}
		g.running.Add(1)
		g.mu.Unlock()
		defer g.running.Done()
		return fn()
	}
}
func (g *commandGate) close(cancel context.CancelFunc) {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	cancel()
	g.running.Wait()
}

type actionFinished struct {
	intervalPreview store.IntervalPreview
	generation      uint64
	kind            string
	outcome         store.TaskMutationOutcome
	undo            app.UndoPreview
	taskPreview     store.TaskOperationPreview
	columnPreview   store.TaskColumnPreview
	err             error
}

type actionDialog struct {
	kind              string
	original          model.Task
	undo              app.UndoPreview
	keepItems         bool
	offset            int
	columnDestination string
	columnPreview     *store.TaskColumnPreview
	err               string
}

func (m browserModel) actionCommand(kind string, fn func() actionFinished) (browserModel, tea.Cmd) {
	m.actionGeneration++
	m.busy = true
	generation := m.actionGeneration
	return m, m.gate.command(func() tea.Msg {
		msg := fn()
		msg.kind, msg.generation = kind, generation
		return msg
	})
}

func (m browserModel) beginUndo() (browserModel, tea.Cmd) {
	m.dialog = &actionDialog{kind: "undo"}
	m.notice = "Reading global undo history..."
	actions, ctx := m.actions, m.ctx
	return m.actionCommand("preview undo", func() actionFinished {
		preview, err := actions.PreviewUndo(ctx)
		return actionFinished{undo: preview, err: err}
	})
}

func (m browserModel) confirmAction() (browserModel, tea.Cmd) {
	if m.dialog == nil {
		return m, nil
	}
	dialog, actions, ctx := *m.dialog, m.actions, m.ctx
	if dialog.kind == "column move" {
		columns, ok := actions.(ColumnActions)
		if !ok || dialog.columnPreview == nil || dialog.err != "" {
			return m, nil
		}
		if !m.columnDialogCurrent() {
			m.dialog.err = "The selected task or project changed."
			m.dialog.columnPreview = nil
			return m, nil
		}
		preview := *dialog.columnPreview
		return m.actionCommand("apply column move", func() actionFinished {
			out, err := columns.ApplyTaskColumn(ctx, preview)
			return actionFinished{outcome: out, err: err}
		})
	}
	if dialog.kind == "undo" {
		return m.actionCommand("undo", func() actionFinished {
			out, err := actions.Undo(ctx, dialog.undo)
			return actionFinished{outcome: out, undo: dialog.undo, err: err}
		})
	}
	if dialog.kind != "complete" {
		m.dialog, m.notice = nil, "Unknown confirmation; no task changes."
		return m, nil
	}
	return m.actionCommand("complete", func() actionFinished {
		out, err := actions.Complete(ctx, dialog.original, dialog.keepItems)
		return actionFinished{outcome: out, err: err}
	})
}

func (m browserModel) finishAction(msg actionFinished) (browserModel, tea.Cmd) {
	if msg.generation != m.actionGeneration || !m.busy {
		return m, nil
	}
	m.busy = false
	if msg.kind == "preview column move" {
		if m.dialog == nil || m.dialog.kind != "column move" {
			return m, nil
		}
		if !m.columnDialogCurrent() {
			m.dialog.err = "The selected task or project changed."
		} else if msg.err != nil {
			m.dialog.err = msg.err.Error()
		} else if msg.columnPreview.Task.Id != m.dialog.original.Id || msg.columnPreview.Task.ProjectId != m.dialog.original.ProjectId || msg.columnPreview.DestinationID != m.dialog.columnDestination {
			m.dialog.err = "The column preview does not match the selected task and destination."
		} else {
			m.dialog.columnPreview = &msg.columnPreview
		}
		return m, nil
	}
	if msg.kind == "apply column move" {
		if m.dialog == nil || m.dialog.kind != "column move" {
			return m, nil
		}
		if msg.err != nil {
			m.dialog.err, m.dialog.columnPreview = msg.err.Error(), nil
			return m, nil
		}
		m.dialog = nil
		m.notice = "Unchanged; no column assignment queued."
		if msg.outcome.Changed {
			m.notice = "Column assignment queued locally. Press s to sync; global undo checks send state."
		}
		return m.load()
	}
	if msg.kind == "preview interval" {
		if m.form == nil || m.form.interval == nil {
			return m, nil
		}
		if msg.err != nil {
			m.form.err = msg.err.Error()
		} else {
			m.form.interval.review, m.form.interval.offset = &msg.intervalPreview, 0
		}
		return m, nil
	}
	if msg.kind == "apply interval" && msg.err != nil && m.form != nil && m.form.interval != nil {
		m.form.interval.review = nil
	}
	if msg.kind == "preview task operation" {
		if m.taskOperation == nil {
			return m, nil
		}
		if msg.err != nil {
			m.taskOperation.err = msg.err.Error()
		} else {
			m.taskOperation.preview = &msg.taskPreview
		}
		return m, nil
	}
	if msg.kind == "apply task operation" {
		if m.taskOperation == nil {
			return m, nil
		}
		if msg.err != nil {
			m.taskOperation.err = msg.err.Error()
			m.taskOperation.preview = nil
			return m, nil
		}
		if m.taskOperation.move {
			m.notice = "Native move queued with unchanged task ID: " + msg.outcome.Task.Id + ". Sync is separate; global undo checks send state."
		} else {
			m.notice = "Deleted 1 task locally. Any queued remote deletion still requires sync."
		}
		m.taskOperation = nil
		return m.load()
	}
	if msg.kind == "retry queue" {
		if m.queue == nil {
			return m, nil
		}
		m.queue.confirmation = nil
		m.queue.offset = 0
		if msg.err != nil {
			m.notice = "Recovery refused: " + msg.err.Error()
		} else {
			m.notice = "Returned 1 task operation to pending. Press s to sync. Focus upload unchanged."
		}
		next, cmd := m.loadQueue()
		next, load := next.load()
		return next, tea.Batch(cmd, load)
	}
	if msg.kind == "checklist" {
		return m.finishChecklist(msg)
	}
	if msg.err != nil {
		if errors.Is(msg.err, store.ErrNoUndo) {
			m.dialog, m.notice = nil, "Nothing to undo."
			return m, nil
		}
		m.notice = "Action failed: " + msg.err.Error()
		if m.form != nil {
			m.form.err = msg.err.Error()
			m.form.quitAfter = false
		}
		m.dialog = nil
		return m, nil
	}
	if msg.kind == "preview undo" {
		m.dialog.undo = msg.undo
		m.notice = "Global undo: confirm the action shown below."
		return m, nil
	}
	quit := m.form != nil && m.form.quitAfter
	m.form, m.dialog = nil, nil
	m.notice = "Unchanged; no local mutation."
	if msg.outcome.Changed {
		m.notice = fmt.Sprintf("Saved locally (%s). Remote confirmation requires sync.", msg.kind)
	}
	if msg.kind == "create" {
		m.notice += " Created: " + msg.outcome.Task.Title + " [" + msg.outcome.Task.Id + "]."

	}
	if msg.kind == "undo" {
		if len(msg.undo.Group) != 0 {
			m.notice += fmt.Sprintf(" Global history applied to %d grouped operations.", len(msg.undo.Group))
		} else if msg.undo.Entry.Action.EntityRef != nil {
			m.notice = "Canceled the unsent resource operation from global history. No remote request was made."
		} else {
			m.notice += " Global history applied to " + msg.outcome.Task.Title + "."
		}
	}
	if quit {
		return m, tea.Quit
	}
	return m.load()
}

func (m browserModel) mutationKey(s string) (browserModel, tea.Cmd, bool) {
	if m.actions == nil {
		return m, nil, false
	}
	switch s {
	case "t":
		if m.timers != nil {
			m.timer = &timerPanel{report: m.timerReport}
			next, cmd := m.loadTimer()
			return next, cmd, true
		}
		return m, nil, true
	case "Q", "shift+q":
		next, cmd := m.openQueue()
		return next, cmd, true
	case "a", "n":
		m.openCreate()
		if s == "n" {
			m.openCreateKind("NOTE")
		}
		return m, nil, true
	case "c", "e", "E", "shift+e", "d", "space", "ctrl+d", "m":
		if m.detailLoading || m.detail == nil || m.detail.Id != m.taskID {
			m.notice = "Wait for the selected task details before changing it."
			return m, nil, true
		}
		if s == "ctrl+d" || s == "m" {
			next, cmd := m.beginTaskOperation(s == "m")
			return next, cmd, true
		}
		if s == "E" || s == "shift+e" {
			next, cmd := m.beginEditor()
			return next, cmd, true
		}
		if s == "c" {
			m.openChecklist()
			return m, nil, true
		}
		if s == "d" {
			m.openSchedule(*m.detail)
			return m, nil, true
		}
		if s == "e" {
			m.openEdit(*m.detail)
			return m, nil, true
		}
		if m.detail.Status.Done() {
			m.notice = "Already completed; nothing changed. Use global undo to reverse the last action."
			return m, nil, true
		}
		m.dialog = &actionDialog{kind: "complete", original: *m.detail}
		return m, nil, true
	case "u":
		next, cmd := m.beginUndo()
		return next, cmd, true
	case "s":
		next, cmd := m.beginSync()
		return next, cmd, true
	}
	return m, nil, false
}
