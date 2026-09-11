package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

type IntervalActions interface {
	PreviewSchedule(context.Context, model.Task, schedule.Change) (store.IntervalPreview, error)
	PreviewCreateInterval(context.Context, app.Draft) (store.IntervalPreview, error)
	ApplyInterval(context.Context, store.IntervalPreview, bool) (store.TaskMutationOutcome, error)
}

func intervalHelp() []string {
	return []string{"Planned interval: start + elapsed duration; end is due, not focus time",
		"Start: 14:00 / tmr 14:00 / RFC3339; Duration: 1h30min or 90min",
		"Zone: IANA; DST gaps reject, repeated clocks need an explicit offset",
		"Choose Start/Duration or Date/Remind at, not both; repeats/all-day are not editable here",
		"Ctrl+S: read-only review; s: Save or Save anyway; Esc: return to draft",
		"Review: j/k/arrows/PageUp/PageDown/Home/End scroll every warning",
		"Counts and save/cancel stay visible; review full start/end/zone before saving",
		"All cached open timed tasks across lists; future repeats/server completeness unchecked",
		"All-day and partial schedules are separate context; existing reminders stay unchanged",
		"Concurrent target or overlap changes refuse; Ctrl+S prepares a new review"}
}

type intervalForm struct {
	enabled               bool
	start, duration, zone searchInput
	now                   time.Time
	value                 dates.Interval
	err                   string
	review                *store.IntervalPreview
	offset                int
}

func newIntervalForm(zone string) *intervalForm {
	f := &intervalForm{now: time.Now()}
	f.zone.set(dates.DefaultIntervalZone(zone))
	return f
}
func (f *intervalForm) active() bool {
	return f.enabled && (len(f.start.value) != 0 || len(f.duration.value) != 0)
}

func (f *intervalForm) prefill(task model.Task) {
	if !schedule.EditableInterval(task) {
		return
	}
	loc, err := dates.IntervalZone(task.TimeZone)
	if err != nil {
		return
	}
	f.start.set(task.StartDate.In(loc).Format(time.RFC3339Nano))
	f.duration.set(dates.ElapsedLabel(task.DueDate.Sub(task.StartDate.Time)))
}
func (f *intervalForm) parse() {
	f.value, f.err, f.review = dates.Interval{}, "", nil
	if !f.active() {
		return
	}
	v, err := dates.ParseIntervalFields(string(f.start.value), string(f.duration.value), string(f.zone.value), f.now)
	if err != nil {
		f.err = err.Error()
		return
	}
	f.value = v
}
func (f *taskForm) intervalField() int {
	if f.interval == nil {
		return -1
	}
	base := 5
	if f.schedule != nil {
		base = 3
	}
	return f.field - base
}
func (m browserModel) intervalKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f := m.form.interval
	inputs := []*searchInput{&f.start, &f.duration, &f.zone}
	i := m.form.intervalField()
	if i >= 0 && i < len(inputs) {
		before := string(inputs[i].value)
		inputs[i].update(msg)
		if before != string(inputs[i].value) {
			f.enabled, f.now = true, time.Now()
			f.parse()
			m.form.err = ""
		}
	}
	return m, nil
}
func (m *browserModel) intervalPaste(text string) {
	f := m.form.interval
	if f == nil || f.review != nil {
		return
	}
	inputs := []*searchInput{&f.start, &f.duration, &f.zone}
	i := m.form.intervalField()
	if i >= 0 && i < len(inputs) {
		inputs[i].insertExact(text)
		f.enabled, f.now = true, time.Now()
		f.parse()
		m.form.err = ""
	}
}
func (m browserModel) previewInterval(change schedule.Change) (browserModel, tea.Cmd) {
	actions, ok := m.actions.(IntervalActions)
	if !ok {
		m.form.err = "Interval review is unavailable"
		return m, nil
	}
	f, ctx := m.form, m.ctx
	if f.interval == nil {
		f.interval = newIntervalForm("")
	}
	if f.interval.err != "" {
		f.err = f.interval.err
		return m, nil
	}
	f.err, f.discard = "", false
	if f.original != nil {
		original := *f.original
		return m.actionCommand("preview interval", func() actionFinished {
			p, err := actions.PreviewSchedule(ctx, original, change)
			return actionFinished{intervalPreview: p, err: err}
		})
	}
	d := f.draft()
	return m.actionCommand("preview interval", func() actionFinished {
		p, err := actions.PreviewCreateInterval(ctx, d)
		return actionFinished{intervalPreview: p, err: err}
	})
}
func (m browserModel) intervalReviewKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, key := m.form.interval, msg.String()
	switch key {
	case "esc", "n":
		f.review = nil
		m.form.quitAfter = false
	case "s":
		actions, ok := m.actions.(IntervalActions)
		if !ok {
			return m, nil
		}
		p, ctx := *f.review, m.ctx
		return m.actionCommand("apply interval", func() actionFinished {
			out, err := actions.ApplyInterval(ctx, p, true)
			return actionFinished{outcome: out, err: err}
		})
	default:
		lines := m.intervalReviewLines()
		f.offset = scroll(f.offset, key, max(1, m.height-9), len(lines))
	}
	return m, nil
}
func (m browserModel) intervalReviewLines() []string {

	var lines []string
	for _, line := range m.form.interval.review.Lines() {
		lines = append(lines, strings.Split(ansi.Hardwrap(line, max(1, m.width-4), true), "\n")...)
	}
	return lines
}
func (m browserModel) intervalReviewView() (string, []string) {
	f := m.form.interval
	lines := m.intervalReviewLines()
	rows := max(1, m.height-8)
	offset := min(f.offset, max(0, len(lines)-rows))
	head := []string{fmt.Sprintf("%d overlaps; cached only", len(f.review.Overlaps)), "s: Save anyway | Esc: cancel"}
	if len(f.review.Overlaps) == 0 {
		head[1] = "s: Save | Esc: cancel"
	}
	head = append(head, lines[offset:min(len(lines), offset+rows)]...)
	return fmt.Sprintf("Interval review %d/%d", offset+1, len(lines)), head
}
func (m browserModel) intervalInputView() (string, []string) {
	f := m.form.interval
	i := m.form.intervalField()
	inputs := []*searchInput{&f.start, &f.duration, &f.zone}
	names := []string{"Start", "Duration (h/min)", "Zone (IANA)"}
	width := max(1, m.width-4)
	lines := []string{"Field: " + names[i], inputs[i].view(width)}
	if !f.value.Start.IsZero() {
		loc, _ := dates.IntervalZone(f.value.Zone)
		lines = append(lines, "End: "+f.value.End.In(loc).Format("2006-01-02 15:04 -07:00"), "Ctrl+S: review before saving")
	} else if f.err != "" {
		lines = append(lines, wrapText(f.err, width)...)
	} else {
		lines = append(lines, "Start: tmr 14:00; duration: 1h30min", "Untouched fields keep dates")
	}
	return "Planned interval", lines
}
