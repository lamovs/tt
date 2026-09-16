//go:build unix

package ai

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts cmd as the leader of a new process group and makes
// cancelling its context SIGKILL that whole group, so that whatever the
// agent CLI started in turn dies with it instead of outliving tt.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killGroup(cmd.Process.Pid)
	}
}

// killGroupAfterExit blocks until the group leader pid exits, then SIGKILLs
// whatever is left of its group: a child the agent CLI left running, still
// holding the output pipes open or not. The leader is not reaped by then -
// that is cmd.Wait's job, afterwards - so its pid, which is also the group
// id, cannot have been reused, and the signal reaches only processes the
// agent started. Where waiting without reaping is not supported, nothing is
// killed.
func killGroupAfterExit(pid int) {
	if err := waitExited(pid); err != nil {
		return
	}
	_ = killGroup(pid)
}

// killGroup SIGKILLs the process group pgid, reporting a group that is
// already gone as os.ErrProcessDone.
func killGroup(pgid int) error {
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
