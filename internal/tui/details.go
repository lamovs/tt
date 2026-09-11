package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

func wrapText(text string, width int) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, strings.Split(ansi.Hardwrap(display(line), max(1, width), true), "\n")...)
	}
	return lines
}

func (m browserModel) fullDetailLines(width int) []string {
	t := *m.detail
	project := t.ProjectId
	for _, p := range m.projects {
		if p.Id == t.ProjectId {
			project = p.Name
			break
		}
	}
	lines := strings.Split(ansi.Hardwrap(display(t.Title), max(1, width), true), "\n")
	field := func(label, value string) {

		line := label + ": " + display(value)
		lines = append(lines, strings.Split(ansi.Hardwrap(line, max(1, width), true), "\n")...)
	}
	field("ID", t.Id)
	field("List", project)
	field("Kind", t.Kind)
	field("Status", t.Status.String())
	field("Priority", t.Priority.String())
	if t.ParentId != "" {
		field("Parent", t.ParentId)
	}
	if len(t.ChildIds) > 0 {
		field("Children", strings.Join(t.ChildIds, ", "))
	}
	if t.ColumnId != "" {
		field("Column", t.ColumnName+" ["+t.ColumnId+"]")
	}
	if t.EstimatedDuration != 0 {
		field("Estimated seconds", fmt.Sprint(t.EstimatedDuration))
	}
	if t.EstimatedPomo != 0 {
		field("Estimated Pomodoros", fmt.Sprint(t.EstimatedPomo))
	}
	if t.SortOrder != 0 {
		field("Sort order", fmt.Sprint(t.SortOrder))
	}
	field("Due", m.detailDue)
	field("Due value", stamp(t.DueDate))
	field("Start", stamp(t.StartDate))
	field("All day", fmt.Sprint(t.IsAllDay))
	field("Timezone", t.TimeZone)
	if schedule.TimedInterval(t) {
		field("Planned duration", dates.ElapsedLabel(t.DueDate.Sub(t.StartDate.Time)))
	}
	if len(t.Tags) > 0 {
		field("Tags", strings.Join(t.Tags, ", "))
	}
	if t.RepeatFlag != "" {
		field("Repeat", t.RepeatFlag)
	}
	for i, reminder := range t.Reminders {
		if reminder == "" {
			reminder = "(empty)"
		}
		field(fmt.Sprintf("Reminder %d", i+1), reminder)
	}
	for _, item := range schedule.InterpretReminders(t) {
		if item.Delivery == "legacy_allowlist" {
			continue
		}
		if item.Computable {
			field("Reminder time", item.At.String())
		}
		field("Reminder delivery", item.Delivery+": "+item.Reason)
	}
	if !t.CreatedTime.IsZero() {
		field("Created", stamp(t.CreatedTime))
	}
	if !t.ModifiedTime.IsZero() {
		field("Modified", stamp(t.ModifiedTime))
	}
	if !t.CompletedTime.IsZero() {
		field("Completed", stamp(t.CompletedTime))
	}
	if len(t.Items) > 0 {
		lines = append(lines, "", "Checklist:")
		for i, item := range t.Items {
			box := "[ ]"
			if item.Status.Done() {
				box = "[x]"
			}
			field(fmt.Sprintf("%d %s", i+1, box), item.Title)
			if !item.StartDate.IsZero() {
				field("  Start", stamp(item.StartDate))
			}
			if item.TimeZone != "" {
				field("  Timezone", item.TimeZone)
			}
			if item.IsAllDay {
				field("  All day", "true")
			}
			if !item.CompletedTime.IsZero() {
				field("  Completed", stamp(item.CompletedTime))
			}
		}
	}
	lines = append(lines, "", "Body:")
	return append(lines, wrapText(t.Content, width)...)
}

func stamp(value model.Time) string {
	if value.IsZero() {
		return "none"
	}
	return value.String()
}
