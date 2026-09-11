package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
)

type syncPanel struct {
	generation         uint64
	running, canceling bool
	phase              string
	result             app.SyncOutcome
	offset             int
	events             chan string
	context            context.Context
	cancel             context.CancelFunc
}
type syncProgress struct {
	generation uint64
	phase      string
}
type syncFinished struct {
	generation uint64
	result     app.SyncOutcome
}

func (m browserModel) beginSync() (browserModel, tea.Cmd) {
	m.syncGeneration++
	ctx, cancel := context.WithCancel(m.ctx)
	p := &syncPanel{generation: m.syncGeneration, running: true, phase: "Starting explicit sync", events: make(chan string, 4), context: ctx, cancel: cancel}
	m.sync = p
	actions, generation := m.actions, p.generation
	run := m.gate.command(func() tea.Msg {
		defer close(p.events)
		result := actions.Sync(ctx, func(phase string) {
			select {
			case p.events <- phase:
			case <-ctx.Done():
			}
		})
		return syncFinished{generation: generation, result: result}
	})
	return m, tea.Batch(run, p.progressCommand())
}
func (p *syncPanel) progressCommand() tea.Cmd {
	generation, events, ctx := p.generation, p.events, p.context
	return func() tea.Msg {
		select {
		case phase, ok := <-events:
			if ok {
				return syncProgress{generation: generation, phase: phase}
			}
		case <-ctx.Done():
		}
		return nil
	}
}
func (p *syncPanel) summary() string {
	if p.running {
		if p.canceling {
			return "Canceling sync; waiting for recorded outcomes..."
		}
		return p.phase + "... Esc cancels"
	}
	r := p.result
	status := "Sync pass finished"
	if r.Err != nil || r.Tasks.ErrorCount() > 0 || len(r.Focus.Errors) > 0 || r.Resources.Failed > 0 || len(r.Resources.Errors) > 0 || r.RefreshFailed() {
		status = "Sync failed or partially completed"
	}
	if r.Canceled {
		status = "Sync canceled; recorded outcomes retained"
	}
	return status
}
func (m browserModel) syncLines() []string {
	p, width := m.sync, max(1, m.width-4)
	lines := wrapText(p.summary(), width)
	if p.running {
		return append(lines, wrapText("Only server-confirmed outcomes count as sent. Canceling does not undo accepted work.", width)...)
	}
	r := p.result
	add := func(s string) { lines = append(lines, wrapText(s, width)...) }
	add(fmt.Sprintf("Remote confirmed: %d task operations", r.Tasks.Pushed))
	add(fmt.Sprintf("Requeued: %d | Newly held: %d | Not sent: %d", r.Tasks.Requeued, r.Tasks.Failed, r.Tasks.UnsentCount()))
	add(fmt.Sprintf("Cache: %d pulled | %d skipped | %d removed | %d kept", r.Tasks.Pulled, r.Tasks.Skipped, r.Tasks.Deleted, r.Tasks.Kept))
	if r.Tasks.ParkedUnsent > 0 {
		add(fmt.Sprintf("%d earlier uncertain creates held without sending", r.Tasks.ParkedUnsent))
	}
	if r.Err != nil {
		add("Error: " + r.Err.Error())
	}
	for _, err := range r.Tasks.Errors {
		add("Error: " + err.Error())
	}
	for _, err := range r.Tasks.Unsent {
		add("Not sent: " + err.Error())
	}
	for _, warning := range r.Tasks.Warnings {
		add(fmt.Sprintf("Warning: %v", warning))
	}
	if r.FocusAttempted {
		add(fmt.Sprintf("Focus separately: uploaded %d | held %d", r.Focus.Uploaded, r.Focus.Held))
		for _, err := range r.Focus.Errors {
			add("Focus error: " + err.Error())
		}
	}
	add(fmt.Sprintf("Resources separately: confirmed %d | failed or held %d", r.Resources.Confirmed, r.Resources.Failed))
	for _, err := range r.Resources.Errors {
		add("Resource error: " + err)
	}
	for _, refresh := range r.Refreshes {
		add(refresh.Summary())
	}
	if r.FocusSkipped != "" {
		add("Focus: " + r.FocusSkipped)
	} else if !r.FocusAttempted && r.Err == nil {
		add("Focus: no sessions waiting for upload")
	}
	if m.stateErr != nil {
		add("Queue status unavailable: " + m.stateErr.Error())
	} else if m.stateKnown {
		q := m.cacheState.Queue
		add(fmt.Sprintf("Queue now: pending %d | in flight %d | held %d | unknown %d", q.Pending, q.Inflight, q.Failed, q.Unknown))
	}
	add("A finished pass does not mean the queue is empty. Held work is retained; Q opens the queue after closing this report.")
	return lines
}
func (m browserModel) syncKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p := m.sync
	if msg.String() == "esc" {
		if p.running {
			p.canceling = true
			p.cancel()
		} else {
			p.cancel()
			m.sync = nil
		}
	} else if !p.running {
		rows, count := max(1, m.height-6), len(m.syncLines())
		p.offset = scroll(min(p.offset, max(0, count-rows)), msg.String(), rows, count)
	}
	return m, nil
}
