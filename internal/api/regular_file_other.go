//go:build !unix && !windows

package api

import (
	"errors"
	"os"
)

var errNonblockingCredentialReadUnsupported = errors.New("secure nonblocking credential reads are not supported on this platform")

func openNonblocking(path string) (*os.File, error) {
	return nil, errNonblockingCredentialReadUnsupported
}

func openDirectoryNonblocking(path string) (*os.File, error) {
	return nil, errNonblockingCredentialReadUnsupported
}
