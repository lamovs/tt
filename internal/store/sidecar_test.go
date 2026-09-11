package store

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectAcceptsAValidLiveWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.DB().ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.DB().ExecContext(ctx, `INSERT INTO tasks(id, title) VALUES ('wal-task', 'in wal')`); err != nil {
		t.Fatal(err)
	}
	inspection, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("Inspect valid live WAL: %v", err)
	}
	defer inspection.Close()
	var n int
	if err := inspection.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id = 'wal-task'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sentinel count = %d, %v", n, err)
	}
}

func TestInspectRefusesInvalidWALHeader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, path); !errors.Is(err, ErrUnusableSidecar) {
		t.Fatalf("Inspect error = %v, want ErrUnusableSidecar", err)
	}
}

func TestInspectAllowsStructurallyValidTruncatedFrameForSQLiteToJudge(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 33)
	binary.BigEndian.PutUint32(header[0:4], 0x377f0682)
	binary.BigEndian.PutUint32(header[8:12], 4096)
	if err := os.WriteFile(path+"-wal", header, 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("Inspect truncated uncommitted frame: %v", err)
	}
	inspection.Close()
}

func TestInspectRefusesWALPageSizeOne(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 32)
	binary.BigEndian.PutUint32(header[0:4], 0x377f0682)
	binary.BigEndian.PutUint32(header[8:12], 1)
	if err := os.WriteFile(path+"-wal", header, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, path); !errors.Is(err, ErrUnusableSidecar) {
		t.Fatalf("Inspect error = %v, want ErrUnusableSidecar", err)
	}
}
