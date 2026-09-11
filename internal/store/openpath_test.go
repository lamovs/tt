package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestOpenPathWithURICharacters(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"cache.db", "ca?che.db", "ca#che.db", "ca che.db", "100%.db"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("open %q: %v", path, err)
			}
			defer s.Close()

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %q: %v", path, err)
			}
			if fi.Size() == 0 {
				t.Fatalf("%q is empty: the database went somewhere else", path)
			}
			if got := fi.Mode().Perm(); got != filePerm {
				t.Errorf("mode %o, want %o", got, filePerm)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			allowed := map[string]bool{name: true, name + "-wal": true, name + "-shm": true}
			var got []string
			for _, e := range entries {
				got = append(got, e.Name())
				if !allowed[e.Name()] {
					t.Errorf("stray file %q", e.Name())
					continue
				}
				info, err := e.Info()
				if err != nil {
					t.Fatal(err)
				}
				if perm := info.Mode().Perm(); perm != filePerm {
					t.Errorf("%s mode %o, want %o", e.Name(), perm, filePerm)
				}
			}
			sort.Strings(got)
			if len(got) == 0 {
				t.Fatalf("no files in %s", dir)
			}

			var mode string
			if err := s.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(mode, "wal") {
				t.Errorf("journal_mode = %q", mode)
			}
			if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.create"}); err != nil {
				t.Fatalf("write: %v", err)
			}
			counts, err := s.OutboxCounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if counts.Pending != 1 {
				t.Fatalf("counts %+v", counts)
			}
		})
	}
}

func TestOpenRelativePath(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	s, err := Open(ctx, "cache.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.create"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat("cache.db")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Fatal("the database is not in cache.db")
	}
}
