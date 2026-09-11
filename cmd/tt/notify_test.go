package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
)

func TestApplyBraceTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"kind={kind} note={note} task={task} project={project} duration={duration} cycle={cycle}": `kind="$TT_KIND" note="$TT_NOTE" task="$TT_TASK" project="$TT_PROJECT" duration="$TT_DURATION" cycle="$TT_CYCLE"`,

		"echo ${task} {task}{project}": `echo ${task} "$TT_TASK""$TT_PROJECT"`,
	} {
		if got := applyBraceTemplate(in); got != want {
			t.Errorf("applyBraceTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyBraceTemplateLeavesOtherBracesAlone(t *testing.T) {
	for _, in := range []string{
		`notify-send "${TT_TASK}"`,
		`notify-send "${task}"`,
		`t=$(date); notify-send "${task} ${project}"`,
		`echo "$TT_TASK" | awk '{print $1}'`,
		`echo {TASK}`,
		`echo {tsak}`,
		`echo {}`,
	} {
		if got := applyBraceTemplate(in); got != in {
			t.Errorf("applyBraceTemplate(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestApplyBraceTemplateSpansLengthInvariant(t *testing.T) {
	for _, name := range config.Placeholders() {
		_, spans := applyBraceTemplateSpans("{" + name + "}")
		if len(spans) != 1 {
			t.Fatalf("applyBraceTemplateSpans(%q) returned %d spans, want 1", "{"+name+"}", len(spans))
		}
		s := spans[0]
		if got, want := s.to-s.from, len(`"$TT_"`)+len(name); got != want {
			t.Errorf("span for %q has length %d, want %d", name, got, want)
		}
	}
}

func TestApplyBraceTemplateSpansTwoPlaceholders(t *testing.T) {
	got, spans := applyBraceTemplateSpans("notify-send {task} {project}")
	if want := `notify-send "$TT_TASK" "$TT_PROJECT"`; got != want {
		t.Fatalf("applyBraceTemplateSpans result = %q, want %q", got, want)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(spans), spans)
	}
	for i, want := range []templateSpan{
		{name: "task", match: "{task}"},
		{name: "project", match: "{project}"},
	} {
		s := spans[i]
		if s.name != want.name || s.match != want.match {
			t.Errorf("span[%d] = {name: %q, match: %q}, want {%q, %q}", i, s.name, s.match, want.name, want.match)
		}
		if text := got[s.from:s.to]; text != `"$TT_`+strings.ToUpper(s.name)+`"` {
			t.Errorf("span[%d] covers %q, want the quoted reference for %s", i, text, s.name)
		}
	}
	if spans[0].from >= spans[1].from {
		t.Errorf("spans not reported in order of appearance: %+v", spans)
	}
}

func TestApplyBraceTemplateSpansSkipsNonSubstitutions(t *testing.T) {
	in := "echo ${task} {unknown}"
	got, spans := applyBraceTemplateSpans(in)
	if got != in {
		t.Errorf("applyBraceTemplateSpans(%q) = %q, want it unchanged", in, got)
	}
	if len(spans) != 0 {
		t.Errorf("got %d spans, want 0: %+v", len(spans), spans)
	}
}

func TestBracePlaceholdersMapToTheAdvertisedVariables(t *testing.T) {
	env := NotifyVars{}.env()
	names := config.Placeholders()
	if len(names) != len(env) {
		t.Errorf("%d placeholders but %d environment variables: %v", len(names), len(env), env)
	}
	for _, name := range names {
		v := notifyVarName(name)
		if got, want := applyBraceTemplate("{"+name+"}"), `"$`+v+`"`; got != want {
			t.Errorf("applyBraceTemplate(\"{%s}\") = %q, want %q", name, got, want)
		}
		if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, v+"=") }) {
			t.Errorf("{%s} expands to $%s, which RunNotify does not set: %v", name, v, env)
		}
	}
}

func TestRepairedPlaceholderReachesNotifierAsOneHostileArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(sh, filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	recorder := filepath.Join(dir, "argv-recorder")
	script := "#!" + sh + "\nprintf 'argc=<%s>\\n' \"$#\"\nprintf 'arg=<%s>\\n' \"$1\"\n"
	if err := os.WriteFile(recorder, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	repaired, changed, err := repairOnEndCommand(`argv-recorder "{task}"`)
	if err != nil || !changed {
		t.Fatalf("repair = %q, %v, %v", repaired, changed, err)
	}
	hostile := "-dash space * ' \" ; $(ignored)\nsecond line"
	result, err := RunNotify(context.Background(), repaired, NotifyVars{Task: hostile})
	if err != nil || result.ExitCode != 0 || result.Stderr != "" {
		t.Fatalf("RunNotify = %+v, %v", result, err)
	}
	if want := "argc=<1>\narg=<" + hostile + ">\n"; result.Stdout != want {
		t.Fatalf("stdout = %q, want %q", result.Stdout, want)
	}
}

func TestNotifyTestFailsWhenTheNotifierExitsNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	isolate(t)
	writeOnEndConfig(t, "echo talking; echo complaining >&2; exit 7")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"notify", "test"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "status 7") {
		t.Errorf("stderr = %q, want the exit status in it", stderr.String())
	}

	if !strings.Contains(stdout.String(), "talking") {
		t.Errorf("stdout = %q, want the command's own output in it", stdout.String())
	}
	if !strings.Contains(stderr.String(), "complaining") {
		t.Errorf("stderr = %q, want the command's own stderr in it", stderr.String())
	}
}

func TestNotifyTestFailsWhenTheNotifierIsKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	isolate(t)

	oldTimeout, oldDelay := notifyTimeout, notifyWaitDelay
	notifyTimeout, notifyWaitDelay = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { notifyTimeout, notifyWaitDelay = oldTimeout, oldDelay })
	writeOnEndConfig(t, "sleep 30")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"notify", "test"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "was killed") {
		t.Errorf("stderr = %q, want it to say the command was killed", stderr.String())
	}

	if !strings.Contains(stderr.String(), "did not finish within") {
		t.Errorf("stderr = %q, want it to say the command ran out of its time", stderr.String())
	}
}

func TestNotifyTestSaysNoStatusWhenASignalEndedTheNotifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	isolate(t)
	writeOnEndConfig(t, "kill -TERM $$")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"notify", "test"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitError, stderr.String())
	}
	if strings.Contains(stderr.String(), "status") {
		t.Errorf("stderr = %q, want no exit status claimed for a command that never exited", stderr.String())
	}
	if !strings.Contains(stderr.String(), "killed before it could exit") {
		t.Errorf("stderr = %q, want it to say the command was killed instead", stderr.String())
	}
	if !strings.Contains(stderr.String(), "terminated") {
		t.Errorf("stderr = %q, want the signal that ended the command named in it", stderr.String())
	}
}

func TestNotifyTestSucceedsWhenTheNotifierWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	isolate(t)
	writeOnEndConfig(t, "echo notified {task}")

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"notify", "test"}, strings.NewReader(""), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "notified Test task") {
		t.Errorf("stdout = %q, want the substituted command's output in it", stdout.String())
	}
}

func TestNotifyTestSaysNothingWhenTheRunWasCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`the configured command runs through "sh -c"`)
	}
	isolate(t)

	marker := filepath.Join(t.TempDir(), "notifier-started")
	writeOnEndConfig(t, "touch "+marker+"; sleep 30")

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- notifyTest(ctx, &stdout, &stderr) }()

	waitForFile(t, marker)
	cancel()

	select {
	case got := <-done:
		if got != exitInterrupted {
			t.Errorf("notifyTest() = %d, want %d", got, exitInterrupted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("notifyTest never returned after the context was cancelled")
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing at all: the notifier did not fail, tt was interrupted", stderr.String())
	}
}

func writeOnEndConfig(t *testing.T, command string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "tt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[timer]\non_end = '" + command + "'\n"
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyOutcome(t *testing.T) {

	killed := &exec.ExitError{}
	for _, tc := range []struct {
		name         string
		runErr       error
		ctxErr       error
		wantTimedOut bool
		wantErr      error
	}{
		{

			name:   "finished in the instant the deadline expired",
			ctxErr: context.DeadlineExceeded,
		},
		{
			name:   "finished in the instant the caller gave up",
			ctxErr: context.Canceled,
		},
		{
			name:         "killed by the deadline",
			runErr:       killed,
			ctxErr:       context.DeadlineExceeded,
			wantTimedOut: true,
		},
		{
			name:    "killed because the caller gave up",
			runErr:  killed,
			ctxErr:  context.Canceled,
			wantErr: context.Canceled,
		},
		{
			name:   "left something behind on the pipes",
			runErr: exec.ErrWaitDelay,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := notifyOutcome(NotifyResult{}, tc.runErr, tc.ctxErr)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.TimedOut != tc.wantTimedOut {
				t.Errorf("TimedOut = %v, want %v", res.TimedOut, tc.wantTimedOut)
			}
		})
	}
}
