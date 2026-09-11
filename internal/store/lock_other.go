//go:build !unix

package store

import (
	"errors"
	"os"
)

var errNoLocking = errors.New("process locking is not implemented on this platform")

func lockFile(*os.File) error { return errNoLocking }

func unlockFile(*os.File) error { return nil }
