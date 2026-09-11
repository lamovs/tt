package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

type taskForm struct {
	interval           *intervalForm
	schedule           *scheduleForm
	kind               string
	quick              searchInput
	quickDate          dates.Result
	quickError         string
	quickPreview       []string
	original           *model.Task
	title, body        searchInput
	priority           model.Priority
	projects           []model.Project
	project            int
	field              int
	initial            app.Draft
	discard, quitAfter bool
	err                string
}

func (f *taskForm) draft() app.Draft {
	project := ""
	if f.original != nil {
		project = f.original.ProjectId
	} else if f.project >= 0 && f.project < len(f.projects) {
		project = f.projects[f.project].Id
	}
	d := app.Draft{Kind: f.kind, ProjectID: project, Title: string(f.title.value), Body: string(f.body.value), Priority: f.priority, ReminderWhen: strings.TrimSpace(string(f.quick.value)), ReminderDate: f.quickDate}
	if f.interval != nil {
		d.Interval = f.interval.value
	}
	return d
}
func (f *taskForm) dirty() bool {
	if f.interval != nil && f.interval.active() {
		return true
	}
	if f.schedule != nil {
		return f.schedule.dirty()
	}
	return f.draft() != f.initial
}

func (m *browserModel) openCreate() { m.openCreateKind("TEXT") }

func (m *browserModel) openCreateKind(kind string) {
	f := &taskForm{kind: kind, project: -1, interval: newIntervalForm("")}
	for _, p := range m.projects {
		if p.CreateUnavailable() == "" {
			f.projects = append(f.projects, p)
		}
	}
	selected, resolveErr := cli.ResolveDefaultProject(m.projects, m.defaultProject)
	if m.query.ProjectID != "" {
		selected = model.Project{Id: m.query.ProjectID}
	}
	for i, p := range f.projects {
		if p.Id == selected.Id {
			f.project = i
			break
		}
	}
	if f.project < 0 {
		f.err = "Choose a cached list explicitly; no default target is available."
		if m.query.ProjectID == "" && resolveErr != nil {
			f.err = resolveErr.Error()
		}
	}
	f.parseQuickReminder(time.Now())
	f.initial = f.draft()
	m.form, m.notice = f, "New task: choose a list, enter a title and save locally."
	if kind == "NOTE" {
		m.notice = "New note: choose a list, enter a title and save locally."
	}
}
func (m *browserModel) openEdit(original model.Task) {
	f := &taskForm{original: &original, priority: original.Priority}
	f.title.set(original.Title)
	f.body.set(original.Content)
	f.initial = f.draft()
	m.form, m.notice = f, "Edit the selected task. Ctrl+S saves locally."
}

func (m browserModel) saveForm() (browserModel, tea.Cmd) {
	f := m.form
	if f.schedule != nil {
		return m.saveSchedule()
	}
	if f.quickError != "" {
		f.err = f.quickError
		return m, nil
	}
	d := f.draft()
	if f.interval != nil && f.interval.active() {
		return m.previewInterval(schedule.Change{})
	}
	if err := d.Validate(); err != nil {
		f.err = err.Error()
		return m, nil
	}
	if d.ProjectID == "" {
		f.err = "Choose a cached list; sync first if the cache is empty."
		return m, nil
	}
	f.err, f.discard = "", false
	actions, ctx := m.actions, m.ctx
	if f.original == nil {
		return m.actionCommand("create", func() actionFinished {
			out, err := actions.Create(ctx, d)
			return actionFinished{outcome: out, err: err}
		})
	}
	original := *f.original
	return m.actionCommand("edit", func() actionFinished {
		out, err := actions.Edit(ctx, original, d)
		return actionFinished{outcome: out, err: err}
	})
}

func (m browserModel) formKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, s := m.form, msg.String()
	if !f.discard && f.interval != nil && f.interval.review != nil {
		return m.intervalReviewKey(msg)
	}
	if f.discard {
		switch s {
		case "s", "ctrl+s":
			return m.saveForm()
		case "d":
			quit := f.quitAfter
			m.form, m.notice = nil, "Draft discarded; no local mutation."
			if quit {
				return m, tea.Quit
			}
		case "esc":
			f.discard, f.quitAfter = false, false
		}
		return m, nil
	}
	fields := 5
	if f.original != nil {
		fields = 3
	}
	if f.interval != nil {
		fields += 3
	}
	switch s {
	case "esc":
		if f.dirty() {
			f.discard = true
		} else {
			m.form = nil
			m.notice = "Unchanged; no local mutation."
		}
	case "ctrl+s":
		return m.saveForm()
	case "ctrl+e":
		if f.schedule == nil {
			return m.beginEditor()
		}
		return m.scheduleKey(msg)
	case "tab":
		f.field = (f.field + 1) % fields
	case "shift+tab":
		f.field = (f.field + fields - 1) % fields
	default:
		if f.intervalField() >= 0 {
			return m.intervalKey(msg)
		}
		if f.schedule != nil {
			return m.scheduleKey(msg)
		}
		switch f.field {
		case 0:
			if s == "enter" {
				f.field = 1
			} else {
				f.title.update(msg)
			}
		case 1:
			f.body.updateBody(msg)
		case 2:
			options := []model.Priority{model.PriorityNone, model.PriorityLow, model.PriorityMedium, model.PriorityHigh}
			i := 0
			for n, p := range options {
				if p == f.priority {
					i = n
				}
			}
			switch s {
			case "right", "down", "space":
				f.priority = options[(i+1)%len(options)]
			case "left", "up":
				f.priority = options[(i+len(options)-1)%len(options)]
			case "enter":
				f.field = (f.field + 1) % fields
			}
		case 4:
			before := string(f.quick.value)
			f.quick.update(msg)
			if before != string(f.quick.value) {
				f.parseQuickReminder(time.Now())
				f.err = ""
			}
		case 3:
			if len(f.projects) > 0 {
				switch s {
				case "right", "down", "space":
					f.project = (f.project + 1) % len(f.projects)
					f.err = ""
				case "left", "up":
					f.err = ""
					if f.project < 0 {
						f.project = len(f.projects) - 1
					} else {
						f.project = (f.project + len(f.projects) - 1) % len(f.projects)
					}
				case "enter":
					f.field = 4
				}
			}
		}
	}
	return m, nil
}

func (s *searchInput) insertExact(text string) {
	chars := []rune(text)
	value := append([]rune(nil), s.value[:s.pos]...)
	value = append(value, chars...)
	s.value = append(value, s.value[s.pos:]...)
	s.pos += len(chars)
}

func (s *searchInput) updateBody(msg tea.KeyPressMsg) {
	start, end := s.pos, s.pos
	for start > 0 && s.value[start-1] != '\n' {
		start--
	}
	for end < len(s.value) && s.value[end] != '\n' {
		end++
	}
	switch msg.String() {
	case "enter":
		s.insertExact("\n")
	case "home":
		s.pos = start
	case "end":
		s.pos = end
	case "up":
		if start > 0 {
			previous := start - 1
			for previous > 0 && s.value[previous-1] != '\n' {
				previous--
			}
			s.pos = min(previous+s.pos-start, start-1)
		}
	case "down":
		if end < len(s.value) {
			nextEnd := end + 1
			for nextEnd < len(s.value) && s.value[nextEnd] != '\n' {
				nextEnd++
			}
			s.pos = min(end+1+s.pos-start, nextEnd)
		}
	default:
		s.update(msg)
	}
}

func (m browserModel) formView() (string, []string) {
	f, width := m.form, max(1, m.width-4)
	if !f.discard && f.interval != nil && f.interval.review != nil {
		return m.intervalReviewView()
	}
	title := "Create task"
	if f.original != nil {
		title = "Edit task"
	}
	if f.kind == "NOTE" {
		title = "Create note (NOTE)"
	}
	if f.original != nil && f.original.Kind == "NOTE" {
		title = "Edit note (NOTE)"
	}
	if f.discard {
		return "Unsaved changes", wrapText("Save locally, discard the draft, or continue editing.\ns: save | d: discard | Esc: continue", width)
	}
	if f.intervalField() >= 0 {
		return m.intervalInputView()
	}
	if f.schedule != nil {
		return m.scheduleView()
	}
	list := ""
	if f.original != nil {
		list = f.original.ProjectId
	} else if f.project >= 0 && f.project < len(f.projects) {
		list = f.projects[f.project].Id
	}
	for _, p := range m.projects {
		if p.Id == list {
			list = p.Name + " [" + p.Id + "]"
			break
		}
	}

	fields := []string{"Title", "Body", "Priority", "List", "Remind at"}
	head := []string{fit("List: "+display(list), width), "Field: " + fields[f.field] + " (Tab switches)"}
	var content []string
	switch f.field {
	case 0:
		content = []string{f.title.view(width)}
	case 1:
		content = f.body.bodyView(width, max(1, m.height-8))

	case 2:
		content = []string{f.priority.String(), "Left/right: none / low / med / high"}
	case 4:

		lines := append([]string{"Field: Remind at", f.quick.view(width)}, f.quickPreview...)
		if m.height >= 16 {
			lines = append(lines, "in 2h / +2h / tmr 09:00 / 18:00", "Sets the due time and a reminder at that time.", "Empty leaves the new task without a date or reminder.")
		}
		return title, lines
	case 3:
		content = wrapText(list+"\nLeft/right: choose cached list", width)
	}
	if m.height >= 16 {
		head = append(head, fit("Title: "+f.title.summary(width), width), "Priority: "+f.priority.String())
		if f.field == 1 {
			content = f.body.bodyView(width, max(1, m.height-10))
		}
	}
	return title, append(head, content...)
}

func (m browserModel) dialogView() (string, []string) {
	d, width := m.dialog, max(1, m.width-4)
	if m.busy {
		return "Working", []string{"Waiting for the local operation..."}
	}
	if d.kind == "column move" {
		return m.columnDialogView()
	}
	if d.kind == "undo" {
		e := d.undo.Entry
		if e.Action.EntityRef != nil {
			ref := *e.Action.EntityRef
			return "Global undo", wrapText(fmt.Sprintf("Cancel unsent resource operation: %d\nResource: %s\nKind: %s\nKey: %s\nOnly this queued local mutation is canceled.\nNo remote reversal or new request is made.\nEnter confirms | Esc cancels", e.Action.OperationSeq, d.undo.Title, ref.Kind, ref.Key), width)
		}
		return "Global undo", wrapText(fmt.Sprintf("Action: %s\nTask: %s\nID: %s\nApplies to global history, including CLI changes.\nRemote rollback is not promised.\nEnter confirms | Esc cancels", e.Action.Op, d.undo.Title, e.Action.TaskID), width)
	}
	if d.kind != "complete" {
		return "Task confirmation", []string{"Unknown confirmation; Esc closes without changes."}
	}
	count := 0
	for _, item := range d.original.Items {
		if !item.Status.Done() {
			count++
		}
	}
	policy := "Complete checklist items too"
	if d.keepItems {
		policy = "Leave checklist items unchanged"
	}
	return "Complete task", wrapText(fmt.Sprintf("%s\nID: %s\nOpen checklist items: %d\n%s\nTab changes checklist policy\nEnter confirms | Esc cancels", d.original.Title, d.original.Id, count, policy), width)
}
