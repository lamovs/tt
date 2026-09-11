package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type Row struct {
	Num     int
	Task    model.Task
	Project string
}

func Rows(tasks []model.Task, projects map[string]string) []Row {
	rows := make([]Row, len(tasks))
	for i, t := range tasks {
		rows[i] = Row{Num: i + 1, Task: t, Project: projects[t.ProjectId]}
	}
	return rows
}

func TaskIDs(tasks []model.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.Id
	}
	return ids
}

func ProjectNames(ctx context.Context, st *store.Store) (map[string]string, error) {
	projects, err := st.Projects(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.Id] = p.Name
	}
	return names, nil
}

const (
	gapNum   = 2
	gapBox   = 1
	gapPri   = 2
	gapTitle = 2
	gapDue   = 2

	boxWidth = 3
	priWidth = 2

	minTitle   = 20
	minProject = 8

	maxProject = Width / 4
)

func ListLines(rows []Row, p Palette, now time.Time) []string {
	if len(rows) == 0 {
		return nil
	}
	dues := make([]string, len(rows))
	titles := make([]string, len(rows))
	projects := make([]string, len(rows))
	for i, r := range rows {
		dues[i] = dates.Format(r.Task.DueDate, r.Task.IsAllDay, dates.Zone(r.Task.TimeZone), now)

		titles[i] = escape(r.Task.Title)

		projects[i] = escape(r.Project)
	}
	lay := measure(rows, titles, projects, dues)
	for i, r := range rows {

		if DisplayWidth(titles[i]) > lay.title {
			titles[i] = escapeCell(r.Task.Title, lay.title)
		}
		if DisplayWidth(projects[i]) > lay.project {
			projects[i] = escapeCell(r.Project, lay.project)
		}
	}

	lines := make([]string, len(rows))
	for i, r := range rows {
		var b strings.Builder
		fmt.Fprintf(&b, "%*d%s", lay.num, r.Num, strings.Repeat(" ", gapNum))
		b.WriteString(checkbox(r.Task.Status))
		b.WriteString(strings.Repeat(" ", gapBox))
		b.WriteString(pad(priorityMark(r.Task.Priority), priWidth, p.accentIf(r.Task.Priority == model.PriorityHigh)))
		b.WriteString(strings.Repeat(" ", gapPri))
		b.WriteString(pad(titles[i], lay.title, nil))
		b.WriteString(strings.Repeat(" ", gapTitle))
		b.WriteString(pad(dues[i], lay.due, p.accentIf(isOverdue(dues[i]))))
		b.WriteString(strings.Repeat(" ", gapDue))
		b.WriteString(projects[i])

		lines[i] = strings.TrimRight(b.String(), " ")
	}
	return lines
}

type layout struct {
	num, title, due, project int
}

func measure(rows []Row, titles, projects, dues []string) layout {
	lay := layout{}
	titleMax := 0
	for i, r := range rows {
		lay.num = max(lay.num, len(strconv.Itoa(r.Num)))
		lay.due = max(lay.due, DisplayWidth(dues[i]))
		lay.project = max(lay.project, DisplayWidth(projects[i]))
		titleMax = max(titleMax, DisplayWidth(titles[i]))
	}

	lay.project = min(lay.project, maxProject)
	fixed := lay.num + gapNum + boxWidth + gapBox + priWidth + gapPri + gapTitle + lay.due + gapDue
	room := Width - fixed - lay.project
	if room < minTitle {
		if give := min(minTitle-room, lay.project-minProject); give > 0 {
			lay.project -= give
			room += give
		}
	}
	lay.title = min(titleMax, room)
	if lay.title < 1 {

		lay.title = 1
	}
	return lay
}

func checkbox(s model.TaskStatus) string {
	if s.Done() {
		return "[x]"
	}
	return "[ ]"
}

func priorityMark(p model.Priority) string {
	switch p {
	case model.PriorityHigh:
		return "!!"
	case model.PriorityMedium:
		return "!"
	default:
		return ""
	}
}

func isOverdue(due string) bool { return strings.HasPrefix(due, "overdue") }
