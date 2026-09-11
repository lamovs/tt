package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type RecoveryActions interface {
	RecoveryQueue(context.Context) (store.RecoveryQueue, error)
	RetryQueueEntry(context.Context, store.QueueEntry) error
	PreviewTaskOperation(context.Context, model.Task, string) (store.TaskOperationPreview, error)
	ApplyTaskOperation(context.Context, store.TaskOperationPreview) (store.TaskMutationOutcome, error)
}

type queuePanel struct {
	data                   store.RecoveryQueue
	seq                    int64
	sessionID              string
	focus, detail, loading bool
	lost                   bool
	offset                 int
	confirmation           *store.QueueEntry
	err                    string
}

type queueLoaded struct {
	generation uint64
	data       store.RecoveryQueue
	err        error
}

type queueRefreshDue struct{ generation uint64 }

func (m browserModel) openQueue() (browserModel, tea.Cmd) {
	if _, ok := m.actions.(RecoveryActions); !ok {
		return m, nil
	}
	m.queue = &queuePanel{}
	return m.loadQueue()
}

func (m browserModel) loadQueue() (browserModel, tea.Cmd) {
	actions, ok := m.actions.(RecoveryActions)
	if !ok || m.queue == nil {
		return m, nil
	}
	m.queueGeneration++
	m.queue.loading = true
	generation, ctx := m.queueGeneration, m.ctx
	return m, m.gate.command(func() tea.Msg {
		data, err := actions.RecoveryQueue(ctx)
		return queueLoaded{generation: generation, data: data, err: err}
	})
}

func (m browserModel) finishQueue(msg queueLoaded) (browserModel, tea.Cmd) {
	if m.queue == nil || msg.generation != m.queueGeneration {
		return m, nil
	}
	p := m.queue
	p.loading = false
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	p.err = ""
	p.data = msg.data
	if p.seq != 0 && p.task() == nil {
		p.seq = 0
		p.lost = true
		m.notice = "Queue operation disappeared; select another explicitly."
	}
	if p.sessionID != "" && p.session() == nil {
		p.sessionID = ""
		p.lost = true
		m.notice = "Focus session left the queue; select another explicitly."
	}
	if !p.lost {
		if p.seq == 0 && len(p.data.Tasks) > 0 {
			p.seq = p.data.Tasks[0].Item.Seq
		}
		if p.sessionID == "" && len(p.data.Focus) > 0 {
			p.sessionID = p.data.Focus[0].SessionID
		}
	}
	generation := m.queueGeneration
	return m, tea.Tick(2*time.Second, func(_ time.Time) tea.Msg { return queueRefreshDue{generation} })
}

func (p *queuePanel) task() *store.QueueEntry {
	for i := range p.data.Tasks {
		if p.data.Tasks[i].Item.Seq == p.seq {
			return &p.data.Tasks[i]
		}
	}
	return nil
}

func (p *queuePanel) session() *store.FocusQueueEntry {
	for i := range p.data.Focus {
		if p.data.Focus[i].SessionID == p.sessionID {
			return &p.data.Focus[i]
		}
	}
	return nil
}

func (m browserModel) queueKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.queue, msg.String()
	if s == "?" && p.confirmation == nil {
		m.help.ShowAll = true
		m.help.offset = 0
		return m, nil
	}
	if p.confirmation != nil {
		switch s {
		case "esc":
			p.confirmation = nil
			p.offset = 0
		case "enter":
			expected, actions, ctx := *p.confirmation, m.actions.(RecoveryActions), m.ctx
			return m.actionCommand("retry queue", func() actionFinished { return actionFinished{err: actions.RetryQueueEntry(ctx, expected)} })
		default:
			_, lines := m.queueView()
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(lines))
		}
		return m, nil
	}
	switch s {
	case "q":
		return m, tea.Quit
	case "esc":
		if p.detail {
			p.detail = false
			p.offset = 0
		} else {
			m.queue = nil
			m.queueGeneration++
		}
	case "tab", "shift+tab":
		p.focus = !p.focus
		p.detail = false
		p.offset = 0
	case "r":
		return m.loadQueue()
	case "s":
		return m.beginSync()
	case "u":
		return m.beginUndo()
	case "ctrl+o":
		m.jump = true
	case "enter":
		p.detail = true
		p.offset = 0
	case "f":
		if p.focus {
			m.notice = "Focus uploads are read-only here. Task recovery never retries focus."
			break
		}
		e := p.task()
		if e == nil {
			m.notice = "Select an exact queue operation first."
			break
		}
		if e.Refusal != "" {
			p.detail = true
			p.offset = 0
			m.notice = e.Refusal
			break
		}
		copy := *e
		p.confirmation = &copy
		p.offset = 0
	default:
		if p.detail {
			_, lines := m.queueView()
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(lines))
			break
		}
		index, count := -1, len(p.data.Tasks)
		if p.focus {
			count = len(p.data.Focus)
			for i, e := range p.data.Focus {
				if e.SessionID == p.sessionID {
					index = i
				}
			}
		} else {
			for i, e := range p.data.Tasks {
				if e.Item.Seq == p.seq {
					index = i
				}
			}
		}
		if s != "j" && s != "k" && s != "up" && s != "down" && s != "home" && s != "end" && s != "pgup" && s != "pgdown" {
			return m, nil
		}
		if count > 0 {
			if index < 0 {
				index = 0
			} else {
				index = scroll(index, s, max(1, m.height-8), count)
			}
			if p.focus {
				p.sessionID = p.data.Focus[index].SessionID
			} else {
				p.seq = p.data.Tasks[index].Item.Seq
			}
			p.lost = false
		}
	}
	return m, nil
}

func (m browserModel) queueView() (string, []string) {
	p := m.queue
	title := "Task sync queue"
	if p.focus {
		title = "Focus upload queue (read-only)"
	}
	lines := []string{fmt.Sprintf("Task operations: %d | Focus sessions: %d", len(p.data.Tasks), len(p.data.Focus))}
	add := func(s string) { lines = append(lines, wrapText(s, max(1, m.width-4))...) }
	if p.err != "" {
		add("Queue read failed: " + p.err)
	}
	if p.loading {
		add("Refreshing local queue...")
	}
	if p.confirmation != nil {
		title = "Confirm recovery of 1 task operation"
		add("Enter returns only the operation below to pending. Esc cancels.")
		add("No HTTP or focus upload runs here. Press s separately to sync.")
		m.queueEntryLines(*p.confirmation, add)
		return title, lines
	}
	if p.detail {
		if p.focus {
			e := p.session()
			if e == nil {
				add("Selected session is no longer queued.")
				return title, lines
			}
			add("Session ID: " + e.SessionID)
			add("Task: " + e.Title)
			add("Task ID: " + e.TaskID)
			add("Outcome: " + e.Outcome + " | Upload: " + e.Phase)
			add("Error: " + e.LastError)
			add("Local retained history. Aborted/legacy sessions may be excluded by upload policy.")
			add("This screen never reconciles timers, uploads or rearms a focus request.")
		} else if e := p.task(); e != nil {
			m.queueEntryLines(*e, add)
		} else {
			add("Selected operation is no longer queued.")
		}
		return title, lines
	}
	var rows []string
	selected := 0
	if p.focus {
		for i, e := range p.data.Focus {
			if e.SessionID == p.sessionID {
				selected = i
			}
			rows = append(rows, marker(e.SessionID == p.sessionID)+display(e.Phase+" | "+e.Title+" ["+e.SessionID+"]"))
		}
	} else {
		for i, e := range p.data.Tasks {
			if e.Item.Seq == p.seq {
				selected = i
			}
			state := string(e.Item.State)
			if e.Item.State == store.OutboxFailed {
				state = "failed/parked"
			}
			rows = append(rows, marker(e.Item.Seq == p.seq)+display(fmt.Sprintf("#%d %s %s | %s [%s]", e.Item.Seq, state, e.Item.Op, e.Title, e.Item.TaskID)))
		}
	}
	if len(rows) == 0 {
		add("No queued entries.")
	}
	visible := max(1, m.height-9)
	start := max(0, selected-visible+1)
	lines = append(lines, rows[start:min(len(rows), start+visible)]...)
	return title, lines
}

func (m browserModel) queueEntryLines(e store.QueueEntry, add func(string)) {
	i := e.Item
	add("Operation: #" + strconv.FormatInt(i.Seq, 10) + " " + i.Op + " | " + string(i.State))
	add("Task: " + e.Title)
	add("Task ID: " + i.TaskID)
	add("List ID: " + i.ProjectID)
	add(fmt.Sprintf("Attempts: %d | Created: %s", i.Attempts, i.CreatedAt.Format(time.RFC3339)))
	add("Error: " + i.LastError)
	if e.Refusal != "" {
		add("Unavailable: " + e.Refusal)
	} else {
		add("Recovery: " + e.Recovery)
	}
	add("Version: " + e.Version)
}

func queueHelp() []string {
	return []string{"Sync queue", "Tab: task operations / focus upload history", "j/k or arrows: select by operation sequence or session ID", "Enter: full target, state, error and recovery details", "f: preview recovery of 1 selected task operation; Enter confirms, Esc cancels", "Uncertain allocating requests require existing read-only confirmation evidence; no blind repeat POST", "Recovery only changes the exact approved queue entry. New rows never join the selection", "Focus upload is read-only here; task recovery does not upload or rearm focus", "s: explicit sync; u: global undo; r: refresh local queue", "Ctrl+O: return to browser panels; Esc: back", "drop-parked and project-drop are not exposed"}
}
