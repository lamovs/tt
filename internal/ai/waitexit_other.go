//go:build unix && !linux && !darwin

package ai

import "errors"

// waitExited is not supported here: there is no way in use to wait for a
// child's exit without reaping it, so killGroupAfterExit kills nothing.
func waitExited(int) error {
	return errors.ErrUnsupported
}
