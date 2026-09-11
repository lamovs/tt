package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestMeta(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if _, ok, err := s.Meta(ctx, "last_pull_at"); err != nil || ok {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	if err := s.SetMeta(ctx, "last_pull_at", "2026-09-03T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "last_pull_at", "2026-09-03T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Meta(ctx, "last_pull_at")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if v != "2026-09-03T11:00:00Z" {
		t.Fatalf("value %q", v)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		return SetMetaTx(ctx, tx, "cursor", "42")
	}); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.Meta(ctx, "cursor"); !ok || v != "42" {
		t.Fatalf("cursor %q ok=%v", v, ok)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	first, err := s.AppendEvent(ctx, "task.completed", []byte(`{"id":"t1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		_, err := AppendEventTx(ctx, tx, "timer.started", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.EventsSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d events, want 2", len(all))
	}
	if all[0].Seq != first || all[0].Kind != "task.completed" || string(all[0].Payload) != `{"id":"t1"}` {
		t.Fatalf("first event %+v", all[0])
	}
	if all[0].At.IsZero() {
		t.Error("event timestamp not set")
	}
	if string(all[1].Payload) != "{}" {
		t.Fatalf("empty payload stored as %q", all[1].Payload)
	}

	tail, err := s.EventsSince(ctx, first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Kind != "timer.started" {
		t.Fatalf("tail %+v", tail)
	}
	if limited, err := s.EventsSince(ctx, 0, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit: %v %d", err, len(limited))
	}
	if _, err := s.AppendEvent(ctx, "", nil); err == nil {
		t.Error("empty kind accepted")
	}
}
