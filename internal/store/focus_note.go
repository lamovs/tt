package store

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"
)

func (s *Store) PendingFocusNoteReviews(ctx context.Context) ([]FocusTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM focus_sessions
		WHERE note_review_pending=1 AND time_precision=3 AND ended_at IS NOT NULL
		AND outcome='done' AND synced_at IS NULL
		AND NOT EXISTS(SELECT 1 FROM focus_uploads WHERE session_id=focus_sessions.id)
		ORDER BY ended_at,id LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []FocusTarget{}
	for _, id := range ids {
		target, err := s.ReadFocusTarget(ctx, id)
		if err != nil {
			return nil, err
		}
		if target.Session.NoteReviewPending && target.Session.SyncedAt == nil && target.Upload == nil {
			result = append(result, target)
		}
	}
	return result, nil
}

func (s *Store) ResolveFocusNote(ctx context.Context, target FocusTarget, addition string, skip bool) (TimerSession, error) {
	if target.Session.ID == "" || target.Version == "" {
		return TimerSession{}, errors.New("select an exact focus session for note review")
	}
	if skip && addition != "" {
		return TimerSession{}, errors.New("skipping a focus note cannot include new text")
	}
	if !utf8.ValidString(addition) {
		return TimerSession{}, errors.New("focus note must be valid UTF-8")
	}
	var session TimerSession
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkFocusTarget(ctx, tx, target.Session.ID, target.Version); err != nil {
			return err
		}
		var err error
		session, err = readTimerSession(ctx, tx, target.Session.ID)
		if err != nil {
			return err
		}
		var frozen bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM focus_uploads WHERE session_id=?)`, session.ID).Scan(&frozen); err != nil {
			return err
		}
		if frozen || session.SyncedAt != nil {
			return errors.New("focus note is frozen for upload; the retained request cannot be changed")
		}
		if !session.NoteReviewPending || !session.MillisecondPrecision || session.EndedAt.IsZero() || session.Outcome != "done" {
			return errors.New("focus session is not awaiting a completion note")
		}
		note := session.Note
		if !skip && addition != "" {
			if note != "" {
				note += "\n\n"
			}
			note += addition
		}
		if !utf8.ValidString(note) || utf8.RuneCountInString(note) > 5000 {
			return errors.New("combined focus note must be valid UTF-8 and at most 5000 characters")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE focus_sessions SET note=?,note_review_pending=0 WHERE id=?`, note, session.ID); err != nil {
			return err
		}
		session.Note, session.NoteReviewPending = note, false
		return nil
	})
	if err != nil {
		return TimerSession{}, err
	}
	return session, nil
}
