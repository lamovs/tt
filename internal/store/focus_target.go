package store

import (
	"context"
	"database/sql"
	"errors"
)

type FocusTarget struct {
	Session TimerSession
	Upload  *FocusUpload
	Version string
}

func focusTargetVersion(ctx context.Context, q execer, id string) (string, error) {
	session, err := rowVersion(ctx, q, "SELECT * FROM focus_sessions WHERE id=?", id)
	if err != nil {
		return "", err
	}
	upload, err := rowVersion(ctx, q, "SELECT * FROM focus_uploads WHERE session_id=?", id)
	return session + ":" + upload, err
}

func (s *Store) ReadFocusTarget(ctx context.Context, id string) (FocusTarget, error) {
	var out FocusTarget
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out.Session, err = readTimerSession(ctx, tx, id)
	if err != nil {
		return out, err
	}
	row, err := readFocusUpload(ctx, tx, id)
	if err == nil {
		out.Upload = &row
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.Version, err = focusTargetVersion(ctx, tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func checkFocusTarget(ctx context.Context, q execer, id, expected string) error {
	if expected == "" {
		return nil
	}
	current, err := focusTargetVersion(ctx, q, id)
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("focus session or upload changed; reopen its preview")
	}
	return nil
}

func (s *Store) CheckFocusTarget(ctx context.Context, target FocusTarget) error {
	return checkFocusTarget(ctx, s.db, target.Session.ID, target.Version)
}
