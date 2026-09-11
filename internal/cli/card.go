package cli

import (
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

const (
	cardIndent = "    "
	cardLabel  = 10
)

func CardLines(t model.Task, project string, p Palette, now time.Time) []string {

	title := Wrap(escape(t.Title), Width-len(cardIndent))
	lines := []string{checkbox(t.Status) + " " + p.Bold(title[0])}
	for _, more := range title[1:] {
		lines = append(lines, cardIndent+p.Bold(more))
	}

	due := dates.Format(t.DueDate, t.IsAllDay, dates.Zone(t.TimeZone), now)
	lines = append(lines, field(p, "list", project, nil)...)
	lines = append(lines, field(p, "due", due, p.accentIf(isOverdue(due)))...)
	if !t.StartDate.IsZero() && !t.DueDate.IsZero() && !t.IsAllDay && t.DueDate.After(t.StartDate.Time) {
		lines = append(lines, field(p, "start", t.StartDate.String(), nil)...)
		lines = append(lines, field(p, "end/due", t.DueDate.String(), nil)...)
		lines = append(lines, field(p, "zone", t.TimeZone, nil)...)
		lines = append(lines, field(p, "planned", dates.ElapsedLabel(t.DueDate.Sub(t.StartDate.Time)), nil)...)
	}
	lines = append(lines, field(p, "priority", t.Priority.String(), p.accentIf(t.Priority == model.PriorityHigh))...)
	if t.ParentId != "" {
		lines = append(lines, field(p, "parent", t.ParentId, nil)...)
	}
	if len(t.ChildIds) > 0 {
		lines = append(lines, field(p, "children", strings.Join(t.ChildIds, ", "), nil)...)
	}
	if t.ColumnId != "" {
		lines = append(lines, field(p, "column", t.ColumnName+" ["+t.ColumnId+"]", nil)...)
	}
	if t.EstimatedDuration != 0 {
		lines = append(lines, field(p, "estimate", strconv.FormatInt(t.EstimatedDuration, 10)+" seconds", nil)...)
	}
	if t.EstimatedPomo != 0 {
		lines = append(lines, field(p, "est. pomo", strconv.Itoa(t.EstimatedPomo), nil)...)
	}
	if t.SortOrder != 0 {
		lines = append(lines, field(p, "sort", strconv.FormatInt(t.SortOrder, 10), nil)...)
	}
	if len(t.Tags) > 0 {
		lines = append(lines, field(p, "tags", strings.Join(t.Tags, ", "), nil)...)
	}
	if t.RepeatFlag != "" {
		lines = append(lines, field(p, "repeat", t.RepeatFlag, nil)...)
	}
	for _, reminder := range t.Reminders {
		lines = append(lines, field(p, "reminder", reminder, nil)...)
	}

	if len(t.Items) > 0 {
		lines = append(lines, "")
		for i, item := range t.Items {

			position := strconv.Itoa(i+1) + ". "
			prefix := cardIndent + position + itemBox(item.Status) + " "
			wrapped := Wrap(escape(item.Title), Width-len(prefix))
			lines = append(lines, prefix+wrapped[0])
			for _, more := range wrapped[1:] {
				lines = append(lines, strings.Repeat(" ", len(prefix))+more)
			}
		}
	}

	if body := strings.TrimSpace(t.Content); body != "" {
		lines = append(lines, "")

		for _, line := range Wrap(escapeBlock(body), Width-len(cardIndent)) {

			if line == "" {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, cardIndent+line)
		}
	}
	return lines
}

func field(p Palette, label, value string, style styler) []string {

	value = escape(value)
	if strings.TrimSpace(value) == "" {
		value = "--"
	}
	hang := cardIndent + strings.Repeat(" ", cardLabel)
	wrapped := Wrap(value, Width-DisplayWidth(hang))
	styled := func(s string) string {
		if style == nil {
			return s
		}
		return style(s)
	}
	lines := []string{cardIndent + pad(label, cardLabel, p.Dim) + styled(wrapped[0])}
	for _, more := range wrapped[1:] {
		lines = append(lines, hang+styled(more))
	}
	return lines
}

func itemBox(s model.ItemStatus) string {
	if s.Done() {
		return "[x]"
	}
	return "[ ]"
}
