package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/store"
)

type TimerNoteActions interface {
	ReviewNote(context.Context, store.FocusTarget, string, bool) (store.TimerSession, error)
}

type timerNoteForm struct {
	target             store.FocusTarget
	addition           searchInput
	field, offset      int
	err                string
	taskTitle          string
	discard, quitAfter bool
}

func (m browserModel) canOfferTimerNote() bool {
	if _, ok := m.timers.(TimerNoteActions); !ok {
		return false
	}
	return m.usable() && !m.busy && !m.timerWorking && !m.systemWorking && m.editor == nil && m.login == nil &&
		!m.help.ShowAll && !m.jump && !m.searching && m.form == nil && m.dialog == nil && m.sync == nil &&
		m.resource == nil && m.palette == nil && m.serverTasks == nil && m.recovery == nil && m.taskOperation == nil &&
		m.systemPanel == nil && m.checklist == nil && m.queue == nil &&
		(m.timer == nil || m.timer.form == nil && m.timer.confirm == nil && m.timer.review == nil && m.timer.report == "")
}

func (m *browserModel) offerTimerNoteReview() {
	if !m.canOfferTimerNote() {
		return
	}
	for _, target := range m.timerData.NoteReviews {
		if !m.timerNoteDeferred[target.Session.ID] {
			m.openTimerNoteReview(target)
			return
		}
	}
}

func (m *browserModel) openTimerNoteReview(target store.FocusTarget) {
	if m.timer == nil {
		m.timer = &timerPanel{}
	}
	m.timer.review = &timerNoteForm{target: target}
	for _, task := range m.tasks {
		if task.Id == target.Session.TaskID {
			m.timer.review.taskTitle = task.Title
			break
		}
	}
	m.timer.offset = 0
}

func (m *browserModel) deferTimerNote(id string) {
	if m.timerNoteDeferred == nil {
		m.timerNoteDeferred = make(map[string]bool)
	}
	m.timerNoteDeferred[id] = true
}

func (m browserModel) timerNoteKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, key := m.timer.review, msg.String()
	if f.discard {
		switch key {
		case "esc":
			f.discard, f.quitAfter = false, false
		case "d":
			m.deferTimerNote(f.target.Session.ID)
			m.timer.review = nil
			m.notice = "Unsaved addition discarded; stored note preserved and review still pending."
			if f.quitAfter {
				return m, tea.Quit
			}
		case "s":
			f.discard = false
			return m.saveTimerNote(false)
		}
		return m, nil
	}
	switch key {
	case "esc":
		if len(f.addition.value) != 0 {
			f.discard = true
			return m, nil
		}
		m.deferTimerNote(f.target.Session.ID)
		m.timer.review = nil
		m.notice = "Completion note deferred; upload remains held. Press e in Focus to reopen it."
		return m, nil
	case "tab":
		f.field = (f.field + 1) % 3
	case "shift+tab":
		f.field = (f.field + 2) % 3
	case "ctrl+s":
		return m.saveTimerNote(false)
	case "enter":
		if f.field > 0 {
			return m.saveTimerNote(f.field == 2)
		}
		if len(f.addition.value) == 0 {
			return m.saveTimerNote(true)
		}
		f.addition.updateBody(msg)
		f.err = ""
	case "pgup", "pgdown":
		f.offset = scroll(f.offset, key, max(1, m.height-8), len(m.timerNoteMetadata()))
	default:
		if f.field == 0 {
			f.addition.updateBody(msg)
			f.err = ""
		}
	}
	return m, nil
}

func (m browserModel) saveTimerNote(skip bool) (browserModel, tea.Cmd) {
	f := m.timer.review
	if m.timerWorking {
		f.err = "Focus state is refreshing; try Save or Skip again."
		f.offset = 0
		return m, nil
	}
	if _, ok := m.timers.(TimerNoteActions); !ok {
		f.err = "Completion-note review is unavailable in this session."
		return m, nil
	}
	if !skip && len(f.addition.value) == 0 {
		f.err = "Enter an addition or choose Skip to keep the existing note."
		f.offset = 0
		return m, nil
	}
	addition := string(f.addition.value)
	if skip {
		addition = ""
	}
	return m.runTimer(timerConfirmation{action: "review note", target: f.target, addition: addition, skip: skip}, true)
}

func (m browserModel) timerNoteMetadata() []string {
	f := m.timer.review
	var out []string
	add := func(text string) {
		for i, line := range strings.Split(ansi.Hardwrap(display(text), max(1, m.width-6), true), "\n") {
			if i != 0 {
				line = "  " + line
			}
			out = append(out, line)
		}
	}
	if f.err != "" {
		add("Not saved: " + f.err)
	}
	kind := "Timer"
	if f.target.Session.FocusType == 0 {
		kind = "Pomodoro"
	}
	add("Session ID: " + f.target.Session.ID)
	add("Kind: " + kind + "; active: " + focusClock(f.target.Session.ActiveDuration))
	add("Started: " + f.target.Session.StartedAt.Format(time.RFC3339))
	add("Ended: " + f.target.Session.EndedAt.Format(time.RFC3339))
	if f.target.Session.TaskID == "" {
		add("Task: none")
	} else {
		if f.taskTitle != "" {
			add("Task: " + f.taskTitle)
		}
		add("Task ID: " + f.target.Session.TaskID)
	}
	add("Existing focus note: " + f.target.Session.Note)
	add("Adds to focus history, not a task comment. Skip preserves the existing note.")
	add("Upload waits for Save or Skip. Esc defers; changed input asks before discard. Pending review survives restart.")
	return out
}

func (m browserModel) timerNoteView() (string, []string) {
	f := m.timer.review
	if f.discard {
		return "Unsaved completion note", wrapText("s: save addition | d: discard addition | Esc: continue editing\nDiscard keeps the stored start note and leaves review pending.", max(1, m.width-4))
	}
	metadata := m.timerNoteMetadata()
	rows := max(1, m.height-8)
	start := min(f.offset, max(0, len(metadata)-rows))
	lines := append([]string(nil), metadata[start:min(len(metadata), start+rows)]...)
	width := max(1, m.width-4)
	lines = append(lines, marker(f.field == 0)+"Add: "+f.addition.view(max(1, width-7)))
	lines = append(lines, marker(f.field == 1)+"Save addition  "+marker(f.field == 2)+"Skip")
	return "Focus completion note", lines
}
