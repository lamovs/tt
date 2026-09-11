package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func timerTestInvocation(verb string, args []string, cfg config.Config) (*invocation, *bytes.Buffer, *bytes.Buffer) {
	data, refinements := splitArgs(args)
	var out, diagnostic bytes.Buffer
	return &invocation{ctx: context.Background(), verb: verb, args: args, data: data, refinements: refinements,
		stdin: strings.NewReader(""), stdout: &out, stderr: &diagnostic, cfg: cfg, cfgLoaded: true}, &out, &diagnostic
}

func TestTimerArgumentsRefuseBeforeOpeningCache(t *testing.T) {
	for _, args := range [][]string{
		{"timer", "bogus"}, {"timer", "pause", "extra"}, {"timer", "start", "-d", "5m"},
		{"pomodoro", "start", "-d", "0"}, {"pomodoro", "start", "-d", "1ms"},
		{"pomodoro", "start", "-d", ".5s"}, {"pomodoro", "start", "-d", "1.5s"},
		{"pomodoro", "start", "-d", "25h"}, {"pomodoro", "start", "-d"},
		{"pomodoro", "start", "-d", "1m", "-d", "2m"}, {"timer", "status", "--note", "x"},
		{"timer", "start", "--note"}, {"timer", "start", "--note", "x", "--note", "y"},
		{"timer", "start", "1", "--topic", "Work"}, {"timer", "start", "--none", "--topic", "Work"},
		{"timer", "start", "--topic"}, {"timer", "start", "--topic", "Work", "--topic", "Study"},
		{"timer", "pause", "--retry-failed"}, {"timer", "sync", "--retry-failed", "--retry-failed"},
		{"timer", "history", "2"}, {"timer", "_watch"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			code, _, diagnostic := runFeatureCommand(t, args...)
			if code != exitUsage {
				t.Fatalf("code %d: %s", code, diagnostic)
			}
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid command touched cache: %v", err)
			}
		})
	}
}

func TestTimerLifecycleSelectionAndSharedState(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Read book"})
	cfg := config.Default()
	cfg.Timer.OnEnd = "fixture"
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	var watched []string
	var notified []NotifyVars
	uploads := 0
	rt := timerRuntime{now: func() time.Time { return now },
		watch:  func(id string) error { watched = append(watched, id); return nil },
		upload: func(*invocation, *store.Store) int { uploads++; return exitOK },
		notify: func(_ context.Context, _ string, vars NotifyVars) (NotifyResult, error) {
			notified = append(notified, vars)
			return NotifyResult{}, nil
		}}
	call := func(verb string, args ...string) string {
		t.Helper()
		inv, out, diagnostic := timerTestInvocation(verb, args, cfg)
		if code := cmdTimerWithRuntime(inv, rt); code != exitOK {
			t.Fatalf("%s %v: %d %s", verb, args, code, diagnostic.String())
		}
		return out.String()
	}
	call("pomodoro", "start", "1", "-d", "10s", "--note", "exact  note\nline")
	now = now.Add(3 * time.Second)
	call("timer", "pause")
	now = now.Add(11 * time.Second)
	if got := call("pomodoro", "status"); !strings.Contains(got, "3s active, 7s remaining") || !strings.Contains(got, "paused") {
		t.Fatalf("paused status: %s", got)
	}
	call("timer", "resume")
	now = now.Add(2 * time.Second)
	if got := call("timer", "stop"); !strings.Contains(got, "5s active, 11s paused") {
		t.Fatalf("stop: %s", got)
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	history, err := st.TimerHistory(context.Background(), 20)
	if err != nil || len(history) != 1 {
		t.Fatalf("history %v: %v", history, err)
	}
	if history[0].TaskID != id || history[0].Note != "exact  note\nline" || history[0].ActiveDuration != 5*time.Second || history[0].Outcome != "done" {
		t.Fatalf("session changed: %+v", history[0])
	}
	if len(watched) != 3 || watched[0] != watched[1] || watched[0] != watched[2] {
		t.Fatalf("watcher restart IDs: %v", watched)
	}
	if len(notified) != 1 || notified[0].Note != "exact  note\nline" || notified[0].Task != "Read book" || notified[0].Project != "Personal" || notified[0].Kind != "focus" || notified[0].Duration != "5s" || notified[0].Cycle != "1" {
		t.Fatalf("notification vars: %+v", notified)
	}
	call("timer", "history")
	if uploads != 0 {
		t.Fatalf("disabled upload/history made %d upload calls", uploads)
	}
	call("timer", "sync", "--retry-failed")
	if uploads != 1 {
		t.Fatalf("explicit sync made %d calls", uploads)
	}
	call("timer", "start")
	now = now.Add(4 * time.Second)
	call("pomodoro", "cancel")
	if len(watched) != 3 || len(notified) != 1 || uploads != 1 {
		t.Fatal("count-up/cancel started watcher, notification, or automatic upload")
	}
}

func TestTimerStartRejectsMultipleTasksAndHelpHidesWatcher(t *testing.T) {
	isolate(t)
	first := seedFeatureCommandTask(t, model.Task{Title: "First"})
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	second, err := st.CreateTask(context.Background(), model.Task{ProjectId: "p1", Title: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetListing(context.Background(), []string{first, second.Id}); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"timer", "pomodoro"} {
		inv, _, diagnostic := timerTestInvocation(verb, []string{"start", "1-2"}, config.Default())
		if code := cmdTimerWithRuntime(inv, defaultTimerRuntime()); code != exitUsage {
			t.Fatalf("multiple tasks: %d %s", code, diagnostic.String())
		}
		if text := helpText(t, verb); strings.Contains(text, "_watch") || !strings.Contains(text, "--retry-failed") || !strings.Contains(text, "cancel") {
			t.Fatalf("public help: %s", text)
		}
	}
	status, err := st.TimerStatus(context.Background(), time.Now())
	if err != nil || status.State != nil {
		t.Fatalf("rejected selection started a timer: %+v %v", status, err)
	}
}

func TestTimerWatcherLockDoesNotFollowSymlinksOrDuplicateOwners(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("background watcher is supported on Unix")
	}
	path := filepath.Join(t.TempDir(), "watcher.lock")
	unlock, acquired, err := lockTimerWatcher(path)
	if err != nil || !acquired {
		t.Fatalf("first lock: %v %v", acquired, err)
	}
	defer unlock()
	if _, acquired, err := lockTimerWatcher(path); err != nil || acquired {
		t.Fatalf("second owner: %v %v", acquired, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions: %v %v", info, err)
	}
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := lockTimerWatcher(link); err == nil || acquired {
		t.Fatalf("followed symlink: %v %v", acquired, err)
	}
	if _, acquired, err := lockTimerWatcher(t.TempDir()); err == nil || acquired {
		t.Fatalf("accepted directory: %v %v", acquired, err)
	}
	unlock()
	secondUnlock, acquired, err := lockTimerWatcher(path)
	if err != nil || !acquired {
		t.Fatalf("lock did not release: %v %v", acquired, err)
	}
	secondUnlock()
}

func TestTimerAutomaticUploadNotificationFailureAndWatcherRecovery(t *testing.T) {
	isolate(t)
	cfg := config.Default()
	cfg.Timer.Focus = model.Duration(time.Second)
	cfg.Timer.OnEnd = "fixture"
	cfg.FocusUpload.Enabled = true
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	notifications, uploads, wakes := 0, 0, 0
	rt := timerRuntime{now: func() time.Time { return now },
		watch:  func(string) error { return errors.New("fixture watcher failure") },
		upload: func(*invocation, *store.Store) int { uploads++; return exitOK },
		wake:   func() error { wakes++; return nil },
		notify: func(context.Context, string, NotifyVars) (NotifyResult, error) {
			notifications++
			return NotifyResult{ExitCode: 7}, nil
		}}
	inv, _, diagnostic := timerTestInvocation("pomodoro", []string{"start"}, cfg)
	if code := cmdTimerWithRuntime(inv, rt); code != exitError || !strings.Contains(diagnostic.String(), "session is saved") {
		t.Fatalf("watcher failure %d: %s", code, diagnostic.String())
	}
	now = now.Add(3 * time.Second)
	inv, out, diagnostic := timerTestInvocation("timer", nil, cfg)
	if code := cmdTimerWithRuntime(inv, rt); code != exitOK || !strings.Contains(out.String(), "1s active") || !strings.Contains(diagnostic.String(), "status 7") {
		t.Fatalf("missed deadline %d: %s / %s", code, out.String(), diagnostic.String())
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	history, err := st.TimerHistory(context.Background(), 20)
	if err != nil || len(history) != 1 || history[0].ActiveDuration != time.Second || !history[0].NotificationClaimed {
		t.Fatalf("completion lost: %+v %v", history, err)
	}
	if notifications != 1 || uploads != 0 || wakes != 1 {
		t.Fatalf("notification/upload/wake counts %d/%d/%d", notifications, uploads, wakes)
	}
	if code := timerAfterCompletion(inv, st, history[0], rt); code != exitOK || notifications != 1 {
		t.Fatalf("notification delivered twice: %d/%d", code, notifications)
	}
}

func TestTimerOutputEscapesAndBoundsForeignFields(t *testing.T) {
	isolate(t)
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	foreign := "line\n\x1b[31m  " + strings.Repeat("界", 160)
	inv, out, diagnostic := timerTestInvocation("timer", nil, config.Default())
	printTimerSession(inv, st, store.TimerSession{ID: foreign, TaskID: foreign, Note: foreign, Outcome: foreign,
		FocusType: 1, ActiveDuration: time.Second, EndedAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)})
	timerFailure(inv, errors.New(foreign))
	for _, output := range []string{out.String(), diagnostic.String()} {
		if strings.Contains(output, "\x1b") || strings.Contains(output, "\nline\n") {
			t.Fatalf("raw control reached output: %q", output)
		}
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if cli.DisplayWidth(line) > cli.Width {
				t.Fatalf("line exceeds %d: %q", cli.Width, line)
			}
		}
		if !strings.Contains(output, `line\n\x1b[31m  `) {
			t.Fatalf("escape or repeated spaces changed: %q", output)
		}
	}
}

func TestTimerWatcherProcess(t *testing.T) {
	if os.Getenv("TT_TIMER_WATCHER_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := config.Default()
	cfg.Timer.OnEnd = "fixture"
	inv, _, _ := timerTestInvocation("timer", []string{"_watch", os.Getenv("TT_TIMER_SESSION")}, cfg)
	inv.ctx, inv.stdout, inv.stderr = ctx, os.Stdout, os.Stderr
	rt := defaultTimerRuntime()
	rt.upload = func(*invocation, *store.Store) int { return 91 }
	rt.notify = func(context.Context, string, NotifyVars) (NotifyResult, error) {
		f, err := os.OpenFile(os.Getenv("TT_TIMER_OBSERVED"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return NotifyResult{}, err
		}
		_, err = f.WriteString("notification\n")
		f.Close()
		return NotifyResult{}, err
	}
	code := cmdTimerWithRuntime(inv, rt)
	if err := os.WriteFile(os.Getenv("TT_TIMER_DONE"), []byte(strconv.Itoa(code)), 0o600); err != nil {
		os.Exit(92)
	}
	os.Exit(code)
}

func TestTimerDetachedWatchersCompleteOnceAndExit(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("background watcher is supported on Unix")
	}
	isolate(t)
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	started, err := st.StartTimer(context.Background(), store.TimerStartOptions{FocusType: 0, Planned: 2 * time.Second}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	observed := filepath.Join(dir, "observed")
	t.Setenv("TT_TIMER_WATCHER_HELPER", "1")
	t.Setenv("TT_TIMER_SESSION", started.State.SessionID)
	t.Setenv("TT_TIMER_OBSERVED", observed)
	var donePaths []string
	t.Cleanup(func() {

		deadline := time.Now().Add(12 * time.Second)
		for _, path := range donePaths {
			for {
				if _, err := os.Stat(path); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Errorf("watcher did not exit: %s", path)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	for i := 0; i < 2; i++ {
		path := filepath.Join(dir, fmt.Sprintf("done-%d", i))
		t.Setenv("TT_TIMER_DONE", path)
		if err := startTimerWatchProcess(os.Args[0], []string{"-test.run=^TestTimerWatcherProcess$"}); err != nil {
			t.Fatal(err)
		}
		donePaths = append(donePaths, path)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		done := 0
		for _, path := range donePaths {
			if data, err := os.ReadFile(path); err == nil {
				if string(data) != "0" {
					t.Fatalf("watcher exit: %s", data)
				}
				done++
			}
		}
		if done == len(donePaths) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("countdown watchers did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(observed)
	if err != nil || string(data) != "notification\n" {
		t.Fatalf("notification count: %q %v", data, err)
	}
	history, err := st.TimerHistory(context.Background(), 20)
	if err != nil || len(history) != 1 || history[0].ID != started.State.SessionID || history[0].ActiveDuration != 2*time.Second {
		t.Fatalf("countdown history: %+v %v", history, err)
	}
	if active, err := st.TimerStatus(context.Background(), time.Now()); err != nil || active.State != nil {
		t.Fatalf("watcher left active timer: %+v %v", active, err)
	}
}
