package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
)

func TestScheduleFormSavesOneAtomicChangeWithFrozenPreview(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'd', 0)
	if m.form == nil || m.form.schedule == nil || m.form.dirty() {
		t.Fatal("schedule not opened cleanly")
	}
	m, _ = step(m, tea.PasteMsg{Content: "+2h"})
	date := *m.form.schedule.parsed
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, 'u', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "weekly"})
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, 'n', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "-1h"})
	m, _ = press(m, 'n', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "at"})
	m, _ = press(m, tea.KeyUp, tea.ModAlt)
	if !slices.Equal(reminderValues(m.form.schedule.reminders), []string{"at", "-1h"}) {
		t.Fatal("reorder failed")
	}
	before := rowsIn(t, st, "events")
	m, cmd := press(m, 's', tea.ModCtrl)
	oldGeneration := m.actionGeneration - 1
	m, _ = step(m, actionFinished{generation: oldGeneration, kind: "schedule"})
	if !m.busy || m.form == nil {
		t.Fatal("late response closed draft")
	}
	if _, again := press(m, 's', tea.ModCtrl); again != nil {
		t.Fatal("duplicate submit")
	}
	m = finishLocal(t, m, cmd)
	got, err := st.Task(m.ctx, m.taskID)
	if err != nil || !got.DueDate.Equal(date.Time.Time) || got.IsAllDay || got.RepeatFlag != "RRULE:FREQ=WEEKLY;INTERVAL=1" || !slices.Equal(got.Reminders, []string{"TRIGGER:PT0S", "TRIGGER:-PT1H"}) {
		t.Fatalf("save: %+v %v", got, err)
	}
	if rowsIn(t, st, "events") != before+1 || !strings.Contains(m.notice, "Saved locally (schedule)") {
		t.Fatal("not one atomic local save")
	}
	m, _ = press(m, 'd', 0)
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "events") != before+1 {
		t.Fatal("unchanged schedule mutated")
	}
}

func TestScheduleInvalidConflictClearAndUndo(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'd', 0)
	m, _ = step(m, tea.PasteMsg{Content: "in 0h"})
	before := rowsIn(t, st, "events")
	m, cmd := press(m, 's', tea.ModCtrl)
	if cmd != nil || m.form.err == "" || rowsIn(t, st, "events") != before {
		t.Fatal("invalid date saved")
	}
	m, _ = press(m, 'u', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "in 3h"})
	original := *m.form.original
	if _, err := st.UpdateTask(m.ctx, original.Id, model.TaskEdit{Title: model.Ptr("External CLI title")}); err != nil {
		t.Fatal(err)
	}
	m, cmd = m.load()
	m = settle(t, m, cmd)
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form == nil || m.form.err == "" || string(m.form.schedule.date.value) != "in 3h" || m.form.original.Title != original.Title {
		t.Fatal("conflict lost frozen draft")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'd', 0)
	m, _ = press(m, 'd', 0)
	m, _ = press(m, 'x', tea.ModCtrl)
	if !strings.Contains(strings.Join(m.form.schedule.preview, " "), "start + due") {
		t.Fatal("clear preview missing coupled fields")
	}
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form != nil {
		t.Fatal("clear failed")
	}
	m, _ = press(m, 'd', 0)
	m, _ = step(m, tea.PasteMsg{Content: "tmr"})
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	m, cmd = press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	got, err := st.Task(m.ctx, original.Id)
	if err != nil || !got.DueDate.IsZero() || got.Title != "External CLI title" {
		t.Fatalf("global undo schedule: %+v %v", got, err)
	}
}

func TestQuickReminderCreatePreviewAndUndersizedRefusal(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'a', 0)
	m, _ = step(m, tea.PasteMsg{Content: "Call"})
	for i := 0; i < 4; i++ {
		m, _ = press(m, tea.KeyTab, 0)
	}
	m, _ = step(m, tea.PasteMsg{Content: "in 1h30min"})
	date := m.form.quickDate
	if date.Time.IsZero() || time.Until(date.Time.Time) < 89*time.Minute {
		t.Fatal("missing quick preview")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 32, Height: 10})
	_, lines := m.formView()
	if len(lines) != 4 || !strings.Contains(lines[3], "local, timed") {
		t.Fatalf("preview hidden: %q", lines)
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 20, Height: 8})
	before := rowsIn(t, st, "tasks")
	if _, cmd := press(m, 's', tea.ModCtrl); cmd != nil {
		t.Fatal("hidden quick submit")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 140, Height: 32})
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "tasks") != before+1 {
		t.Fatal("quick create failed")
	}
	var due, reminders string
	if err := st.DB().QueryRow("SELECT due_date,reminders FROM tasks WHERE title='Call'").Scan(&due, &reminders); err != nil {
		t.Fatal(err)
	}
	parsed, err := model.ParseTime(due)
	if err != nil || !parsed.Equal(date.Time.Time) || reminders != `["TRIGGER:PT0S"]` {
		t.Fatalf("preview drift: %q %q %v", due, reminders, err)
	}
}

func TestScheduleUntouchedUnsupportedAndDuplicateRows(t *testing.T) {
	m, st := writableModel(t)
	_, err := st.UpdateTask(m.ctx, m.taskID, model.TaskEdit{RepeatFlag: model.Ptr("vendor-repeat"), Reminders: model.NewEditList([]string{"vendor-trigger", "vendor-trigger"})})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	m, _ = press(m, 'd', 0)
	m, _ = step(m, tea.PasteMsg{Content: "tmr"})
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form != nil {
		t.Fatal("untouched unsupported values blocked unrelated date")
	}
	got, err := st.Task(m.ctx, m.taskID)
	if err != nil || got.RepeatFlag != "vendor-repeat" || !slices.Equal(got.Reminders, []string{"vendor-trigger", "vendor-trigger"}) {
		t.Fatalf("rewrote unsupported: %+v %v", got, err)
	}
	m, _ = press(m, 'd', 0)
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, 'x', tea.ModCtrl)
	for i := 0; i < 2; i++ {
		m, _ = press(m, 'n', tea.ModCtrl)
		m, _ = step(m, tea.PasteMsg{Content: "at"})
	}
	before := rowsIn(t, st, "events")
	m, cmd = press(m, 's', tea.ModCtrl)
	if cmd != nil || !strings.Contains(m.form.err, "duplicate") || rowsIn(t, st, "events") != before {
		t.Fatal("duplicate reminders accepted")
	}
}

func TestScheduleViewEscapesRawFieldsAndDoesNoIO(t *testing.T) {
	m, st := writableModel(t)
	original := *m.detail
	original.TimeZone = "UTC\x1b]52;c;bad\a"
	original.Id += "\x1b[2J"
	original.RepeatFlag = "RRULE:x\x1b]52;c;bad\a"
	original.Reminders = []string{"TRIGGER:x\x1b]52;c;bad\a"}
	m.openSchedule(original)
	m.width, m.height = 140, 32
	before := rowsIn(t, st, "events")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for field := 0; field < 3; field++ {
		m.form.field = field
		for i := 0; i < 3; i++ {
			_, lines := m.formView()
			rendered := strings.Join(lines, "\n")
			if strings.ContainsAny(rendered, "\x1b\a") {
				t.Fatalf("terminal control leaked: %q", rendered)
			}
			_ = m.View()
		}
	}
	if before != 1 {
		t.Fatal("unexpected fixture mutation")
	}
}
