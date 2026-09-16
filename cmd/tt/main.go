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

// ttSignals returns the signals that end a run the way an interrupt does: they
// cancel its context, so what the run started is stopped before tt exits, and
// a second one kills tt. A quit and a hangup are among them, because pressing
// Ctrl-\ or closing the terminal must also stop a child, such as an agent,
// that runs in a process group of its own. A hangup is left alone when tt
// started with it ignored, as nohup starts it: listening for a signal would
// stop ignoring it. A quit needs no such check - Go never keeps a quit
// ignored, and reports none as ignored.
func ttSignals() []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	return signals
}

func main() {
	os.Exit(ttMain())
}

func ttMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), ttSignals()...)
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
