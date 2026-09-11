package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrTimerChanged = errors.New("timer changed; refresh and review the exact session again")

type TimerGuard struct {
	Revision                            int64
	StateVersion                        string
	SessionID                           string
	TaskID                              string
	TaskVersion                         string
	TaskTitle                           string
	DefaultTask                         bool
	TopicID, TopicName, TopicCredential string
}

type TimerSnapshot struct {
	Guard      TimerGuard
	State      *TimerState
	ObservedAt time.Time
	TaskTitle  string
	TaskFound  bool
}

func (s *Store) ReadTimer(ctx context.Context, now time.Time) (TimerSnapshot, error) {
	out := TimerSnapshot{ObservedAt: now}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM timer_control WHERE id=1").Scan(&out.Guard.Revision); err != nil {
		return out, err
	}
	out.Guard.StateVersion, err = timerStateVersion(ctx, tx)
	if err != nil {
		return out, err
	}
	out.State, err = readTimerState(ctx, tx, now.UnixMilli())
	if err != nil {
		return out, err
	}
	if out.State != nil {
		out.Guard.SessionID = out.State.SessionID
		out.Guard.TopicID = out.State.TopicID
		out.Guard.TopicName = out.State.TopicName
		out.Guard.TopicCredential = out.State.TopicCredential
		if out.State.TaskID != "" {
			err = tx.QueryRowContext(ctx, "SELECT title FROM tasks WHERE id=?", out.State.TaskID).Scan(&out.TaskTitle)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
			out.TaskFound = err == nil
		}
	}
	return out, tx.Commit()
}

func checkTimerGuard(ctx context.Context, tx *sql.Tx, expected *TimerGuard) error {
	if expected == nil {
		return nil
	}
	var revision int64
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT revision, COALESCE((SELECT session_id FROM timer_state WHERE id=1),'') FROM timer_control WHERE id=1").Scan(&revision, &id); err != nil {
		return err
	}
	version, err := timerStateVersion(ctx, tx)
	if err != nil {
		return err
	}
	if revision != expected.Revision || id != expected.SessionID || version != expected.StateVersion {
		return ErrTimerChanged
	}
	if expected.TaskID != "" {
		if expected.DefaultTask {
			if _, err := defaultFocusTask(ctx, tx, expected.TaskID); err != nil {
				return err
			}
		}
		version, err := timerTaskVersion(ctx, tx, expected.TaskID)
		if err != nil {
			return err
		}
		if version != expected.TaskVersion {
			return ErrTimerChanged
		}
	}
	return nil
}

func (s *Store) GuardTimerTask(ctx context.Context, guard TimerGuard, id string) (TimerGuard, error) {
	guard.TaskID = id
	guard.TaskVersion, guard.TaskTitle, guard.DefaultTask = "", "", false
	if id == "" {
		return guard, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return guard, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "SELECT title FROM tasks WHERE id=?", id).Scan(&guard.TaskTitle); err != nil {
		return guard, err
	}
	guard.TaskVersion, err = timerTaskVersion(ctx, tx, id)
	if err != nil {
		return guard, err
	}
	return guard, tx.Commit()
}

func timerTaskVersion(ctx context.Context, q execer, id string) (string, error) {
	return rowVersion(ctx, q, "SELECT * FROM tasks WHERE id=?", id)
}

func (s *Store) ControlTimer(ctx context.Context, action string, options TimerStartOptions, expected *TimerGuard, now time.Time) (TimerResult, error) {
	if expected != nil && action == "start" && (expected.SessionID != "" || options.TaskID != expected.TaskID || options.TopicID != expected.TopicID || options.TopicName != expected.TopicName || options.TopicCredential != expected.TopicCredential) {
		return TimerResult{}, ErrTimerChanged
	}
	switch action {
	case "start":
		return s.startTimer(ctx, options, expected, now)
	case "pause":
		return s.pauseTimer(ctx, expected, now)
	case "resume":
		return s.resumeTimer(ctx, expected, now)
	case "stop":
		return s.endTimerReviewed(ctx, expected, now, "done", options.ReviewNote)
	case "cancel":
		return s.endTimerGuarded(ctx, expected, now, "aborted")
	case "status":
		return s.timerTransactionGuarded(ctx, now, expected, nil)
	}
	return TimerResult{}, errors.New("unknown timer action")
}

func timerStateVersion(ctx context.Context, q execer) (string, error) {
	return rowVersion(ctx, q, "SELECT id, session_id, task_id, topic_id, topic_name, topic_credential, kind, note, focus_type, started_at, paused_at, planned_ms, pause_ms, time_precision, planned_sec, pause_sec, cycle, indicator_mode FROM timer_state ORDER BY id")
}
