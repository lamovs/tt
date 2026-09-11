package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/config"
)

var notifyTimeout = 5 * time.Second

var notifyWaitDelay = 500 * time.Millisecond

var notifyPlaceholders = config.Placeholders()

type NotifyVars struct {
	Kind     string
	Note     string
	Task     string
	Project  string
	Duration string
	Cycle    string
}

func (v NotifyVars) env() []string {
	return []string{
		"TT_KIND=" + v.Kind,
		"TT_NOTE=" + v.Note,
		"TT_TASK=" + v.Task,
		"TT_PROJECT=" + v.Project,
		"TT_DURATION=" + v.Duration,
		"TT_CYCLE=" + v.Cycle,
	}
}

func notifyVarName(placeholder string) string {
	return "TT_" + strings.ToUpper(placeholder)
}

func applyBraceTemplate(command string) string {
	result, _ := applyBraceTemplateSpans(command)
	return result
}

type templateSpan struct {
	name       string
	match      string
	sourceFrom int
	sourceTo   int
	from       int
	to         int
}

func applyBraceTemplateSpans(command string) (string, []templateSpan) {
	matches := config.PlaceholderPattern().FindAllStringIndex(command, -1)
	var out strings.Builder
	var spans []templateSpan
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		match := command[start:end]
		out.WriteString(command[last:start])
		last = end

		name, ok := config.PlaceholderName(match)
		if !ok || !slices.Contains(notifyPlaceholders, name) {
			out.WriteString(match)
			continue
		}
		from := out.Len()
		out.WriteString(`"$` + notifyVarName(name) + `"`)
		spans = append(spans, templateSpan{
			name: name, match: match,
			sourceFrom: start, sourceTo: end,
			from: from, to: out.Len(),
		})
	}
	out.WriteString(command[last:])
	return out.String(), spans
}

type NotifyResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Killed   string
	TimedOut bool
}

func RunNotify(ctx context.Context, command string, vars NotifyVars) (NotifyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", applyBraceTemplate(command))
	cmd.Env = append(cmd.Environ(), vars.env()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cmd.WaitDelay = notifyWaitDelay
	setProcessGroup(cmd)

	runErr := cmd.Run()
	return notifyOutcome(NotifyResult{Stdout: stdout.String(), Stderr: stderr.String()}, runErr, ctx.Err())
}

func notifyOutcome(res NotifyResult, runErr, ctxErr error) (NotifyResult, error) {
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return res, nil
	case errors.Is(ctxErr, context.DeadlineExceeded):

		res.TimedOut = true
		return res, nil
	case ctxErr != nil:

		return res, fmt.Errorf("run notify command: %w", ctxErr)
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if res.ExitCode < 0 {

			res.Killed = exitErr.ProcessState.String()
		}
		return res, nil
	case errors.Is(runErr, exec.ErrWaitDelay):

		return res, nil
	default:
		return res, fmt.Errorf("run notify command: %w", runErr)
	}
}

func cmdNotify(ctx context.Context, stdout, stderr io.Writer, args []string) int {
	data, refinements := splitArgs(args)
	if len(refinements) != 0 || len(data) != 1 || data[0] != "test" {
		fmt.Fprint(stderr, "usage: tt notify test\n")
		return exitUsage
	}
	return notifyTest(ctx, stdout, stderr)
}

func notifyTest(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		printConfigError(stderr, err)
		return exitError
	}
	if cfg.Timer.OnEnd == "" {
		fmt.Fprint(stdout, "timer.on_end is not configured; run \"tt doctor\" for a suggestion\n")
		return exitError
	}

	vars := NotifyVars{
		Kind:     "focus",
		Note:     "Deep work",
		Task:     "Test task",
		Project:  "Inbox",
		Duration: "25m",
		Cycle:    "1",
	}
	fmt.Fprintf(stdout, "running: %s\n", cfg.Timer.OnEnd)
	res, err := RunNotify(ctx, cfg.Timer.OnEnd, vars)

	if interrupted(ctx) {
		return exitInterrupted
	}
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}

	code := exitOK
	switch {
	case res.TimedOut:
		fmt.Fprintf(stderr, "the command did not finish within %s and was killed\n", notifyTimeout)
		code = exitError
	case res.Killed != "":

		fmt.Fprintf(stderr, "the command was killed before it could exit (%s)\n", res.Killed)
		code = exitError
	case res.ExitCode != 0:
		fmt.Fprintf(stderr, "the command exited with status %d\n", res.ExitCode)
		code = exitError
	}

	if res.Stdout != "" {
		fmt.Fprintf(stdout, "stdout: %s\n", res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprintf(stderr, "stderr: %s\n", res.Stderr)
	}
	return code
}
