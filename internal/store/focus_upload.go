package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type FocusUpload struct {
	SessionID    string
	Phase        string
	Request      json.RawMessage
	PriorIDs     []string
	RemoteID     string
	Response     json.RawMessage
	Confirmation json.RawMessage
	LastError    string
}

func readFocusUpload(ctx context.Context, q execer, id string) (FocusUpload, error) {
	var row FocusUpload
	var request, prior, response, confirmation string
	err := q.QueryRowContext(ctx, `SELECT session_id, phase, request, prior_ids,
		remote_id, response, confirmation, last_error FROM focus_uploads WHERE session_id = ?`, id).
		Scan(&row.SessionID, &row.Phase, &request, &prior, &row.RemoteID, &response, &confirmation, &row.LastError)
	if err != nil {
		return row, err
	}
	row.Request, row.Response = []byte(request), []byte(response)
	row.Confirmation = []byte(confirmation)
	if !json.Valid(row.Request) || json.Unmarshal([]byte(prior), &row.PriorIDs) != nil || row.PriorIDs == nil {
		return row, errors.New("invalid retained focus upload; nothing was sent")
	}
	return row, nil
}

func (s *Store) FocusUpload(ctx context.Context, id string) (FocusUpload, error) {
	return readFocusUpload(ctx, s.db, id)
}

func (s *Store) ArmFocusUpload(ctx context.Context, id, taskID string, request json.RawMessage, prior []string) (row FocusUpload, send bool, err error) {
	return s.ArmFocusUploadChecked(ctx, id, taskID, request, prior, "")
}

func (s *Store) ArmFocusUploadChecked(ctx context.Context, id, taskID string, request json.RawMessage, prior []string, expected string) (row FocusUpload, send bool, err error) {
	if !json.Valid(request) || prior == nil {
		return row, false, errors.New("invalid focus upload snapshot")
	}
	priorJSON, err := json.Marshal(prior)
	if err != nil {
		return row, false, err
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if suppressed, err := focusSuppressed(ctx, tx, id); err != nil {
			return err
		} else if suppressed {
			return errors.New("remote focus record was intentionally deleted; re-upload is suppressed")
		}
		if err := checkFocusTarget(ctx, tx, id, expected); err != nil {
			return err
		}
		var existingErr error
		row, existingErr = readFocusUpload(ctx, tx, id)
		if existingErr == nil {
			return nil
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		var currentTask, outcome string
		var ended sql.NullInt64
		var precision int
		var notePending bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(task_id,''), COALESCE(outcome,''), ended_at, time_precision, note_review_pending FROM focus_sessions WHERE id = ?`, id).
			Scan(&currentTask, &outcome, &ended, &precision, &notePending); err != nil {
			return err
		}
		if notePending {
			return errors.New("focus session is awaiting a completion note; save or skip the note before upload")
		}
		if currentTask != taskID || !ended.Valid || outcome == "" || precision != 3 {
			return errors.New("focus session changed before upload; nothing was sent")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO focus_uploads(session_id,phase,request,prior_ids) VALUES (?, 'armed', ?, ?)`, id, string(request), string(priorJSON)); err != nil {
			return err
		}
		row, existingErr = readFocusUpload(ctx, tx, id)
		send = existingErr == nil
		return existingErr
	})
	return row, send && err == nil, err
}

func (s *Store) RearmRejectedFocusUpload(ctx context.Context, row FocusUpload) (bool, error) {
	return s.RearmRejectedFocusUploadChecked(ctx, row, "")
}

func (s *Store) RearmRejectedFocusUploadChecked(ctx context.Context, row FocusUpload, expected string) (bool, error) {
	var send bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if suppressed, err := focusSuppressed(ctx, tx, row.SessionID); err != nil {
			return err
		} else if suppressed {
			return errors.New("remote focus record was intentionally deleted; re-upload is suppressed")
		}
		if err := checkFocusTarget(ctx, tx, row.SessionID, expected); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE focus_uploads SET revision=revision+1, phase='armed', last_error='' WHERE session_id=? AND phase='rejected' AND request=? AND prior_ids=?`, row.SessionID, string(row.Request), focusPriorJSON(row.PriorIDs))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		send = n == 1
		return err
	})
	return send && err == nil, err
}

func focusPriorJSON(ids []string) string { raw, _ := json.Marshal(ids); return string(raw) }

func (s *Store) AcceptFocusUpload(ctx context.Context, frozen FocusUpload, remoteID string, raw json.RawMessage) error {
	if remoteID == "" || !json.Valid(raw) {
		return errors.New("invalid accepted focus response")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		row, err := readFocusUpload(ctx, tx, frozen.SessionID)
		if err != nil {
			return err
		}
		if !bytes.Equal(row.Request, frozen.Request) || row.Phase == "rejected" || row.RemoteID != "" && row.RemoteID != remoteID {
			return errors.New("focus upload changed before acceptance")
		}
		if row.Phase == "confirmed" || row.Phase == "accepted" {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE focus_uploads SET revision=revision+1, phase='accepted',remote_id=?,response=?,last_error='' WHERE session_id=?`, remoteID, string(raw), row.SessionID)
		return err
	})
}

func (s *Store) RecordFocusUploadError(ctx context.Context, frozen FocusUpload, message string, rejected bool) error {
	phase := frozen.Phase
	if rejected {
		phase = "rejected"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE focus_uploads SET revision=revision+1, phase=?,last_error=? WHERE session_id=? AND phase=? AND request=?`, phase, message, frozen.SessionID, frozen.Phase, string(frozen.Request))
	return err
}

func (s *Store) ConfirmFocusUpload(ctx context.Context, frozen FocusUpload, remoteID string, raw json.RawMessage, now time.Time) error {
	if remoteID == "" || !json.Valid(raw) {
		return errors.New("invalid confirmed focus response")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		row, err := readFocusUpload(ctx, tx, frozen.SessionID)
		if err != nil {
			return err
		}
		if !bytes.Equal(row.Request, frozen.Request) || row.Phase == "rejected" || row.RemoteID != "" && row.RemoteID != remoteID {
			return errors.New("focus upload changed before confirmation")
		}
		if row.Phase == "confirmed" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE focus_uploads SET revision=revision+1, phase='confirmed',remote_id=?,confirmation=?,last_error='' WHERE session_id=?`, remoteID, string(raw), row.SessionID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET synced_at=? WHERE id=? AND ended_at IS NOT NULL AND time_precision=3`, now.Unix(), row.SessionID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("focus session changed before settlement")
		}
		return nil
	})
}

func (s *Store) PendingFocusSessionIDs(ctx context.Context, includeAborted bool) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM focus_sessions WHERE time_precision=3 AND ended_at IS NOT NULL AND synced_at IS NULL AND (outcome='done' OR (? AND outcome='aborted'))
		AND (note_review_pending=0 OR EXISTS(SELECT 1 FROM focus_uploads u WHERE u.session_id=focus_sessions.id))
		AND NOT EXISTS (SELECT 1 FROM focus_uploads u JOIN focus_remote_deletions d ON d.remote_id=u.remote_id AND d.focus_type=focus_sessions.focus_type WHERE u.session_id=focus_sessions.id)
		ORDER BY started_at,id LIMIT 1000`, includeAborted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
