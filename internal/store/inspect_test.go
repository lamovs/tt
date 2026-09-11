package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func hashOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestInspectDoesNotCreateTheCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ticktick", "cache.db")

	insp, err := Inspect(ctx, path)
	if err == nil {
		insp.Close()
		t.Fatal("Inspect succeeded where there is no cache")
	}
	if !errors.Is(err, ErrNoCache) {
		t.Fatalf("err = %v, want ErrNoCache", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %q, want the path in it: every line printed about this names the file", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory exists after Inspect: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cache exists after Inspect: %v", err)
	}
	if got := dirListing(t, dir); len(got) != 0 {
		t.Errorf("Inspect left %v behind", got)
	}
}

func TestInspectTreatsAnEmptyFileAsNoCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	if err := os.WriteFile(path, nil, filePerm); err != nil {
		t.Fatal(err)
	}

	insp, err := Inspect(context.Background(), path)
	if err == nil {
		insp.Close()
		t.Fatal("Inspect succeeded on a zero-byte file")
	}
	if !errors.Is(err, ErrNoCache) {
		t.Fatalf("err = %v, want ErrNoCache", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("the file is %d bytes after Inspect, want the 0 it was", fi.Size())
	}
}

func TestInspectRefusesWhatIsNotAFile(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("no mkfifo here: this platform cannot make the fixture")
	}
	path := filepath.Join(t.TempDir(), "cache.db")
	if out, err := exec.Command(mkfifo, path).CombinedOutput(); err != nil {
		t.Skipf("mkfifo %s: %v: %s", path, err, out)
	}

	done := make(chan error, 1)
	go func() {
		insp, err := Inspect(context.Background(), path)
		if err == nil {
			insp.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotAFile) {
			t.Fatalf("err = %v, want ErrNotAFile", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Inspect on a named pipe had not returned after four seconds: reading a pipe waits for whoever writes to it, and a check that waits for ever has nothing to report")
	}
}

func TestInspectReportsAStatItCouldNotMake(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "ticktick")
	path := filepath.Join(dir, "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, dirPerm) })
	if _, err := os.Stat(path); err == nil {
		t.Skip("the directory mode does not bite here: running as root")
	}

	insp, err := Inspect(ctx, path)
	if err == nil {
		insp.Close()
		t.Fatal("Inspect succeeded through a directory it may not traverse")
	}
	if errors.Is(err, ErrNoCache) || errors.Is(err, ErrNotAFile) {
		t.Fatalf("err = %v, want the stat failure itself", err)
	}
	if !errors.Is(err, ErrCannotLook) {
		t.Fatalf("err = %v, want ErrCannotLook: a caller has to tell a stat that failed from an open that failed", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %q, want the path in it", err)
	}

	var perr *fs.PathError
	if !errors.As(err, &perr) {
		t.Errorf("err = %v does not unwrap to *fs.PathError: os.Stat's own error is what ErrCannotLook carries", err)
	}
}

func TestInspectLeavesTheDatabaseByteForByte(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.create"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if got := dirListing(t, dir); len(got) != 1 || got[0] != "cache.db" {
		t.Fatalf("after Open and Close the directory holds %v, want cache.db alone", got)
	}
	before := hashOf(t, path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := fi.Mode().Perm()
	mtime := fi.ModTime()

	time.Sleep(1100 * time.Millisecond)

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var queued int
	if err := insp.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("read %d queued entries, want the 1 that was written", queued)
	}
	if err := insp.Close(); err != nil {
		t.Fatal(err)
	}

	if got := hashOf(t, path); got != before {
		t.Errorf("cache.db is %s after the inspection, was %s", got, before)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != mode {
		t.Errorf("cache.db mode %o after the inspection, was %o", got, mode)
	}
	if got := fi.ModTime(); !got.Equal(mtime) {
		t.Errorf("cache.db was modified at %s after the inspection, was %s: a second of wall clock passed in between, so a write would show", got, mtime)
	}
	want := []string{"cache.db", "cache.db-shm", "cache.db-wal"}
	got := dirListing(t, dir)
	if len(got) != len(want) {
		t.Fatalf("the directory holds %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the directory holds %v, want exactly %v", got, want)
		}
	}
	for _, name := range []string{"cache.db-wal", "cache.db-shm"} {
		si, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := si.Mode().Perm(); perm != mode {
			t.Errorf("%s mode %o, want the database's own %o: the token lives in this directory", name, perm, mode)
		}
	}
}

func TestInspectLeavesACacheOffWALWhereItIs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	var mode string
	if err := s.DB().QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "delete") {
		t.Fatalf("PRAGMA journal_mode=DELETE answered %q: the fixture is not off WAL", mode)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if walInHeader(t, path) {
		t.Fatalf("the fixture is still marked WAL in its header: there is nothing here to measure")
	}
	before := hashOf(t, path)

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect a cache on a rollback journal: %v", err)
	}

	found, hasMeta, hasRow, err := insp.Version(ctx)
	if err != nil || !hasMeta || !hasRow || found != latestVersion(t) {
		t.Fatalf("version %d (meta %v, row %v) err %v: want the version read out of a cache that is "+
			"not in WAL", found, hasMeta, hasRow, err)
	}
	if err := insp.Close(); err != nil {
		t.Fatal(err)
	}

	if walInHeader(t, path) {
		t.Errorf("the cache is marked WAL in its header after an inspection: converting it is a write, " +
			"and it is a write to the one kind of file this open exists to leave alone")
	}
	if got := hashOf(t, path); got != before {
		t.Errorf("cache.db is %s after the inspection, was %s", got, before)
	}

	if got := dirListing(t, dir); len(got) != 1 || got[0] != "cache.db" {
		t.Errorf("the directory holds %v, want cache.db alone", got)
	}
}

func walInHeader(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 20 {
		t.Fatalf("%s is %d bytes and has no SQLite header to read", path, len(data))
	}
	return data[18] == 2
}

func TestInspectDoesNotMigrate(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	for k := 1; k < latest; k++ {
		t.Run("v"+strconv.Itoa(k), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.db")
			db := openAtVersion(t, path, k)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			insp, err := Inspect(ctx, path)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			found, hasMeta, hasRow, err := insp.Version(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !hasMeta || !hasRow || found != k {
				t.Errorf("version %d (meta %v, row %v), want %d in a row of a meta table",
					found, hasMeta, hasRow, k)
			}
			if insp.SchemaLatest != latest {
				t.Errorf("SchemaLatest %d, want %d", insp.SchemaLatest, latest)
			}
			if err := insp.Close(); err != nil {
				t.Fatal(err)
			}

			after, err := sql.Open("sqlite", inspectDSN(ctx, path))
			if err != nil {
				t.Fatal(err)
			}
			defer after.Close()
			var value string
			if err := after.QueryRowContext(ctx,
				`SELECT value FROM meta WHERE key = ?`, schemaVersionKey).Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != strconv.Itoa(k) {
				t.Errorf("the cache says schema version %q after the inspection, want %d", value, k)
			}
			diff, err := (&Store{db: after, path: path}).CompareSchema(ctx, k+1)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff.Missing) == 0 {
				t.Errorf("a cache at v%d already has everything v%d describes: nothing here would show a migration having been applied", k, k+1)
			}
		})
	}
}

func TestInspectRefusesToWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	defer insp.Close()

	_, err = insp.Store.DB().ExecContext(ctx, `CREATE TABLE zz (a)`)
	if err == nil {
		t.Fatal("a CREATE TABLE went through on an inspection")
	}
	if !IsReadOnlyRefusal(err) {
		t.Errorf("CREATE TABLE refused with %v, which IsReadOnlyRefusal does not recognise", err)
	}
	if want := "attempt to write a readonly database"; !strings.Contains(err.Error(), want) {
		t.Errorf("CREATE TABLE refused with %q, want SQLite's own %q", err, want)
	}
	if err := insp.Store.SetMeta(ctx, "zz", "1"); err == nil {
		t.Error("SetMeta went through on an inspection")
	} else if !IsReadOnlyRefusal(err) {
		t.Errorf("SetMeta refused with %v, which IsReadOnlyRefusal does not recognise", err)
	}

	var n int
	if err := insp.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE name = 'zz'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the table the write was refused is in the schema %d time(s)", n)
	}

	t.Run("through a transaction", func(t *testing.T) {
		err := insp.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE zz2 (a)`)
			return err
		})
		if err == nil {
			t.Fatal("a CREATE TABLE inside Tx went through on an inspection")
		}
		if !IsReadOnlyRefusal(err) {
			t.Errorf("Tx refused with %v, which IsReadOnlyRefusal does not recognise", err)
		}
		if err := insp.Store.Tx(ctx, func(tx *sql.Tx) error {
			var n int
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
		}); err != nil {
			t.Errorf("a transaction that only reads failed with %v: what the connection refuses is the write, not the BEGIN", err)
		}
	})

	t.Run("on a second connection", func(t *testing.T) {
		rows, err := insp.Store.DB().QueryContext(ctx, `PRAGMA integrity_check`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("integrity_check answered no rows: the fixture holds no connection")
		}
		if n := insp.Store.DB().Stats().OpenConnections; n != 1 {
			t.Fatalf("%d connections open, want the 1 the Rows holds", n)
		}
		_, err = insp.Store.DB().ExecContext(ctx, `CREATE TABLE zz4 (a)`)
		if n := insp.Store.DB().Stats().OpenConnections; n < 2 {
			t.Fatalf("the write did not reach a second connection: %d open", n)
		}
		if err == nil {
			t.Fatal("a CREATE TABLE went through on the second connection")
		}
		if !IsReadOnlyRefusal(err) {
			t.Errorf("the second connection refused with %v, which IsReadOnlyRefusal does not recognise", err)
		}
	})
}

func TestInspectReportsAVersionFromTheFuture(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	latest := latestVersion(t)
	future := latest + 1
	if err := s.SetMeta(ctx, schemaVersionKey, strconv.Itoa(future)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if s2, err := Open(ctx, path); err == nil {
		s2.Close()
		t.Fatal("Open succeeded on a cache from the future: the fixture no longer stands for the state this is about")
	} else {
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("Open failed with %v, want *VersionError", err)
		}
	}

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect a cache from the future: %v", err)
	}
	defer insp.Close()
	found, hasMeta, hasRow, err := insp.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if found != future || !hasMeta || !hasRow {
		t.Errorf("version %d (meta %v, row %v), want %d", found, hasMeta, hasRow, future)
	}
	if insp.SchemaLatest != latest {
		t.Errorf("SchemaLatest %d, want %d", insp.SchemaLatest, latest)
	}
}

func TestInspectTellsTheThreeZerosApart(t *testing.T) {
	ctx := context.Background()

	t.Run("a database tt did not write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.db")
		db, err := sql.Open("sqlite", dsn(path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE notes (a TEXT)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		insp, err := Inspect(ctx, path)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		defer insp.Close()
		found, hasMeta, hasRow, err := insp.Version(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if found != 0 || hasMeta || hasRow {
			t.Errorf("version %d (meta %v, row %v), want 0 and no meta table", found, hasMeta, hasRow)
		}
	})

	t.Run("a cache whose row is gone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, schemaVersionKey); err != nil {
			t.Fatal(err)
		}
		s.Close()

		insp, err := Inspect(ctx, path)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		defer insp.Close()
		found, hasMeta, hasRow, err := insp.Version(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if found != 0 || !hasMeta || hasRow {
			t.Errorf("version %d (meta %v, row %v), want 0, a meta table and no row",
				found, hasMeta, hasRow)
		}
	})

	t.Run("a cache whose row reads zero", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx,
			`UPDATE meta SET value = '0' WHERE key = ?`, schemaVersionKey); err != nil {
			t.Fatal(err)
		}
		s.Close()

		insp, err := Inspect(ctx, path)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		defer insp.Close()
		found, hasMeta, hasRow, err := insp.Version(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if found != 0 || !hasMeta || !hasRow {
			t.Errorf("version %d (meta %v, row %v), want 0 with the row there: a row reading 0 and "+
				"a missing row are two different faults and this is the one that has a row",
				found, hasMeta, hasRow)
		}
	})

	t.Run("a cache whose row reads below zero", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx,
			`UPDATE meta SET value = '-3' WHERE key = ?`, schemaVersionKey); err != nil {
			t.Fatal(err)
		}
		s.Close()

		insp, err := Inspect(ctx, path)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		defer insp.Close()
		found, hasMeta, hasRow, err := insp.Version(ctx)
		if err != nil {
			t.Fatalf("version: %v, want the number read back rather than an error: the row parses", err)
		}
		if found != -3 || !hasMeta || !hasRow {
			t.Errorf("version %d (meta %v, row %v), want -3 with the row there", found, hasMeta, hasRow)
		}
	})
}

func TestInspectReadsThroughADamagedMetaTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var pageSize, rootPage int64
	if err := s.DB().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT rootpage FROM sqlite_schema WHERE name = 'meta'`).Scan(&rootPage); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 16), (rootPage-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect a cache damaged in meta: %v, want the open to succeed so that the damage can be reported as damage", err)
	}
	defer insp.Close()

	found, hasMeta, _, err := insp.Version(ctx)
	if err == nil || !hasMeta {
		t.Fatalf("version %d (meta %v) err %v: want the meta table found and the row read to fail", found, hasMeta, err)
	}
}

func TestIsReadOnlyRefusalTellsTheTwoApart(t *testing.T) {
	ctx := context.Background()

	t.Run("a directory nothing may write", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "ticktick")
		path := filepath.Join(dir, "cache.db")
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()

		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, dirPerm) })
		probe, err := os.OpenFile(filepath.Join(dir, "probe"), os.O_CREATE|os.O_WRONLY, filePerm)
		if err == nil {
			probe.Close()
			os.Remove(filepath.Join(dir, "probe"))
			t.Skip("the directory mode does not bite here: running as root")
		}

		insp, err := Inspect(ctx, path)
		if err == nil {
			insp.Close()
			t.Fatal("the inspection opened a cache in a directory it cannot make its scratch files in")
		}
		if !IsReadOnlyRefusal(err) {
			t.Errorf("err = %v, want a refusal IsReadOnlyRefusal recognises", err)
		}
	})

	t.Run("a file that is not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.db")
		if err := os.WriteFile(path, bytes.Repeat([]byte("nonsense"), 512), filePerm); err != nil {
			t.Fatal(err)
		}
		insp, err := Inspect(ctx, path)
		if err == nil {
			insp.Close()
			t.Fatal("the inspection opened a file that is not a database")
		}
		if IsReadOnlyRefusal(err) {
			t.Errorf("err = %v was read as a refusal to write, which would print the wrong sentence about it", err)
		}

		if errors.Is(err, ErrCannotLook) {
			t.Errorf("err = %v carries ErrCannotLook, but the stat succeeded and it was the open that failed", err)
		}
	})
}

func TestInspectPassesADeadContextThroughAsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	insp, err := Inspect(ctx, path)
	if err == nil {
		insp.Close()
		t.Fatal("Inspect succeeded on a context that was already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled: nothing else tells this apart from the driver refusing the file", err)
	}
	if errors.Is(err, ErrNoCache) || errors.Is(err, ErrNotAFile) || errors.Is(err, ErrCannotLook) {
		t.Errorf("err = %v carries one of Inspect's own sentinels: the file was fine and the caller gave up", err)
	}
}

func TestInspectDoesNotBlameSQLiteForItsOwnMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	broken := errors.New("migration versions must run 1..N without gaps, got 6 at position 5")
	insp, err := inspect(ctx, path, func() ([]migration, error) { return nil, broken })
	if err == nil {
		insp.Close()
		t.Fatal("the inspection went ahead on a build whose migrations do not load")
	}
	if !errors.Is(err, ErrBadMigrations) {
		t.Fatalf("err = %v, want ErrBadMigrations: the defect is in this binary and not in the file it was pointed at", err)
	}
	if !errors.Is(err, broken) {
		t.Errorf("err = %v does not carry the load failure: the sentinel says which failure this is, the wrapped error says what was wrong", err)
	}
	if errors.Is(err, ErrNoCache) || errors.Is(err, ErrNotAFile) || errors.Is(err, ErrCannotLook) {
		t.Errorf("err = %v carries a sentinel about the cache path, and the path is a good cache", err)
	}
	if IsReadOnlyRefusal(err) {
		t.Errorf("err = %v was read as SQLite refusing the file", err)
	}

	if got := dirListing(t, filepath.Dir(path)); len(got) != 1 || got[0] != "cache.db" {
		t.Errorf("the directory holds %v, want cache.db alone: the load failure has to come before the open", got)
	}
}

func TestInspectAnswersWithAResultSetStillOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	defer insp.Close()

	rows, err := insp.Store.DB().QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("integrity_check answered no rows at all: the fixture no longer holds a connection open")
	}

	if _, _, _, err := insp.Version(ctx); err != nil {
		t.Fatalf("the version read failed with %v while integrity_check's rows were still open: this is the shape tt doctor asks its questions in", err)
	}

	if err := ctx.Err(); err != nil {
		t.Errorf("the context is %v after the read: the reads were serialised and only the deadline got the test out", err)
	}

	want := []string{"cache.db", "cache.db-shm", "cache.db-wal"}
	if got := dirListing(t, filepath.Dir(path)); !reflect.DeepEqual(got, want) {
		t.Errorf("the directory holds %v, want exactly %v: a second connection to one file is not a second set of files", got, want)
	}
}

func TestCompareSchemaIsEmptyForACacheOpenBuilt(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)

	s := testStore(t)
	diff, err := s.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Missing) > 0 || len(diff.Extra) > 0 {
		t.Fatalf("a cache Open built is not what this build's migrations produce: missing %v, extra %v", diff.Missing, diff.Extra)
	}

	for k := 1; k <= latest; k++ {
		t.Run("v"+strconv.Itoa(k), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.db")
			db := openAtVersion(t, path, k)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			insp, err := Inspect(ctx, path)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			defer insp.Close()
			diff, err := insp.Store.CompareSchema(ctx, k)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff.Missing) > 0 || len(diff.Extra) > 0 {
				t.Errorf("a cache at v%d is not what migrations 1..%d produce: missing %v, extra %v", k, k, diff.Missing, diff.Extra)
			}
		})
	}
}

func TestCompareSchemaNamesWhatAVersionClaimsAndTheCacheLacks(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	path := filepath.Join(t.TempDir(), "cache.db")
	db := openAtVersion(t, path, 1)
	if _, err := db.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = ?`, strconv.Itoa(latest), schemaVersionKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	insp, err := Inspect(ctx, path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	defer insp.Close()
	diff, err := insp.Store.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}

	want := SchemaObject{Kind: "column", Name: "outbox.rev"}
	if !hasObject(diff.Missing, want) {
		t.Errorf("missing %v, want %v among them", diff.Missing, want)
	}
	if !hasObject(diff.Blocking(), want) {
		t.Errorf("Blocking() %v, want %v among them: a column a query names stops it", diff.Blocking(), want)
	}
	if len(diff.Extra) > 0 {
		t.Errorf("extra %v, want nothing: the cache carries less than the version claims, not more", diff.Extra)
	}
}

func TestCompareSchemaNamesWhatTheVersionDoesNotDescribe(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	s := testStore(t)
	for _, q := range []string{`CREATE TABLE scratch (a)`, `ALTER TABLE tasks ADD COLUMN zz TEXT`} {
		if _, err := s.DB().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	diff, err := s.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}

	want := []SchemaObject{{Kind: "column", Name: "tasks.zz"}, {Kind: "table", Name: "scratch"}}
	if !reflect.DeepEqual(diff.Extra, want) {
		t.Errorf("extra %v, want exactly %v in that order: two runs must print the same lines", diff.Extra, want)
	}
	if len(diff.Missing) > 0 {
		t.Errorf("missing %v, want nothing", diff.Missing)
	}
}

func TestCompareSchemaNamesAnObjectWhoseNameOnlyLooksLikeSQLitesOwn(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	s := testStore(t)
	if _, err := s.DB().ExecContext(ctx, `CREATE TABLE sqlitex_scratch (a)`); err != nil {
		t.Fatal(err)
	}

	diff, err := s.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	want := SchemaObject{Kind: "table", Name: "sqlitex_scratch"}
	if !hasObject(diff.Extra, want) {
		t.Errorf("extra %v, want %v among them: only sqlite's own names are meant to be dropped, and this is not one", diff.Extra, want)
	}
}

func TestCompareSchemaRefusesAVersionThisBuildDoesNotDescribe(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	s := testStore(t)
	for _, v := range []int{0, -1, latest + 1} {
		if _, err := s.CompareSchema(ctx, v); err == nil {
			t.Errorf("CompareSchema(%d) answered, want a complaint: this build describes 1..%d", v, latest)
		} else if !strings.Contains(err.Error(), strconv.Itoa(latest)) {
			t.Errorf("CompareSchema(%d) failed with %q, want the range this build does describe in it", v, err)
		}
	}
}

func TestCompareSchemaSaysWhenItWasItsOwnSchemaThatGaveWay(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.CompareSchema(ctx, latestVersion(t))
	if err == nil {
		t.Fatal("CompareSchema answered over a context that was already done")
	}
	if !errors.Is(err, ErrBadReferenceSchema) {
		t.Errorf("err = %v, want ErrBadReferenceSchema: what gave way is a database this build makes "+
			"for itself, and a caller cannot tell that from the wording", err)
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancelled context still findable under the sentinel", err)
	}

	if _, err := s.CompareSchema(context.Background(), latestVersion(t)); err != nil {
		t.Fatalf("CompareSchema over a live context: %v", err)
	}
}

func TestReferenceSchemaNeverHandsOutASecondConnection(t *testing.T) {
	ctx := context.Background()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := referenceSchema(ctx, migrations, migrations[len(migrations)-1].version)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	rows, err := ref.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the reference has no tables at all: the fixture no longer holds a connection open")
	}

	waiting, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	cols, err := columnNames(waiting, ref, "tasks")
	if err == nil {
		t.Fatalf("a second query was answered while the first was still open, with %d columns of tasks: it was answered out of a second, empty memory database", len(cols))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second query failed with %v, want it waiting on the one connection the reference has", err)
	}
}

func TestCompareSchemaCountsAMissingIndexAsNotBlocking(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	s := testStore(t)
	if _, err := s.DB().ExecContext(ctx, `DROP INDEX idx_tasks_due`); err != nil {
		t.Fatal(err)
	}

	diff, err := s.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	want := SchemaObject{Kind: "index", Name: "idx_tasks_due"}
	if !hasObject(diff.Missing, want) {
		t.Errorf("missing %v, want %v among them", diff.Missing, want)
	}
	if got := diff.Blocking(); len(got) > 0 {
		t.Errorf("Blocking() %v, want nothing", got)
	}
}

func TestCompareSchemaCountsAMissingTableAsBlocking(t *testing.T) {
	ctx := context.Background()
	latest := latestVersion(t)
	s := testStore(t)
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE focus_sessions`); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM focus_sessions`).Scan(&n); err == nil {
		t.Fatal("a query naming the dropped table still answers: the fixture no longer stands for a table that is gone")
	}

	diff, err := s.CompareSchema(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	want := []SchemaObject{{Kind: "table", Name: "focus_sessions"}}
	if !reflect.DeepEqual(diff.Blocking(), want) {
		t.Errorf("Blocking() %v, want exactly %v: the table stops a query and the index it took with it does not", diff.Blocking(), want)
	}
	if !hasObject(diff.Missing, SchemaObject{Kind: "index", Name: "idx_focus_started"}) {
		t.Errorf("missing %v, want the dropped table's index reported among them", diff.Missing)
	}
}

func TestSchemaHasNothingWhoseAbsenceChangesAnAnswer(t *testing.T) {
	ctx := context.Background()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := referenceSchema(ctx, migrations, migrations[len(migrations)-1].version)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	rows, err := ref.QueryContext(ctx,
		`SELECT type, name, ifnull(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	seen := map[string]int{}
	var uniqueIndexes []string
	for rows.Next() {
		var kind, name, text string
		if err := rows.Scan(&kind, &name, &text); err != nil {
			t.Fatal(err)
		}
		seen[kind]++
		switch {
		case kind == "trigger" || kind == "view":
			t.Errorf("the schema has %s %s: SchemaDifference.Blocking counts only a missing table or column as stopping a query, and an absent %s changes what one answers", kind, name, kind)
		case kind == "index" && strings.Contains(strings.ToUpper(text), "UNIQUE"):
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	objects, err := schemaObjects(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range uniqueIndexes {
		o, ok := objects["unique index "+name]
		if !ok || !hasObject((SchemaDifference{Missing: []SchemaObject{o}}).Blocking(), o) {
			t.Errorf("the schema has a UNIQUE index %s, but SchemaDifference.Blocking does not count its absence", name)
		}
	}

	if seen["table"] < 2 || seen["index"] < 1 {
		t.Fatalf("the reference schema holds %v: this guard needs the schema the migrations build, not what is left when they do not run", seen)
	}
}

func hasObject(objects []SchemaObject, want SchemaObject) bool {
	for _, o := range objects {
		if o == want {
			return true
		}
	}
	return false
}
