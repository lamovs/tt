package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type FocusRecord struct {
	TaskID       string    `json:"task_id,omitempty"`
	FocusType    int       `json:"type"`
	Note         string    `json:"note,omitempty"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	PauseSeconds int64     `json:"pause_seconds"`
}

func ValidateFocusRecord(record FocusRecord) error {
	if record.FocusType != 0 && record.FocusType != 1 {
		return errors.New("focus type must be 0 or 1")
	}
	if !utf8.ValidString(record.Note) || utf8.RuneCountInString(record.Note) > 5000 {
		return errors.New("invalid focus note")
	}
	if record.Start.Year() < 1970 || record.End.Year() > 9999 || !record.End.After(record.Start) {
		return errors.New("focus record needs an increasing timestamp range")
	}
	span := record.End.Sub(record.Start)
	if span > 30*24*time.Hour-2*time.Second || span < time.Second || record.PauseSeconds < 0 || record.PauseSeconds >= int64(span/time.Second) {
		return errors.New("focus record duration is outside the confirmation window")
	}
	if record.Start.Nanosecond()%int(time.Second) != 0 || record.End.Nanosecond()%int(time.Second) != 0 {
		return errors.New("manual focus records require whole-second timestamps")
	}
	return nil
}

// ImportFocusRecord only adds completed history; it never replaces the active timer.
func (s *Store) ImportFocusRecord(ctx context.Context, previewID string, record FocusRecord) (TimerSession, error) {
	if err := ValidateFocusRecord(record); err != nil {
		return TimerSession{}, err
	}
	if len(previewID) != 64 || strings.Trim(previewID, "0123456789abcdef") != "" {
		return TimerSession{}, errors.New("invalid focus preview identity")
	}
	var result TimerSession
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT session_id FROM focus_record_imports WHERE preview_id=?`, previewID).Scan(&existing)
		if err == nil {
			result, err = readTimerSession(ctx, tx, existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if record.TaskID != "" {
			exists, err := taskExists(ctx, tx, record.TaskID)
			if err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
		}
		id, err := NewLocalID()
		if err != nil {
			return err
		}
		planned := int64(0)
		if record.FocusType == 0 {
			planned = record.End.Sub(record.Start).Milliseconds() - record.PauseSeconds*1000
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO focus_sessions(id,task_id,kind,started_at,ended_at,planned_sec,pause_sec,note,outcome,focus_type,time_precision,planned_ms,pause_ms,notification_claimed)
			VALUES (?,NULLIF(?,''),'focus',?,?,?,?,?,'done',?,3,?,?,1)`, id, record.TaskID, record.Start.UnixMilli(), record.End.UnixMilli(), planned/1000, record.PauseSeconds, record.Note, record.FocusType, planned, record.PauseSeconds*1000)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO focus_record_imports(preview_id,session_id) VALUES (?,?)`, previewID, id); err != nil {
			return err
		}
		result, err = readTimerSession(ctx, tx, id)
		return err
	})
	return result, err
}

func (s *Store) ConfirmRemoteFocusDeletion(ctx context.Context, id string, kind int) error {
	if id == "" || (kind != 0 && kind != 1) {
		return errors.New("invalid remote focus identity")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO focus_remote_deletions(remote_id,focus_type,confirmed_at) VALUES (?,?,?)
		ON CONFLICT(remote_id,focus_type) DO UPDATE SET confirmed_at=excluded.confirmed_at`, id, kind, time.Now().Unix())
	return err
}

func focusSuppressed(ctx context.Context, q execer, session string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM focus_uploads u JOIN focus_sessions s ON s.id=u.session_id
		JOIN focus_remote_deletions d ON d.remote_id=u.remote_id AND d.focus_type=s.focus_type WHERE s.id=?`, session).Scan(&count)
	return count > 0, err
}
