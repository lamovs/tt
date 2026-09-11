package tui

import (
	"context"
	"fmt"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
)

type draftJournal struct {
	mu    sync.Mutex
	paths []string
}

func (j *draftJournal) add(path string) {
	if j == nil || path == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.paths = append(j.paths, path)
}

func (j *draftJournal) write(w io.Writer) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, path := range j.paths {
		fmt.Fprintln(w, "Draft kept at")
		fmt.Fprintln(w, cli.Foreign(path, 4*len(path)+2))
	}
}

type editorFinished struct {
	generation uint64
	result     app.DocumentResult
	err        error
	ran        bool
}

type editorCommand struct {
	once               sync.Once
	gate               *commandGate
	drafts             *draftJournal
	actions            Actions
	ctx                context.Context
	request            app.DocumentRequest
	input              io.Reader
	output, diagnostic io.Writer
	finished           editorFinished
}

func (c *editorCommand) Run() error {
	c.once.Do(func() {
		c.gate.command(func() tea.Msg {
			c.finished.ran = true
			c.finished.result, c.finished.err = c.actions.Document(c.ctx, c.request, c.input, c.output, c.diagnostic)
			c.drafts.add(c.finished.result.DraftPath)
			return nil
		})()
	})

	return nil
}

func (m browserModel) beginEditor() (browserModel, tea.Cmd) {
	if m.timerWorking {
		m.notice = "Wait for the focus operation to finish before opening the editor."
		return m, nil
	}
	if m.busy || m.actions == nil || !m.usable() {
		return m, nil
	}
	var request app.DocumentRequest
	if f := m.form; f != nil {
		if f.schedule != nil || f.discard {
			return m, nil
		}
		if f.interval != nil && f.interval.active() {
			f.err = "Save the interval through Ctrl+S review before using the Markdown editor"
			return m, nil
		}
		if f.quickError != "" {
			f.err = f.quickError
			return m, nil
		}
		d := f.draft()
		if d.ProjectID == "" {
			f.err = "Choose a cached list before opening the editor."
			return m, nil
		}
		request.Original = f.original
		if f.original != nil {
			request.Seed = *f.original
		} else {
			request.Seed = model.Task{ProjectId: d.ProjectID, Kind: d.Kind}
			if d.ReminderWhen != "" {
				request.Seed.DueDate = d.ReminderDate.Time
				request.Seed.Reminders = []string{"TRIGGER:PT0S"}
			}
		}
		request.Seed.Title, request.Seed.Content, request.Seed.Priority = d.Title, d.Body, d.Priority
		f.err = ""
	} else {
		if m.detailLoading || m.detail == nil || m.detail.Id != m.taskID {
			m.notice = "Wait for the selected task details before opening the editor."
			return m, nil
		}
		original := *m.detail
		request = app.DocumentRequest{Original: &original, Seed: original}
	}
	m.actionGeneration++
	m.generation++
	m.detailGeneration++
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	if m.cancelDetail != nil {
		m.cancelDetail()
	}
	m.loading, m.busy = false, true
	c := &editorCommand{gate: m.gate, drafts: m.drafts, actions: m.actions, ctx: m.ctx, request: request,
		finished: editorFinished{generation: m.actionGeneration}}
	m.editor = c
	return m, tea.Quit
}

type recoveryPanel struct {
	message, path string
	offset        int
}

func (m browserModel) recoveryLines() []string {
	r := m.recovery
	lines := wrapText(r.message, max(1, m.width-4))
	if r.path != "" {
		lines = append(lines, "", "Draft kept at:")
		lines = append(lines, wrapText(r.path, max(1, m.width-4))...)
		lines = append(lines, "")
		lines = append(lines, wrapText("Reopen the task and copy the intended edits from this draft.\nThe complete recovery path also prints when tt ui exits.", max(1, m.width-4))...)
	}
	return lines
}

func (m browserModel) finishEditor(msg editorFinished) (browserModel, tea.Cmd) {
	if !m.busy || msg.generation != m.actionGeneration {
		return m, nil
	}
	m.busy = false
	m.editor = nil
	m.timerGeneration++
	m.timerTickGeneration++
	m.timerEnsured = ""
	failed := msg.err != nil || !msg.ran
	if failed {
		m.notice = "Editor did not save a local change."
		if msg.err != nil {
			m.notice += " " + msg.err.Error()
		}
		if m.form != nil {
			m.form.err = m.notice
			m.form.quitAfter = false
		}
	} else if msg.result.Canceled {
		m.notice = "Unchanged editor document; no local mutation."
	} else {
		m.form = nil
		m.notice = "Unchanged; no local mutation."
		if msg.result.Outcome.Changed {
			m.notice = "Saved locally (editor). Remote confirmation requires sync."
			task := msg.result.Outcome.Task
			m.notice += " " + task.Title + " [" + task.Id + "]."
		}
	}
	if msg.result.CleanupError != nil {
		m.notice += " Could not remove editor draft."
	}
	if failed || msg.result.DraftPath != "" {
		m.recovery = &recoveryPanel{message: m.notice, path: msg.result.DraftPath}
	}
	return m.load()
}
