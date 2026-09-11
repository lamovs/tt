//go:build aix || linux || solaris

package main

import "golang.org/x/sys/unix"

const (
	ioctlReadTermios       = unix.TCGETS
	ioctlWriteTermios      = unix.TCSETS
	ioctlWriteTermiosFlush = unix.TCSETSF
)
