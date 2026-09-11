package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

type focusPicker struct {
	choices           app.FocusChoices
	projectID, taskID string
	tasks             bool
}

func (p *focusPicker) taskRows() []model.Task {
	var tasks []model.Task
	for _, task := range p.choices.Tasks {
		if task.ProjectId == p.projectID {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (m browserModel) focusPickerKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, key := m.systemPanel.focus, msg.String()
	if key == "esc" {
		m.systemPanel.focus = nil
		return m.readSettings()
	}
	if key == "?" {
		m.help.ShowAll, m.help.offset = true, 0
		return m, nil
	}
	if key == "tab" || key == "shift+tab" {
		p.tasks = !p.tasks
		return m, nil
	}
	if key == "enter" && !p.tasks {
		p.tasks = true
		return m, nil
	}
	if key == "n" || key == "enter" {
		ref := config.FocusReference{}
		if key == "enter" {
			if p.taskID == "" {
				m.notice = "Select an exact cached task, or press n for none."
				return m, nil
			}
			ref.TaskID = p.taskID
		}
		system := m.system
		m.systemPanel.focus = nil
		return m.systemCommand("prepare default focus", func(ctx context.Context) systemFinished {
			preview, err := system.PrepareDefaultFocus(ctx, ref)
			return systemFinished{preview: preview, err: err}
		})
	}
	if key != "j" && key != "k" && key != "up" && key != "down" && key != "pgup" && key != "pgdown" && key != "home" && key != "end" {
		return m, nil
	}
	ids := []string{}
	selected := p.projectID
	if p.tasks {
		selected = p.taskID
		for _, task := range p.taskRows() {
			ids = append(ids, task.Id)
		}
	} else {
		for _, project := range p.choices.Projects {
			ids = append(ids, project.Id)
		}
	}
	index := -1
	for i, id := range ids {
		if id == selected {
			index = i
		}
	}
	if len(ids) == 0 {
		return m, nil
	}
	if index < 0 {
		index = 0
	} else {
		index = scroll(index, key, max(1, m.height/2-5), len(ids))
	}
	if p.tasks {
		p.taskID = ids[index]
	} else if p.projectID != ids[index] {
		p.projectID, p.taskID = ids[index], ""
	}
	return m, nil
}

func (m browserModel) focusPickerLines() []string {
	p := m.systemPanel.focus
	width, count := max(1, m.width-6), max(1, m.height-8)
	var labels, ids []string
	selected, heading := p.projectID, "Cached lists"
	if p.choices.Notice != "" {
		heading = "Default list unavailable; choose a list"
	}
	if p.tasks {
		selected, heading = p.taskID, "Cached open tasks in "+p.projectID
		for _, task := range p.taskRows() {
			labels = append(labels, task.Title)
			ids = append(ids, task.Id)
		}
	} else {
		for _, project := range p.choices.Projects {
			labels = append(labels, project.Name)
			ids = append(ids, project.Id)
		}
	}
	lines := []string{displayClipped(heading, width), "Tab: lists/tasks | n: none"}
	index := -1
	for i, id := range ids {
		if id == selected {
			index = i
		}
	}
	start := max(0, index-count+1)
	if len(ids) == 0 {
		lines = append(lines, "No available entries.")
	}
	for i := start; i < min(len(ids), start+count); i++ {
		lines = append(lines, marker(ids[i] == selected)+displayClipped(labels[i]+" ["+ids[i]+"]", width))
	}
	return lines
}
