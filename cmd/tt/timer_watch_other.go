//go:build !unix

package main

import (
	"errors"
	"os/exec"
)

func detachTimerWatcher(cmd *exec.Cmd) error {
	return errors.New("background countdown watcher is not supported on this platform")
}

func lockTimerWatcher(path string) (func(), bool, error) {
	return nil, false, errors.New("background countdown watcher is not supported on this platform")
}
