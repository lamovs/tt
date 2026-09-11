package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectionReferenceChecksDistinguishDeclaredAndLogicalRelations(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if _, err := s.DB().ExecContext(ctx, `INSERT INTO tasks(id, project_id) VALUES
		('without-project', ''),
		('uncached-project', 'project-not-cached')`); err != nil {
		t.Fatal(err)
	}
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO items(task_id, id) VALUES ('task-not-cached', 'item-1')`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	conn.Close()

	fk, err := s.ForeignKeyViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fk) != 1 || fk[0].Table != "items" || fk[0].Parent != "tasks" {
		t.Fatalf("foreign key violations = %+v, want the dangling item relation", fk)
	}
	projects, err := s.UncachedTaskProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].TaskID != "uncached-project" || projects[0].ProjectID != "project-not-cached" {
		t.Fatalf("uncached projects = %+v, want the non-empty missing project alone", projects)
	}
	items, err := s.DanglingItemTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TaskID != "task-not-cached" || items[0].ItemID != "item-1" {
		t.Fatalf("dangling items = %+v", items)
	}
}

func TestOutboxCountsUnknownStatesWithoutChangingRows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.delete", TaskID: "already-gone"}); err != nil {
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

	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Unknown: 1}) {
		t.Fatalf("counts = %+v, want one unknown row", counts)
	}
	var state string
	if err := s.DB().QueryRowContext(ctx, `SELECT state FROM outbox`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "mystery" {
		t.Fatalf("state = %q, want inspection to leave the row unchanged", state)
	}
}

func TestLegitimateDeleteOutboxDoesNotBecomeAReferenceProblem(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.delete", TaskID: "task-already-absent"}); err != nil {
		t.Fatal(err)
	}
	if fk, err := s.ForeignKeyViolations(ctx); err != nil || len(fk) != 0 {
		t.Fatalf("foreign key violations = %+v, %v", fk, err)
	}
	if projects, err := s.UncachedTaskProjects(ctx); err != nil || len(projects) != 0 {
		t.Fatalf("uncached projects = %+v, %v", projects, err)
	}
	if items, err := s.DanglingItemTasks(ctx); err != nil || len(items) != 0 {
		t.Fatalf("dangling items = %+v, %v", items, err)
	}
}

func TestInspectionReferenceReadersReportScanFailures(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T, schema string) *Store {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fixture.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.ExecContext(ctx, schema); err != nil {
			t.Fatal(err)
		}
		return &Store{db: db}
	}

	t.Run("task project", func(t *testing.T) {
		s := newStore(t, `
			CREATE TABLE tasks(id, project_id);
			CREATE TABLE projects(id);
			INSERT INTO tasks VALUES(NULL, 'missing');`)
		if _, err := s.UncachedTaskProjects(ctx); err == nil || !strings.Contains(err.Error(), "uncached task projects") {
			t.Fatalf("error = %v, want categorized scan failure", err)
		}
	})

	t.Run("item task", func(t *testing.T) {
		s := newStore(t, `
			CREATE TABLE items(task_id, id);
			CREATE TABLE tasks(id);
			INSERT INTO items VALUES(NULL, 'item');`)
		if _, err := s.DanglingItemTasks(ctx); err == nil || !strings.Contains(err.Error(), "dangling item tasks") {
			t.Fatalf("error = %v, want categorized scan failure", err)
		}
	})

	t.Run("outbox state", func(t *testing.T) {
		s := newStore(t, `CREATE TABLE outbox(state); INSERT INTO outbox VALUES(NULL);`)
		if _, err := s.OutboxCounts(ctx); err == nil || !strings.Contains(err.Error(), "outbox counts") {
			t.Fatalf("error = %v, want categorized scan failure", err)
		}
	})
}
