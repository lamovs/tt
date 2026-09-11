package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
)

func TestIntervalFormReviewCancelStaleAndSave(t *testing.T) {
	m, st := writableModel(t)
	m, _ = press(m, 'd', 0)
	m.form.field = 3
	m, _ = step(m, tea.PasteMsg{Content: "2026-09-10 14:00"})
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = step(m, tea.PasteMsg{Content: "90min"})
	m, _ = press(m, tea.KeyTab, 0)
	m, _ = press(m, 'u', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "UTC"})
	before := rowsIn(t, st, "events")
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.form == nil || m.form.interval.review == nil || rowsIn(t, st, "events") != before {
		t.Fatal("preview mutated or missing")
	}
	if _, cmd := press(m, tea.KeyEnter, 0); cmd != nil {
		t.Fatal("Enter silently saved")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.form == nil || m.form.interval.review != nil || !m.form.interval.active() {
		t.Fatal("cancel lost draft")
	}
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	want := m.form.interval.value
	_, err := st.CreateTask(m.ctx, model.Task{ProjectId: m.form.original.ProjectId, Title: "new conflict", StartDate: want.Start, DueDate: want.End, TimeZone: want.Zone})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd = press(m, 's', 0)
	m = finishLocal(t, m, cmd)
	if m.form == nil || m.form.err == "" || m.form.interval.review != nil {
		t.Fatal("stale preview saved or lost draft")
	}
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if len(m.form.interval.review.Overlaps) != 1 {
		t.Fatal("phantom missing from new preview")
	}
	m.width, m.height = 32, 10
	_, lines := m.formView()
	if footer := mustGlobalFooter(m); !strings.Contains(footer, "s save") || strings.Contains(footer, "^S save") {
		t.Fatalf("wrong confirmation shortcut: %s", footer)
	}
	if strings.Contains(strings.Join(m.intervalReviewLines(), "\n"), `\"`) {
		t.Fatal("report quoted its escapes twice")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Save anyway") {
		t.Fatal("confirmation clipped")
	}
	m, _ = press(m, tea.KeyEnd, 0)
	if m.form.interval.offset == 0 {
		t.Fatal("preview did not scroll")
	}
	m, cmd = press(m, 's', 0)
	m = finishLocal(t, m, cmd)
	if m.form != nil {
		t.Fatalf("save failed: %+v", m.form)
	}
	got, err := st.Task(m.ctx, m.taskID)
	if err != nil || got.DueDate.Sub(got.StartDate.Time) != 90*time.Minute {
		t.Fatalf("saved=%+v %v", got, err)
	}
}

func TestIntervalPrefillIsCleanAndKeepsExplicitStart(t *testing.T) {
	m, st := writableModel(t)
	start := model.NewTime(time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC))
	end := model.NewTime(start.Add(90 * time.Minute))
	if _, err := st.UpdateTask(m.ctx, m.taskID, model.TaskEdit{StartDate: model.NewEditTime(start), DueDate: model.NewEditTime(end), TimeZone: model.Ptr("UTC"), IsAllDay: model.Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	original, err := st.Task(m.ctx, m.taskID)
	if err != nil {
		t.Fatal(err)
	}
	m.openSchedule(original)
	if m.form.dirty() || m.form.interval.active() || string(m.form.interval.start.value) != "2026-09-10T14:00:00Z" {
		t.Fatal("prefill dirty or implicit")
	}
	m.form.field = 4
	m, _ = press(m, 'u', tea.ModCtrl)
	m, _ = step(m, tea.PasteMsg{Content: "2h"})
	if !m.form.interval.value.Start.Equal(start.Time) || m.form.interval.value.End.Sub(start.Time) != 2*time.Hour {
		t.Fatal("duration edit did not keep explicit start")
	}
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	m, _ = press(m, 'c', tea.ModCtrl)
	if !m.form.discard {
		t.Fatal("Ctrl+C did not offer unsaved choice")
	}
	title, _ := m.formView()
	if title != "Unsaved changes" {
		t.Fatal(title)
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.form.interval.review == nil || m.form.quitAfter {
		t.Fatal("continue did not retain preview")
	}
}
