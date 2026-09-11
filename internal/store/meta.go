package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) Meta(ctx context.Context, key string) (value string, ok bool, err error) {
	return getMeta(ctx, s.db, key)
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return setMeta(ctx, s.db, key, value)
}

func SetMetaTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	return setMeta(ctx, tx, key, value)
}

func getMeta(ctx context.Context, q execer, key string) (string, bool, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read meta %s: %w", key, err)
	}
	return value, true, nil
}

func setMeta(ctx context.Context, q execer, key, value string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", key, err)
	}
	return nil
}
