package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
)

type resourcePalette struct {
	input searchInput
	index int
}

type paletteEntry struct{ key, label string }

func (p *resourcePalette) entries() []paletteEntry {
	all := []paletteEntry{
		{"project", "Projects"}, {"folder", "Project folders"}, {"column", "Kanban columns (current project)"},
		{"tag", "Tags"}, {"habit", "Habits"}, {"countdown", "Countdowns"}, {"comment", "Comments (selected task)"},
		{"remote-focus", "Remote focus history"}, {"queue", "Task and focus queue"}, {"focus", "Local focus timer"},
		{"completed", "Server completed tasks (read-only)"}, {"filter", "Server filtered tasks (read-only)"}, {"search", "Server task search (read-only)"},
	}
	query := strings.ToLower(strings.TrimSpace(string(p.input.value)))
	if query == "" {
		return all
	}
	var entries []paletteEntry
	for _, entry := range all {
		if strings.Contains(strings.ToLower(entry.label), query) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (m browserModel) paletteKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.palette, msg.String()
	entries := p.entries()
	switch s {
	case "esc":
		m.palette = nil
	case "enter":
		if len(entries) == 0 {
			return m, nil
		}
		entry := entries[min(p.index, len(entries)-1)]
		m.palette = nil
		return m.activatePalette(entry.key)
	case "up", "down", "pgup", "pgdown", "home", "end":
		p.index = scroll(p.index, s, max(1, m.height-7), len(entries))
	default:
		p.input.update(msg)
		p.index = 0
	}
	return m, nil
}

func (m browserModel) activatePalette(key string) (browserModel, tea.Cmd) {
	query := app.ResourceQuery{Kind: key}
	switch key {
	case "completed", "filter", "search":
		return m.openServerTasks(key)
	case "queue":
		m.closeResources()
		return m.openQueue()
	case "focus":
		m.closeResources()
		next, cmd, _ := m.mutationKey("t")
		return next, cmd
	case "column":
		if m.query.View != app.ProjectView || m.query.ProjectID == "" {
			m.notice = "Choose a project in Lists before opening its columns."
			return m, nil
		}
		query.ProjectID = m.query.ProjectID
	case "comment":
		i := m.taskIndex()
		if i < 0 {
			m.notice = "Select a task before opening its comments."
			return m, nil
		}
		query.ProjectID, query.TaskID = m.tasks[i].ProjectId, m.tasks[i].Id
	case "remote-focus":
		now := time.Now()
		end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		query = app.ResourceQuery{Kind: "focus", From: end.AddDate(0, 0, -30).Format(time.RFC3339), To: end.Format(time.RFC3339)}
	}
	return m.openResources(query)
}

func (m browserModel) paletteView() (string, []string) {
	p := m.palette
	entries := p.entries()
	lines := []string{"Find: " + p.input.view(max(1, m.width-10))}
	rows := max(1, m.height-7)
	start := max(0, p.index-rows+1)
	for i := start; i < min(len(entries), start+rows); i++ {
		lines = append(lines, marker(i == p.index)+entries[i].label)
	}
	if len(entries) == 0 {
		lines = append(lines, "No matching workspace.")
	}
	return "Workspaces", lines
}
