package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/data/ticktick/cache.db"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	t.Setenv("XDG_DATA_HOME", "")
	got, err = DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".local", "share", "ticktick", "cache.db"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpenPermissionsAndPragmas(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ticktick")
	path := filepath.Join(dir, "cache.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("dir mode %o, want %o", got, dirPerm)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("file mode %o, want %o", got, filePerm)
	}

	for _, tc := range []struct{ pragma, want string }{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"},
	} {
		var got string
		if err := s.DB().QueryRow("PRAGMA " + tc.pragma).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("pragma %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestOpenAfterCacheRemoved(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.create"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen after removal: %v", err)
	}
	defer s2.Close()
	counts, err := s2.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{}) {
		t.Fatalf("counts %+v, want empty", counts)
	}
}

func TestStampRoundTrip(t *testing.T) {
	if got := FormatStamp(time.Time{}); got != nil {
		t.Fatalf("zero time stored as %v, want NULL", got)
	}
	moscow := time.FixedZone("MSK", 3*60*60)
	in := time.Date(2026, 9, 3, 12, 30, 0, 500*int(time.Millisecond), moscow)
	got := FormatStamp(in)
	if want := "2026-09-03T09:30:00.500Z"; got != want {
		t.Fatalf("stored %v, want %q", got, want)
	}
	back, err := ParseStamp(sql.NullString{String: got.(string), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(in) {
		t.Fatalf("read back %s, want %s", back, in)
	}
	if v, err := ParseStamp(sql.NullString{}); err != nil || !v.IsZero() {
		t.Fatalf("NULL: %v %v", v, err)
	}
	if _, err := ParseStamp(sql.NullString{String: "3 sep 2026", Valid: true}); err == nil {
		t.Error("garbage accepted")
	}

	earlier := FormatStamp(time.Date(2026, 9, 3, 12, 0, 0, 0, moscow)).(string)
	later := FormatStamp(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)).(string)
	if !(earlier < later) {
		t.Fatalf("%q sorts after %q", earlier, later)
	}
}

func TestTxRollback(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	sentinel := errors.New("boom")
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := EnqueueTx(ctx, tx, OutboxEntry{Target: TargetV2, Op: "focus.push"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 0 {
		t.Fatalf("pending %d after rollback, want 0", counts.Pending)
	}
}

func TestTxNamesTheOperationOnBeginFailure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := s.Tx(ctx, func(tx *sql.Tx) error { return nil })
	if err == nil {
		t.Fatal("Tx on a closed database returned nil, want an error")
	}
	if !strings.HasPrefix(err.Error(), "begin transaction: ") {
		t.Fatalf("err = %q, want prefix %q", err.Error(), "begin transaction: ")
	}
}

func TestTxNamesTheOperationOnCommitFailure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		return tx.Commit()
	})
	if err == nil {
		t.Fatal("Tx over an already-committed transaction returned nil, want an error")
	}
	if !strings.HasPrefix(err.Error(), "commit transaction: ") {
		t.Fatalf("err = %q, want prefix %q", err.Error(), "commit transaction: ")
	}
}
