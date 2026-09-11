package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
)

type fakeSystem struct {
	reads, doctors, prepares, writes int
	result                           SettingsResult
	err                              error
}

func (s *fakeSystem) Settings(context.Context) (SettingsResult, error) {
	s.reads++
	return s.result, s.err
}
func (s *fakeSystem) Doctor(context.Context) ([]string, error) {
	s.doctors++
	return []string{"warn: synthetic"}, nil
}
func (s *fakeSystem) FocusChoices(context.Context) (app.FocusChoices, error) {
	return app.FocusChoices{}, s.err
}
func (s *fakeSystem) PrepareDefaultFocus(ctx context.Context, ref config.FocusReference) (SystemPreview, error) {
	return s.Prepare(ctx, "default focus "+ref.String())
}
func (s *fakeSystem) Prepare(_ context.Context, action string) (SystemPreview, error) {
	s.prepares++
	return SystemPreview{Action: action, Lines: []string{"Exact target 1: synthetic\x1b]52;c;secret\a"}, Apply: func(context.Context) (string, error) { s.writes++; return "Saved", nil }}, nil
}
func (s *fakeSystem) Login(context.Context, bool, *os.File, *os.File) string {
	return "Synthetic login"
}

func TestSettingsExplicitActionsAndFrozenPreview(t *testing.T) {
	m, _ := writableModel(t)
	s := &fakeSystem{result: SettingsResult{Lines: []string{"tt version test"}, Queries: m.queries, Actions: m.actions, DefaultProject: "Work"}}
	m.system = s
	m, cmd := press(m, 'S', 0)
	if s.reads != 0 || cmd == nil {
		t.Fatal("settings performed inline I/O")
	}
	m, _ = step(m, cmd())
	for range 10 {
		_ = m.View()
		m, _ = step(m, timerTick{m.timerTickGeneration, time.Now()})
	}
	if s.reads != 1 || s.doctors+s.prepares+s.writes != 0 {
		t.Fatal("opening/painting caused action")
	}
	m, cmd = press(m, 'i', 0)
	m, _ = step(m, cmd())
	if s.writes != 0 || m.systemPanel.preview == nil {
		t.Fatal("preparation wrote")
	}
	if strings.Contains(m.View().Content, "\x1b]52") {
		t.Fatal("preview injected OSC")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if s.writes != 0 || m.systemPanel.preview != nil {
		t.Fatal("cancel wrote")
	}
	m, cmd = press(m, 'i', 0)
	m, _ = step(m, cmd())
	m, cmd = press(m, tea.KeyEnter, 0)
	_, repeat := press(m, tea.KeyEnter, 0)
	if repeat != nil {
		t.Fatal("duplicate submission")
	}
	result := cmd()
	_ = cmd()
	if s.writes != 1 {
		t.Fatal("command executed twice")
	}
	m, _ = step(m, result)
	if m.busy || !strings.Contains(strings.Join(m.systemPanel.lines, "\n"), "Saved") {
		t.Fatal("result lost")
	}
}

func TestSettingsValidationFencesAndHandoff(t *testing.T) {
	m, _ := writableModel(t)
	old := m.actions
	s := &fakeSystem{err: errors.New("invalid config")}
	m.system = s
	m, cmd := m.openSystem()
	m, _ = step(m, cmd())
	if m.actions != old {
		t.Fatal("invalid config replaced services")
	}
	m.systemWorking, m.busy = true, true
	m.systemGeneration = 5
	m, _ = step(m, systemFinished{generation: 4, settings: SettingsResult{}})
	if !m.busy {
		t.Fatal("stale response released busy gate")
	}
	m.busy, m.systemWorking = false, false
	m.systemPanel.preview = &SystemPreview{Action: "login", Apply: func(context.Context) (string, error) { return "", nil }}
	m.timerWorking = true
	m, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil || m.login != nil {
		t.Fatal("handoff dropped active focus operation")
	}
	m.timerWorking = false
	m, cmd = press(m, tea.KeyEnter, 0)
	if cmd == nil || m.login == nil {
		t.Fatal("explicit login did not hand off")
	}
	m, _ = step(m, timerLoaded{generation: m.timerGeneration, data: m.timerData})
	if m.timerWorking {
		t.Fatal("timer completion started during handoff")
	}
}

func TestSettingsHiddenPreviewCannotSubmitAndHelpReturns(t *testing.T) {
	m, _ := writableModel(t)
	s := &fakeSystem{}
	m.system = s
	m.systemPanel = &systemPanel{preview: &SystemPreview{Action: "test", Apply: func(context.Context) (string, error) { t.Fatal("hidden submit"); return "", nil }}}
	m.width, m.height = 20, 5
	_, cmd := press(m, tea.KeyEnter, 0)
	if cmd != nil {
		t.Fatal("hidden submit command")
	}
	m.width, m.height = 80, 24
	m.systemPanel.preview = nil
	m, _ = press(m, '?', 0)
	if !strings.Contains(strings.Join(m.helpLines(), "\n"), "existing hidden token") {
		t.Fatal("wrong context help")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.systemPanel == nil || m.help.ShowAll {
		t.Fatal("help lost settings context")
	}
}
