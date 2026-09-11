package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/store"
)

func stage2DoctorStore(t *testing.T) *store.Store {
	t.Helper()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCheckCacheReportsUnknownOutboxStateWithoutRewritingIt(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := stage2DoctorStore(t)
	if _, err := s.Enqueue(ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: "task.delete", TaskID: "already-absent"}); err != nil {
		t.Fatal(err)
	}
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE outbox SET state = 'mystery'`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	conn.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	result, _ := checkCache(ctx)
	if result.status != statusWarn || !strings.Contains(collapsed(noteText(result)), "unknown state") {
		t.Fatalf("result = %v, want unknown-state warning", checkReport(result))
	}
	inspection, err := store.Inspect(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var state string
	if err := inspection.Store.DB().QueryRowContext(ctx, `SELECT state FROM outbox`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "mystery" {
		t.Fatalf("state = %q, want doctor to leave the row unchanged", state)
	}
}

func TestCheckCacheDistinguishesDeclaredAndUncachedReferences(t *testing.T) {
	t.Run("empty project is no association", func(t *testing.T) {
		isolate(t)
		s := stage2DoctorStore(t)
		if _, err := s.DB().Exec(`INSERT INTO tasks(id, project_id) VALUES ('no-association', '')`); err != nil {
			t.Fatal(err)
		}
		s.Close()
		result, _ := checkCache(context.Background())
		if strings.Contains(collapsed(noteText(result)), "uncached project") {
			t.Fatalf("result = %v, empty project became a false warning", checkReport(result))
		}
	})

	t.Run("missing project is local cache uncertainty", func(t *testing.T) {
		isolate(t)
		s := stage2DoctorStore(t)
		if _, err := s.DB().Exec(`INSERT INTO tasks(id, project_id) VALUES ('task-1', 'project-not-cached')`); err != nil {
			t.Fatal(err)
		}
		s.Close()
		result, _ := checkCache(context.Background())
		text := collapsed(noteText(result))
		if result.status != statusWarn || !strings.Contains(text, "uncached project") || !strings.Contains(text, "does not prove server corruption") {
			t.Fatalf("result = %v, want local-cache uncertainty wording", checkReport(result))
		}
	})

	t.Run("dangling item is declared violation", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		s := stage2DoctorStore(t)
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO items(task_id, id) VALUES ('missing-task', 'item-1')`); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		conn.Close()
		s.Close()
		result, _ := checkCache(ctx)
		text := collapsed(noteText(result))
		if result.status != statusWarn || !strings.Contains(text, "declared foreign-key") || !strings.Contains(text, "checklist item") {
			t.Fatalf("result = %v, want declared and concrete item diagnostics", checkReport(result))
		}
	})

	t.Run("queued delete may outlive task", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		s := stage2DoctorStore(t)
		if _, err := s.Enqueue(ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: "task.delete", TaskID: "already-absent"}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		result, _ := checkCache(ctx)
		text := collapsed(noteText(result))
		if strings.Contains(text, "foreign-key") || strings.Contains(text, "uncached project") || strings.Contains(text, "checklist item") {
			t.Fatalf("result = %v, legitimate delete became an orphan", checkReport(result))
		}
	})
}

func TestCheckCacheDoesNotWriteDatabaseRowsOrMainFile(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := stage2DoctorStore(t)
	if _, err := s.Enqueue(ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: "task.delete", TaskID: "already-absent"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)
	if result, _ := checkCache(ctx); result.status == statusFail {
		t.Fatalf("healthy cache failed: %v", checkReport(result))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterHash := sha256.Sum256(after); afterHash != beforeHash {
		t.Fatalf("main database changed: before=%x after=%x", beforeHash, afterHash)
	}
	insp, err := store.Inspect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer insp.Close()
	counts, err := insp.Store.OutboxCounts(ctx)
	if err != nil || counts.Pending != 1 {
		t.Fatalf("outbox after doctor = %+v, %v", counts, err)
	}
}

func TestCheckCacheCapsDisplayedReferenceEvidenceWithoutCappingTruth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := stage2DoctorStore(t)
	for i := 0; i < maxDoctorListed+2; i++ {
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks(id, project_id) VALUES (?, ?)`,
			fmt.Sprintf("task-%02d", i), fmt.Sprintf("missing-project-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	result, _ := checkCache(ctx)
	text := collapsed(noteText(result))
	if result.status != statusWarn || !strings.Contains(text, "22 task(s) reference a project") ||
		!strings.Contains(text, "and 2 more, not listed here") {
		t.Fatalf("result = %v, want complete count and capped evidence", checkReport(result))
	}
	if got := strings.Count(text, "references uncached project"); got != maxDoctorListed {
		t.Fatalf("listed references = %d, want %d", got, maxDoctorListed)
	}
}

func TestCheckCacheRefusesInvalidWALWithoutRepair(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s := stage2DoctorStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	wal := path + "-wal"
	bad := make([]byte, 32)
	if err := os.WriteFile(wal, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	result, _ := checkCache(ctx)
	if result.status != statusFail || !strings.Contains(result.summary, "sidecar doctor cannot trust") {
		t.Fatalf("result = %v, want refusal", checkReport(result))
	}
	after, err := os.ReadFile(wal)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(bad) {
		t.Fatal("doctor changed the invalid WAL")
	}
}
