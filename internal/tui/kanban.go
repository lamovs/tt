package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
)

type kanbanColumn struct {
	id, name string
	order    int64
}
type kanbanBoard struct {
	projectID, columnKey string
	columns              []kanbanColumn
	loading              bool
	err                  string
}
type kanbanGroup struct {
	key, name string
	tasks     []model.Task
}
type kanbanLoaded struct {
	generation uint64
	projectID  string
	listing    app.ResourceListing
	err        error
}

func (m browserModel) toggleKanban() (browserModel, tea.Cmd) {
	if m.board != nil {
		m.board = nil
		m.boardGeneration++
		return m, nil
	}
	if m.query.View != app.ProjectView || m.query.ProjectID == "" {
		m.notice = "Choose a project in Lists before opening its Kanban board."
		return m, nil
	}
	m.board = &kanbanBoard{projectID: m.query.ProjectID}
	m.focus = tasksPane
	m.reconcileBoardSelection()
	return m.loadKanban(false)
}

func (m browserModel) loadKanban(remote bool) (browserModel, tea.Cmd) {
	if m.board == nil || m.resources == nil {
		return m, nil
	}
	m.boardGeneration++
	m.board.loading = true
	generation, id, service, ctx := m.boardGeneration, m.board.projectID, m.resources, m.ctx
	return m, m.gate.command(func() tea.Msg {
		result, err := service.List(ctx, app.ResourceQuery{Kind: "column", ProjectID: id}, remote)
		return kanbanLoaded{generation: generation, projectID: id, listing: result, err: err}
	})
}

func (m browserModel) finishKanbanLoad(msg kanbanLoaded) (browserModel, tea.Cmd) {
	if m.board == nil || msg.generation != m.boardGeneration || msg.projectID != m.board.projectID || m.query.ProjectID != msg.projectID {
		return m, nil
	}
	m.board.loading = false
	if msg.err != nil {
		m.board.err = msg.err.Error()
		return m, nil
	}
	columns := make([]kanbanColumn, 0, len(msg.listing.Entities))
	seen := map[string]bool{}
	for _, entity := range msg.listing.Entities {
		var item struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
			Name      string `json:"name"`
			SortOrder int64  `json:"sortOrder"`
		}
		if json.Unmarshal(entity.Data, &item) != nil || entity.Ref.Kind != "column" || entity.ProjectKey != msg.projectID || item.ProjectID != "" && item.ProjectID != msg.projectID {
			m.board.err = "Column snapshot is invalid; previous board metadata remains."
			return m, nil
		}
		id := entity.ServerID
		if id == "" {
			id = entity.Ref.Key
		}
		if id == "" || seen[id] {
			m.board.err = "Column snapshot has missing or duplicate IDs."
			return m, nil
		}
		seen[id] = true
		name := item.Name
		if entity.Dirty {
			name += " [pending]"
		}
		columns = append(columns, kanbanColumn{id: id, name: name, order: item.SortOrder})
	}
	sort.SliceStable(columns, func(i, j int) bool {
		if columns[i].order != columns[j].order {
			return columns[i].order < columns[j].order
		}
		return columns[i].id < columns[j].id
	})
	m.board.columns, m.board.err = columns, ""
	m.reconcileBoardSelection()
	return m, nil
}

func (m browserModel) boardGroups() []kanbanGroup {
	if m.board == nil {
		return nil
	}
	groups := make([]kanbanGroup, 0, len(m.board.columns)+2)
	index := map[string]int{}
	for _, column := range m.board.columns {
		key := "column:" + column.id
		index[column.id] = len(groups)
		groups = append(groups, kanbanGroup{key: key, name: column.name})
	}
	unassigned, unknown := []model.Task{}, []model.Task{}
	for _, task := range m.tasks {
		if task.ProjectId != m.board.projectID {
			continue
		}
		if task.ColumnId == "" {
			unassigned = append(unassigned, task)
			continue
		}
		if position, ok := index[task.ColumnId]; ok {
			groups[position].tasks = append(groups[position].tasks, task)
		} else {
			unknown = append(unknown, task)
		}
	}
	if len(unassigned) > 0 || len(groups) == 0 {
		groups = append(groups, kanbanGroup{key: "unassigned", name: "No column", tasks: unassigned})
	}
	if len(unknown) > 0 {
		groups = append(groups, kanbanGroup{key: "unknown", name: "Unknown columns", tasks: unknown})
	}
	for i := range groups {
		sort.SliceStable(groups[i].tasks, func(a, b int) bool {
			left, right := groups[i].tasks[a], groups[i].tasks[b]
			if left.SortOrder != right.SortOrder {
				return left.SortOrder < right.SortOrder
			}
			return left.Id < right.Id
		})
	}
	return groups
}

func (m *browserModel) reconcileBoardSelection() {
	if m.board == nil {
		return
	}
	groups := m.boardGroups()
	exists := false
	for _, group := range groups {
		if group.key == m.board.columnKey {
			exists = true
		}
		for _, task := range group.tasks {
			if task.Id == m.taskID {
				m.board.columnKey = group.key
				return
			}
		}
	}
	if !exists && len(groups) > 0 {
		m.board.columnKey = groups[0].key
	}
}

func (m browserModel) kanbanKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd, bool) {
	s := msg.String()
	if m.focus != tasksPane {
		return m, nil, false
	}
	switch s {
	case "esc":
		m.board = nil
		m.boardGeneration++
		return m, nil, true
	case "R":
		next, cmd := m.loadKanban(true)
		return next, cmd, true
	case "C":
		next, cmd := m.openResources(app.ResourceQuery{Kind: "column", ProjectID: m.board.projectID})
		return next, cmd, true
	case "r":
		next, tasks := m.load()
		next, columns := next.loadKanban(false)
		return next, tea.Batch(tasks, columns), true
	case "alt+shift+left", "alt+shift+right":
		direction := 1
		if s == "alt+shift+left" {
			direction = -1
		}
		next, cmd := m.beginColumnMove(direction)
		return next, cmd, true
	}
	groups := m.boardGroups()
	if len(groups) == 0 {
		return m, nil, false
	}
	column := 0
	for i := range groups {
		if groups[i].key == m.board.columnKey {
			column = i
		}
	}
	target := ""
	if s == "alt+left" || s == "alt+right" {
		if s == "alt+left" {
			column = max(0, column-1)
		} else {
			column = min(len(groups)-1, column+1)
		}
		m.board.columnKey = groups[column].key
		if len(groups[column].tasks) > 0 {
			target = groups[column].tasks[0].Id
		}
	} else {
		switch s {
		case "j", "k", "up", "down", "pgup", "pgdown", "home", "end":
		default:
			return m, nil, false
		}
		tasks := groups[column].tasks
		current := -1
		for i, task := range tasks {
			if task.Id == m.taskID {
				current = i
			}
		}
		if len(tasks) > 0 {
			target = tasks[scroll(current, s, max(1, m.height-6), len(tasks))].Id
		}
	}
	if target != m.taskID {
		m.taskID, m.selectionLost, m.selectionNotice = target, target == "", ""
		m.clearDetail()
		next, cmd := m.loadDetail()
		return next, cmd, true
	}
	return m, nil, true
}

func (m browserModel) boardView() []string {
	groups := m.boardGroups()
	if len(groups) == 0 {
		return m.panel("Kanban", []string{"No columns or tasks."}, m.width, m.height-4, tasksPane)
	}
	selected := 0
	for i := range groups {
		if groups[i].key == m.board.columnKey {
			selected = i
		}
	}
	count := min(len(groups), max(1, m.width/32))
	start := min(max(0, selected-count+1), max(0, len(groups)-count))
	columnWidth := (m.width - count + 1) / count
	height, rows := max(1, m.height-4), max(1, m.height-6)
	area := make([]string, height)
	for i := 0; i < count; i++ {
		group := groups[start+i]
		width := columnWidth
		if i == count-1 {
			width = m.width - (columnWidth+1)*(count-1)
		}
		current := -1
		for j, task := range group.tasks {
			if task.Id == m.taskID {
				current = j
			}
		}
		first := max(0, current-rows+1)
		lines := []string{}
		for _, task := range group.tasks[first:min(len(group.tasks), first+rows)] {
			box := "[ ] "
			if task.Status.Done() {
				box = "[x] "
			}
			if task.Kind == "NOTE" {
				box = "[N] "
			}
			text := marker(task.Id == m.taskID) + box + displayClipped(task.Title, max(1, width-10))
			lines = append(lines, text)
		}
		if len(lines) == 0 {
			lines = []string{"No cards."}
		}
		title := fmt.Sprintf("%s (%d)", displayClipped(group.name, max(1, width-12)), len(group.tasks))
		copy := m
		if group.key != m.board.columnKey {
			copy.focus = listsPane
		}
		rendered := copy.panel(title, lines, width, height, tasksPane)
		for j, line := range rendered {
			if i > 0 {
				area[j] += " "
			}
			area[j] += line
		}
	}
	for i := range area {
		area[i] = ansi.Truncate(area[i], m.width, "")
		area[i] += strings.Repeat(" ", max(0, m.width-ansi.StringWidth(area[i])))
	}
	return area
}
