//go:build windows

package api

import (
	"os"

	"golang.org/x/sys/windows"
)

func openNonblocking(path string) (*os.File, error) {
	return openWindows(path, windows.FILE_ATTRIBUTE_NORMAL)
}

func openDirectoryNonblocking(path string) (*os.File, error) {
	return openWindows(path, windows.FILE_FLAG_BACKUP_SEMANTICS)
}

func openWindows(path string, flags uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
