package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	os.Exit(ttMain())
}

func ttMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop()
	}()

	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if ctx.Err() != nil {

		reportInterrupted(os.Stderr)
		return exitInterrupted
	}
	return code
}

func reportInterrupted(w io.Writer) {
	fmt.Fprint(w, "\ntt: interrupted\n")
}
