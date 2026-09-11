package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

type ResourceActions interface {
	List(context.Context, app.ResourceQuery, bool) (app.ResourceListing, error)
	Prepare(context.Context, store.EntityMutation) (app.ResourcePreview, error)
	Apply(context.Context, string, string, string) (store.EntityMutationOutcome, error)
}

type resourcePanel struct {
	query                          app.ResourceQuery
	listing                        app.ResourceListing
	key                            string
	lost, loading, working, detail bool
	offset                         int
	err                            string
	form                           *resourceForm
	preview                        *app.ResourcePreview
	focusPreview                   *app.FocusRecordPreview
	queue                          *resourceQueuePanel
	operationTarget                string
}

type resourceLoaded struct {
	generation uint64
	query      app.ResourceQuery
	listing    app.ResourceListing
	err        error
}

type resourcePrepared struct {
	generation uint64
	query      app.ResourceQuery
	target     string
	preview    app.ResourcePreview
	err        error
}

type resourceApplied struct {
	generation uint64
	query      app.ResourceQuery
	previewID  string
	outcome    store.EntityMutationOutcome
	err        error
}

func (m *browserModel) closeResources() {
	if m.resourceCancel != nil {
		m.resourceCancel()
		m.resourceCancel = nil
	}
	m.resourceGeneration++
	m.resource = nil
}

func (m browserModel) openResources(query app.ResourceQuery) (browserModel, tea.Cmd) {
	if m.resources == nil {
		m.notice = "Resource workspace is unavailable until settings and cache are ready."
		return m, nil
	}
	m.closeResources()
	m.closeServerTasks()
	m.resource = &resourcePanel{query: query}
	return m.loadResources(false)
}

func (m browserModel) loadResources(remote bool) (browserModel, tea.Cmd) {
	if m.resource == nil || m.resources == nil {
		return m, nil
	}
	if m.resourceCancel != nil {
		m.resourceCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.resourceCancel = cancel
	m.resourceGeneration++
	m.resource.loading, m.resource.err = true, ""
	generation, query, service := m.resourceGeneration, m.resource.query, m.resources
	return m, m.gate.command(func() tea.Msg {
		listing, err := service.List(ctx, query, remote)
		return resourceLoaded{generation: generation, query: query, listing: listing, err: err}
	})
}

func (m browserModel) finishResourceLoad(msg resourceLoaded) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || msg.generation != m.resourceGeneration || msg.query != p.query {
		return m, nil
	}
	p.loading = false
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	p.err, p.listing = "", msg.listing
	if p.key != "" && p.selected() == nil {
		p.key, p.lost, p.detail = "", true, false
		p.err = "Selected resource left this result. Select another explicitly."
	}
	if p.key == "" && !p.lost && len(p.listing.Entities) > 0 {
		p.key = p.listing.Entities[0].Ref.Key
	}
	return m, nil
}

func (p *resourcePanel) selected() *store.ResourceEntity {
	for i := range p.listing.Entities {
		if p.listing.Entities[i].Ref.Key == p.key {
			return &p.listing.Entities[i]
		}
	}
	return nil
}

func (p *resourcePanel) index() int {
	for i := range p.listing.Entities {
		if p.listing.Entities[i].Ref.Key == p.key {
			return i
		}
	}
	return -1
}

func (m browserModel) resourceKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.resource, msg.String()
	if p.queue != nil {
		return m.resourceQueueKey(msg)
	}
	if p.focusPreview != nil {
		switch s {
		case "esc":
			p.focusPreview, p.offset = nil, 0
		case "enter":
			return m.applyFocusRecord()
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.focusRecordPreviewLines()))
		}
		return m, nil
	}
	if p.form != nil && p.preview == nil {
		return m.resourceFormKey(msg)
	}
	if p.preview != nil {
		switch s {
		case "esc":
			p.preview, p.offset = nil, 0
		case "enter":
			return m.applyResourcePreview()
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.resourcePreviewLines()))
		}
		return m, nil
	}
	if p.detail {
		switch s {
		case "esc", "enter":
			p.detail, p.offset = false, 0
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.resourceDetailLines()))
		}
		return m, nil
	}
	switch s {
	case "esc":
		m.closeResources()
	case "q":
		return m, tea.Quit
	case ":":
		m.palette = &resourcePalette{}
	case "?":
		m.help.ShowAll, m.help.offset = true, 0
	case "r":
		return m.loadResources(false)
	case "R":
		return m.loadResources(true)
	case "Q":
		return m.openResourceQueue()
	case "a":
		return m.openResourceForm(false)
	case "e":
		return m.openResourceForm(true)
	case "0", "1":
		if p.query.Kind == "focus" {
			if s == "0" {
				return m.changeFocusHistoryType(0)
			}
			return m.changeFocusHistoryType(1)
		}
	case "c":
		if p.query.Kind == "habit" {
			return m.openCheckinForm()
		}
	case "h":
		if p.query.Kind == "habit" {
			return m.openHabitHistory()
		}
	case "b":
		if p.query.Kind == "project" {
			if entity := p.selected(); entity != nil && entity.ServerID != "" {
				id := entity.ServerID
				m.closeResources()
				m, cmd := m.changeQuery(app.BrowseQuery{View: app.ProjectView, ProjectID: id})
				m.board = &kanbanBoard{projectID: id}
				m.focus = tasksPane
				next, columns := m.loadKanban(false)
				return next, tea.Batch(cmd, columns)
			}
		}
	case "ctrl+d":
		if p.query.Kind != "comment" && p.query.Kind != "focus" {
			p.err = "Deletion is unavailable for this resource or awaits verified cascade handling."
			return m, nil
		}
		if entity := p.selected(); entity != nil {
			return m.prepareResourceMutation(store.EntityMutation{Ref: entity.Ref, Action: "delete", ProjectKey: entity.ProjectKey, Patch: json.RawMessage("{}")})
		}
	case "enter":
		if p.selected() != nil {
			p.detail, p.offset = true, 0
		}
	default:
		index := scroll(p.index(), s, max(1, m.height-6), len(p.listing.Entities))
		if len(p.listing.Entities) > 0 && index != p.index() {
			p.key, p.lost = p.listing.Entities[index].Ref.Key, false
		}
	}
	return m, nil
}

func (m browserModel) prepareResourceMutation(mutation store.EntityMutation) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || m.resources == nil || m.busy {
		return m, nil
	}
	m.resourceGeneration++
	p.working, p.err, p.operationTarget = true, "", mutation.Ref.Key
	m.busy = true
	generation, query, service, ctx := m.resourceGeneration, p.query, m.resources, m.ctx
	var once sync.Once
	var result resourcePrepared
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			preview, err := service.Prepare(ctx, mutation)
			result = resourcePrepared{generation: generation, query: query, target: mutation.Ref.Key, preview: preview, err: err}
		})
		return result
	})
}

func (m browserModel) finishResourcePreview(msg resourcePrepared) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || msg.generation != m.resourceGeneration || msg.query != p.query || msg.target != p.operationTarget || !p.working {
		return m, nil
	}
	m.busy, p.working = false, false
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	p.preview, p.offset = &msg.preview, 0
	return m, nil
}

func (m browserModel) applyResourcePreview() (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || p.preview == nil || m.busy {
		return m, nil
	}
	preview := *p.preview
	m.resourceGeneration++
	m.busy, p.working = true, true
	generation, query, service, ctx := m.resourceGeneration, p.query, m.resources, m.ctx
	var once sync.Once
	var result resourceApplied
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			mutation := preview.Preview.Mutation
			outcome, err := service.Apply(ctx, preview.ID, mutation.Ref.Kind, mutation.Action)
			result = resourceApplied{generation: generation, query: query, previewID: preview.ID, outcome: outcome, err: err}
		})
		return result
	})
}

func (m browserModel) finishResourceApply(msg resourceApplied) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || msg.generation != m.resourceGeneration || msg.query != p.query || !p.working || p.preview == nil || msg.previewID != p.preview.ID {
		return m, nil
	}
	m.busy, p.working = false, false
	p.preview = nil
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	m.notice = fmt.Sprintf("Queued %s operation %d locally. Sync sends it separately.", msg.outcome.Entity.Ref.Kind, msg.outcome.OperationSeq)
	quitAfter := p.form != nil && p.form.quitAfter
	p.form, p.detail, p.offset = nil, false, 0
	if quitAfter {
		m.interrupted = true
		return m, tea.Quit
	}
	if msg.outcome.Entity.Ref.Kind == p.query.Kind {
		p.key = msg.outcome.Entity.Ref.Key
	}
	return m.loadResources(false)
}

func resourceLabel(entity store.ResourceEntity) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(entity.Data, &fields) != nil {
		return entity.Ref.Key
	}
	for _, field := range []string{"name", "title", "label", "note"} {
		var text string
		if json.Unmarshal(fields[field], &text) == nil && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return entity.Ref.Key
}

func (m browserModel) resourceView() (string, []string) {
	p := m.resource
	if p.queue != nil {
		lines := m.resourceQueueLines()
		start := min(p.queue.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Resource queue", lines[start:]
	}
	if p.focusPreview != nil {
		lines := m.focusRecordPreviewLines()
		start := min(p.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Review completed focus record", lines[start:]
	}
	if p.preview != nil {
		lines := m.resourcePreviewLines()
		start := min(p.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Review resource operation", lines[start:]
	}
	if p.form != nil {
		return m.resourceFormView()
	}
	if p.detail {
		lines := m.resourceDetailLines()
		start := min(p.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Resource details", lines[start:]
	}
	title := fmt.Sprintf("%s (%d) | %s", resourceTitle(p.query.Kind), len(p.listing.Entities), p.listing.Meta.Source)
	if p.query.Kind == "focus" {
		title += fmt.Sprintf(" | type %d", p.query.FocusType)
	}
	if p.loading {
		title += " | Loading"
	}
	if len(p.listing.Entities) == 0 {
		return title, []string{"No cached resources for this view.", "R: explicitly fetch from TickTick.", "Coverage: " + p.listing.Meta.Completeness}
	}
	rows := max(1, m.height-6)
	start := max(0, p.index()-rows+1)
	lines := []string{}
	for _, entity := range p.listing.Entities[start:min(len(p.listing.Entities), start+rows)] {
		dirty := ""
		if entity.Dirty {
			dirty = " [pending]"
		}
		text := marker(entity.Ref.Key == p.key) + displayClipped(resourceLabel(entity), max(1, m.width-8-len(dirty))) + dirty
		lines = append(lines, text)
	}
	return title, lines
}

func resourceTitle(kind string) string {
	switch kind {
	case "project":
		return "Projects"
	case "folder":
		return "Folders"
	case "column":
		return "Columns"
	case "tag":
		return "Tags"
	case "habit":
		return "Habits"
	case "checkin":
		return "Habit history"
	case "comment":
		return "Task comments"
	case "focus":
		return "Remote focus"
	case "focus-record":
		return "Completed focus record"
	case "focus-history":
		return "Focus history range"
	case "countdown":
		return "Countdowns"
	}
	return "Resources"
}

func rawResourceLines(raw json.RawMessage, width int) []string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return wrapText("Invalid resource snapshot", width)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		var value string
		if json.Unmarshal(fields[key], &value) != nil {
			value = string(fields[key])
		}
		lines = append(lines, wrapText(key+": "+value, width)...)
	}
	return lines
}

func (m browserModel) resourceDetailLines() []string {
	entity := m.resource.selected()
	if entity == nil {
		return []string{"Select a resource."}
	}
	lines := []string{"Kind: " + entity.Ref.Kind, "Local key: " + entity.Ref.Key, "Server ID: " + entity.ServerID, "Revision: " + strconv.FormatInt(entity.Revision, 10), "Pending: " + strconv.FormatBool(entity.Dirty), "Coverage: " + m.resource.listing.Meta.Completeness, ""}
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrapText(line, max(1, m.width-4))...)
	}
	return append(wrapped, rawResourceLines(entity.Data, max(1, m.width-4))...)
}

func (m browserModel) resourcePreviewLines() []string {
	p := m.resource.preview
	mutation := p.Preview.Mutation
	var lines []string
	for _, line := range []string{mutation.Action + " " + mutation.Ref.Kind, "Target: " + mutation.Ref.Key, "Parent: " + mutation.ProjectKey, "Revision: " + strconv.FormatInt(p.Preview.Before.Revision, 10), "Only this displayed operation is queued locally.", "Undo: " + p.Undo, ""} {
		lines = append(lines, wrapText(line, max(1, m.width-4))...)
	}
	if p.Preview.Exists {
		lines = append(lines, "Before:")
		lines = append(lines, rawResourceLines(p.Preview.Before.Data, max(1, m.width-4))...)
	}
	lines = append(lines, "Changed fields:")
	return append(lines, rawResourceLines(mutation.Patch, max(1, m.width-4))...)
}
