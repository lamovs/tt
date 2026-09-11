package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type taskOperationPanel struct {
	original    model.Task
	projects    []model.Project
	destination string
	move        bool
	help        bool
	preview     *store.TaskOperationPreview
	offset      int
	err         string
}

func (m browserModel) beginTaskOperation(move bool) (browserModel, tea.Cmd) {
	if _, ok := m.actions.(RecoveryActions); !ok {
		return m, nil
	}
	p := &taskOperationPanel{original: *m.detail, move: move}
	m.taskOperation = p
	if move {
		for _, project := range m.projects {
			if !project.Closed && project.Id != p.original.ProjectId {
				p.projects = append(p.projects, project)
			}
		}
		if len(p.projects) > 0 {
			p.destination = p.projects[0].Id
		}
		return m, nil
	}
	return m.previewTaskOperation()
}

func (m browserModel) previewTaskOperation() (browserModel, tea.Cmd) {
	p, actions, ctx := m.taskOperation, m.actions.(RecoveryActions), m.ctx
	original, destination := p.original, p.destination
	return m.actionCommand("preview task operation", func() actionFinished {
		preview, err := actions.PreviewTaskOperation(ctx, original, destination)
		return actionFinished{taskPreview: preview, err: err}
	})
}

func (m browserModel) taskOperationKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.taskOperation, msg.String()
	if s == "?" {
		p.help = !p.help
		p.offset = 0
		return m, nil
	}
	if p.help {
		if s == "esc" {
			p.help = false
			p.offset = 0
		} else {
			_, lines := m.taskOperationView()
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(lines))
		}
		return m, nil
	}
	if s == "esc" {
		if p.preview != nil && p.move {
			p.preview = nil
			p.offset = 0
		} else {
			m.taskOperation = nil
		}
		return m, nil
	}
	if s == "enter" {
		if p.preview == nil {
			if p.err != "" {
				return m, nil
			}
			if p.move && p.destination != "" {
				return m.previewTaskOperation()
			}
			return m, nil
		}
		expected, actions, ctx := *p.preview, m.actions.(RecoveryActions), m.ctx
		return m.actionCommand("apply task operation", func() actionFinished {
			out, err := actions.ApplyTaskOperation(ctx, expected)
			return actionFinished{outcome: out, err: err}
		})
	}
	if p.preview != nil || p.err != "" {
		_, lines := m.taskOperationView()
		p.offset = scroll(p.offset, s, max(1, m.height-6), len(lines))
		return m, nil
	}
	index := 0
	for i, project := range p.projects {
		if project.Id == p.destination {
			index = i
		}
	}
	if len(p.projects) > 0 {
		index = scroll(index, s, max(1, m.height-10), len(p.projects))
		p.destination = p.projects[index].Id
	}
	return m, nil
}

func (m browserModel) taskOperationView() (string, []string) {
	p := m.taskOperation
	title := "Confirm deletion of 1 task"
	if p.move {
		title = "Move 1 task with its existing ID"
	}
	var lines []string
	add := func(s string) { lines = append(lines, wrapText(s, max(1, m.width-4))...) }
	if p.help {
		for _, line := range []string{"Task operation help", "j/k or arrows: select the destination, or scroll the consequence preview", "Enter: preview first, then apply the exact displayed task and queue snapshot", "Esc: cancel; a move preview returns to its frozen destination list", "Concurrent changes require closing and reopening the action", "Deletion uses global undo rules; remote restoration may create a new ID", "Move keeps the task ID and uses the native server endpoint. Global undo checks send state first.", "?/Esc: close help"} {
			add(line)
		}
		return "Task operation help", lines
	}
	add("Task: " + p.original.Title)
	add("Task ID: " + p.original.Id)
	add("Source list ID: " + p.original.ProjectId)
	if p.err != "" {
		add("Not applied: " + p.err)
		add("Esc returns. Reopen the action to review current targets.")
		return title, lines
	}
	if p.preview == nil {
		if !p.move {
			add("Reading exact queue consequences...")
			return title, lines
		}
		add("Select the destination list; Enter previews consequences.")
		if len(p.projects) == 0 {
			add("No other open cached list.")
		}
		selected := 0
		for i, project := range p.projects {
			if project.Id == p.destination {
				selected = i
			}
		}
		visible := max(1, m.height-len(lines)-7)
		start := max(0, selected-visible+1)
		for _, project := range p.projects[start:min(len(p.projects), start+visible)] {
			lines = append(lines, marker(project.Id == p.destination)+display(project.Name+" ["+project.Id+"]"))
		}
		return title, lines
	}
	v := p.preview
	if v.Native != nil {
		add("Destination: " + v.Destination.Name + " [" + v.Destination.Id + "]")
		add("One native move is queued. The task ID, comments, checklist identities and focus references remain unchanged locally.")
		add("The server result is read back before confirmation. An uncertain move is never blindly repeated.")
		add("Global undo cancels an unsent move or previews a reverse move after confirmation.")
		add("Enter applies this exact snapshot; Esc cancels.")
		return title, lines
	}
	add(fmt.Sprintf("Tasks: 1 | Checklist items: %d | Queued operations: %d", len(v.Task.Items), len(v.Queue)))
	add(fmt.Sprintf("Queued operations discarded: %d | In-flight creates retained: %d", len(v.Queue)-v.RetainedCreates, v.RetainedCreates))
	if p.move {
		add("Destination: " + v.Destination.Name + " [" + v.Destination.Id + "]")
		add("Legacy recreate/delete preview: new task ID; no move undo.")
		add("The copied checklist receives new private keys and server IDs. This action creates a local copy and queues its upload.")
	} else {
		add("This removes the local task, its body and checklist. The existing global undo rules apply; remote rollback is not guaranteed.")
	}
	if v.RemoteDelete {
		add("Deletion of the original is queued for the server; remote confirmation requires sync.")
	} else {
		add("No remote delete is queued for this local task. An unrecorded remote copy cannot be removed by this action.")
	}
	for _, e := range v.Queue {
		add(fmt.Sprintf("Queue #%d: %s | %s | task %s", e.Item.Seq, e.Item.Op, e.Item.State, e.Item.TaskID))
	}
	add("Version: " + v.Version)
	add("Enter applies this exact snapshot; Esc cancels. Changed targets require a new preview.")
	return title, lines
}
