package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func finishNoteFixture(t *testing.T, st *Store, note string, review bool) TimerSession {
	t.Helper()
	ctx := context.Background()
	now := timerTestNow()
	if _, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1, Note: note, ReviewNote: review}, now); err != nil {
		t.Fatal(err)
	}
	result, err := st.StopTimer(ctx, now.Add(time.Minute))
	if err != nil || result.Completed == nil {
		t.Fatalf("finish: %+v %v", result, err)
	}
	return *result.Completed
}

func TestFocusNoteReviewPersistsAcrossBackgroundCompletionAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := timerTestNow()
	started, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Minute, Note: "before", ReviewNote: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	done, err := st.TimerStatus(ctx, now.Add(24*time.Hour))
	if err != nil || done.Completed == nil || !done.Completed.NoteReviewPending || !done.Completed.EndedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("background completion: %+v %v", done, err)
	}
	reviews, err := st.PendingFocusNoteReviews(ctx)
	if err != nil || len(reviews) != 1 || reviews[0].Session.ID != started.State.SessionID {
		t.Fatalf("reviews: %+v %v", reviews, err)
	}
	if ids, err := st.PendingFocusSessionIDs(ctx, false); err != nil || len(ids) != 0 {
		t.Fatalf("review leaked into upload queue: %v %v", ids, err)
	}
	if _, send, err := st.ArmFocusUpload(ctx, reviews[0].Session.ID, "", json.RawMessage(`{"note":"before"}`), []string{}); err == nil || send {
		t.Fatalf("pending review armed: %t %v", send, err)
	}
	if _, err := st.FocusUpload(ctx, reviews[0].Session.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("blocked arm left a frozen upload: %v", err)
	}
	resolved, err := st.ResolveFocusNote(ctx, reviews[0], "after", false)
	if err != nil || resolved.Note != "before\n\nafter" || resolved.NoteReviewPending || resolved.ActiveDuration != time.Minute {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	if _, err := st.ResolveFocusNote(ctx, reviews[0], "duplicate", false); err == nil {
		t.Fatal("stale review appended twice")
	}
	if ids, err := st.PendingFocusSessionIDs(ctx, false); err != nil || len(ids) != 1 || ids[0] != resolved.ID {
		t.Fatalf("resolved review did not release upload: %v %v", ids, err)
	}
	if pending, err := st.PendingFocusNoteReviews(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("resolved review still pending: %+v %v", pending, err)
	}
}

func TestFocusNoteReviewStopAndCancellationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                                  string
		startReview, stopReview, elapsed, cancel, wantPending bool
	}{
		{name: "headless"},
		{name: "interactive start", startReview: true, wantPending: true},
		{name: "interactive stop", stopReview: true, wantPending: true},
		{name: "interactive overdue stop", stopReview: true, elapsed: true, wantPending: true},
		{name: "aborted", startReview: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			now := timerTestNow()
			options := TimerStartOptions{FocusType: 1, ReviewNote: tc.startReview}
			if tc.elapsed {
				options.FocusType, options.Planned = 0, time.Second
			}
			if _, err := st.StartTimer(ctx, options, now); err != nil {
				t.Fatal(err)
			}
			action := "stop"
			if tc.cancel {
				action = "cancel"
			}
			result, err := st.ControlTimer(ctx, action, TimerStartOptions{ReviewNote: tc.stopReview}, nil, now.Add(time.Minute))
			if err != nil || result.Completed == nil || result.Completed.NoteReviewPending != tc.wantPending {
				t.Fatalf("policy: %+v %v", result, err)
			}
			ids, err := st.PendingFocusSessionIDs(ctx, true)
			if err != nil || (len(ids) == 0) != tc.wantPending {
				t.Fatalf("upload policy: %v %v", ids, err)
			}
		})
	}
}

func TestFocusNoteReviewSkipValidationAndUnrelatedTiming(t *testing.T) {
	for _, tc := range []struct {
		name, initial, addition, want string
		skip, fail                    bool
	}{
		{name: "append", initial: "start", addition: "end", want: "start\n\nend"},
		{name: "empty start", addition: "end", want: "end"},
		{name: "skip", initial: "start", skip: true, want: "start"},
		{name: "empty addition", initial: "start", want: "start"},
		{name: "skip with text", addition: "lost", skip: true, fail: true},
		{name: "invalid UTF-8", addition: string([]byte{0xff}), fail: true},
		{name: "combined too long", initial: "start", addition: strings.Repeat("a", 4994), fail: true},
		{name: "unicode boundary", initial: strings.Repeat("a", 4997), addition: "\u044f", want: strings.Repeat("a", 4997) + "\n\n\u044f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			session := finishNoteFixture(t, st, tc.initial, true)
			target, err := st.ReadFocusTarget(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := st.ResolveFocusNote(ctx, target, tc.addition, tc.skip)
			if tc.fail {
				if err == nil {
					t.Fatal("invalid note was accepted")
				}
				current, readErr := st.TimerSessionByID(ctx, session.ID)
				if readErr != nil || !reflect.DeepEqual(current, session) {
					t.Fatalf("failed review mutated timing or note: %+v %v", current, readErr)
				}
				return
			}
			want := session
			want.Note, want.NoteReviewPending = tc.want, false
			if err != nil || !reflect.DeepEqual(resolved, want) {
				t.Fatalf("review changed other session data: %+v %v", resolved, err)
			}
		})
	}
}

func TestFocusNoteReviewNeverRewritesFrozenUpload(t *testing.T) {
	for _, phase := range []string{"armed", "accepted", "rejected", "confirmed"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			session := finishNoteFixture(t, st, "frozen", false)
			request := json.RawMessage(`{"note":"frozen","extra":9007199254740993}`)
			if _, send, err := st.ArmFocusUpload(ctx, session.ID, "", request, []string{}); err != nil || !send {
				t.Fatalf("arm: %t %v", send, err)
			}
			if _, err := st.DB().Exec(`UPDATE focus_uploads SET phase=? WHERE session_id=?`, phase, session.ID); err != nil {
				t.Fatal(err)
			}
			// Retained upload recovery has priority even if a damaged row says pending.
			if _, err := st.DB().Exec(`UPDATE focus_sessions SET note_review_pending=1 WHERE id=?`, session.ID); err != nil {
				t.Fatal(err)
			}
			target, err := st.ReadFocusTarget(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ResolveFocusNote(ctx, target, "late", false); err == nil {
				t.Fatal("frozen note changed")
			}
			after, err := st.FocusUpload(ctx, session.ID)
			if err != nil || !reflect.DeepEqual(after, *target.Upload) {
				t.Fatalf("frozen request changed: %+v %v", after, err)
			}
			if ids, err := st.PendingFocusSessionIDs(ctx, false); err != nil || len(ids) != 1 || ids[0] != session.ID {
				t.Fatalf("note flag blocked recovery: %v %v", ids, err)
			}
		})
	}
}

func TestFocusNoteReviewConcurrentAcceptanceAppendsOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	session := finishNoteFixture(t, st, "start", true)
	target, err := st.ReadFocusTarget(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.ResolveFocusNote(ctx, target, "end", false)
		}(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("expected exactly one acceptance: %v", errs)
	}
	current, err := st.TimerSessionByID(ctx, session.ID)
	if err != nil || current.Note != "start\n\nend" || current.NoteReviewPending {
		t.Fatalf("concurrent review: %+v %v", current, err)
	}
}

func TestFocusNoteMigrationKeepsLegacyIntentAndFrozenRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 14)
	if _, err := db.Exec(`INSERT INTO focus_sessions(id,kind,started_at,ended_at,note,outcome,focus_type,time_precision,planned_ms,pause_ms)
		VALUES ('old','focus',1000,2000,'old note','done',1,3,0,0)`); err != nil {
		t.Fatal(err)
	}
	request := `{"note":"old note","extra":9007199254740993}`
	if _, err := db.Exec(`INSERT INTO focus_uploads(session_id,phase,request,prior_ids) VALUES ('old','armed',?,'[]')`, request); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var intent, pending int
	if err := st.DB().QueryRow(`SELECT note_review_intent,note_review_pending FROM focus_sessions WHERE id='old'`).Scan(&intent, &pending); err != nil || intent != 0 || pending != 0 {
		t.Fatalf("legacy intent changed: %d %d %v", intent, pending, err)
	}
	row, err := st.FocusUpload(context.Background(), "old")
	if err != nil || string(row.Request) != request || row.Phase != "armed" {
		t.Fatalf("migration rewrote upload: %+v %v", row, err)
	}
}
