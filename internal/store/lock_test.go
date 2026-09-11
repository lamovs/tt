//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockIsExclusive(t *testing.T) {
	path := LockPath(filepath.Join(t.TempDir(), "cache.db"))

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := AcquireLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: %v, want ErrLocked", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("lock file mode %o, want %o", got, filePerm)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
}

func TestLockPath(t *testing.T) {
	if got, want := LockPath("/a/cache.db"), "/a/cache.db.lock"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
