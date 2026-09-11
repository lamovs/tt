package main

import (
	"fmt"
	"io"
)

func cmdVersion(stdout, stderr io.Writer, args []string) int {
	if len(args) != 0 {
		fmt.Fprint(stderr, "usage: tt version\n")
		return exitUsage
	}
	fmt.Fprintf(stdout, "tt version %s\n", version)
	return exitOK
}
