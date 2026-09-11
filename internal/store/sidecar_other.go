//go:build !unix && !windows

package store

import (
	"errors"
	"os"
)

var errNonblockingSidecarReadUnsupported = errors.New("secure nonblocking cache sidecar reads are not supported on this platform")

func openSidecarNonblocking(path string) (*os.File, error) {
	return nil, errNonblockingSidecarReadUnsupported
}
