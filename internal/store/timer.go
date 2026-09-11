package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrTimerActive        = errors.New("a timer is already active")
	ErrNoActiveTimer      = errors.New("no timer is active")
	ErrTimerPaused        = errors.New("timer is already paused")
	ErrTimerRunning       = errors.New("timer is already running")
	ErrTimerClockReversed = errors.New("clock moved before the last timer observation")
	ErrLegacyTimerState   = errors.New("active timer has unsupported legacy state; retained unchanged")
)

type TimerStartOptions struct {
	TaskID, TopicID, TopicName, TopicCredential string
	FocusType                                   int
	Planned                                     time.Duration
	Note                                        string
	Indicator                                   IndicatorMode
	ReviewNote                                  bool
}

type TimerState struct {
	SessionID, TaskID, TopicID, TopicName, TopicCredential, Kind, Note string
	FocusType                                                          int
	StartedAt                                                          time.Time
	PausedAt                                                           *time.Time
	PlannedDuration, PauseDuration, ActiveDuration                     time.Duration
	Deadline                                                           *time.Time
	LastEventAt                                                        time.Time
	MillisecondPrecision                                               bool
	Indicator                                                          IndicatorMode
}

type TimerSession struct {
	ID, TaskID, TopicID, TopicName, TopicCredential, Kind, Note, Outcome string
	FocusType                                                            int
	StartedAt, EndedAt                                                   time.Time
	PlannedDuration, PauseDuration, ActiveDuration                       time.Duration
	SyncedAt                                                             *time.Time
	MillisecondPrecision, NotificationClaimed                            bool
	NoteReviewPending                                                    bool
}

type TimerResult struct {
	State     *TimerState
	Completed *TimerSession
}

func (s *Store) StartTimer(ctx context.Context, options TimerStartOptions, now time.Time) (TimerResult, error) {
	return s.startTimer(ctx, options, nil, now)
}

func (s *Store) startTimer(ctx context.Context, options TimerStartOptions, expected *TimerGuard, now time.Time) (TimerResult, error) {
	if !options.Indicator.Valid() {
		return TimerResult{}, errors.New("invalid timer indicator mode")
	}
	if !utf8.ValidString(options.Note) || utf8.RuneCountInString(options.Note) > 5000 {
		return TimerResult{}, errors.New("timer note must be valid UTF-8 and at most 5000 characters")
	}
	if options.TaskID != "" && options.TopicID != "" {
		return TimerResult{}, errors.New("a timer destination cannot be both a task and a Timer topic")
	}
	if options.TopicID == "" && (options.TopicName != "" || options.TopicCredential != "") ||
		options.TopicID != "" && (strings.TrimSpace(options.TopicName) == "" || options.TopicCredential == "") {
		return TimerResult{}, errors.New("Timer topic destination is incomplete")
	}
	if !utf8.ValidString(options.TopicName) || utf8.RuneCountInString(options.TopicName) > 500 {
		return TimerResult{}, errors.New("Timer topic name is invalid")
	}
	if options.FocusType != 0 && options.FocusType != 1 {
		return TimerResult{}, errors.New("focus type must be 0 (Pomodoro) or 1 (Timing)")
	}
	if options.FocusType == 0 && (options.Planned <= 0 || options.Planned > 24*time.Hour || options.Planned%time.Millisecond != 0) {
		return TimerResult{}, errors.New("Pomodoro duration must be positive, at most 24 hours, and use whole milliseconds")
	}
	if options.FocusType == 1 && options.Planned != 0 {
		return TimerResult{}, errors.New("Timing has no planned duration")
	}
	return s.timerTransactionGuarded(ctx, now, expected, func(tx *sql.Tx, result *TimerResult, stamp int64) error {
		if result.State != nil {
			return ErrTimerActive
		}
		if options.TaskID != "" {
			exists, err := taskExists(ctx, tx, options.TaskID)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("timer task %s does not exist", options.TaskID)
			}
		}
		id, err := NewLocalID()
		if err != nil {
			return err
		}
		planned := options.Planned.Milliseconds()
		_, err = tx.ExecContext(ctx, `INSERT INTO focus_sessions
			(id, task_id, topic_id, topic_name, topic_credential, kind, started_at, planned_sec, note, focus_type, time_precision, planned_ms, pause_ms, note_review_intent)
			VALUES (?, NULLIF(?, ''), ?, ?, ?, 'focus', ?, ?, ?, ?, 3, ?, 0, ?)`,
			id, options.TaskID, options.TopicID, options.TopicName, options.TopicCredential, stamp, planned/1000, options.Note, options.FocusType, planned, options.ReviewNote)
		if err != nil {
			return fmt.Errorf("start focus session: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO timer_state
			(id, kind, task_id, topic_id, topic_name, topic_credential, started_at, planned_sec, session_id, focus_type, note,
			 time_precision, planned_ms, pause_ms, last_event_at, indicator_mode)
			VALUES (1, 'focus', NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, 3, ?, 0, ?, ?)`,
			options.TaskID, options.TopicID, options.TopicName, options.TopicCredential, stamp, planned/1000, id, options.FocusType, options.Note, planned, stamp, options.Indicator)
		if err != nil {
			return fmt.Errorf("start timer: %w", err)
		}
		result.State, err = readTimerState(ctx, tx, stamp)
		return err
	})
}

func (s *Store) PauseTimer(ctx context.Context, now time.Time) (TimerResult, error) {
	return s.pauseTimer(ctx, nil, now)
}

func (s *Store) pauseTimer(ctx context.Context, expected *TimerGuard, now time.Time) (TimerResult, error) {
	return s.timerTransactionGuarded(ctx, now, expected, func(tx *sql.Tx, result *TimerResult, stamp int64) error {
		if result.State == nil {
			return timerMissingAfterReconcile(result)
		}
		if result.State.PausedAt != nil {
			return ErrTimerPaused
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timer_state SET paused_at = ? WHERE id = 1`, stamp); err != nil {
			return err
		}
		var err error
		result.State, err = readTimerState(ctx, tx, stamp)
		return err
	})
}

func (s *Store) ResumeTimer(ctx context.Context, now time.Time) (TimerResult, error) {
	return s.resumeTimer(ctx, nil, now)
}

func (s *Store) resumeTimer(ctx context.Context, expected *TimerGuard, now time.Time) (TimerResult, error) {
	return s.timerTransactionGuarded(ctx, now, expected, func(tx *sql.Tx, result *TimerResult, stamp int64) error {
		if result.State == nil {
			return timerMissingAfterReconcile(result)
		}
		if result.State.PausedAt == nil {
			return ErrTimerRunning
		}
		pause := result.State.PauseDuration.Milliseconds()
		if _, err := timerDuration(pause); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timer_state SET paused_at = NULL, pause_ms = ?, pause_sec = ? WHERE id = 1`, pause, pause/1000); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET pause_ms = ?, pause_sec = ? WHERE id = ?`, pause, pause/1000, result.State.SessionID); err != nil {
			return err
		}
		var err error
		result.State, err = readTimerState(ctx, tx, stamp)
		return err
	})
}

func (s *Store) StopTimer(ctx context.Context, now time.Time) (TimerResult, error) {
	return s.endTimer(ctx, now, "done")
}

func (s *Store) CancelTimer(ctx context.Context, now time.Time) (TimerResult, error) {
	return s.endTimer(ctx, now, "aborted")
}

func (s *Store) endTimer(ctx context.Context, now time.Time, outcome string) (TimerResult, error) {
	return s.endTimerGuarded(ctx, nil, now, outcome)
}

func (s *Store) endTimerGuarded(ctx context.Context, expected *TimerGuard, now time.Time, outcome string) (TimerResult, error) {
	return s.endTimerReviewed(ctx, expected, now, outcome, false)
}

func (s *Store) endTimerReviewed(ctx context.Context, expected *TimerGuard, now time.Time, outcome string, review bool) (TimerResult, error) {
	return s.timerTransactionReviewed(ctx, now, expected, review, func(tx *sql.Tx, result *TimerResult, stamp int64) error {
		if result.State == nil {
			return timerMissingAfterReconcile(result)
		}
		completed, err := finishTimer(ctx, tx, *result.State, stamp, outcome)
		if err != nil {
			return err
		}
		result.State, result.Completed = nil, &completed
		return nil
	})
}

func timerMissingAfterReconcile(result *TimerResult) error {
	if result.Completed != nil {
		return nil
	}
	return ErrNoActiveTimer
}

func (s *Store) TimerStatus(ctx context.Context, now time.Time) (TimerResult, error) {
	return s.timerTransaction(ctx, now, nil)
}

func (s *Store) timerTransaction(ctx context.Context, now time.Time, change func(*sql.Tx, *TimerResult, int64) error) (TimerResult, error) {
	return s.timerTransactionGuarded(ctx, now, nil, change)
}

func (s *Store) timerTransactionGuarded(ctx context.Context, now time.Time, expected *TimerGuard, change func(*sql.Tx, *TimerResult, int64) error) (TimerResult, error) {
	return s.timerTransactionReviewed(ctx, now, expected, false, change)
}

func (s *Store) timerTransactionReviewed(ctx context.Context, now time.Time, expected *TimerGuard, review bool, change func(*sql.Tx, *TimerResult, int64) error) (TimerResult, error) {
	if now.IsZero() || now.Year() < 1970 || now.Year() > 9999 {
		return TimerResult{}, errors.New("timer requires a valid current timestamp")
	}
	stamp := now.UnixMilli()
	var result TimerResult
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkTimerGuard(ctx, tx, expected); err != nil {
			return err
		}
		var err error
		result.State, err = readTimerState(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if result.State != nil {
			state := *result.State
			if review {
				res, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET note_review_intent=1
					WHERE id=? AND ended_at IS NULL AND outcome IS NULL AND time_precision=3
					AND NOT EXISTS(SELECT 1 FROM focus_uploads WHERE session_id=focus_sessions.id)`, state.SessionID)
				if err != nil {
					return err
				}
				changed, err := res.RowsAffected()
				if err != nil {
					return err
				}
				if changed != 1 {
					return errors.New("focus session is already completed or frozen for upload; note review was not requested")
				}
			}
			if state.Deadline != nil && stamp >= state.Deadline.UnixMilli() {
				completed, err := finishTimer(ctx, tx, state, state.Deadline.UnixMilli(), "done")
				if err != nil {
					return err
				}
				result.State, result.Completed = nil, &completed
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE timer_state SET last_event_at = ? WHERE id = 1`, stamp); err != nil {
					return err
				}
				result.State.LastEventAt = time.UnixMilli(stamp).UTC()
			}
		}
		if change != nil {
			if err := change(tx, &result, stamp); err != nil {
				return err
			}
		}
		if change != nil || result.Completed != nil {
			_, err := tx.ExecContext(ctx, "UPDATE timer_control SET revision=revision+1 WHERE id=1")
			return err
		}
		return nil
	})
	if err != nil {
		return TimerResult{}, err
	}
	return result, nil
}

func readTimerState(ctx context.Context, q execer, now int64) (*TimerState, error) {
	var state TimerState
	var session, task sql.NullString
	var mode, planned, pause, last, paused sql.NullInt64
	var started int64
	var precision int
	err := q.QueryRowContext(ctx, `SELECT session_id, task_id, topic_id, topic_name, topic_credential, kind, note, focus_type,
		started_at, paused_at, planned_ms, pause_ms, last_event_at, time_precision, indicator_mode
		FROM timer_state WHERE id = 1`).Scan(&session, &task, &state.TopicID, &state.TopicName, &state.TopicCredential, &state.Kind, &state.Note, &mode,
		&started, &paused, &planned, &pause, &last, &precision, &state.Indicator)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !state.Indicator.Valid() || precision != 3 || !session.Valid || session.String == "" || state.Kind != "focus" || !mode.Valid || mode.Int64 < 0 || mode.Int64 > 1 ||
		!planned.Valid || !pause.Valid || !last.Valid || started < 0 || last.Int64 < started ||
		planned.Int64 < 0 || pause.Int64 < 0 || mode.Int64 == 0 && (planned.Int64 == 0 || planned.Int64 > int64((24*time.Hour)/time.Millisecond)) || mode.Int64 == 1 && planned.Int64 != 0 {
		return nil, ErrLegacyTimerState
	}
	if now < last.Int64 {
		return nil, ErrTimerClockReversed
	}
	if paused.Valid && (paused.Int64 < started || paused.Int64 > last.Int64) {
		return nil, ErrLegacyTimerState
	}
	state.SessionID, state.TaskID, state.FocusType = session.String, task.String, int(mode.Int64)
	if state.TaskID != "" && state.TopicID != "" || state.TopicID == "" && (state.TopicName != "" || state.TopicCredential != "") || state.TopicID != "" && (state.TopicName == "" || state.TopicCredential == "") {
		return nil, ErrLegacyTimerState
	}
	state.StartedAt, state.LastEventAt = time.UnixMilli(started).UTC(), time.UnixMilli(last.Int64).UTC()
	state.MillisecondPrecision = true
	state.PlannedDuration, err = timerDuration(planned.Int64)
	if err != nil {
		return nil, err
	}
	state.PauseDuration, err = timerDuration(pause.Int64)
	if err != nil {
		return nil, err
	}
	activeUntil := now
	if paused.Valid {
		stamp := time.UnixMilli(paused.Int64).UTC()
		state.PausedAt = &stamp
		activeUntil = paused.Int64
		state.PauseDuration, err = timerDuration(pause.Int64 + now - paused.Int64)
		if err != nil {
			return nil, err
		}
	}
	state.ActiveDuration, err = timerDuration(activeUntil - started - pause.Int64)
	if err != nil {
		return nil, err
	}
	if state.FocusType == 0 && state.PausedAt == nil {
		deadline := time.UnixMilli(started + pause.Int64 + planned.Int64).UTC()
		state.Deadline = &deadline
	}
	return &state, nil
}

func timerDuration(millis int64) (time.Duration, error) {
	if millis < 0 || millis > math.MaxInt64/int64(time.Millisecond) {
		return 0, errors.New("timer duration is outside the supported range")
	}
	return time.Duration(millis) * time.Millisecond, nil
}

func finishTimer(ctx context.Context, tx *sql.Tx, state TimerState, ended int64, outcome string) (TimerSession, error) {
	pause := state.PauseDuration.Milliseconds()
	if _, err := timerDuration(pause); err != nil {
		return TimerSession{}, err
	}
	if _, err := timerDuration(ended - state.StartedAt.UnixMilli() - pause); err != nil {
		return TimerSession{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET ended_at = ?, pause_ms = ?, pause_sec = ?, outcome = ?,
		note_review_pending = CASE WHEN ?='done' THEN note_review_intent ELSE 0 END
		WHERE id = ? AND outcome IS NULL AND ended_at IS NULL AND time_precision = 3`, ended, pause, pause/1000, outcome, outcome, state.SessionID)
	if err != nil {
		return TimerSession{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return TimerSession{}, err
	}
	if n != 1 {
		return TimerSession{}, errors.New("timer session is missing or already ended; active timer retained")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM timer_state WHERE id = 1 AND session_id = ?`, state.SessionID); err != nil {
		return TimerSession{}, err
	}
	return readTimerSession(ctx, tx, state.SessionID)
}

const timerSessionColumns = `id, ifnull(task_id, ''), topic_id, topic_name, topic_credential, kind, note, ifnull(outcome, ''),
	ifnull(focus_type, -1), started_at, ended_at, planned_sec, pause_sec, synced_at,
	time_precision, planned_ms, pause_ms, notification_claimed, note_review_pending`

func readTimerSession(ctx context.Context, q execer, id string) (TimerSession, error) {
	return scanTimerSession(q.QueryRowContext(ctx, `SELECT `+timerSessionColumns+` FROM focus_sessions WHERE id = ?`, id))
}

func scanTimerSession(row interface{ Scan(...any) error }) (TimerSession, error) {
	var session TimerSession
	var started, plannedSeconds, pauseSeconds int64
	var ended, synced, plannedMillis, pauseMillis sql.NullInt64
	var precision int
	if err := row.Scan(&session.ID, &session.TaskID, &session.TopicID, &session.TopicName, &session.TopicCredential, &session.Kind, &session.Note, &session.Outcome,
		&session.FocusType, &started, &ended, &plannedSeconds, &pauseSeconds, &synced,
		&precision, &plannedMillis, &pauseMillis, &session.NotificationClaimed, &session.NoteReviewPending); err != nil {
		return TimerSession{}, err
	}
	if session.TaskID != "" && session.TopicID != "" || session.TopicID == "" && (session.TopicName != "" || session.TopicCredential != "") || session.TopicID != "" && (session.TopicName == "" || session.TopicCredential == "") {
		return TimerSession{}, errors.New("timer history has an invalid destination")
	}
	toTime := func(value int64) time.Time { return time.Unix(value, 0).UTC() }
	planned, pause := plannedSeconds, pauseSeconds
	if precision == 3 {
		if session.Kind != "focus" || session.FocusType < 0 || session.FocusType > 1 || started < 0 || ended.Valid && ended.Int64 < started {
			return TimerSession{}, errors.New("millisecond timer history has an invalid kind, focus type or time range")
		}
		if !plannedMillis.Valid || !pauseMillis.Valid {
			return TimerSession{}, errors.New("millisecond timer history lacks precise durations")
		}
		session.MillisecondPrecision = true
		toTime = func(value int64) time.Time { return time.UnixMilli(value).UTC() }
		planned, pause = plannedMillis.Int64, pauseMillis.Int64
	} else {
		if planned < 0 || pause < 0 || planned > math.MaxInt64/1000 || pause > math.MaxInt64/1000 {
			return TimerSession{}, errors.New("legacy timer duration is outside the supported range")
		}
		planned, pause = planned*1000, pause*1000
	}
	session.StartedAt = toTime(started)
	var err error
	session.PlannedDuration, err = timerDuration(planned)
	if err != nil {
		return TimerSession{}, err
	}
	session.PauseDuration, err = timerDuration(pause)
	if err != nil {
		return TimerSession{}, err
	}
	if ended.Valid {
		session.EndedAt = toTime(ended.Int64)
		session.ActiveDuration, err = timerDuration(session.EndedAt.UnixMilli() - session.StartedAt.UnixMilli() - pause)
		if err != nil {
			return TimerSession{}, err
		}
	}
	if synced.Valid {

		stamp := time.Unix(synced.Int64, 0).UTC()
		session.SyncedAt = &stamp
	}
	return session, nil
}

func (s *Store) TimerSessionByID(ctx context.Context, id string) (TimerSession, error) {
	return readTimerSession(ctx, s.db, id)
}

func (s *Store) TimerHistory(ctx context.Context, limit int) ([]TimerSession, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("timer history limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+timerSessionColumns+` FROM focus_sessions
		WHERE time_precision = 0 OR outcome IS NOT NULL
		ORDER BY CASE WHEN time_precision = 3 THEN started_at ELSE started_at * 1000 END DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TimerSession{}
	for rows.Next() {
		session, err := scanTimerSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (s *Store) ClaimTimerNotification(ctx context.Context, sessionID string) (bool, error) {
	var claimed bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET notification_claimed = 1
			WHERE id = ? AND time_precision = 3 AND focus_type = 0 AND outcome = 'done'
			AND ended_at IS NOT NULL AND notification_claimed = 0`, sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1
		return err
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}
