//go:build !unix

package main

import "os"

func defaultToggleEcho(*os.File, bool, pendingInput) error { return nil }
