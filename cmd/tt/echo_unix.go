//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func defaultToggleEcho(f *os.File, on bool, pending pendingInput) error {

	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return err
	}
	if on {
		t.Lflag |= unix.ECHO
	} else {
		t.Lflag &^= unix.ECHO
	}

	if pending == discardInput {
		return unix.IoctlSetTermios(fd, ioctlWriteTermiosFlush, t)
	}
	return unix.IoctlSetTermios(fd, ioctlWriteTermios, t)
}
