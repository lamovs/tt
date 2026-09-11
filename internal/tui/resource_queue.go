package tui

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/store"
)

type ResourceQueueActions interface {
	Queue(context.Context) ([]store.EntityOperation, error)
	Cancel(context.Context, int64, int64) error
	Recover(context.Context, int64, int64) error
}

type resourceQueuePanel struct {
	operations            []store.EntityOperation
	seq                   int64
	loading, lost, detail bool
	offset                int
	confirmation          *resourceQueueConfirmation
}

type resourceQueueConfirmation struct {
	seq, revision            int64
	action, kind, key, phase string
}

type resourceQueueLoaded struct {
	generation uint64
	operations []store.EntityOperation
	err        error
}
type resourceQueueFinished struct {
	generation    uint64
	seq, revision int64
	action        string
	err           error
}

func (m browserModel) openResourceQueue() (browserModel, tea.Cmd) {
	if _, ok := m.resources.(ResourceQueueActions); !ok {
		m.resource.err = "Resource queue service is unavailable."
		return m, nil
	}
	m.resource.queue = &resourceQueuePanel{}
	return m.loadResourceQueue()
}

func (m browserModel) loadResourceQueue() (browserModel, tea.Cmd) {
	service, ok := m.resources.(ResourceQueueActions)
	if !ok || m.resource == nil || m.resource.queue == nil {
		return m, nil
	}
	m.resourceGeneration++
	m.resource.queue.loading = true
	generation, ctx := m.resourceGeneration, m.ctx
	return m, m.gate.command(func() tea.Msg {
		operations, err := service.Queue(ctx)
		return resourceQueueLoaded{generation: generation, operations: operations, err: err}
	})
}

func (m browserModel) finishResourceQueueLoad(msg resourceQueueLoaded) (browserModel, tea.Cmd) {
	if m.resource == nil || m.resource.queue == nil || msg.generation != m.resourceGeneration {
		return m, nil
	}
	p := m.resource.queue
	p.loading = false
	if msg.err != nil {
		m.resource.err = msg.err.Error()
		return m, nil
	}
	m.resource.err = ""
	p.operations = msg.operations
	if p.seq != 0 && p.selected() == nil {
		p.seq, p.lost, p.detail = 0, true, false
		m.resource.err = "Selected resource operation disappeared. Select another explicitly."
	}
	if p.seq == 0 && !p.lost && len(p.operations) > 0 {
		p.seq = p.operations[0].Item.Seq
	}
	return m, nil
}

func (p *resourceQueuePanel) selected() *store.EntityOperation {
	for i := range p.operations {
		if p.operations[i].Item.Seq == p.seq {
			return &p.operations[i]
		}
	}
	return nil
}

func (m browserModel) resourceQueueKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.resource.queue, msg.String()
	if p.confirmation != nil {
		switch s {
		case "esc":
			p.confirmation, p.offset = nil, 0
		case "enter":
			return m.applyResourceQueue()
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.resourceQueueLines()))
		}
		return m, nil
	}
	switch s {
	case "esc":
		if p.detail {
			p.detail, p.offset = false, 0
		} else {
			m.resource.queue = nil
			return m.loadResources(false)
		}
	case "r":
		return m.loadResourceQueue()
	case "enter":
		if p.selected() != nil {
			p.detail, p.offset = !p.detail, 0
		}
	case "c", "ctrl+d", "f":
		if op := p.selected(); op != nil {
			action := "cancel"
			if s == "f" {
				action = "recover"
			}
			p.confirmation = &resourceQueueConfirmation{seq: op.Item.Seq, revision: op.Revision, action: action, kind: op.Mutation.Ref.Kind, key: op.Mutation.Ref.Key, phase: op.Phase}
			p.offset = 0
		}
	case "?":
		m.help.ShowAll, m.help.offset = true, 0
	default:
		if p.detail {
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.resourceQueueLines()))
			return m, nil
		}
		index := -1
		for i, op := range p.operations {
			if op.Item.Seq == p.seq {
				index = i
			}
		}
		next := scroll(index, s, max(1, m.height-6), len(p.operations))
		if len(p.operations) > 0 && next != index {
			p.seq, p.lost = p.operations[next].Item.Seq, false
		}
	}
	return m, nil
}

func (m browserModel) applyResourceQueue() (browserModel, tea.Cmd) {
	service, ok := m.resources.(ResourceQueueActions)
	p := m.resource.queue
	if !ok || p.confirmation == nil || m.busy {
		return m, nil
	}
	confirmation := *p.confirmation
	m.resourceGeneration++
	m.busy, m.resource.working = true, true
	generation, ctx := m.resourceGeneration, m.ctx
	var once sync.Once
	var result resourceQueueFinished
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			var err error
			if confirmation.action == "cancel" {
				err = service.Cancel(ctx, confirmation.seq, confirmation.revision)
			} else {
				err = service.Recover(ctx, confirmation.seq, confirmation.revision)
			}
			result = resourceQueueFinished{generation: generation, seq: confirmation.seq, revision: confirmation.revision, action: confirmation.action, err: err}
		})
		return result
	})
}

func (m browserModel) finishResourceQueueAction(msg resourceQueueFinished) (browserModel, tea.Cmd) {
	if m.resource == nil || m.resource.queue == nil || msg.generation != m.resourceGeneration {
		return m, nil
	}
	p := m.resource.queue
	if p.confirmation == nil || p.confirmation.seq != msg.seq || p.confirmation.revision != msg.revision || p.confirmation.action != msg.action {
		return m, nil
	}
	m.busy, m.resource.working = false, false
	p.confirmation, p.offset = nil, 0
	if msg.err != nil {
		m.resource.err = msg.err.Error()
		return m, nil
	}
	m.notice = fmt.Sprintf("Resource operation %d: %s applied locally. Sync remains separate.", msg.seq, msg.action)
	return m.loadResourceQueue()
}

func (m browserModel) resourceQueueLines() []string {
	p := m.resource.queue
	var lines []string
	if c := p.confirmation; c != nil {
		lines = []string{c.action + " exactly one resource operation", "Sequence: " + strconv.FormatInt(c.seq, 10), "Revision: " + strconv.FormatInt(c.revision, 10), "Resource: " + c.kind + "/" + c.key, "Phase: " + c.phase}
		if c.action == "cancel" {
			lines = append(lines, "Cancel before sending. Unsafe cancellation is refused.", "Remote accepted work cannot be undone by canceling a queue row.")
		} else {
			lines = append(lines, "Recover only this operation and revision.", "An uncertain write remains read-back only; this action never resends it.")
		}
	} else if p.detail {
		if op := p.selected(); op != nil {
			lines = []string{"Sequence: " + strconv.FormatInt(op.Item.Seq, 10), "Resource: " + op.Mutation.Ref.Kind + "/" + op.Mutation.Ref.Key, "Action: " + op.Mutation.Action, "Phase: " + op.Phase, "Revision: " + strconv.FormatInt(op.Revision, 10), "State: " + string(op.Item.State), "Changed fields:"}
			lines = append(lines, rawResourceLines(op.Mutation.Patch, max(1, m.width-4))...)
		}
	} else {
		if p.loading {
			return []string{"Loading resource queue..."}
		}
		if len(p.operations) == 0 {
			return []string{"No queued resource operations."}
		}
		selected := 0
		for i, op := range p.operations {
			if op.Item.Seq == p.seq {
				selected = i
			}
		}
		rows := max(1, m.height-6)
		start := max(0, selected-rows+1)
		for _, op := range p.operations[start:min(len(p.operations), start+rows)] {
			lines = append(lines, marker(op.Item.Seq == p.seq)+fmt.Sprintf("%d %s %s %s", op.Item.Seq, op.Mutation.Ref.Kind, op.Mutation.Action, op.Phase))
		}
		return lines
	}
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrapText(line, max(1, m.width-4))...)
	}
	return wrapped
}

func (m browserModel) resourceFooters() (string, string) {
	p := m.resource
	if p.queue != nil {
		if p.queue.confirmation != nil {
			return "Exact resource operation", "Enter confirm Esc cancel j/k scroll"
		}
		return "Resource queue: r refresh", "j/k Enter c cancel f recover Esc"
	}
	if p.preview != nil {
		return "Exact local operation | ? help", "Enter queue Esc back j/k scroll"
	}
	if p.focusPreview != nil {
		return "Completed record; upload separate", "Enter accept Esc back j/k scroll"
	}
	if p.form != nil {
		if p.form.discard {
			return "Unsaved fields", "s review d discard Esc continue"
		}
		if p.form.kind == "checkin" {
			return "Ctrl+R fetch exact date", "Tab fields Ctrl+S preview Esc back"
		}
		return "Tab fields Ctrl+S preview", "Esc back; text keys edit fields"
	}
	if p.detail {
		return "Resource details", "j/k scroll Enter/Esc back"
	}
	if p.query.Kind == "habit" {
		return "a new e edit c check-in h history", "j/k Enter r local R remote Q queue"
	}
	if p.query.Kind == "focus" {
		return "a record e range 0/1 type ^D delete", "r local R remote Q queue Enter"
	}
	return ": workspaces a new e edit Q queue", "j/k Enter r local R remote Esc back"
}
