//go:build unix

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool {

	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlReadTermios)
	return err == nil
}
