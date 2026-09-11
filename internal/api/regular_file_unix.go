//go:build unix

package api

import (
	"os"

	"golang.org/x/sys/unix"
)

func openNonblocking(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openDirectoryNonblocking(path string) (*os.File, error) { return openNonblocking(path) }
