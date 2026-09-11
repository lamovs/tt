package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

type scheduleForm struct {
	date, repeat     searchInput
	reminders        []searchInput
	row              int
	parsed           *dates.Result
	dateError        string
	preview          []string
	current          []string
	initialRepeat    string
	initialReminders []string
}

func reminderValues(rows []searchInput) []string {
	values := make([]string, len(rows))
	for i := range rows {
		values[i] = string(rows[i].value)
	}
	return values
}

func (f *scheduleForm) dirty() bool {
	return len(f.date.value) != 0 || string(f.repeat.value) != f.initialRepeat || !slices.Equal(reminderValues(f.reminders), f.initialReminders)
}

func datePreview(result dates.Result) []string {
	if result.Clear {
		return []string{"Clear start + due dates", "All-day: off; other fields kept"}
	}
	if result.AllDay {
		return []string{result.Time.Format("2006-01-02") + " (all-day)", display(result.Time.Format("MST -07:00")) + " (local)"}
	}
	return []string{result.Time.Format("2006-01-02 15:04:05.000"), display(result.Time.Format("MST -07:00")) + " (local, timed)"}
}

func (f *scheduleForm) parseDate(now time.Time) {
	f.parsed, f.dateError, f.preview = nil, "", []string{"Keep existing dates and timezone"}
	if len(f.date.value) == 0 {
		return
	}
	result, err := dates.Parse(string(f.date.value), now)
	if err != nil {
		f.dateError = err.Error()
		f.preview = []string{"Invalid date; cannot save"}
		return
	}
	f.parsed, f.preview = &result, datePreview(result)
}

func (m *browserModel) openSchedule(original model.Task) {
	f := &scheduleForm{initialRepeat: original.RepeatFlag, initialReminders: nil}
	if f.initialRepeat == "" {
		f.initialRepeat = "none"
	}
	for _, preset := range []string{"daily", "weekly", "monthly", "yearly"} {
		rule, _ := schedule.ParseRepeat(preset)
		if rule == f.initialRepeat {
			f.initialRepeat = preset
			break
		}
	}
	f.repeat.set(f.initialRepeat)
	for _, value := range original.Reminders {
		var input searchInput
		input.set(schedule.ReminderInput(value))
		f.initialReminders = append(f.initialReminders, string(input.value))
		f.reminders = append(f.reminders, input)
	}
	f.current = []string{"ID: " + original.Id, "Start: " + original.StartDate.String(), "Due: " + original.DueDate.String(), "Zone: " + original.TimeZone, fmt.Sprintf("All-day: %t", original.IsAllDay)}
	for i := range f.current {
		f.current[i] = display(f.current[i])
	}
	f.parseDate(time.Now())
	m.form = &taskForm{original: &original, schedule: f, interval: newIntervalForm(original.TimeZone)}
	m.form.interval.prefill(original)
	m.notice = "Schedule: edit fields, review the date preview, Ctrl+S saves locally."
}

func (f *scheduleForm) change() (schedule.Change, error) {
	change := schedule.Change{Due: f.parsed}
	if f.dateError != "" {
		return change, fmt.Errorf("%s", f.dateError)
	}
	if string(f.repeat.value) != f.initialRepeat {
		repeat, err := schedule.ParseRepeat(string(f.repeat.value))
		if err != nil {
			return change, err
		}
		change.Repeat = &repeat
	}
	values := reminderValues(f.reminders)
	if !slices.Equal(values, f.initialReminders) {
		reminders, err := schedule.ParseReminders(values)
		if err != nil {
			return change, err
		}
		change.Reminders = &reminders
	}
	return change, nil
}

func (m browserModel) saveSchedule() (browserModel, tea.Cmd) {
	f := m.form
	change, err := f.schedule.change()
	if err != nil {
		f.err = err.Error()
		return m, nil
	}
	if f.interval != nil && f.interval.active() {
		if f.interval.err != "" {
			f.err = f.interval.err
			return m, nil
		}
		change.Interval = &f.interval.value
	}
	edit, err := change.Edit(*f.original)
	if err != nil {
		f.err = err.Error()
		return m, nil
	}
	if change.Interval != nil || schedule.ReviewsInterval(*f.original, edit) {
		return m.previewInterval(change)
	}
	f.err, f.discard = "", false
	original, actions, ctx := *f.original, m.actions, m.ctx
	return m.actionCommand("schedule", func() actionFinished {
		out, err := actions.Schedule(ctx, original, change)
		return actionFinished{outcome: out, err: err}
	})
}

func (m browserModel) scheduleKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, key := m.form.schedule, msg.String()
	switch m.form.field {
	case 0:
		before := string(f.date.value)
		if key == "ctrl+x" {
			f.date.set("none")
		} else {
			f.date.update(msg)
		}
		if before != string(f.date.value) {
			f.parseDate(time.Now())
			m.form.err = ""
		}
	case 1:
		switch key {
		case "ctrl+x":
			f.repeat.set("none")
		case "ctrl+p":
			presets := []string{"none", "daily", "weekly", "monthly", "yearly"}
			index := slices.Index(presets, string(f.repeat.value))
			f.repeat.set(presets[(index+1)%len(presets)])
		default:
			f.repeat.update(msg)
		}
	case 2:
		switch key {
		case "ctrl+n":
			f.reminders = append(f.reminders, searchInput{})
			f.row = len(f.reminders) - 1
		case "ctrl+x":
			f.reminders, f.row = nil, 0
		case "ctrl+d":
			if len(f.reminders) > 0 {
				f.reminders = slices.Delete(f.reminders, f.row, f.row+1)
				f.row = min(f.row, max(0, len(f.reminders)-1))
			}
		case "up":
			f.row = max(0, f.row-1)
		case "down":
			f.row = min(f.row+1, max(0, len(f.reminders)-1))
		case "alt+up", "alt+down":
			next := f.row - 1
			if key == "alt+down" {
				next = f.row + 1
			}
			if next >= 0 && next < len(f.reminders) {
				f.reminders[f.row], f.reminders[next] = f.reminders[next], f.reminders[f.row]
				f.row = next
			}
		default:
			if len(f.reminders) > 0 {
				f.reminders[f.row].update(msg)
			}
		}
	}
	return m, nil
}

func (m *browserModel) schedulePaste(text string) {
	f := m.form.schedule
	switch m.form.field {
	case 0:
		f.date.insertExact(text)
		f.parseDate(time.Now())
	case 1:
		f.repeat.insertExact(text)
	case 2:
		if len(f.reminders) > 0 {
			f.reminders[f.row].insertExact(text)
		}
	}
}

func (m browserModel) scheduleView() (string, []string) {
	f, width := m.form.schedule, max(1, m.width-4)
	var lines []string
	switch m.form.field {
	case 0:
		lines = []string{"Field: Date", f.date.view(width)}
		lines = append(lines, f.preview...)
		if m.height >= 16 {
			lines = append(lines, "in 2h / +2h / fri 18:00 / tmr", "No time: all-day. Empty: keep. none: clear.")
			lines = append(lines, f.current...)
		}
	case 1:
		lines = []string{"Field: Repeat", f.repeat.view(width), "Ctrl+P: next preset", "none clears; raw RRULE: allowed"}
		if m.height >= 16 {
			lines = append(lines, "daily / weekly / monthly / yearly", "Raw grammar is checked by the API during sync.")
		}
	case 2:
		lines = []string{fmt.Sprintf("Field: Reminders (%d)", len(f.reminders))}
		if len(f.reminders) == 0 {
			lines = append(lines, "No reminders. Ctrl+N adds one.")
		}
		rows := max(1, m.height-10)
		start := max(0, f.row-rows+1)
		for i := start; i < min(len(f.reminders), start+rows); i++ {
			prefix := fmt.Sprintf("  %d ", i+1)
			value := display(string(f.reminders[i].value))
			if i == f.row {
				prefix = fmt.Sprintf("> %d ", i+1)
				value = f.reminders[i].view(max(1, width-len(prefix)))
			}
			lines = append(lines, prefix+value)
		}
		lines = append(lines, "at / -10min / -1h / TRIGGER:", "Up/down select; Alt+up/down move")
	}
	for i := range lines {
		lines[i] = fit(lines[i], width)
	}
	return "Schedule: " + display(m.form.original.Title), lines
}

func (f *taskForm) parseQuickReminder(now time.Time) {
	f.quickDate, f.quickError, f.quickPreview = dates.Result{}, "", []string{"No reminder shortcut"}
	if strings.TrimSpace(string(f.quick.value)) == "" {
		return
	}
	result, err := schedule.ReminderDate(string(f.quick.value), now)
	if err != nil {
		f.quickError = err.Error()
		f.quickPreview = []string{"Invalid reminder time; cannot save"}
		return
	}
	f.quickDate, f.quickPreview = result, datePreview(result)
}
