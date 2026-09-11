package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

const configUsage = "usage: tt config [--init | default-project [id:ID]]\n       tt config default-focus [task:ID|none]\n"

func cmdConfig(ctx context.Context, stdout, stderr io.Writer, args []string) int {
	data, refinements := splitArgs(args)
	if len(data) != 0 {
		fmt.Fprintln(stderr, quoteWord("tt: config: unexpected argument ", data[0]))
		fmt.Fprint(stderr, configUsage)
		return exitUsage
	}

	init := false
	for _, r := range refinements {
		if r != "--init" {
			fmt.Fprintln(stderr, quoteWord("tt: config: unknown option ", r))
			fmt.Fprint(stderr, configUsage)
			return exitUsage
		}
		init = true
	}
	if init {
		return initConfig(ctx, stdout, stderr)
	}
	return printEffectiveConfig(stdout, stderr)
}

func printEffectiveConfig(stdout, stderr io.Writer) int {
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		fmt.Fprintf(stdout, "# config file: %s (found)\n", fullReportAtom(path))
	case errors.Is(statErr, fs.ErrNotExist):
		fmt.Fprintf(stdout, "# config file: %s (not found, using defaults)\n", fullReportAtom(path))
	default:
		fmt.Fprintf(stderr, "tt: stat %s: %s\n", fullReportAtom(path), configPathReason(path, statErr))
		return exitError
	}

	cfg, err := config.Load()
	if err != nil {
		printConfigError(stderr, err)
		return exitError
	}
	enc, err := cfg.Encode()
	if err != nil {
		fmt.Fprintf(stderr, "tt: encode config: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "%s", enc)
	return exitOK
}

func initConfig(ctx context.Context, stdout, stderr io.Writer) int {
	path, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}

	if _, err := os.Lstat(path); err == nil {
		fmt.Fprintf(stderr, "tt: config file already exists: %s\n", fullReportAtom(path))
		return exitError
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "tt: stat %s: %s\n", fullReportAtom(path), configPathReason(path, err))
		return exitError
	}

	if interrupted(ctx) {
		return exitInterrupted
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(stderr, "tt: %s\n", fullReportAtom(err.Error()))
		return exitError
	}

	if err := createConfigFile(path); err != nil {
		fmt.Fprintf(stderr, "tt: %s\n", fullReportAtom(err.Error()))
		return exitError
	}
	fmt.Fprintf(stdout, "wrote %s\n", fullReportAtom(path))
	return exitOK
}

func configPathReason(path string, err error) string {
	reason := err
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Path == path {
		reason = pathErr.Err
	}
	return fullReportAtom(reason.Error())
}

const configProblemIndent = "  "

const configErrorProblemBudget = cli.Width - len(configProblemIndent)

func printConfigError(w io.Writer, err error) {
	var cerr *config.Error
	if !errors.As(err, &cerr) {

		reason := err.Error()
		fmt.Fprintf(w, "tt: %s\n", cli.Foreign(reason, 2+quotedRuneMax*utf8.RuneCountInString(reason)))
		return
	}
	fmt.Fprintf(w, "%s: %d problem(s):\n", fullReportAtom(cerr.Path), len(cerr.Problems))
	for _, p := range cerr.Problems {

		lead := ""
		if p.Line > 0 {
			lead = fmt.Sprintf("line %d: ", p.Line)
		}
		fmt.Fprintf(w, "%s%s%s\n", configProblemIndent, lead,
			cli.Foreign(p.Msg, configErrorProblemBudget-len(lead)))
	}
}
