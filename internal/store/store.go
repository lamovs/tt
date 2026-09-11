package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/movsar/tt/internal/model"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600

	busyTimeout = 5 * time.Second
	busyRetry   = 20 * time.Millisecond
)

func DefaultPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "ticktick", "cache.db"), nil
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if err := prepare(path); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(busyTimeout)
	for {
		s, err := open(ctx, path)
		if err == nil || !isBusy(err) || !time.Now().Before(deadline) {
			return s, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(busyRetry):
		}
	}
}

func isBusy(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code()&0xff == sqlite3.SQLITE_BUSY
}

func open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(ctx, db, path); err != nil {
		db.Close()
		return nil, err
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, filePerm); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("chmod %s: %w", path+suffix, err)
		}
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Path() string { return s.path }

func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}

func prepare(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	f.Close()
	if err := os.Chmod(path, filePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func FormatStamp(t time.Time) any {
	s := model.NewTime(t).StoreString()
	if s == "" {
		return nil
	}
	return s
}

func ParseStamp(v sql.NullString) (time.Time, error) {
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	t, err := model.ParseStoreTime(v.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp %q: %w", v.String, err)
	}
	return t.UTC(), nil
}

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}
