//go:build !unix

package cli

import "os"

func isTerminal(*os.File) bool { return false }
