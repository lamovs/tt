package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Event struct {
	Seq     int64
	At      time.Time
	Kind    string
	Payload json.RawMessage
}

func (s *Store) AppendEvent(ctx context.Context, kind string, payload json.RawMessage) (int64, error) {
	return appendEvent(ctx, s.db, kind, payload)
}

func AppendEventTx(ctx context.Context, tx *sql.Tx, kind string, payload json.RawMessage) (int64, error) {
	return appendEvent(ctx, tx, kind, payload)
}

func appendEvent(ctx context.Context, q execer, kind string, payload json.RawMessage) (int64, error) {
	if kind == "" {
		return 0, errors.New("event: empty kind")
	}
	body := "{}"
	if len(payload) > 0 {
		body = string(payload)
	}
	res, err := q.ExecContext(ctx, `INSERT INTO events (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), kind, body)
	if err != nil {
		return 0, fmt.Errorf("append event %s: %w", kind, err)
	}
	return res.LastInsertId()
}

func (s *Store) EventsSince(ctx context.Context, seq int64, limit int) ([]Event, error) {
	query := `SELECT seq, at, kind, payload FROM events WHERE seq > ? ORDER BY seq`
	args := []any{seq}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e       Event
			at      int64
			payload []byte
		)
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &payload); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		e.At = time.Unix(at, 0)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return out, nil
}
