package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type timerNoteNoInput struct{ reads int }

func (r *timerNoteNoInput) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("unexpected completion-note input read")
}

func timerNoteStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func pendingTimerNote(t *testing.T, st *store.Store, note string) store.TimerSession {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if _, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1, Note: note, ReviewNote: true}, now); err != nil {
		t.Fatal(err)
	}
	done, err := st.StopTimer(ctx, now.Add(time.Minute))
	if err != nil || done.Completed == nil || !done.Completed.NoteReviewPending {
		t.Fatalf("pending note fixture: %+v %v", done, err)
	}
	return *done.Completed
}

func timerNoteRuntime() timerRuntime {
	return timerRuntime{now: func() time.Time { return time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC) }, watch: func(string) error { return nil }}
}

func timerNoteAsk(t *testing.T, ask bool) {
	t.Helper()
	before := canAsk
	canAsk = func(*invocation) bool { return ask }
	t.Cleanup(func() { canAsk = before })
}

func TestTimerCompletionNoteInteractiveAppendSkipAndEOF(t *testing.T) {
	for _, input := range []string{"finished\n", "\n", ""} {
		t.Run(input, func(t *testing.T) {
			isolate(t)
			timerNoteAsk(t, true)
			st := timerNoteStore(t)
			session := pendingTimerNote(t, st, "start note")
			inv, stdout, stderr := timerTestInvocation("timer", []string{"status"}, config.Default())
			inv.stdin = strings.NewReader(input)
			if code := cmdTimerWithRuntime(inv, timerNoteRuntime()); code != exitOK {
				t.Fatalf("interactive note: %d %s %s", code, stdout.String(), stderr.String())
			}
			got, err := st.TimerSessionByID(inv.ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "start note"
			if input == "finished\n" {
				want += "\n\nfinished"
			}
			if got.Note != want || got.NoteReviewPending != (input == "") || !strings.Contains(stderr.String(), "Focus-history note") {
				t.Fatalf("note outcome: %+v stderr=%s", got, stderr.String())
			}
		})
	}
}

func TestTimerNoteNeverReadsJSONRedirectedOrNoPromptInput(t *testing.T) {
	for _, mode := range []string{"json", "redirected", "no-prompt"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			timerNoteAsk(t, mode != "redirected")
			st := timerNoteStore(t)
			session := pendingTimerNote(t, st, "keep")
			args := []string{"status"}
			if mode == "no-prompt" {
				args = append(args, "--no-prompt")
			}
			inv, _, stderr := timerTestInvocation("timer", args, config.Default())
			input := &timerNoteNoInput{}
			inv.stdin, inv.jsonOutput = input, mode == "json"
			if code := cmdTimerWithRuntime(inv, timerNoteRuntime()); code != exitOK || input.reads != 0 {
				t.Fatalf("noninteractive note: %d reads=%d stderr=%s", code, input.reads, stderr.String())
			}
			got, err := st.TimerSessionByID(inv.ctx, session.ID)
			if err != nil || !got.NoteReviewPending || got.Note != "keep" {
				t.Fatalf("noninteractive command resolved pending review: %+v %v", got, err)
			}
		})
	}
}

func TestTimerInteractiveStartAndStopReviewIntent(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		t.Run(map[bool]string{false: "interactive", true: "no-prompt"}[suppress], func(t *testing.T) {
			isolate(t)
			timerNoteAsk(t, true)
			st := timerNoteStore(t)
			args := []string{"start", "--none"}
			if suppress {
				args = append(args, "--no-prompt")
			}
			inv, _, stderr := timerTestInvocation("timer", args, config.Default())
			input := &timerNoteNoInput{}
			inv.stdin = input
			rt := timerNoteRuntime()
			if code := cmdTimerWithRuntime(inv, rt); code != exitOK || input.reads != 0 {
				t.Fatalf("start: %d %s", code, stderr.String())
			}
			done, err := st.StopTimer(inv.ctx, rt.now().Add(time.Minute))
			if err != nil || done.Completed == nil || done.Completed.NoteReviewPending == suppress {
				t.Fatalf("start review intent: %+v %v", done, err)
			}
		})
	}
	t.Run("interactive stop adopts headless timer", func(t *testing.T) {
		isolate(t)
		timerNoteAsk(t, true)
		st := timerNoteStore(t)
		rt := timerNoteRuntime()
		started, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 1, Note: "before"}, rt.now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		inv, _, stderr := timerTestInvocation("timer", []string{"stop"}, config.Default())
		inv.stdin = strings.NewReader("after\n")
		if code := cmdTimerWithRuntime(inv, rt); code != exitOK {
			t.Fatalf("stop: %d %s", code, stderr.String())
		}
		got, err := st.TimerSessionByID(inv.ctx, started.State.SessionID)
		if err != nil || got.Note != "before\n\nafter" || got.NoteReviewPending || got.ActiveDuration != time.Minute {
			t.Fatalf("stop note: %+v %v", got, err)
		}
	})
}

func TestTimerNoteExplicitSessionAndReplayNeverRetarget(t *testing.T) {
	isolate(t)
	st := timerNoteStore(t)
	first := pendingTimerNote(t, st, "first")
	second := pendingTimerNote(t, st, "second")
	result, code := runMachine(t, context.Background(), "timer", "note", first.ID, "--note", "result")
	if code != exitOK || result.Status != "recorded" || !strings.Contains(string(result.Data), "first\\n\\nresult") {
		t.Fatalf("exact note: %d error=%+v data=%s", code, result.Error, result.Data)
	}
	if _, code := runMachine(t, context.Background(), "timer", "note", first.ID, "--note", "duplicate"); code == exitOK {
		t.Fatal("replayed review appended again")
	}
	if _, code := runMachine(t, context.Background(), "timer", "note", "missing", "--skip"); code == exitOK {
		t.Fatal("missing ID resolved another session")
	}
	got, err := st.TimerSessionByID(context.Background(), second.ID)
	if err != nil || !got.NoteReviewPending || got.Note != "second" {
		t.Fatalf("unselected session changed: %+v %v", got, err)
	}
}

func TestTimerDetachedDeadlineRetainsReviewWithoutInput(t *testing.T) {
	isolate(t)
	timerNoteAsk(t, true)
	st := timerNoteStore(t)
	rt := timerNoteRuntime()
	started, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 0, Planned: time.Minute, ReviewNote: true}, rt.now().Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	inv, _, stderr := timerTestInvocation("timer", []string{"_watch", started.State.SessionID}, config.Default())
	input := &timerNoteNoInput{}
	inv.stdin = input
	if code := cmdTimerWithRuntime(inv, rt); code != exitOK || input.reads != 0 {
		t.Fatalf("watcher: %d reads=%d %s", code, input.reads, stderr.String())
	}
	got, err := st.TimerSessionByID(inv.ctx, started.State.SessionID)
	if err != nil || !got.NoteReviewPending || got.ActiveDuration != time.Minute {
		t.Fatalf("deadline note hold: %+v %v", got, err)
	}
}

func TestReadTimerNotePreservesSpacesAndDefersPartialEOF(t *testing.T) {
	text, err := readTimerNoteAnswer(context.Background(), strings.NewReader("  exact note  \r\n"))
	if err != nil || text != "  exact note  " {
		t.Fatalf("note text: %q %v", text, err)
	}
	_, err = readTimerNoteAnswer(context.Background(), strings.NewReader("incomplete"))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("partial line must defer: %v", err)
	}
}

func TestTimerStatusDiscoversWatcherFirstReviewBeyondHistoryLimitWithoutPrompt(t *testing.T) {
	isolate(t)
	timerNoteAsk(t, false)
	st := timerNoteStore(t)
	rt := timerNoteRuntime()
	now := rt.now().Add(-2 * time.Hour)
	started, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 0, Planned: time.Minute, ReviewNote: true, Note: "old review"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TimerStatus(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 21; i++ {
		at := now.Add(time.Duration(i+1) * 3 * time.Minute)
		if _, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 1}, at); err != nil {
			t.Fatal(err)
		}
		if _, err := st.StopTimer(context.Background(), at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	history, err := st.TimerHistory(context.Background(), 20)
	if err != nil || len(history) != 20 {
		t.Fatalf("history fixture: %d %v", len(history), err)
	}
	for _, session := range history {
		if session.ID == started.State.SessionID {
			t.Fatal("old review fixture is still in the latest history")
		}
	}
	result, code := runMachine(t, context.Background(), "timer", "status")
	var data struct {
		NoteReviews []struct {
			SessionID string `json:"session_id"`
			Pending   bool   `json:"note_review_pending"`
			Version   string `json:"version"`
		} `json:"note_reviews"`
	}
	if code != exitOK || json.Unmarshal(result.Data, &data) != nil || len(data.NoteReviews) != 1 || data.NoteReviews[0].SessionID != started.State.SessionID || !data.NoteReviews[0].Pending || data.NoteReviews[0].Version == "" {
		t.Fatalf("JSON status lost old review: %d error=%+v data=%s", code, result.Error, result.Data)
	}
	inv, stdout, stderr := timerTestInvocation("timer", []string{"status"}, config.Default())
	input := &timerNoteNoInput{}
	inv.stdin = input
	if code := cmdTimerWithRuntime(inv, rt); code != exitOK || input.reads != 0 || !strings.Contains(stdout.String(), "Pending completion-note reviews: 1") || !strings.Contains(stdout.String(), started.State.SessionID) {
		t.Fatalf("headless status lost old review: %d reads=%d output=%s stderr=%s", code, input.reads, stdout.String(), stderr.String())
	}
}

func TestTimerHeadlessAndJSONStartsDoNotRequestReview(t *testing.T) {
	for _, mode := range []string{"headless", "json"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			timerNoteAsk(t, mode == "json")
			st := timerNoteStore(t)
			inv, _, stderr := timerTestInvocation("timer", []string{"start", "--none"}, config.Default())
			input := &timerNoteNoInput{}
			inv.stdin, inv.jsonOutput = input, mode == "json"
			rt := timerNoteRuntime()
			if code := cmdTimerWithRuntime(inv, rt); code != exitOK || input.reads != 0 {
				t.Fatalf("start: %d reads=%d %s", code, input.reads, stderr.String())
			}
			done, err := st.StopTimer(inv.ctx, rt.now().Add(time.Minute))
			if err != nil || done.Completed == nil || done.Completed.NoteReviewPending {
				t.Fatalf("noninteractive start requested a review: %+v %v", done, err)
			}
		})
	}
}

func TestTimerCompletionNotePromptIdentifiesOlderSessionAndEscapesCacheTitle(t *testing.T) {
	isolate(t)
	timerNoteAsk(t, true)
	id := seedFeatureCommandTask(t, model.Task{Title: "cached\nSession: forged\x1b]52;c;x\a"})
	st := timerNoteStore(t)
	rt := timerNoteRuntime()
	started, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 1, TaskID: id, ReviewNote: true, Note: "start\nTask: forged"}, rt.now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(context.Background(), rt.now().Add(-59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	inv, _, stderr := timerTestInvocation("timer", []string{"status"}, config.Default())
	if code := cmdTimerWithRuntime(inv, rt); code != exitOK {
		t.Fatalf("prompt: %d %s", code, stderr.String())
	}
	output := stderr.String()
	unwrapped := strings.ReplaceAll(output, "\n  ", "")
	for _, want := range []string{started.State.SessionID, "Kind: Timer; active: 1m", "Started: ", "Ended: ", "Task: cached", "(ID: " + id + ")"} {
		if !strings.Contains(unwrapped, want) {
			t.Fatalf("missing %q in prompt %q", want, output)
		}
	}
	if strings.Contains(output, "\nSession: forged") || strings.Contains(output, "\nTask: forged") || strings.Contains(output, "\x1b]52") || strings.ContainsRune(output, '\a') {
		t.Fatal("completion prompt emitted a forged field or terminal control")
	}
	for _, line := range strings.Split(output, "\n") {
		if ansi.StringWidth(line) > cli.Width {
			t.Fatalf("prompt overflow: %q", line)
		}
	}
}

func TestTimerCompletionNoteWakeWarningBoundsAndEscapes(t *testing.T) {
	for _, warning := range []string{"bad\n\r\x1b]52;c;data\a", strings.Repeat("long warning \u754c", 100)} {
		t.Run(warning[:3], func(t *testing.T) {
			isolate(t)
			st := timerNoteStore(t)
			session := pendingTimerNote(t, st, "start note")
			cfg := config.Default()
			cfg.FocusUpload.Enabled = true
			inv, stdout, stderr := timerTestInvocation("timer", []string{"note", session.ID, "--note", "completion"}, cfg)
			rt, wakes := timerNoteRuntime(), 0
			rt.wake = func() error {
				wakes++
				return errors.New(warning)
			}
			if code := cmdTimerWithRuntime(inv, rt); code != exitOK || wakes != 1 {
				t.Fatalf("saved note with wake warning: code=%d wakes=%d stderr=%q", code, wakes, stderr.String())
			}
			line := strings.TrimSuffix(stderr.String(), "\n")
			if !strings.Contains(line, "note review saved") || cli.DisplayWidth(line) > cli.Width || strings.ContainsAny(line, "\n\r\x1b\a") {
				t.Fatalf("unsafe final wake warning: %q", line)
			}
			for _, line := range strings.Split(stdout.String(), "\n") {
				if cli.DisplayWidth(line) > cli.Width {
					t.Fatalf("saved-note report overflow: %q", line)
				}
			}
			got, err := st.TimerSessionByID(inv.ctx, session.ID)
			if err != nil || got.NoteReviewPending || got.Note != "start note\n\ncompletion" || !strings.Contains(stdout.String(), session.ID) {
				t.Fatalf("wake warning lost the saved note: %+v %v stdout=%q", got, err, stdout.String())
			}
		})
	}
}
