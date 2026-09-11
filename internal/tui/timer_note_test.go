package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func tuiPendingNote(t *testing.T, st *store.Store, note string) store.TimerSession {
	t.Helper()
	now := time.Now().Add(-2 * time.Minute)
	if _, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 1, Note: note, ReviewNote: true}, now); err != nil {
		t.Fatal(err)
	}
	done, err := st.StopTimer(context.Background(), now.Add(time.Minute))
	if err != nil || done.Completed == nil || !done.Completed.NoteReviewPending {
		t.Fatalf("TUI pending note fixture: %+v %v", done, err)
	}
	return *done.Completed
}

func refreshTUINote(m browserModel) browserModel {
	m, cmd := m.loadTimer()
	m, _ = step(m, cmd())
	return m
}

func TestTimerNoteFormAppendsToExactSessionAndKeepsTaskShortcutsAsText(t *testing.T) {
	m, st := focusModel(t)
	session := tuiPendingNote(t, st, "start note")
	m = refreshTUINote(m)
	if m.timer.review == nil || m.timer.review.target.Session.ID != session.ID {
		t.Fatal("pending completion did not open its exact note review")
	}
	selected := m.taskID
	for _, char := range "qaeus+_123?/" {
		var cmd tea.Cmd
		m, cmd = press(m, char, 0)
		if cmd != nil {
			t.Fatal("completion-note text triggered an operation")
		}
	}
	if m.taskID != selected || m.jump || m.searching || m.form != nil || m.dialog != nil {
		t.Fatal("completion-note text activated browser shortcuts")
	}
	m.taskID = "another-selected-task"
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishFocus(t, m, cmd)
	got, err := st.TimerSessionByID(m.ctx, session.ID)
	if err != nil || got.NoteReviewPending || got.Note != "start note\n\nqaeus+_123?/" || got.TaskID != session.TaskID || m.timer.review != nil {
		t.Fatalf("note saved to wrong target: %+v %v", got, err)
	}
}

func TestTimerNoteEmptyEnterSkipsBothFocusModes(t *testing.T) {
	for _, focusType := range []int{0, 1} {
		for _, note := range []string{"", "keep exactly\nsecond line"} {
			name := map[int]string{0: "pomodoro", 1: "timer"}[focusType]
			t.Run(name+"/"+map[bool]string{true: "with start note", false: "no start note"}[note != ""], func(t *testing.T) {
				m, st := focusModel(t)
				now := time.Now().Add(-2 * time.Minute)
				options := store.TimerStartOptions{FocusType: focusType, Note: note, ReviewNote: true}
				if focusType == 0 {
					options.Planned = time.Minute
				}
				if _, err := st.StartTimer(m.ctx, options, now); err != nil {
					t.Fatal(err)
				}
				done, err := st.StopTimer(m.ctx, now.Add(time.Minute))
				if err != nil || done.Completed == nil || !done.Completed.NoteReviewPending {
					t.Fatalf("completion fixture: %+v %v", done, err)
				}
				m = refreshTUINote(m)
				if m.timer.review == nil || m.timer.review.field != 0 {
					t.Fatal("review did not focus the empty input")
				}
				m, cmd := press(m, tea.KeyEnter, 0)
				if cmd == nil {
					t.Fatal("empty Enter did not resolve the review")
				}
				m = finishFocus(t, m, cmd)
				got, err := st.TimerSessionByID(m.ctx, done.Completed.ID)
				if err != nil || got.NoteReviewPending || got.Note != note || got.FocusType != focusType || m.timer.review != nil {
					t.Fatalf("empty Enter changed the session or kept its hold: %+v %v", got, err)
				}
				if ids, err := st.PendingFocusSessionIDs(m.ctx, false); err != nil || len(ids) != 1 || ids[0] != got.ID {
					t.Fatalf("skipped session is not eligible for upload: %v %v", ids, err)
				}
			})
		}
	}
}

func TestTimerNoteNonemptyEnterKeepsDraftUntilSave(t *testing.T) {
	for _, addition := range []string{"thought", " ", "first\nsecond"} {
		t.Run(addition, func(t *testing.T) {
			m, st := focusModel(t)
			session := tuiPendingNote(t, st, "before")
			m = refreshTUINote(m)
			m, _ = step(m, tea.PasteMsg{Content: addition})
			m, cmd := press(m, tea.KeyEnter, 0)
			if cmd != nil || m.timer.review == nil || string(m.timer.review.addition.value) != addition+"\n" {
				t.Fatal("Enter submitted or discarded nonempty input")
			}
			got, err := st.TimerSessionByID(m.ctx, session.ID)
			if err != nil || !got.NoteReviewPending || got.Note != session.Note {
				t.Fatalf("editing changed stored note or hold: %+v %v", got, err)
			}
			m, cmd = press(m, 's', tea.ModCtrl)
			m = finishFocus(t, m, cmd)
			got, err = st.TimerSessionByID(m.ctx, session.ID)
			if err != nil || got.NoteReviewPending || got.Note != "before\n\n"+addition+"\n" || m.timer.review != nil {
				t.Fatalf("explicit save lost the exact draft: %+v %v", got, err)
			}
		})
	}
}

func TestTimerNoteSkipDeferReopenAndStaleTarget(t *testing.T) {
	t.Run("skip", func(t *testing.T) {
		m, st := focusModel(t)
		session := tuiPendingNote(t, st, "keep exactly")
		m = refreshTUINote(m)
		m, _ = press(m, tea.KeyTab, 0)
		m, _ = press(m, tea.KeyTab, 0)
		m, cmd := press(m, tea.KeyEnter, 0)
		m = finishFocus(t, m, cmd)
		got, err := st.TimerSessionByID(m.ctx, session.ID)
		if err != nil || got.NoteReviewPending || got.Note != session.Note || m.timer.review != nil {
			t.Fatalf("skip did not preserve start note: %+v %v", got, err)
		}
	})
	t.Run("defer and reopen", func(t *testing.T) {
		m, st := focusModel(t)
		session := tuiPendingNote(t, st, "keep")
		m = refreshTUINote(m)
		m, _ = press(m, tea.KeyEscape, 0)
		m = refreshTUINote(m)
		if m.timer.review != nil {
			t.Fatal("polling immediately reopened a deferred review")
		}
		got, err := st.TimerSessionByID(m.ctx, session.ID)
		if err != nil || !got.NoteReviewPending || got.Note != session.Note {
			t.Fatalf("defer resolved the stored review: %+v %v", got, err)
		}
		m.timer, m.timerNoteDeferred = nil, nil
		m = refreshTUINote(m)
		if m.timer == nil || m.timer.review == nil || m.timer.review.target.Session.ID != session.ID {
			t.Fatal("reopened UI did not discover durable pending review")
		}
	})
	t.Run("stale", func(t *testing.T) {
		m, st := focusModel(t)
		session := tuiPendingNote(t, st, "before")
		m = refreshTUINote(m)
		if _, err := st.ResolveFocusNote(m.ctx, m.timer.review.target, "other frontend", false); err != nil {
			t.Fatal(err)
		}
		m, _ = step(m, tea.PasteMsg{Content: "duplicate"})
		m, cmd := press(m, 's', tea.ModCtrl)
		m = finishFocus(t, m, cmd)
		if m.timer.review == nil || m.timer.review.err == "" {
			t.Fatal("stale review did not retain the refusal")
		}
		got, err := st.TimerSessionByID(m.ctx, session.ID)
		if err != nil || got.Note != "before\n\nother frontend" {
			t.Fatalf("stale review overwrote resolved note: %+v %v", got, err)
		}
	})
}

func TestTimerNoteWatcherFirstDeadlineAndModalPrecedence(t *testing.T) {
	m, st := focusModel(t)
	now := time.Now().Add(-2 * time.Minute)
	started, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 0, Planned: time.Minute, ReviewNote: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TimerStatus(m.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	m.form = &taskForm{}
	m = refreshTUINote(m)
	if m.timer.review != nil || m.form == nil {
		t.Fatal("deadline replaced an existing task form")
	}
	m.form = nil
	m = refreshTUINote(m)
	if m.timer.review == nil || m.timer.review.target.Session.ID != started.State.SessionID {
		t.Fatal("watcher-first completion was lost after modal closed")
	}
	if ids, err := st.PendingFocusSessionIDs(m.ctx, false); err != nil || len(ids) != 0 {
		t.Fatalf("pending review leaked into upload selection: %v %v", ids, err)
	}
}

func TestTimerNoteDirtyEscapeAndInterruptNeedExplicitDiscard(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "escape", true: "interrupt"}[interrupt], func(t *testing.T) {
			m, st := focusModel(t)
			session := tuiPendingNote(t, st, "stored")
			m = refreshTUINote(m)
			m, _ = step(m, tea.PasteMsg{Content: "unsaved addition"})
			var cmd tea.Cmd
			if interrupt {
				m, cmd = press(m, 'c', tea.ModCtrl)
			} else {
				m, cmd = press(m, tea.KeyEscape, 0)
			}
			if cmd != nil || m.timer.review == nil || !m.timer.review.discard || m.timer.review.quitAfter != interrupt {
				t.Fatal("dirty note exited without a choice")
			}
			m, _ = step(m, tea.PasteMsg{Content: "ignored at choice"})
			m, _ = press(m, tea.KeyEscape, 0)
			if string(m.timer.review.addition.value) != "unsaved addition" || m.timer.review.discard || m.timer.review.quitAfter {
				t.Fatal("continue editing changed the exact draft")
			}
			m, _ = press(m, tea.KeyEscape, 0)
			m, cmd = press(m, 'd', 0)
			if cmd != nil || m.timer.review != nil {
				t.Fatal("explicit discard did not defer the review")
			}
			got, err := st.TimerSessionByID(m.ctx, session.ID)
			if err != nil || !got.NoteReviewPending || got.Note != "stored" {
				t.Fatalf("discard altered stored note or hold: %+v %v", got, err)
			}
		})
	}
}

func TestTimerNoteFormEscapesMetadataAndFitsNarrowTerminals(t *testing.T) {
	m, st := focusModel(t)
	tuiPendingNote(t, st, "start\nSession ID: forged\x1b]52;c;x\a")
	m = refreshTUINote(m)
	m, _ = step(m, tea.PasteMsg{Content: "result\nqsu\x1b]52;c;x\a"})
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m, _ = step(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		metadata := strings.Join(m.timerNoteMetadata(), "\n")
		if strings.Contains(metadata, "\nSession ID: forged") || strings.Contains(metadata, "\x1b]52") || strings.ContainsRune(metadata, '\a') {
			t.Fatal("note metadata injected a field or terminal control")
		}
		if size[0] == 160 && (!strings.Contains(metadata, "Kind: Timer; active: 00:01:00") || !strings.Contains(metadata, "Started: ") || !strings.Contains(metadata, "Ended: ")) {
			t.Fatal("older note review lacks recognizable session metadata")
		}
		view := m.View().Content
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("completion-note form overflow: %q", line)
			}
		}
		if strings.Contains(view, "\x1b]52") || strings.ContainsRune(view, '\a') || m.modalHelp()[0] != "Focus completion note" {
			t.Fatal("note form escaped its input/help context")
		}
	}
}

func TestTimerNoteSavedDespiteWakeFailureDoesNotOfferDuplicateAppend(t *testing.T) {
	m, st := focusModel(t)
	cfg := config.Default()
	cfg.FocusUpload.Enabled = true
	m.timers = app.NewTimers(st, cfg, nil, nil, nil).WithCompletionWake(func() error { return errors.New("synthetic wake failure") })
	session := tuiPendingNote(t, st, "before")
	m = refreshTUINote(m)
	m, _ = step(m, tea.PasteMsg{Content: "after"})
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishFocus(t, m, cmd)
	got, err := st.TimerSessionByID(m.ctx, session.ID)
	if err != nil || got.NoteReviewPending || got.Note != "before\n\nafter" || m.timer.review != nil || !strings.Contains(m.notice, "synthetic wake failure") {
		t.Fatalf("saved note mishandled wake failure: %+v %v notice=%s", got, err, m.notice)
	}
}

func TestCompletionNoteMetadataContinuationCannotImpersonateFields(t *testing.T) {
	m, st := focusModel(t)
	tuiPendingNote(t, st, "start")
	m = refreshTUINote(m)
	for _, width := range []int{32, 80, 160} {
		m.width = width
		for padding := 0; padding < width; padding++ {
			m.timer.review.target.Session.Note = strings.Repeat("x", padding) + "\nSession ID: forged"
			lines := m.timerNoteMetadata()
			text := strings.Join(lines, "\n")
			if strings.Contains(text, "\nSession ID: forged") {
				t.Fatalf("wrapped note impersonated a field at width %d padding %d", width, padding)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > width-4 {
					t.Fatal("indented note metadata exceeded its panel width")
				}
			}
		}
	}
}

func TestTUIManualStopRequestsCompletionReviewForHeadlessSession(t *testing.T) {
	m, st := focusModel(t)
	started, err := st.StartTimer(m.ctx, store.TimerStartOptions{FocusType: 1, Note: "headless"}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	m = refreshTUINote(m)
	m, _ = press(m, 'x', 0)
	if m.timer.confirm == nil || !m.timer.confirm.options.ReviewNote {
		t.Fatal("manual stop did not request a review")
	}
	m, cmd := press(m, tea.KeyEnter, 0)
	m = finishFocus(t, m, cmd)
	if m.timer.review == nil || m.timer.review.target.Session.ID != started.State.SessionID || m.timerData.Active.State != nil {
		t.Fatal("manual stop did not finish timing before note input")
	}
}

var _ TimerNoteActions = (*app.Timers)(nil)
