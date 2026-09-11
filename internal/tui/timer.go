package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type TimerActions interface {
	Read(context.Context, time.Time) (app.TimerData, error)
	BindTask(context.Context, store.TimerGuard, string) (store.TimerGuard, error)
	BindDefault(context.Context, store.TimerGuard) (store.TimerGuard, error)
	Control(context.Context, string, store.TimerStartOptions, *store.TimerGuard) (app.TimerOutcome, error)
	Upload(context.Context, store.FocusTarget, bool) (focus.Result, error)
}

type TimerTopicActions interface {
	RefreshTopics(context.Context) (int, error)
}

type timerPanel struct {
	report                string
	history, detail, lost bool
	selected              string
	offset                int
	form                  *timerForm
	confirm               *timerConfirmation
	review                *timerNoteForm
}

type timerForm struct {
	guard               store.TimerGuard
	anonymous, pomodoro bool
	topic               int
	duration, note      searchInput
	field               int
	discard, quitAfter  bool
	indicator           store.IndicatorMode
}

type timerConfirmation struct {
	action    string
	guard     store.TimerGuard
	options   store.TimerStartOptions
	target    store.FocusTarget
	state     *store.TimerState
	taskTitle string
	addition  string
	skip      bool
}

type timerLoaded struct {
	generation uint64
	data       app.TimerData
	err        error
}
type timerRefreshDue struct{ generation uint64 }
type timerTick struct {
	generation uint64
	now        time.Time
}
type timerFinished struct {
	generation  uint64
	action      string
	result      app.TimerOutcome
	upload      focus.Result
	guard       store.TimerGuard
	err         error
	noteSession store.TimerSession
	topicCount  int
}

func (m browserModel) timerReadCmd() tea.Cmd {
	if m.timers == nil {
		return nil
	}
	actions, ctx, generation := m.timers, m.ctx, m.timerGeneration
	return m.gate.command(func() tea.Msg {
		data, err := actions.Read(ctx, time.Now())
		return timerLoaded{generation, data, err}
	})
}

func (m browserModel) loadTimer() (browserModel, tea.Cmd) {
	if m.timers == nil {
		return m, nil
	}
	m.timerGeneration++
	return m, m.timerReadCmd()
}

func (m browserModel) timerTickCmd() tea.Cmd {
	generation := m.timerTickGeneration
	return tea.Tick(time.Second, func(now time.Time) tea.Msg { return timerTick{generation, now} })
}

func (m browserModel) finishTimerRead(msg timerLoaded) (browserModel, tea.Cmd) {
	if msg.generation != m.timerGeneration {
		return m, nil
	}
	m.timerErr = msg.err
	m.timerKnown = msg.err == nil
	if msg.err == nil {
		m.timerData = msg.data
		m.timerNow = msg.data.Active.ObservedAt
		if p := m.timer; p != nil {
			found := false
			for _, row := range msg.data.History {
				if row.Session.ID == p.selected {
					found = true
				}
			}
			if p.selected != "" && !found {
				p.selected, p.lost = "", true
			}
			if p.selected == "" && !p.lost && len(msg.data.History) > 0 {
				p.selected = msg.data.History[0].Session.ID
			}
		}
		m.offerTimerNoteReview()
	}
	generation := m.timerGeneration
	refresh := tea.Tick(2*time.Second, func(time.Time) tea.Msg { return timerRefreshDue{generation} })
	state := m.timerData.Active.State
	if msg.err == nil && m.editor == nil && m.login == nil && !m.systemWorking && !m.timerWorking && state != nil && state.FocusType == 0 &&
		(m.timerEnsured != state.SessionID || state.Deadline != nil && !m.timerNow.Before(*state.Deadline)) {
		m.timerEnsured = state.SessionID
		c := timerConfirmation{action: "status", guard: m.timerData.Active.Guard}
		next, cmd := m.runTimer(c, false)
		return next, tea.Batch(cmd, refresh)
	}
	return m, refresh
}

func (m browserModel) runTimer(c timerConfirmation, foreground bool) (browserModel, tea.Cmd) {
	if m.timerWorking {
		return m, nil
	}
	m.timerWorking, m.timerForeground = true, foreground
	if foreground {
		m.busy = true
	}
	m.timerOperation++
	generation, actions, ctx := m.timerOperation, m.timers, m.ctx
	ctx, m.timerCancel = context.WithCancel(ctx)
	m.timerAction = c.action
	return m, m.gate.command(func() tea.Msg {
		msg := timerFinished{generation: generation, action: c.action}
		switch c.action {
		case "bind":
			msg.guard, msg.err = actions.BindTask(ctx, c.guard, c.options.TaskID)
		case "bind default":
			msg.guard, msg.err = actions.BindDefault(ctx, c.guard)
		case "upload", "retry":
			msg.upload, msg.err = actions.Upload(ctx, c.target, c.action == "retry")
		case "review note":
			if reviewer, ok := actions.(TimerNoteActions); ok {
				msg.noteSession, msg.err = reviewer.ReviewNote(ctx, c.target, c.addition, c.skip)
			} else {
				msg.err = fmt.Errorf("completion-note review is unavailable")
			}
		case "refresh topics":
			if topics, ok := actions.(TimerTopicActions); ok {
				msg.topicCount, msg.err = topics.RefreshTopics(ctx)
			} else {
				msg.err = fmt.Errorf("Timer topic refresh is unavailable")
			}
		default:
			msg.result, msg.err = actions.Control(ctx, c.action, c.options, &c.guard)
		}
		return msg
	})
}

func (m browserModel) finishTimerOperation(msg timerFinished) (browserModel, tea.Cmd) {
	if msg.generation != m.timerOperation || !m.timerWorking {
		return m, nil
	}
	foreground := m.timerForeground
	if m.timerCancel != nil {
		m.timerCancel()
		m.timerCancel = nil
	}
	m.timerWorking, m.timerForeground = false, false
	if foreground {
		m.busy = false
	}
	if msg.action == "review note" {
		if m.timer == nil || m.timer.review == nil {
			return m.loadTimer()
		}
		f := m.timer.review
		if msg.err != nil && (msg.noteSession.ID != f.target.Session.ID || msg.noteSession.NoteReviewPending) {
			f.err, f.offset = msg.err.Error(), 0
			return m, nil
		}
		if msg.noteSession.ID != f.target.Session.ID {
			f.err, f.offset = "The note result does not match the reviewed session.", 0
			return m, nil
		}
		m.deferTimerNote(f.target.Session.ID)
		m.timer.review = nil
		m.notice = "Focus note review saved for session " + msg.noteSession.ID + "."
		if msg.err != nil {
			m.notice += " " + msg.err.Error()
		}
		if f.quitAfter && msg.err == nil {
			return m, tea.Quit
		}
		return m.loadTimer()
	}
	if msg.err != nil {
		m.notice = "Focus action refused: " + msg.err.Error()
		m.timerReport = m.notice
		if m.timer != nil {
			m.timer.confirm = nil
			m.timer.report = m.notice
			m.timer.offset = 0
		}
		return m.loadTimer()
	}
	if msg.action == "bind" || msg.action == "bind default" {
		if m.timer != nil {
			f := &timerForm{guard: msg.guard, anonymous: msg.guard.TaskID == "", topic: -1}
			f.duration.set(model.Duration(m.timerData.DefaultDuration).String())
			m.timer.form = f
		}
		return m, nil
	}
	if msg.action == "refresh topics" {
		m.notice = fmt.Sprintf("Timer topics refreshed: %d.", msg.topicCount)
		return m.loadTimer()
	}
	if foreground {
		m.notice = "Focus: " + msg.action + " saved locally."
		if msg.action == "upload" || msg.action == "retry" {
			m.notice = fmt.Sprintf("Focus: confirmed %d, held %d.", msg.upload.Uploaded, msg.upload.Held)
		}
		if m.timer != nil {
			quit := m.timer.form != nil && m.timer.form.quitAfter
			m.timer.confirm, m.timer.form = nil, nil
			m.timer.offset = 0
			if quit {
				return m, tea.Quit
			}
		}
	}
	if msg.result.Result.Completed != nil {
		session := msg.result.Result.Completed
		m.notice = "Focus session saved locally (" + session.Outcome + ", " + focusClock(session.ActiveDuration) + " active): " + session.ID
	}
	problems := append(append([]error(nil), msg.result.Problems...), msg.upload.Errors...)
	for _, err := range problems {
		m.notice += " " + err.Error()
	}
	if len(problems) != 0 {
		m.timerReport = m.notice
		if m.timer != nil {
			m.timer.report = m.notice
			m.timer.offset = 0
		}
	}
	return m.loadTimer()
}

func (m browserModel) selectedFocus() *store.FocusTarget {
	if m.timer == nil {
		return nil
	}
	for i := range m.timerData.History {
		if m.timerData.History[i].Session.ID == m.timer.selected {
			return &m.timerData.History[i]
		}
	}
	return nil
}

func (m browserModel) timerKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, key := m.timer, msg.String()
	if p.review != nil {
		return m.timerNoteKey(msg)
	}
	if p.report != "" {
		if key == "esc" {
			p.report = ""
			m.timerReport = ""
			p.offset = 0
		} else {
			_, lines := m.timerView()
			p.offset = scroll(p.offset, key, max(1, m.height-6), len(lines))
		}
		return m, nil
	}
	if p.confirm != nil {
		switch key {
		case "esc":
			p.confirm = nil
			p.offset = 0
		case "enter":
			return m.runTimer(*p.confirm, true)
		default:
			_, lines := m.timerView()
			p.offset = scroll(p.offset, key, max(1, m.height-6), len(lines))
		}
		return m, nil
	}
	if p.form != nil {
		return m.timerFormKey(msg)
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "esc":
		if p.detail {
			p.detail = false
			p.offset = 0
		} else {
			m.timer = nil
		}
	case "ctrl+o":
		m.jump = true
	case "?":
		m.help.ShowAll = true
		m.help.offset = 0
	case "tab", "shift+tab":
		p.history = !p.history
		p.detail = false
		p.offset = 0
	case "r":
		m.timerEnsured = ""
		return m.loadTimer()
	case "R", "shift+r":
		return m.runTimer(timerConfirmation{action: "refresh topics"}, true)
	case "e":
		for _, target := range m.timerData.NoteReviews {
			if !p.history || target.Session.ID == p.selected {
				m.openTimerNoteReview(target)
				return m, nil
			}
		}
		m.notice = "No pending completion-note review for this selection."
		return m, nil
	case "s":
		return m.beginSync()
	case "u":
		return m.beginUndo()
	case "a", "A", "shift+a", "N", "shift+n":
		if !m.timerKnown || m.timerWorking {
			m.notice = "Wait for the current timer state."
			return m, nil
		}
		if m.timerData.Active.State != nil {
			m.notice = "One session is already active. Stop or cancel it first."
			return m, nil
		}
		id := ""
		if key == "a" && m.taskID != "" {
			if m.detailLoading || m.detail == nil || m.detail.Id != m.taskID {
				m.notice = "Wait for the selected task details."
				return m, nil
			}
			id = m.taskID
		}
		action := "bind"
		if id == "" && key != "N" && key != "shift+n" {
			action = "bind default"
		}
		return m.runTimer(timerConfirmation{action: action, guard: m.timerData.Active.Guard, options: store.TimerStartOptions{TaskID: id}}, true)
	case "p", "x", "ctrl+d":
		if p.history {
			m.notice = "Tab to the active session before pausing, stopping or canceling it."
			return m, nil
		}
		if !m.timerKnown || m.timerWorking || m.timerData.Active.State == nil {
			m.notice = "No available active session."
			return m, nil
		}
		state := *m.timerData.Active.State
		action := "stop"
		if key == "ctrl+d" {
			action = "cancel"
		}
		if key == "p" {
			action = "pause"
			if state.PausedAt != nil {
				action = "resume"
			}
		}
		c := timerConfirmation{action: action, guard: m.timerData.Active.Guard, state: &state, taskTitle: m.timerData.TaskTitle, options: store.TimerStartOptions{ReviewNote: action == "stop"}}
		if key == "p" {
			return m.runTimer(c, true)
		}
		p.confirm = &c
		p.offset = 0
	case "U", "shift+u", "f":
		if !p.history || m.timerWorking {
			return m, nil
		}
		row := m.selectedFocus()
		if row == nil {
			m.notice = "Select an exact history session first."
			return m, nil
		}
		retry := key == "f"
		if err := focus.ValidateTarget(*row, m.timerData.UploadAborted, retry); err != nil {
			m.notice = err.Error()
			p.report = err.Error()
			p.offset = 0
			return m, nil
		}
		action := "upload"
		if retry {
			action = "retry"
		}
		p.confirm = &timerConfirmation{action: action, target: *row}
		p.offset = 0
	case "enter":
		p.detail = true
		p.offset = 0
	default:
		if p.detail || !p.history {
			_, lines := m.timerView()
			p.offset = scroll(p.offset, key, max(1, m.height-6), len(lines))
			return m, nil
		}
		if key != "j" && key != "k" && key != "up" && key != "down" && key != "pgup" && key != "pgdown" && key != "home" && key != "end" {
			return m, nil
		}
		index := -1
		for i, row := range m.timerData.History {
			if row.Session.ID == p.selected {
				index = i
			}
		}
		if len(m.timerData.History) > 0 {
			if index < 0 {
				index = 0
			} else {
				index = scroll(index, key, max(1, m.height-8), len(m.timerData.History))
			}
			p.selected = m.timerData.History[index].Session.ID
			p.lost = false
		}
	}
	return m, nil
}

func (m browserModel) timerFormKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, key := m.timer.form, msg.String()
	if f.discard {
		switch key {
		case "esc":
			f.discard = false
			f.quitAfter = false
		case "d":
			quit := f.quitAfter
			m.timer.form = nil
			if quit {
				return m, tea.Quit
			}
		case "s":
			f.discard = false
			return m.previewTimerStart()
		}
		return m, nil
	}
	switch key {
	case "esc":
		f.discard = true
	case "tab":
		f.field = (f.field + 1) % 5
	case "shift+tab":
		f.field = (f.field + 4) % 5
	case "ctrl+s":
		return m.previewTimerStart()
	default:
		switch f.field {
		case 0:
			if key == "left" || key == "right" || key == "space" {
				f.pomodoro = !f.pomodoro
			}
		case 1:
			if key == "left" || key == "right" || key == "space" {
				m.cycleTimerDestination(key != "left")
			}
		case 2:
			f.duration.update(msg)
		case 3:
			f.note.updateBody(msg)
		case 4:
			if key == "right" || key == "space" {
				f.indicator = (f.indicator + 1) % 3
			} else if key == "left" {
				f.indicator = (f.indicator + 2) % 3
			}
		}
	}
	return m, nil
}

func (m browserModel) previewTimerStart() (browserModel, tea.Cmd) {
	f := m.timer.form
	options := store.TimerStartOptions{FocusType: 1, Note: string(f.note.value), Indicator: f.indicator, ReviewNote: true}
	guard := f.guard
	if f.topic >= 0 && f.topic < len(m.timerData.Topics) {
		topic := m.timerData.Topics[f.topic]
		options.TopicID, options.TopicName, options.TopicCredential = topic.ID, topic.Name, topic.CredentialFingerprint
		guard.TopicID, guard.TopicName, guard.TopicCredential = topic.ID, topic.Name, topic.CredentialFingerprint
		guard.TaskID, guard.TaskVersion, guard.TaskTitle, guard.DefaultTask = "", "", "", false
	} else if !f.anonymous {
		options.TaskID = guard.TaskID
	} else {
		guard.TaskID, guard.TaskVersion, guard.TaskTitle = "", "", ""
		guard.DefaultTask = false
	}
	if f.pomodoro {
		duration, err := model.ParseDuration(string(f.duration.value))
		if err != nil || duration.Duration() < time.Second || duration.Duration() > 24*time.Hour {
			m.notice = "Pomodoro needs a whole-second duration from 1s to 24h, for example 25m or 90s."
			return m, nil
		}
		options.FocusType, options.Planned = 0, duration.Duration()
	}
	m.timer.confirm = &timerConfirmation{action: "start", guard: guard, options: options, taskTitle: guard.TaskTitle}
	m.timer.offset = 0
	return m, nil
}

func (m *browserModel) cycleTimerDestination(forward bool) {
	f := m.timer.form
	total := len(m.timerData.Topics) + 1
	if f.guard.TaskID != "" {
		total++
	}
	current := 0
	if f.guard.TaskID != "" && !f.anonymous && f.topic < 0 {
		current = 0
	} else if f.anonymous && f.topic < 0 {
		current = total - 1
	} else if f.topic >= 0 {
		current = f.topic
		if f.guard.TaskID != "" {
			current++
		}
	}
	if forward {
		current = (current + 1) % total
	} else {
		current = (current + total - 1) % total
	}
	f.topic, f.anonymous = -1, false
	if f.guard.TaskID != "" && current == 0 {
		return
	}
	topicIndex := current
	if f.guard.TaskID != "" {
		topicIndex--
	}
	if topicIndex >= 0 && topicIndex < len(m.timerData.Topics) {
		f.topic = topicIndex
		return
	}
	f.anonymous = true
}

func focusClock(duration time.Duration) string {
	seconds := int64(max(0, duration) / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
}

func (m browserModel) focusStatus() string {
	if m.timerErr != nil {
		return "Focus unavailable (t)"
	}
	state := m.timerData.Active.State
	if state == nil {
		return ""
	}
	elapsed := state.ActiveDuration
	if state.PausedAt == nil && m.timerNow.After(m.timerData.Active.ObservedAt) {
		elapsed += m.timerNow.Sub(m.timerData.Active.ObservedAt)
	}
	mode := "Timer"
	duration := elapsed
	if state.FocusType == 0 {
		mode = "Pomodoro"
		duration = max(0, state.PlannedDuration-elapsed)
	}
	status := "running"
	if state.PausedAt != nil {
		status = "paused"
	}
	return mode + " " + focusClock(duration) + " " + status
}

func (m browserModel) timerView() (string, []string) {
	p := m.timer
	if p.review != nil {
		return m.timerNoteView()
	}
	wrap := func(lines []string) []string {
		var out []string
		for _, line := range lines {
			out = append(out, wrapText(line, max(1, m.width-4))...)
		}
		return out
	}
	if p.report != "" {
		return "Focus result", wrap([]string{p.report, "Esc returns."})
	}
	if c := p.confirm; c != nil {
		lines := []string{"Action: " + c.action, "Exact targets: 1 session"}
		if c.action == "start" {
			mode := "Timer"
			if c.options.FocusType == 0 {
				mode = "Pomodoro " + focusClock(c.options.Planned)
			}
			lines = append(lines, "New "+mode, "Task: "+c.taskTitle, "Task ID: "+c.options.TaskID, "Timer topic: "+c.options.TopicName, "Topic ID: "+c.options.TopicID, "Note: "+c.options.Note)
			lines = append(lines, "External indicator: "+indicatorChoice(c.options.Indicator, m.timerData.IndicatorEnabled))
			if c.options.TaskID == "" && c.options.TopicID == "" {
				lines = append(lines, "Explicitly unassigned; no task will be created.")
			}
		} else if c.action == "upload" || c.action == "retry" {
			lines = append(lines, focusTargetLines(c.target)...)
			lines = append(lines, "Only this session is included. HTTP runs after confirmation.", "Uncertain requests are read back, never sent again.")
			if c.action == "retry" {
				lines = append(lines, "Definitive rejection: resend the original frozen request.")
			}
		} else {
			lines = append(lines, "Session ID: "+c.guard.SessionID, "Task: "+c.taskTitle, "Task ID: "+c.state.TaskID, "Note: "+c.state.Note)
			if c.action == "cancel" {
				lines = append(lines, "Save an aborted local session. No deletion of history.")
			} else {
				lines = append(lines, "Save actual active time up to confirmation (including partial Pomodoro).", "Existing completion notification/upload policies apply.")
			}
			if c.state.FocusType == 0 {
				lines = append(lines, "An elapsed countdown completes at its deadline. A late cancel cannot mark it aborted.")
			}
		}
		lines = append(lines, "Versions are rechecked; changes require a fresh preview.", "Enter confirms; Esc cancels. j/k scroll.")
		return "Confirm focus " + c.action, wrap(lines)
	}
	if f := p.form; f != nil {
		if f.discard {
			return "Unsaved focus form", []string{"s review/save | d discard | Esc continue"}
		}
		mode := "Timer (counts up)"
		if f.pomodoro {
			mode = "Pomodoro (counts down)"
		}
		destination := f.guard.TaskTitle + " [task " + f.guard.TaskID + "]"
		if f.topic >= 0 && f.topic < len(m.timerData.Topics) {
			topic := m.timerData.Topics[f.topic]
			destination = topic.Name + " [Timer topic " + topic.ID + "]"
		} else if f.anonymous {
			destination = "Unassigned"
		}
		values := []string{"Mode: " + mode, "Destination: " + display(destination), "Duration: " + f.duration.view(max(1, m.width-18)), "Note: " + f.note.view(max(1, m.width-14)), "Indicator: " + indicatorChoice(f.indicator, m.timerData.IndicatorEnabled)}
		for i := range values {
			values[i] = marker(i == f.field) + values[i]
		}
		return "New focus session", values
	}
	if m.timerErr != nil {
		return "Focus", wrap([]string{"Cannot read focus state: " + m.timerErr.Error(), "r retries locally; history never triggers upload."})
	}
	if !m.timerKnown {
		return "Focus", []string{"Reading local focus state..."}
	}
	if p.history {
		if p.detail {
			row := m.selectedFocus()
			if row == nil {
				return "Focus history", []string{"Selected session is no longer available."}
			}
			return "Focus session", wrap(focusTargetLines(*row))
		}
		lines := []string{"Latest 20 local sessions; Enter reads full details."}
		if len(m.timerData.History) == 0 {
			lines = append(lines, "No local focus sessions.")
		}
		start := 0
		for i, row := range m.timerData.History {
			if row.Session.ID == p.selected {
				start = max(0, i-max(1, m.height-8)+1)
			}
		}
		for _, row := range m.timerData.History[start:] {
			lines = append(lines, marker(row.Session.ID == p.selected)+focusPhase(row)+" "+focusClock(row.Session.ActiveDuration)+" "+display(row.Session.ID))
		}
		return "Focus history", lines
	}
	lines := []string{m.focusStatus()}
	if state := m.timerData.Active.State; state != nil {
		lines = append(lines, "Session ID: "+state.SessionID, "Task: "+m.timerData.TaskTitle, "Task ID: "+state.TaskID, "Note: "+state.Note,
			"Active time: "+focusClock(state.ActiveDuration), "Paused time: "+focusClock(state.PauseDuration), "Closing tt ui keeps this session running.")
		if state.TopicID != "" {
			lines = append(lines, "Timer topic: "+state.TopicName, "Topic ID: "+state.TopicID)
		}
	} else {
		lines = []string{"No active timer. a: selected task; A: default_focus; N: unassigned."}
	}
	lines = append(lines, fmt.Sprintf("Automatic upload: %t; upload aborted: %t", m.timerData.UploadEnabled, m.timerData.UploadAborted),
		"Tab opens history. One shared session for CLI and TUI.")
	return "Focus - active session", wrap(lines)
}

func focusPhase(row store.FocusTarget) string {
	if row.Session.NoteReviewPending {
		return "note pending"
	}
	if row.Session.SyncedAt != nil {
		return "confirmed"
	}
	if row.Upload != nil {
		return row.Upload.Phase
	}
	return row.Session.Outcome + " local"
}

func focusTargetLines(row store.FocusTarget) []string {
	s := row.Session
	mode := "Timer"
	if s.FocusType == 0 {
		mode = "Pomodoro"
	}
	lines := []string{"Session ID: " + s.ID, "Task ID: " + s.TaskID, "Mode: " + mode, "Outcome: " + s.Outcome, "Upload: " + focusPhase(row),
		"Started: " + s.StartedAt.Format(time.RFC3339Nano), "Ended: " + s.EndedAt.Format(time.RFC3339Nano),
		"Active: " + focusClock(s.ActiveDuration), "Paused: " + focusClock(s.PauseDuration), "Note: " + s.Note}
	if s.TopicID != "" {
		lines = append(lines, "Timer topic: "+s.TopicName, "Topic ID: "+s.TopicID)
	}
	if row.Upload != nil {
		lines = append(lines, "Remote ID: "+row.Upload.RemoteID, "Error: "+row.Upload.LastError, "Frozen request: "+string(row.Upload.Request))
	}
	return lines
}

func timerHelp() []string {
	return []string{"Focus: one shared local timer/Pomodoro for CLI and TUI.",
		"t opens Focus; Tab switches active/history; j/k selects; Enter reads details.",
		"a starts for the selected task (default_focus if no task is selected); A uses default_focus; N is explicitly unassigned.",
		"Arrows choose Timer/Pomodoro and task/Timer topic/unassigned; Tab changes fields. Both modes use the same destination rules.",
		"Run tt login web --same-account once. R refreshes Timer topics from TickTick; CLI can use tt timer topic ls --remote.",
		"S then g chooses default_focus from cached open tasks or none; r reloads settings. Missing defaults refuse new default starts.",
		"A start form freezes its task ID. Renames, deletions and config changes never select a replacement for saved sessions or uploads.",
		"Pomodoro duration: 25m, 1h30m or 90s, from 1s to 24h. This is not a date expression.",
		"Ctrl+S previews a start; Enter confirms. Esc offers save/discard/continue.",
		"In the active section, p pauses/resumes the exact displayed session; x previews stop; Ctrl+D previews cancel.",
		"Stop saves actual active time. Cancel keeps an aborted local history record.",
		"Completion offers an optional focus-history note, never a task comment. Existing start notes are preserved.",
		"Review: type an addition; Tab selects Save/Skip; Ctrl+S saves; Enter in empty input skips, otherwise adds a newline.",
		"Skip keeps the existing note. Esc defers; e reopens a pending review in Focus/history.",
		"Pending review survives restart and holds only that session's upload. PageUp/PageDown reads the full review details.",
		"History: U previews upload/read-back of 1 selected session; f retries a definitive rejection.",
		"Uncertain uploads are only read back; no second allocating POST. Task retries never rearm focus.",
		"Versions, exact IDs and target counts are checked again before writes.",
		"s keeps the ordinary task-sync and optional focus ordering; u remains global task undo.",
		"r reloads local focus state and recovers the countdown watcher; R refreshes Timer topics; ? help; Esc returns; Ctrl+O jumps to browser panels.",
		"Closing TUI keeps the timer running. Pauses survive restart. There is no automatic break loop.",
		"timer.on_end is attempted at most once for completed Pomodoro; a process loss can lose the notification.",
		"focus_upload.enabled controls automatic upload, disabled by default. History and paint do not upload.",
		"Subsecond sessions stay local. Aborted uploads require timer.upload_aborted.",
		"Local completion is separate from remote confirmation. TickTick receives history, not an active timer.",
		"Ordinary terminals, Herdr and tmux need no terminal configuration for this panel."}
}
