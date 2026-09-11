package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2

	exitInterrupted = 130
)

func interrupted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

func runText(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {

		writeIndex(stdout, cli.PaletteFor(indexColorMode(), stdout))
		return exitOK
	}

	if len(args) == 1 {
		switch args[0] {
		case "--version":
			return cmdVersion(stdout, stderr, nil)
		case "--help", "-h":
			writeIndex(stdout, cli.PaletteFor(indexColorMode(), stdout))
			return exitOK
		}
	}

	verb, rest := args[0], args[1:]
	c, known := commands[verb]
	if !known {
		switch {
		case verb == endOfOptions:

			fmt.Fprintf(stderr, "tt: %s\n", endOfOptionsBeforeTheVerb())
		case strings.HasPrefix(verb, "-"):
			fmt.Fprintln(stderr, quoteWord("tt: unknown flag ", verb))
		default:
			fmt.Fprintln(stderr, quoteWord("tt: unknown command ", verb))
		}
		fmt.Fprint(stderr, "run \"tt\" with no arguments for a list of commands\n")
		return exitUsage
	}

	colored, mode, given, colorBeforeMarker, colorErr := takeColor(rest)
	if colorErr == nil {
		rest = colored
	}
	data, refinements := splitArgs(rest)
	inv := &invocation{
		ctx:         ctx,
		verb:        verb,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		args:        rest,
		data:        data,
		refinements: refinements,
		color:       mode,
		colorKnown:  given,
	}

	if wantsHelp(refinements) {
		cli.WriteLines(stdout, c.help.Render(inv.outPalette()))
		return exitOK
	}
	if colorErr != nil {
		fmt.Fprintf(stderr, "tt: %s: %v\n", verb, colorErr)
		return exitUsage
	}

	if colorBeforeMarker || slices.Contains(refinements, endOfOptions) {
		return inv.misuse("%s", endOfOptionsTooLate(verb))
	}
	return c.run(inv)
}

const (
	endOfOptionsRefusal = `"--" goes in front of the data it protects, and everything after it is ` +
		`read as data - so nothing can follow it as an option, as in:`
	endOfOptionsExample = "  tt add -- -5 min plank"

	endOfOptionsFirst = `"--" goes after the command and in front of the data it protects, so a ` +
		`command line cannot begin with it, as in:`
)

func endOfOptionsTooLate(verb string) string {
	wrapped := cli.Wrap(endOfOptionsRefusal, cli.Width-len("tt: "+verb+": "))
	return strings.Join(append(wrapped, endOfOptionsExample), "\n")
}

func endOfOptionsBeforeTheVerb() string {
	wrapped := cli.Wrap(endOfOptionsFirst, cli.Width-len("tt: "))
	return strings.Join(append(wrapped, endOfOptionsExample), "\n")
}

func indexColorMode() config.ColorMode {
	if cfg, err := config.Load(); err == nil {
		return cfg.Color
	}
	return config.ColorAuto
}

const endOfOptions = "--"

func splitArgs(args []string) (data, refinements []string) {
	for i, a := range args {
		if a == endOfOptions {
			return append(data, args[i+1:]...), nil
		}
		if a != "-" && strings.HasPrefix(a, "-") {
			return data, args[i:]
		}
		data = append(data, a)
	}
	return data, nil
}
