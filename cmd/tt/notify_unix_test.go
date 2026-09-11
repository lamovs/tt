//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunNotifySubstitutesEnvVars(t *testing.T) {
	vars := NotifyVars{Kind: "focus", Note: "Errands", Task: "Buy milk", Project: "Home", Duration: "25m", Cycle: "3"}
	res, err := RunNotify(context.Background(), `echo "$TT_KIND|$TT_NOTE|$TT_TASK|$TT_PROJECT|$TT_DURATION|$TT_CYCLE"`, vars)
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	want := "focus|Errands|Buy milk|Home|25m|3\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
	if res.ExitCode != 0 || res.TimedOut || res.Killed != "" {
		t.Fatalf("res = %+v, want exit 0, no timeout and no signal", res)
	}
}

func TestRunNotifyQuoteInTaskDoesNotBreakTheCommand(t *testing.T) {
	vars := NotifyVars{Task: `it's a "test"`}
	res, err := RunNotify(context.Background(), `echo "task: $TT_TASK"`, vars)
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	want := "task: it's a \"test\"\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
}

func TestRunNotifyNonZeroExitIsNotFatal(t *testing.T) {
	res, err := RunNotify(context.Background(), `echo oops >&2; exit 7`, NotifyVars{})
	if err != nil {
		t.Fatalf("RunNotify returned an error for a merely-failing command: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Killed != "" {
		t.Fatalf("Killed = %q, want empty: the field is for a command a signal ended before it reached an exit, and this one exited on its own", res.Killed)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Fatalf("Stderr = %q, want it to contain %q", res.Stderr, "oops")
	}
}

func TestRunNotifyBraceSubstitutionIsInjectionSafe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	evil := `it's a "test"; echo pwned $(touch ` + marker + `)`

	res, err := RunNotify(context.Background(), "echo {task}", NotifyVars{Task: evil})
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	want := evil + "\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q (the value must come through unexecuted)", res.Stdout, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists: the injected command substitution ran")
	}
}

func TestRunNotifyBraceSubstitutionInsideAQuotedString(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	evil := `x"; touch ` + marker + `; echo "y`

	res, err := RunNotify(context.Background(), `echo "task: {task}"`, NotifyVars{Task: evil})
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists: a task title ran as shell code")
	}
	if !strings.Contains(res.Stdout, marker) {
		t.Fatalf("stdout = %q, want the title echoed as text, %q included", res.Stdout, marker)
	}
}

func TestRunNotifyBraceSubstitutionInsideSingleQuotes(t *testing.T) {
	res, err := RunNotify(context.Background(), `echo '{task}'`, NotifyVars{Task: "Buy milk"})
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	want := "\"$TT_TASK\"\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
}

func TestRunNotifyOverridesInheritedVars(t *testing.T) {
	t.Setenv("TT_TASK", "inherited")
	res, err := RunNotify(context.Background(), "echo {task}", NotifyVars{Task: "Buy milk"})
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if res.Stdout != "Buy milk\n" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "Buy milk\n")
	}
}

func TestRunNotifyBraceSubstitutionIsOnePass(t *testing.T) {
	vars := NotifyVars{Task: "{project}", Project: "Home"}
	res, err := RunNotify(context.Background(), "echo {task}", vars)
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if res.Stdout != "{project}\n" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "{project}\n")
	}
}

func TestRunNotifyDoesNotWaitForBackgroundChildren(t *testing.T) {
	shortenNotifyTimers(t, 2*time.Second, 100*time.Millisecond)

	start := time.Now()
	res, err := RunNotify(context.Background(), "sleep 5 &", NotifyVars{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("RunNotify took %s for a command that backgrounded its work and exited", elapsed)
	}
	if res.TimedOut {
		t.Fatal("res.TimedOut = true: the command itself finished, only its child was still around")
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
}

func TestRunNotifyTimeout(t *testing.T) {
	shortenNotifyTimers(t, 50*time.Millisecond, 100*time.Millisecond)

	start := time.Now()
	res, err := RunNotify(context.Background(), "sleep 5", NotifyVars{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("res.TimedOut = false, want true")
	}
	if res.Killed != "" {
		t.Errorf("Killed = %q, want empty: tt's own deadline is what ended this one, and a run that ran out of its time is reported through TimedOut rather than as the signal that carried it out", res.Killed)
	}
	if elapsed > time.Second {
		t.Fatalf("RunNotify took %s, want it to give up around the timeout", elapsed)
	}
}

func TestRunNotifyTimeoutKillsDescendants(t *testing.T) {
	shortenNotifyTimers(t, 50*time.Millisecond, 100*time.Millisecond)
	marker := filepath.Join(t.TempDir(), "late")

	res, err := RunNotify(context.Background(), "(sleep 1; touch "+marker+") & sleep 5", NotifyVars{})
	if err != nil {
		t.Fatalf("RunNotify: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("res.TimedOut = false, want true")
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("marker file appeared: the backgrounded child outlived the timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func shortenNotifyTimers(t *testing.T, timeout, waitDelay time.Duration) {
	t.Helper()
	oldTimeout, oldDelay := notifyTimeout, notifyWaitDelay
	notifyTimeout, notifyWaitDelay = timeout, waitDelay
	t.Cleanup(func() { notifyTimeout, notifyWaitDelay = oldTimeout, oldDelay })
}

func TestRunNotifyReportsASignalRatherThanAStatus(t *testing.T) {
	res, err := RunNotify(context.Background(), "kill -TERM $$", NotifyVars{})
	if err != nil {
		t.Fatalf("RunNotify returned an error for a signalled command: %v", err)
	}
	if res.TimedOut {
		t.Error("res.TimedOut = true: the command ended on its own signal, well inside its time")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: os/exec has no status for a process a signal ended", res.ExitCode)
	}
	if !strings.Contains(res.Killed, "terminated") {
		t.Errorf("Killed = %q, want it to name the signal that ended the command", res.Killed)
	}
}
