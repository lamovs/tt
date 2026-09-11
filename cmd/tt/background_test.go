package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestBackgroundPassDelegatesCleanAndNotifiesUnsyncedOnce(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Work"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 20, 0, time.UTC)
	due := model.NewTime(time.Date(2026, 9, 11, 12, 10, 0, 0, time.UTC))
	clean := model.Task{Id: "clean", ProjectId: "p1", Title: "Server", Kind: "TEXT", DueDate: due, Reminders: []string{"TRIGGER:-PT10M"}}
	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{Task: clean}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Offline", Kind: "TEXT", DueDate: due, Reminders: []string{"TRIGGER:-PT10M"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Timer.OnEnd = "notifier"
	var notified []NotifyVars
	syncs := 0
	runtime := backgroundRuntime{
		now: func() time.Time { return now },
		notify: func(_ context.Context, command string, vars NotifyVars) (NotifyResult, error) {
			if command != "notifier" {
				t.Fatalf("command=%q", command)
			}
			notified = append(notified, vars)
			return NotifyResult{}, nil
		},
		sync: func(context.Context, *store.Store, config.Config) error {
			syncs++
			return nil
		},
	}
	first := runBackgroundPass(ctx, st, cfg, runtime, false)
	if first.err != nil || first.local != 1 || first.provider != 1 || !first.synced || syncs != 1 {
		t.Fatalf("first=%+v syncs=%d", first, syncs)
	}
	if len(notified) != 1 || notified[0].Kind != "task" || notified[0].Task != "Offline" || notified[0].Project != "Work" {
		t.Fatalf("notified=%+v", notified)
	}
	second := runBackgroundPass(ctx, st, cfg, runtime, false)
	if second.err != nil || second.local != 0 || second.provider != 0 || second.synced || syncs != 1 || len(notified) != 1 {
		t.Fatalf("second=%+v syncs=%d notified=%+v", second, syncs, notified)
	}
}

func TestBackgroundPassRetriesWorkButNotHeldEntries(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Enqueue(ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: "task.update", TaskID: "t"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	syncs := 0
	runtime := backgroundRuntime{now: func() time.Time { return now }, notify: RunNotify,
		sync: func(context.Context, *store.Store, config.Config) error { syncs++; return errors.New("offline") }}
	result := runBackgroundPass(ctx, st, cfg, runtime, false)
	if result.err == nil || !result.synced || syncs != 1 {
		t.Fatalf("first=%+v syncs=%d", result, syncs)
	}
	now = now.Add(30 * time.Second)
	result = runBackgroundPass(ctx, st, cfg, runtime, false)
	if result.synced || syncs != 1 {
		t.Fatalf("early retry=%+v syncs=%d", result, syncs)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET state='failed'`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	result = runBackgroundPass(ctx, st, cfg, runtime, false)
	if !result.synced || syncs != 2 {
		t.Fatalf("periodic pull=%+v syncs=%d", result, syncs)
	}
}

func TestBackgroundPassUsesOneMinuteMinimumRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Enqueue(ctx, store.OutboxEntry{Target: store.TargetOpenAPI, Op: "task.update", TaskID: "t"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Sync.Interval = model.Duration(10 * time.Second)
	syncs := 0
	runtime := backgroundRuntime{now: func() time.Time { return now }, notify: RunNotify,
		sync: func(context.Context, *store.Store, config.Config) error { syncs++; return errors.New("offline") }}
	if result := runBackgroundPass(ctx, st, cfg, runtime, false); !result.synced || syncs != 1 {
		t.Fatalf("first=%+v syncs=%d", result, syncs)
	}
	now = now.Add(59 * time.Second)
	if result := runBackgroundPass(ctx, st, cfg, runtime, false); result.synced || syncs != 1 {
		t.Fatalf("early=%+v syncs=%d", result, syncs)
	}
	now = now.Add(time.Second)
	if result := runBackgroundPass(ctx, st, cfg, runtime, false); !result.synced || syncs != 2 {
		t.Fatalf("due=%+v syncs=%d", result, syncs)
	}
}

func TestBackgroundStatusBoundsLastError(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	value := "bad\x1b]52;c;payload\a" + strings.Repeat("x", 200)
	if err := st.SetMeta(ctx, store.BackgroundLastErrorKey, value); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	inv := &invocation{ctx: ctx, verb: "auto", stdout: &stdout, stderr: &stderr}
	if code := backgroundStatus(inv, st, config.Default(), time.Now()); code != exitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if len([]rune(line)) > 80 {
			t.Fatalf("line too wide: %q", line)
		}
	}
	if strings.Contains(stdout.String(), "\x1b]52") || !strings.Contains(stdout.String(), `\x1b`) {
		t.Fatalf("unsafe status output: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Provider-only reminders: 0") {
		t.Fatalf("missing reminder status: %q", stdout.String())
	}
}
