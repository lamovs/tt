package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
)

const runAsTTEnv = "TT_TEST_RUN_AS_TT"

func TestMain(m *testing.M) {
	if os.Getenv(runAsTTEnv) != "" {
		os.Exit(ttMain())
	}
	os.Exit(m.Run())
}

func ttChild(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		runAsTTEnv+"=1",
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		tokenEnvVar+"=",
	)
	return cmd
}

func TestInterruptEndsTheRunWithTheInterruptedCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("neither the signal nor the \"sh -c\" the notifier runs under exists there")
	}
	// A hangup, which closing the terminal sends, and a quit, which Ctrl-\
	// sends, end the run the same way.
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			if sig == syscall.SIGHUP && signal.Ignored(syscall.SIGHUP) {
				t.Skip("the tests run with hangups ignored, as under nohup, so tt is started ignoring them too")
			}
			cmd, stderr, marker := notifierChild(t)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitForFile(t, marker)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()

			if got := cmd.ProcessState.ExitCode(); got != exitInterrupted {
				t.Fatalf("tt ended as %v (code %d), want %d - a code of -1 means the signal killed it instead of cancelling it\nstderr: %s",
					cmd.ProcessState, got, exitInterrupted, stderr.String())
			}
			if !strings.Contains(stderr.String(), "interrupted") {
				t.Errorf("stderr = %q, want it to say the run was interrupted", stderr.String())
			}
		})
	}
}

func TestAHangupTTWasStartedToIgnoreLeavesTheRunGoing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("neither the signal nor the \"sh -c\" the notifier runs under exists there")
	}
	cmd, stderr, marker := notifierChild(t)
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to start tt with hangups ignored")
	}
	// The way nohup starts it: an ignored signal stays ignored across exec.
	cmd.Path, cmd.Args = shell, append([]string{"sh", "-c", `trap "" HUP; exec "$0" "$@"`}, cmd.Args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer cmd.Process.Kill()
	waitForFile(t, marker)

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
		t.Fatalf("tt ended as %v on a hangup it was started to ignore\nstderr: %s", cmd.ProcessState, stderr.String())
	case <-time.After(500 * time.Millisecond):
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("tt did not end on the interrupt after the ignored hangup")
	}
	if got := cmd.ProcessState.ExitCode(); got != exitInterrupted {
		t.Errorf("tt ended as %v (code %d), want %d\nstderr: %s", cmd.ProcessState, got, exitInterrupted, stderr.String())
	}
}

// notifierChild prepares, without starting it, a tt child whose notify test
// runs a notifier that touches marker and then sleeps, so a signal lands while
// the run is busy.
func notifierChild(t *testing.T) (*exec.Cmd, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config", "tt")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "notifier-started")
	body := "[timer]\non_end = 'touch " + marker + "; sleep 30'\n"
	if err := os.WriteFile(filepath.Join(cfgDir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := ttChild(t, dir, "notify", "test")
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	return cmd, &stderr, marker
}

func TestSecondInterruptKillsWhatTheFirstCouldNotStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the signal is not delivered to a process there")
	}
	dir := t.TempDir()
	cmd := ttChild(t, dir, "login", "token")

	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer cmd.Process.Kill()

	waitForPrompt(t, stdout, "token: ")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		t.Fatalf("tt left on the first signal (%v); the read it is blocked in cannot be unwound, so this test would prove nothing about the second", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not take tt down: the watch was still armed and swallowed it too")
	}

	if got := cmd.ProcessState.ExitCode(); got != -1 {
		t.Fatalf("tt ended as %v (code %d), want it killed by the signal", cmd.ProcessState, got)
	}
}

func TestReportInterruptedSaysThatAndNothingElse(t *testing.T) {
	var w bytes.Buffer
	reportInterrupted(&w)
	got := w.String()
	if got != "\ntt: interrupted\n" {
		t.Errorf("reportInterrupted printed %q, want it to say that the run was told to stop, and only that", got)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func waitForPrompt(t *testing.T, r io.Reader, want string) {
	t.Helper()
	seen := make(chan string, 1)
	go func() {
		var got []byte
		br := bufio.NewReader(r)
		for !strings.Contains(string(got), want) {
			b, err := br.ReadByte()
			if err != nil {
				break
			}
			got = append(got, b)
		}
		seen <- string(got)
	}()
	select {
	case got := <-seen:
		if !strings.Contains(got, want) {
			t.Fatalf("tt printed %q before its output ended, want %q in it", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tt never printed its prompt")
	}
}
