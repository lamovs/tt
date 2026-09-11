package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

type tuiTopicClient struct{}

func (*tuiTopicClient) CredentialFingerprint() string { return "browser-a" }
func (*tuiTopicClient) ListTopics(context.Context) ([]webapi.Topic, error) {
	return nil, nil
}
func (*tuiTopicClient) CreateTopicFocus(context.Context, webapi.FocusCreate) (webapi.BatchResponse, error) {
	return webapi.BatchResponse{}, errors.New("not expected")
}
func (*tuiTopicClient) GetTopicFocus(context.Context, string, int) (webapi.FocusCreate, error) {
	return webapi.FocusCreate{}, errors.New("not expected")
}

func focusModel(t *testing.T) (browserModel, *store.Store) {
	m, st := writableModel(t)
	m.timers = app.NewTimers(st, config.Default(), func() (*api.Client, error) { return nil, errors.New("network forbidden") }, func(string) error { return nil }, nil)
	m.timer = &timerPanel{}
	var cmd tea.Cmd
	m, cmd = m.loadTimer()
	m, _ = step(m, cmd())
	return m, st
}

func finishFocus(t *testing.T, m browserModel, cmd tea.Cmd) browserModel {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing focus command")
	}
	m, next := step(m, cmd())
	if next != nil {
		m, _ = step(m, next())
	}
	return m
}

func TestFocusStartPauseStaleStopAndReadOnlyView(t *testing.T) {
	m, st := focusModel(t)
	m, cmd := press(m, 'a', 0)
	m = finishFocus(t, m, cmd)
	if m.timer.form == nil || m.timer.form.guard.TaskID != m.taskID {
		t.Fatal("start lost selected task")
	}
	note := " exact note\n\x1b[31m s\t "
	m.timer.form.field = 3
	m, _ = step(m, tea.PasteMsg{Content: note})
	m, _ = press(m, 's', tea.ModCtrl)
	if m.timer.confirm == nil || m.timer.confirm.options.Note != note || !strings.Contains(m.View().Content, "Exact targets: 1") {
		t.Fatal("preview lost exact note/target")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	if !m.busy {
		t.Fatal("submit not gated")
	}
	_, repeat := press(m, tea.KeyEnter, 0)
	if repeat != nil {
		t.Fatal("duplicate start")
	}
	m = finishFocus(t, m, cmd)
	before, err := st.ReadTimer(m.ctx, time.Now())
	if err != nil || before.State == nil || before.State.Note != note {
		t.Fatalf("start %+v %v", before, err)
	}
	for range 10 {
		_ = m.View()
		m, _ = step(m, timerTick{m.timerTickGeneration, time.Now().Add(time.Second)})
	}
	after, _ := st.ReadTimer(m.ctx, time.Now())
	if before.Guard != after.Guard || !before.State.LastEventAt.Equal(after.State.LastEventAt) {
		t.Fatal("paint or ticks wrote timer state")
	}
	m, cmd = press(m, 'p', 0)
	m = finishFocus(t, m, cmd)
	if m.timerData.Active.State.PausedAt == nil {
		t.Fatal("pause failed")
	}
	m, _ = press(m, 'x', 0)
	frozen := m.timer.confirm.guard.SessionID
	if _, err = st.StopTimer(m.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	newer, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishFocus(t, m, cmd)
	if m.timerData.Active.State.SessionID != newer.State.SessionID || m.timerData.Active.State.SessionID == frozen || !strings.Contains(m.notice, "refused") {
		t.Fatal("stale stop retargeted")
	}
	if rowsIn(t, st, "focus_sessions") != 2 {
		t.Fatal("duplicated session")
	}
}

func TestFocusFormsSmallTerminalHelpAndLateResults(t *testing.T) {
	m, st := focusModel(t)
	m, cmd := press(m, 'a', 0)
	m = finishFocus(t, m, cmd)
	m.timer.form.pomodoro = true
	m.timer.form.duration.set("90s")
	m, _ = press(m, 's', tea.ModCtrl)
	m, _ = step(m, tea.WindowSizeMsg{Width: 31, Height: 9})
	_, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil || rowsIn(t, st, "focus_sessions") != 0 {
		t.Fatal("hidden confirmation submitted")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'c', tea.ModCtrl)
	if !m.timer.form.discard || !m.timer.form.quitAfter {
		t.Fatal("dirty focus draft lost on interrupt")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'd', 0)
	if m.timer.form != nil {
		t.Fatal("discard did not close")
	}
	m, _ = press(m, '?', 0)
	if !m.help.ShowAll || !strings.Contains(strings.Join(m.helpLines(), " "), "Uncertain") {
		t.Fatal("context help missing")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'o', tea.ModCtrl)
	m, _ = press(m, '2', 0)
	if m.timer != nil || m.focus != tasksPane {
		t.Fatal("panel jump failed")
	}
	data := m.timerData
	data.Active.State = &store.TimerState{SessionID: "late"}
	m, _ = step(m, timerLoaded{generation: m.timerGeneration - 1, data: data})
	if m.timerData.Active.State != nil {
		t.Fatal("stale timer read accepted")
	}
}

func TestFocusFormSelectsTimerTopicForBothModes(t *testing.T) {
	for _, pomodoro := range []bool{false, true} {
		t.Run(map[bool]string{false: "timer", true: "pomodoro"}[pomodoro], func(t *testing.T) {
			m, st := focusModel(t)
			if err := st.ReplaceFocusTopics(m.ctx, []store.FocusTopic{
				{ID: "study-id", Name: "Study", SortOrder: 2, Raw: json.RawMessage(`{"id":"study-id","name":"Study"}`)},
				{ID: "work-id", Name: "Work", SortOrder: 1, Raw: json.RawMessage(`{"id":"work-id","name":"Work"}`)},
			}, "browser-a", time.Now()); err != nil {
				t.Fatal(err)
			}
			m.timers.(*app.Timers).WithTopicClient(func() (focus.TopicClient, error) { return &tuiTopicClient{}, nil })
			m, cmd := m.loadTimer()
			m, _ = step(m, cmd())
			m, cmd = press(m, 'a', 0)
			m = finishFocus(t, m, cmd)
			m.timer.form.pomodoro = pomodoro
			m.timer.form.field = 1
			m, _ = press(m, tea.KeyRight, 0)
			m, _ = press(m, tea.KeyRight, 0)
			if m.timer.form.topic < 0 || m.timerData.Topics[m.timer.form.topic].Name != "Work" {
				t.Fatalf("destination %+v in %+v", m.timer.form, m.timerData.Topics)
			}
			m, _ = press(m, 's', tea.ModCtrl)
			if m.timer.confirm == nil || m.timer.confirm.options.TopicID != "work-id" || m.timer.confirm.options.TaskID != "" {
				t.Fatalf("preview %+v", m.timer.confirm)
			}
			m, cmd = press(m, tea.KeyEnter, 0)
			m = finishFocus(t, m, cmd)
			state := m.timerData.Active.State
			if state == nil || state.TopicID != "work-id" || state.TopicName != "Work" || state.TopicCredential != "browser-a" || state.FocusType != map[bool]int{false: 1, true: 0}[pomodoro] {
				t.Fatalf("state %+v", state)
			}
		})
	}
}

func TestFocusSessionSelectionAndPreviewDoNotFollowNewRows(t *testing.T) {
	m, st := focusModel(t)
	now := time.Now().Add(-time.Minute)
	first, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.StopTimer(m.ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var cmd tea.Cmd
	m, cmd = m.loadTimer()
	m, _ = step(m, cmd())
	m.timer.history = true
	m, _ = press(m, 'U', 0)
	if m.timer.confirm == nil || m.timer.confirm.target.Session.ID != first.State.SessionID {
		t.Fatal("upload preview missing")
	}
	second, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 1}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.StopTimer(m.ctx, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	m, cmd = m.loadTimer()
	m, _ = step(m, cmd())
	if m.timer.confirm.target.Session.ID != first.State.SessionID || m.timer.selected != first.State.SessionID {
		t.Fatal("new session retargeted")
	}
	m.timer.confirm = nil
	if _, err = st.DB().ExecContext(context.Background(), "DELETE FROM focus_sessions WHERE id=?", first.State.SessionID); err != nil {
		t.Fatal(err)
	}
	m, cmd = m.loadTimer()
	m, _ = step(m, cmd())
	if m.timer.selected != "" || !m.timer.lost {
		t.Fatal("missing selection selected next row")
	}
	m, _ = press(m, 'U', 0)
	if m.timer.confirm != nil || second.State.SessionID == "" {
		t.Fatal("unselected upload")
	}
}

func TestEditorWaitsForFocusAndDoesNotLoseItsCompletion(t *testing.T) {
	m, _ := focusModel(t)
	m.timer = nil
	m.timerWorking = true
	next, cmd := m.beginEditor()
	if cmd != nil || next.editor != nil || !strings.Contains(next.notice, "focus operation") {
		t.Fatal("editor lost running completion")
	}
	m.timerWorking = false
	m.editor = &editorCommand{}
	data := m.timerData
	data.Active.State = &store.TimerState{SessionID: "expired", FocusType: 0, Deadline: model.Ptr(time.Now().Add(-time.Second))}
	data.Active.ObservedAt = time.Now()
	m, _ = step(m, timerLoaded{generation: m.timerGeneration, data: data})
	if m.timerWorking {
		t.Fatal("completion started during terminal handoff")
	}
}

func TestHistoryKeysCannotControlAnotherActiveSession(t *testing.T) {
	m, st := focusModel(t)
	started, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.loadTimer()
	m, _ = step(m, cmd())
	m.timer.history = true
	for _, key := range []tea.KeyPressMsg{{Code: 'p'}, {Code: 'x'}, {Code: 'd', Mod: tea.ModCtrl}} {
		m, cmd = step(m, key)
		if cmd != nil || m.timer.confirm != nil {
			t.Fatal("history controlled active session")
		}
	}
	current, err := st.ReadTimer(m.ctx, time.Now())
	if err != nil || current.State.SessionID != started.State.SessionID || current.State.PausedAt != nil {
		t.Fatal("active session changed")
	}
}
