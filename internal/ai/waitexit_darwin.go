//go:build darwin

package ai

import "golang.org/x/sys/unix"

// waitExited blocks until the child pid has exited, leaving it unreaped for
// cmd.Wait to collect. A kqueue NOTE_EXIT event reports the exit without
// waiting on the process; a child that has already exited cannot be watched
// any more, and saying so (ESRCH) is just as good an answer.
func waitExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)

	watch := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	for {
		_, err := unix.Kevent(kq, []unix.Kevent_t{watch}, nil, nil)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ESRCH {
			return nil
		}
		if err != nil {
			return err
		}
		break
	}

	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}
