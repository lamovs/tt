//go:build !unix

package ai

import "os/exec"

// setProcessGroup leaves cmd as it is: without Unix process groups,
// cancelling its context kills only the direct child.
func setProcessGroup(*exec.Cmd) {}

// killGroupAfterExit does nothing: there is no process group to clean up.
func killGroupAfterExit(int) {}
