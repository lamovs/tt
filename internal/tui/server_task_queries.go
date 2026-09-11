package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
)

type ServerTaskActions interface {
	List(context.Context, app.ServerTaskQuery, bool) (app.ServerTaskListing, error)
}

type serverTaskPanel struct {
	mode                           string
	fields                         []resourceField
	field, index, offset           int
	editing, detail, loading, lost bool
	key, err, request              string
	listing                        app.ServerTaskListing
}

type serverTasksLoaded struct {
	generation uint64
	request    string
	listing    app.ServerTaskListing
	err        error
}

func (m *browserModel) closeServerTasks() {
	if m.serverTaskCancel != nil {
		m.serverTaskCancel()
		m.serverTaskCancel = nil
	}
	m.serverTaskGeneration++
	m.serverTasks = nil
}

func (m browserModel) openServerTasks(mode string) (browserModel, tea.Cmd) {
	if m.serverTaskActions == nil {
		m.notice = "Server query workspace is unavailable until settings and cache are ready."
		return m, nil
	}
	m.closeResources()
	m.closeServerTasks()
	p := &serverTaskPanel{mode: mode, editing: true}
	field := func(key, label string) { p.fields = append(p.fields, resourceField{key: key, label: label}) }
	if mode == "search" {
		field("text", "Search text")
	}
	field("projects", "Project IDs, comma-separated")
	field("from", "From: RFC3339 timestamp")
	field("to", "To: RFC3339 timestamp")
	if mode != "completed" {
		field("tags", "Tags, comma-separated")
		field("status", "Status: 0,2")
	}
	if mode == "filter" {
		field("priority", "Priority: 0,1,3,5")
		field("kind", "Kind: TEXT,NOTE,CHECKLIST")
	}
	if m.query.View == app.ProjectView {
		for i := range p.fields {
			if p.fields[i].key == "projects" {
				p.fields[i].input.insert(m.query.ProjectID)
			}
		}
	}
	m.serverTasks = p
	if mode == "search" {
		return m, nil
	}
	return m.loadServerTasks(false)
}

func (p *serverTaskPanel) query() (app.ServerTaskQuery, error) {
	q := app.ServerTaskQuery{Mode: p.mode}
	for _, field := range p.fields {
		value := strings.TrimSpace(string(field.input.value))
		switch field.key {
		case "text":
			q.Text = value
		case "projects":
			if value != "" {
				q.ProjectIDs = strings.Split(value, ",")
			}
		case "from":
			q.From = value
		case "to":
			q.To = value
		case "tags":
			if value != "" {
				q.Tags = strings.Split(value, ",")
			}
		case "kind":
			if value != "" {
				q.Kind = strings.Split(value, ",")
			}
		case "priority", "status":
			if value == "" {
				continue
			}
			var values []int
			for _, word := range strings.Split(value, ",") {
				n, err := strconv.Atoi(word)
				if err != nil {
					return q, errors.New("priority and status require comma-separated integers")
				}
				values = append(values, n)
			}
			if field.key == "priority" {
				q.Priority = values
			} else {
				q.Status = values
			}
		}
	}
	return q, nil
}

func (m browserModel) loadServerTasks(remote bool) (browserModel, tea.Cmd) {
	p := m.serverTasks
	if p == nil || m.serverTaskActions == nil {
		return m, nil
	}
	q, err := p.query()
	if err != nil {
		p.err = err.Error()
		return m, nil
	}
	raw, _ := json.Marshal(q)
	if m.serverTaskCancel != nil {
		m.serverTaskCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.serverTaskCancel = cancel
	m.serverTaskGeneration++
	p.request, p.loading, p.err = string(raw), true, ""
	generation, request, service := m.serverTaskGeneration, p.request, m.serverTaskActions
	return m, m.gate.command(func() tea.Msg {
		listing, err := service.List(ctx, q, remote)
		return serverTasksLoaded{generation: generation, request: request, listing: listing, err: err}
	})
}

func (m browserModel) finishServerTasks(msg serverTasksLoaded) (browserModel, tea.Cmd) {
	p := m.serverTasks
	if p == nil || msg.generation != m.serverTaskGeneration || msg.request != p.request {
		return m, nil
	}
	q, err := p.query()
	raw, _ := json.Marshal(q)
	if err != nil || string(raw) != msg.request {
		return m, nil
	}
	p.loading = false
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	p.err, p.listing, p.editing = "", msg.listing, false
	p.index = -1
	for i, task := range p.listing.Tasks {
		if task.Id == p.key {
			p.index = i
			break
		}
	}
	if p.key != "" && p.index < 0 {
		p.key, p.lost, p.detail = "", true, false
		p.err = "Selected query row left this result; select another explicitly."
	}
	if p.key == "" && !p.lost && len(p.listing.Tasks) > 0 {
		p.index, p.key = 0, p.listing.Tasks[0].Id
	}
	return m, nil
}

func (m *browserModel) editServerQuery(text string, msg *tea.KeyPressMsg) {
	p := m.serverTasks
	if p == nil || !p.editing {
		return
	}
	if m.serverTaskCancel != nil {
		m.serverTaskCancel()
		m.serverTaskCancel = nil
	}
	m.serverTaskGeneration++
	p.loading = false
	if msg != nil {
		p.fields[p.field].input.update(*msg)
	} else {
		p.fields[p.field].input.insert(text)
	}
}

func (m browserModel) serverTasksKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.serverTasks, msg.String()
	if p.editing {
		switch s {
		case "esc":
			m.closeServerTasks()
		case "tab":
			p.field = (p.field + 1) % len(p.fields)
		case "shift+tab":
			p.field = (p.field + len(p.fields) - 1) % len(p.fields)
		case "ctrl+r":
			return m.loadServerTasks(true)
		case "ctrl+s", "enter":
			return m.loadServerTasks(false)
		default:
			m.editServerQuery("", &msg)
		}
		return m, nil
	}
	if p.detail {
		if s == "esc" {
			p.detail = false
			p.offset = 0
		} else {
			p.offset = scroll(p.offset, s, max(1, m.height-8), len(m.serverTaskDetailLines()))
		}
		return m, nil
	}
	switch s {
	case "esc":
		m.closeServerTasks()
	case ":":
		m.closeServerTasks()
		m.palette = &resourcePalette{}
	case "e":
		p.editing = true
	case "r":
		return m.loadServerTasks(false)
	case "R":
		return m.loadServerTasks(true)
	case "enter":
		if p.index >= 0 && p.index < len(p.listing.Tasks) {
			p.detail = true
			p.offset = 0
		}
	case "j", "k", "up", "down", "pgup", "pgdown", "home", "end":
		key := s
		if s == "j" {
			key = "down"
		}
		if s == "k" {
			key = "up"
		}
		p.index = scroll(p.index, key, max(1, m.height-9), len(p.listing.Tasks))
		if p.index >= 0 && p.index < len(p.listing.Tasks) {
			p.key, p.lost = p.listing.Tasks[p.index].Id, false
		}
	}
	return m, nil
}

func (m browserModel) serverTaskDetailLines() []string {
	p := m.serverTasks
	if p.index < 0 || p.index >= len(p.listing.Raw) {
		return []string{"No selected source row."}
	}
	return append([]string{"Provider snapshot (read-only; not a local task)"}, rawResourceLines(p.listing.Raw[p.index], max(1, m.width-6))...)
}

func (m browserModel) serverTasksView() (string, []string) {
	p := m.serverTasks
	title := "Server " + p.mode + " (read-only)"
	rows := max(1, m.height-9)
	if p.editing {
		lines := []string{"Edit query; Ctrl+S cache, Ctrl+R server"}
		fieldRows := max(1, (m.height-7)/2)
		start := max(0, p.field-fieldRows+1)
		for i := start; i < min(len(p.fields), start+fieldRows); i++ {
			field := p.fields[i]
			lines = append(lines, marker(i == p.field)+displayClipped(field.label, max(1, m.width-6)))
			lines = append(lines, "  "+field.input.view(max(1, m.width-8)))
		}
		return title, lines
	}
	if p.detail {
		lines := m.serverTaskDetailLines()
		start := min(p.offset, max(0, len(lines)-rows))
		return title, lines[start:]
	}
	meta := p.listing.Meta
	lines := wrapText(fmt.Sprintf("Source: %s; coverage: %s; %d rows", meta.Source, meta.Completeness, len(p.listing.Tasks)), max(1, m.width-6))
	if len(p.listing.Warnings) > 0 {
		lines = append(lines, "Provider conversion warnings; inspect source.")
	}
	start := max(0, p.index-rows+2)
	for i := start; i < min(len(p.listing.Tasks), start+rows-1); i++ {
		task := p.listing.Tasks[i]
		lines = append(lines, marker(task.Id == p.key)+displayClipped(task.Title, max(1, m.width-8)))
	}
	if len(p.listing.Tasks) == 0 {
		lines = append(lines, "No query rows; absence is not authoritative.")
	}
	return title, lines
}
