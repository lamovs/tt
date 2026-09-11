package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func TestTimerNoteReviewDiscoverySaveAndWakeWithoutRenotification(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Add(-time.Minute)
	if _, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1, ReviewNote: true, Note: "start"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.FocusUpload.Enabled, cfg.Timer.OnEnd = true, "synthetic"
	clients, notified, wakes := 0, 0, 0
	timers := NewTimers(st, cfg, func() (*api.Client, error) { clients++; return nil, errors.New("no network") }, nil,
		func(context.Context, store.TimerSession) error { notified++; return nil }).WithCompletionWake(func() error { wakes++; return nil })
	data, err := timers.Read(ctx, time.Now())
	if err != nil || len(data.NoteReviews) != 1 || clients != 0 || wakes != 0 || notified != 0 {
		t.Fatalf("review discovery: %+v %v", data, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := timers.ReviewNote(canceled, data.NoteReviews[0], "lost", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled review: %v", err)
	}
	session, err := timers.ReviewNote(ctx, data.NoteReviews[0], "end", false)
	if err != nil || session.Note != "start\n\nend" || session.NoteReviewPending || wakes != 1 || clients != 0 || notified != 0 {
		t.Fatalf("saved review: %+v %v wakes=%d clients=%d notified=%d", session, err, wakes, clients, notified)
	}
	if _, err := timers.ReviewNote(ctx, data.NoteReviews[0], "duplicate", false); err == nil || wakes != 1 {
		t.Fatalf("duplicate review woke uploader: %v %d", err, wakes)
	}
}

func TestTimerNoteReviewWakeFailureKeepsSavedNote(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Add(-time.Minute)
	if _, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1, ReviewNote: true, Note: "start"}, now); err != nil {
		t.Fatal(err)
	}
	done, err := st.StopTimer(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.ReadFocusTarget(ctx, done.Completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.FocusUpload.Enabled = true
	timers := NewTimers(st, cfg, nil, nil, nil).WithCompletionWake(func() error { return errors.New("synthetic wake failure") })
	session, err := timers.ReviewNote(ctx, target, "", true)
	if err == nil || session.Note != "start" || session.NoteReviewPending {
		t.Fatalf("wake failure lost saved note: %+v %v", session, err)
	}
	current, err := st.TimerSessionByID(ctx, session.ID)
	if err != nil || current.NoteReviewPending || current.Note != "start" {
		t.Fatalf("wake failure changed persistence: %+v %v", current, err)
	}
}
