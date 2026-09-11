package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

func TestDefaultFocusTUIBothModesOverridesAndInvalidDefault(t *testing.T) {
	for _, pomodoro := range []bool{false, true} {
		for _, key := range []rune{'a', 'A', 'N'} {
			t.Run(string(key)+map[bool]string{true: "pomo", false: "timer"}[pomodoro], func(t *testing.T) {
				m, st := focusModel(t)
				other, err := st.CreateTask(m.ctx, model.Task{ProjectId: "home", Title: "Configured", Kind: "TEXT"})
				if err != nil {
					t.Fatal(err)
				}
				cfg := config.Default()
				cfg.DefaultFocus.TaskID = other.Id
				m.timers = app.NewTimers(st, cfg, nil, func(string) error { return nil }, nil)
				want := m.taskID
				if key == 'A' {
					want = other.Id
				}
				if key == 'N' {
					want = ""
				}
				m, cmd := press(m, key, 0)
				m = finishFocus(t, m, cmd)
				if m.timer.form == nil || m.timer.form.guard.TaskID != want {
					t.Fatalf("form %+v", m.timer.form)
				}
				m.timer.form.pomodoro = pomodoro
				m, _ = press(m, 's', tea.ModCtrl)
				m, cmd = press(m, tea.KeyEnter, 0)
				m = finishFocus(t, m, cmd)
				active, err := st.ReadTimer(m.ctx, time.Now())
				if err != nil || active.State == nil || active.State.TaskID != want {
					t.Fatalf("%+v %v", active, err)
				}
			})
		}
	}
	m, st := focusModel(t)
	cfg := config.Default()
	cfg.DefaultFocus.TaskID = "missing"
	m.timers = app.NewTimers(st, cfg, nil, nil, nil)
	m, cmd := press(m, 'A', 0)
	m = finishFocus(t, m, cmd)
	if m.timer.form != nil || !strings.Contains(m.notice, "default_focus") {
		t.Fatal("missing default silently fell back")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, cmd = press(m, 'N', 0)
	m = finishFocus(t, m, cmd)
	if m.timer.form == nil || !m.timer.form.anonymous {
		t.Fatal("explicit none unavailable after invalid default")
	}
}

func TestDefaultFocusPickerRequiresTaskSelectionAndConfirmsNone(t *testing.T) {
	m, st := writableModel(t)
	s := &fakeSystem{}
	m.system = s
	choices, err := app.DefaultFocusChoices(context.Background(), st, "id:work")
	if err != nil {
		t.Fatal(err)
	}
	m.systemPanel = &systemPanel{focus: &focusPicker{choices: choices, projectID: choices.InitialProjectID}}
	for range 3 {
		_ = m.View()
	}
	if s.writes != 0 || s.prepares != 0 {
		t.Fatal("paint caused config action")
	}
	m, _ = press(m, tea.KeyEnter, 0)
	m, cmd := press(m, tea.KeyEnter, 0)
	if cmd != nil || s.prepares != 0 {
		t.Fatal("implicitly selected first task")
	}
	m, _ = press(m, 'j', 0)
	want := m.systemPanel.focus.taskID
	m, cmd = press(m, tea.KeyEnter, 0)
	m, _ = step(m, cmd())
	if m.systemPanel.preview == nil || m.systemPanel.preview.Action != "default focus task:"+want || s.writes != 0 {
		t.Fatal("lost exact choice")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 31, Height: 9})
	_, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil {
		t.Fatal("hidden confirmation submitted")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = press(m, tea.KeyEscape, 0)
	m.systemPanel.focus = &focusPicker{choices: choices}
	m, cmd = press(m, 'n', 0)
	m, _ = step(m, cmd())
	if m.systemPanel.preview == nil || m.systemPanel.preview.Action != "default focus none" {
		t.Fatal("none not previewed")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m, _ = step(m, cmd())
	if s.writes != 1 {
		t.Fatal("none not committed once")
	}
}

func TestDefaultFocusPickerVisibleAtMinimumSize(t *testing.T) {
	m, _ := writableModel(t)
	m, _ = step(m, tea.WindowSizeMsg{Width: 32, Height: 10})
	p := &focusPicker{projectID: "p1", tasks: true}
	for i := 0; i < 30; i++ {
		p.choices.Tasks = append(p.choices.Tasks, model.Task{Id: fmt.Sprint(i), ProjectId: "p1", Title: fmt.Sprintf("Task %02d", i)})
	}
	m.systemPanel = &systemPanel{focus: p}
	m, _ = press(m, tea.KeyEnd, 0)
	m, _ = press(m, tea.KeyEnd, 0)
	if !strings.Contains(m.View().Content, "Task 29") {
		t.Fatal("selected task hidden in minimum terminal")
	}
}
