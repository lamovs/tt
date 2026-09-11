package main

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func TestTimerGuardFieldsAndFailuresKeepFinalLinesBounded(t *testing.T) {
	isolate(t)
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	foreign := "line\n\x1b[31m  " + strings.Repeat("\u754c", 160)
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO tasks (id, project_id, title) VALUES ('known-task', 'p1', ?)`, foreign); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"timer", "pomodoro"} {
		t.Run(verb, func(t *testing.T) {
			inv, out, diagnostic := timerTestInvocation(verb, nil, config.Default())
			printTimerState(inv, st, store.TimerState{SessionID: foreign, TaskID: "known-task", Note: foreign, FocusType: 0, PlannedDuration: time.Minute})
			printTimerSession(inv, st, store.TimerSession{ID: foreign, TaskID: foreign, Note: foreign, Outcome: foreign,
				FocusType: 1, ActiveDuration: time.Second, EndedAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)})
			if code := timerFailure(inv, errors.New(foreign)); code != exitError {
				t.Fatalf("timerFailure exit=%d", code)
			}
			for _, prefix := range []string{"Session: ", "Note: ", "Task: ", "Outcome: ", "Task ID: "} {
				if !strings.Contains(out.String(), prefix+`"line\n\x1b[31m  `) {
					t.Fatalf("final %s field lost escaped text or spaces: %q", prefix, out.String())
				}
			}
			if !strings.HasPrefix(diagnostic.String(), "tt: "+verb+`: "line\n\x1b[31m  `) {
				t.Fatalf("final failure lost escaped text or actual verb prefix: %q", diagnostic.String())
			}
			assertTimerGuardLines(t, out.String())
			assertTimerGuardLines(t, diagnostic.String())
		})
	}
}

func TestTimerGuardWatcherConstructorsReachEscapedDiagnostics(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "missing\n\x1b[31m  "+strings.Repeat("x", 160), "watcher")
	cases := []struct {
		name string
		err  error
	}{
		{"process startup", startTimerWatchProcess(path, nil)},
	}
	if runtime.GOOS != "windows" {
		_, _, err := lockTimerWatcher(path)
		cases = append(cases, struct {
			name string
			err  error
		}{"lock open", err})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("hostile missing path did not produce an error")
			}
			inv, _, diagnostic := timerTestInvocation("pomodoro", nil, config.Default())
			if code := timerFailure(inv, tc.err); code != exitError {
				t.Fatalf("timerFailure exit=%d", code)
			}
			assertTimerGuardLines(t, diagnostic.String())
			if !strings.HasPrefix(diagnostic.String(), `tt: pomodoro: "`) {
				t.Fatalf("watcher error was not one escaped final atom: %q", diagnostic.String())
			}
		})
	}
}

func assertTimerGuardLines(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "\x1b") {
		t.Fatalf("raw escape reached final output: %q", output)
	}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if cli.DisplayWidth(line) > cli.Width {
			t.Fatalf("final line exceeds %d columns: %q", cli.Width, line)
		}
	}
}
