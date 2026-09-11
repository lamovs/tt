package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func timerTestNow() time.Time { return time.Date(2026, 9, 9, 13, 0, 0, 123000000, time.UTC) }

func timerTables(t *testing.T, st *Store) []string {
	t.Helper()
	var result []string
	for _, table := range []string{"timer_state", "focus_sessions"} {
		rows, err := st.DB().QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s:%#v", table, values))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return result
}

func TestTimerPauseReopenAndExactDeadline(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := timerTestNow()
	started, err := st.StartTimer(ctx, TimerStartOptions{Planned: 10 * time.Second, Note: "local note"}, now)
	if err != nil || started.State == nil || started.Completed != nil {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	id := started.State.SessionID
	paused, err := st.PauseTimer(ctx, now.Add(1250*time.Millisecond))
	if err != nil || paused.State.ActiveDuration != 1250*time.Millisecond || paused.State.Deadline != nil {
		t.Fatalf("pause=%+v err=%v", paused, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := st.TimerStatus(ctx, now.Add(20*time.Second))
	if err != nil || status.State.ActiveDuration != 1250*time.Millisecond || status.State.PauseDuration != 18750*time.Millisecond {
		t.Fatalf("reopened pause=%+v err=%v", status, err)
	}
	resumed, err := st.ResumeTimer(ctx, now.Add(20111*time.Millisecond))
	deadline := now.Add(28861 * time.Millisecond)
	if err != nil || resumed.State.Deadline == nil || !resumed.State.Deadline.Equal(deadline) || resumed.State.PauseDuration != 18861*time.Millisecond {
		t.Fatalf("resume=%+v err=%v", resumed, err)
	}
	before, err := st.TimerStatus(ctx, deadline.Add(-time.Millisecond))
	if err != nil || before.State == nil || before.State.ActiveDuration != 9999*time.Millisecond {
		t.Fatalf("before deadline=%+v err=%v", before, err)
	}
	done, err := st.TimerStatus(ctx, deadline.Add(time.Hour))
	if err != nil || done.State != nil || done.Completed == nil {
		t.Fatalf("deadline=%+v err=%v", done, err)
	}
	if done.Completed.ID != id || !done.Completed.EndedAt.Equal(deadline) || done.Completed.ActiveDuration != 10*time.Second || done.Completed.PauseDuration != 18861*time.Millisecond || done.Completed.Outcome != "done" {
		t.Fatalf("completed=%+v", done.Completed)
	}
	again, err := st.TimerStatus(ctx, deadline.Add(2*time.Hour))
	if err != nil || again.State != nil || again.Completed != nil {
		t.Fatalf("second status=%+v err=%v", again, err)
	}
	history, err := st.TimerHistory(ctx, 20)
	if err != nil || len(history) != 1 || history[0].ID != id || !history[0].MillisecondPrecision || history[0].Note != "local note" {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestTimerStopPausedCancelAndTaskPromotion(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P1"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "timer task"})
	if err != nil {
		t.Fatal(err)
	}
	now := timerTestNow()
	started, err := st.StartTimer(ctx, TimerStartOptions{TaskID: task.Id, FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceLocalID(ctx, task.Id, "remote-task"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PauseTimer(ctx, now.Add(1222*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	stopped, err := st.StopTimer(ctx, now.Add(10099*time.Millisecond))
	if err != nil || stopped.Completed == nil {
		t.Fatalf("stop=%+v err=%v", stopped, err)
	}
	done := stopped.Completed
	if done.ID != started.State.SessionID || done.TaskID != "remote-task" || done.ActiveDuration != 1222*time.Millisecond || done.PauseDuration != 8877*time.Millisecond || done.FocusType != 1 {
		t.Fatalf("completed=%+v", done)
	}
	if claimed, err := st.ClaimTimerNotification(ctx, done.ID); err != nil || claimed {
		t.Fatalf("Timing notification=%v,%v", claimed, err)
	}
	if _, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Minute}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := st.CancelTimer(ctx, now.Add(time.Minute+time.Second))
	if err != nil || cancelled.Completed.Outcome != "aborted" || cancelled.Completed.ActiveDuration != time.Second {
		t.Fatalf("cancel=%+v err=%v", cancelled, err)
	}
	if claimed, err := st.ClaimTimerNotification(ctx, cancelled.Completed.ID); err != nil || claimed {
		t.Fatalf("cancel notification=%v,%v", claimed, err)
	}
}

func TestTimerReconcilesBeforeStartingNext(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := timerTestNow()
	first, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Second}, now)
	if err != nil {
		t.Fatal(err)
	}
	next, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now.Add(2*time.Second))
	if err != nil || next.State == nil || next.Completed == nil || next.Completed.ID != first.State.SessionID || next.State.SessionID == first.State.SessionID || !next.Completed.EndedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("start after deadline=%+v err=%v", next, err)
	}
}

func TestTimerClockRegressionAndInvalidStartPreserveState(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := timerTestNow()
	invalid := []TimerStartOptions{{Planned: 0}, {Planned: -time.Second}, {Planned: 25 * time.Hour}, {Planned: time.Nanosecond}, {FocusType: 1, Planned: time.Second}, {FocusType: 2}, {FocusType: 1, TaskID: "missing"}}
	for _, options := range invalid {
		before := timerTables(t, st)
		if result, err := st.StartTimer(ctx, options, now); err == nil || !reflect.DeepEqual(result, TimerResult{}) {
			t.Fatalf("invalid start=%+v err=%v", result, err)
		}
		if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
			t.Fatal("invalid start wrote timer state")
		}
	}
	if _, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PauseTimer(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResumeTimer(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TimerStatus(ctx, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]func(context.Context, time.Time) (TimerResult, error){"status": st.TimerStatus, "pause": st.PauseTimer, "resume": st.ResumeTimer, "stop": st.StopTimer, "cancel": st.CancelTimer} {
		t.Run(name, func(t *testing.T) {
			before := timerTables(t, st)
			if result, err := command(ctx, now.Add(2999*time.Millisecond)); !errors.Is(err, ErrTimerClockReversed) || !reflect.DeepEqual(result, TimerResult{}) {
				t.Fatalf("regression=%+v err=%v", result, err)
			}
			if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
				t.Fatal("clock reversal wrote state")
			}
		})
	}
}

func TestTimerTransactionsRollbackLateFailures(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := timerTestNow()
	if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER fail_timer_start BEFORE INSERT ON timer_state BEGIN SELECT RAISE(ABORT,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	before := timerTables(t, st)
	if result, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Second}, now); err == nil || !reflect.DeepEqual(result, TimerResult{}) {
		t.Fatalf("start failure=%+v,%v", result, err)
	}
	if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
		t.Fatal("failed start left session")
	}
	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER fail_timer_start`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER fail_timer_finish BEFORE DELETE ON timer_state BEGIN SELECT RAISE(ABORT,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	before = timerTables(t, st)
	if result, err := st.TimerStatus(ctx, now.Add(2*time.Second)); err == nil || !reflect.DeepEqual(result, TimerResult{}) {
		t.Fatalf("finish failure=%+v,%v", result, err)
	}
	if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
		t.Fatal("failed finish changed timer/session")
	}
	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER fail_timer_finish`); err != nil {
		t.Fatal(err)
	}
	done, err := st.TimerStatus(ctx, now.Add(3*time.Second))
	if err != nil || done.Completed == nil || done.Completed.ActiveDuration != time.Second {
		t.Fatalf("recovered deadline=%+v,%v", done, err)
	}
}

func TestTimerIndependentHandlesSerializeAndClaimOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	peer, err := Open(ctx, st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	now := timerTestNow()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, handle := range []*Store{st, peer} {
		wg.Add(1)
		go func(handle *Store) {
			defer wg.Done()
			_, err := handle.StartTimer(ctx, TimerStartOptions{Planned: time.Minute}, now)
			results <- err
		}(handle)
	}
	wg.Wait()
	good, active := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			good++
		} else if errors.Is(err, ErrTimerActive) {
			active++
		} else {
			t.Fatal(err)
		}
	}
	if good != 1 || active != 1 {
		t.Fatalf("starts=%d active=%d", good, active)
	}
	for _, step := range []struct {
		at      time.Time
		paused  bool
		wantErr error
	}{
		{now.Add(time.Second), true, ErrTimerPaused},
		{now.Add(2 * time.Second), false, ErrTimerRunning},
	} {
		for _, handle := range []*Store{st, peer} {
			wg.Add(1)
			go func(handle *Store) {
				defer wg.Done()
				var err error
				if step.paused {
					_, err = handle.PauseTimer(ctx, step.at)
				} else {
					_, err = handle.ResumeTimer(ctx, step.at)
				}
				results <- err
			}(handle)
		}
		wg.Wait()
		changed, unchanged := 0, 0
		for i := 0; i < 2; i++ {
			err := <-results
			if err == nil {
				changed++
			} else if errors.Is(err, step.wantErr) {
				unchanged++
			} else {
				t.Fatal(err)
			}
		}
		if changed != 1 || unchanged != 1 {
			t.Fatalf("pause/resume changed=%d unchanged=%d", changed, unchanged)
		}
	}
	completed := make(chan *TimerSession, 2)
	for _, handle := range []*Store{st, peer} {
		wg.Add(1)
		go func(handle *Store) {
			defer wg.Done()
			result, err := handle.StopTimer(ctx, now.Add(3*time.Second))
			results <- err
			completed <- result.Completed
		}(handle)
	}
	wg.Wait()
	good, missing := 0, 0
	var session *TimerSession
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			good++
		} else if errors.Is(err, ErrNoActiveTimer) {
			missing++
		} else {
			t.Fatal(err)
		}
		if got := <-completed; got != nil {
			session = got
		}
	}
	if good != 1 || missing != 1 || session == nil {
		t.Fatalf("stops=%d missing=%d session=%+v", good, missing, session)
	}
	if session.ActiveDuration != 2*time.Second || session.PauseDuration != time.Second {
		t.Fatalf("concurrent pause accounted twice: %+v", session)
	}
	claims := make(chan bool, 2)
	for _, handle := range []*Store{st, peer} {
		wg.Add(1)
		go func(handle *Store) {
			defer wg.Done()
			claimed, err := handle.ClaimTimerNotification(ctx, session.ID)
			claims <- claimed
			results <- err
		}(handle)
	}
	wg.Wait()
	count := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if <-claims {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("notification claims=%d", count)
	}
	if claimed, err := st.ClaimTimerNotification(ctx, session.ID); err != nil || claimed {
		t.Fatalf("notification execution failure retry=%v,%v", claimed, err)
	}
	history, err := st.TimerHistory(ctx, 20)
	if err != nil || len(history) != 1 || !history[0].NotificationClaimed {
		t.Fatalf("history=%+v,%v", history, err)
	}
}

func TestTimerMigrationPreservesLegacyRowsAndRefusesActiveLegacy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 6)
	if _, err := db.ExecContext(ctx, `INSERT INTO focus_sessions(id,kind,started_at,ended_at,planned_sec,pause_sec,outcome,note) VALUES('old','focus',1700000000,1700000100,1500,7,'done','old note')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO timer_state(id,kind,started_at,planned_sec,paused_at,pause_sec,cycle) VALUES(1,'focus',1700001000,1500,1700001005,2,4)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var started, ended, planned, pause int64
	if err := st.DB().QueryRowContext(ctx, `SELECT started_at,ended_at,planned_sec,pause_sec FROM focus_sessions WHERE id='old'`).Scan(&started, &ended, &planned, &pause); err != nil {
		t.Fatal(err)
	}
	if started != 1700000000 || ended != 1700000100 || planned != 1500 || pause != 7 {
		t.Fatal("migration rewrote old timestamp units")
	}
	legacy, err := st.TimerSessionByID(ctx, "old")
	if err != nil || legacy.MillisecondPrecision || legacy.StartedAt.Unix() != 1700000000 || legacy.ActiveDuration != 93*time.Second || legacy.FocusType != -1 {
		t.Fatalf("legacy=%+v,%v", legacy, err)
	}
	before := timerTables(t, st)
	commands := []func(context.Context, time.Time) (TimerResult, error){st.TimerStatus, st.PauseTimer, st.ResumeTimer, st.StopTimer, st.CancelTimer, func(ctx context.Context, now time.Time) (TimerResult, error) {
		return st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, now)
	}}
	for _, command := range commands {
		if _, err := command(ctx, timerTestNow()); !errors.Is(err, ErrLegacyTimerState) {
			t.Fatalf("legacy active err=%v", err)
		}
	}
	if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
		t.Fatal("legacy refusal changed old rows")
	}
	if claimed, err := st.ClaimTimerNotification(ctx, "old"); err != nil || claimed {
		t.Fatalf("legacy claimed=%v,%v", claimed, err)
	}
	if _, err := st.TimerSessionByID(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session err=%v", err)
	}
}

func TestTimerInvalidNoteDoesNotReconcileOrReplaceExistingState(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := timerTestNow()
	if _, err := st.StartTimer(ctx, TimerStartOptions{Planned: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{strings.Repeat("x", 5001), string([]byte{0xff})} {
		before := timerTables(t, st)
		result, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1, Note: note}, now.Add(2*time.Second))
		if err == nil || !reflect.DeepEqual(result, TimerResult{}) {
			t.Fatalf("invalid note start=%+v,%v", result, err)
		}
		if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
			t.Fatal("invalid note changed session/history/deadline")
		}
	}
	valid := strings.Repeat("\u0430", 5000)
	result, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1, Note: valid}, now.Add(2*time.Second))
	if err != nil || result.Completed == nil || result.State == nil || result.State.Note != valid {
		t.Fatalf("valid 5000-rune note=%+v,%v", result, err)
	}
}

func TestTimerSessionRejectsMalformedNewRowsWithoutChangingLegacy(t *testing.T) {
	ctx := context.Background()
	for _, assignment := range []string{"focus_type = NULL", "kind = 'short_break'", "started_at = -1", "ended_at = started_at - 1"} {
		t.Run(assignment, func(t *testing.T) {
			st := testStore(t)
			result, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1}, timerTestNow())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().ExecContext(ctx, "UPDATE focus_sessions SET "+assignment+" WHERE id = ?", result.State.SessionID); err != nil {
				t.Fatal(err)
			}
			before := timerTables(t, st)
			if _, err := st.TimerSessionByID(ctx, result.State.SessionID); err == nil {
				t.Fatal("malformed precise session accepted")
			}
			if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
				t.Fatal("invalid history read rewrote state")
			}
		})
	}
}

func TestTimerRejectsTaskDeletedFromCache(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P1"})
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTask(ctx, task.Id); err != nil {
		t.Fatal(err)
	}
	before := timerTables(t, st)
	if _, err := st.StartTimer(ctx, TimerStartOptions{TaskID: task.Id, FocusType: 1}, timerTestNow()); err == nil {
		t.Fatal("started timer for deleted task")
	}
	if after := timerTables(t, st); !reflect.DeepEqual(after, before) {
		t.Fatal("deleted task refusal changed timer state")
	}
}
