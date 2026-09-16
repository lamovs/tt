//go:build unix

package ai

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunExecKillsTheWholeProcessGroupOnCancel mimics an npm wrapper that
// starts the real agent binary as its own child: sh stands in for the
// wrapper and a background sleep for the binary, holding stdout and stderr
// open. Killing sh alone would leave sleep running and keep runExec waiting
// on the pipes until WaitDelay gave up on them.
func TestRunExecKillsTheWholeProcessGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := runExec(ctx, "sh", []string{"-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidFile}, nil, dir, []string{"PATH=" + os.Getenv("PATH")})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want an error from a run whose context was cancelled")
	}
	if elapsed >= 4*time.Second {
		t.Errorf("runExec returned %s after start, want soon after the 1s cancel, well before the 5s WaitDelay", elapsed)
	}

	requireGone(t, readPID(t, pidFile))
}

// readPID reads the pid a test script wrote to path.
func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the grandchild never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file holds %q", raw)
	}
	return pid
}

// requireGone fails unless pid is gone within a few seconds, and kills it
// if it is not, so no test leaves a process behind. A zombie counts as gone:
// it has exited and only waits for its parent to reap it, which, for an
// orphan in a container with no init process to adopt it, may never come.
func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) || isZombie(pid) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d outlived the run", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isZombie reports whether pid has exited but is not reaped yet: state Z in
// /proc/<pid>/stat where there is a /proc, or in ps otherwise.
func isZombie(pid int) bool {
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		// The state follows the command name, which is in parentheses and
		// may hold spaces or parentheses itself.
		rest := string(stat)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		return len(fields) > 0 && fields[0] == "Z"
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestIsZombie(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if isZombie(pid) {
		t.Errorf("a running process %d counts as a zombie", pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !isZombie(pid) {
		if time.Now().After(deadline) {
			t.Errorf("killed and unreaped process %d never counted as a zombie", pid)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Wait()
	if isZombie(pid) {
		t.Errorf("reaped process %d still counts as a zombie", pid)
	}
}

func requireWaitExited(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("waiting for an exit without reaping is not implemented on %s", runtime.GOOS)
	}
}

// TestRunExecKillsWhatTheLeaderLeavesBehind covers an agent CLI that exits
// on its own but leaves a child running: one still holding stdout, which
// would keep runExec waiting until WaitDelay gave up, and one that closed
// its pipes, which would simply outlive tt.
func TestRunExecKillsWhatTheLeaderLeavesBehind(t *testing.T) {
	requireWaitExited(t)
	cases := map[string]string{
		"holding the output pipes": `sleep 30 & echo $! > "$1"; echo done`,
		"with its pipes closed":    `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$1"; echo done`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "grandchild.pid")

			start := time.Now()
			stdout, _, err := runExec(context.Background(), "sh", []string{"-c", script, "sh", pidFile}, nil, dir, []string{"PATH=" + os.Getenv("PATH")})
			elapsed := time.Since(start)
			pid := readPID(t, pidFile)
			if err != nil {
				t.Errorf("runExec: %v, want the leader's own clean exit", err)
			}
			if strings.TrimSpace(string(stdout)) != "done" {
				t.Errorf("stdout = %q, want %q", stdout, "done")
			}
			if elapsed >= 3*time.Second {
				t.Errorf("runExec returned %s after start, want it right after the leader exits, well before the 5s WaitDelay", elapsed)
			}
			requireGone(t, pid)
		})
	}
}

func TestWaitExitedReturnsForAChildThatAlreadyExited(t *testing.T) {
	requireWaitExited(t)
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- waitExited(cmd.Process.Pid) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("waitExited: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitExited blocked on a child that had already exited")
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("Wait after waitExited: %v, want the child still there to reap", err)
	}
}

func TestWaitExitedWaitsForARunningChild(t *testing.T) {
	requireWaitExited(t)
	cmd := exec.Command("sleep", "0.4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := waitExited(cmd.Process.Pid); err != nil {
		t.Fatalf("waitExited: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("waitExited returned after %s, before the child could have exited", elapsed)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("Wait after waitExited: %v, want the child still there to reap", err)
	}
}
