//go:build linux

package ai

import "golang.org/x/sys/unix"

// waitExited blocks until the child pid has exited, leaving it unreaped:
// WNOWAIT keeps it a zombie for cmd.Wait to collect.
func waitExited(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
