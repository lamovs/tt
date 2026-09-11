package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func cmdTimerNote(inv *invocation, runtime timerRuntime) int {
	if len(inv.data) != 2 || inv.data[1] == "" || inv.rawOutput {
		return inv.misuse("note needs one exact session ID and --note TEXT or --skip")
	}
	addition, skip := "", false
	switch {
	case len(inv.refinements) == 1 && inv.refinements[0] == "--skip":
		skip = true
	case len(inv.refinements) == 2 && inv.refinements[0] == "--note" && inv.refinements[1] != "":
		addition = inv.refinements[1]
	default:
		return inv.misuse("choose --note TEXT or --skip; an empty addition uses --skip")
	}
	st, err := inv.openStore()
	if err != nil {
		return timerFailure(inv, err)
	}
	defer st.Close()
	target, err := st.ReadFocusTarget(inv.ctx, inv.data[1])
	if err != nil {
		return timerFailure(inv, err)
	}
	return resolveTimerNote(inv, st, target, addition, skip, runtime)
}

func resolveTimerNote(inv *invocation, st *store.Store, target store.FocusTarget, addition string, skip bool, runtime timerRuntime) int {
	cfg, err := inv.config()
	if err != nil {
		return timerFailure(inv, err)
	}
	session, err := st.ResolveFocusNote(inv.ctx, target, addition, skip)
	if err != nil {
		return timerFailure(inv, err)
	}
	var wakeErr error
	if cfg.FocusUpload.Enabled && runtime.wake != nil {
		if err := runtime.wake(); err != nil {
			wakeErr = errors.Join(errors.New("note review saved; automatic upload wake failed"), err)
		}
	}
	if inv.jsonOutput {
		result := app.Result(inv.verb, timerSessionData(session), app.ResultMeta{Source: "local"})
		result.Status = "recorded"
		if wakeErr != nil {
			result.Warnings = append(result.Warnings, wakeErr.Error())
		}
		if code := inv.writeResult(result); code != exitOK {
			return code
		}
	} else {
		message := "Completion note saved for session "
		if skip {
			message = "Completion note skipped for session "
		}
		fmt.Fprintln(inv.stdout, cli.ReportLine(message, session.ID, ".", nil))
		if skip {
			fmt.Fprintln(inv.stdout, "Existing note kept unchanged.")
		}
		if wakeErr != nil {
			fmt.Fprintln(inv.stderr, cli.Foreign(wakeErr.Error(), cli.Width))
		}
	}
	return exitOK
}

func offerTimerNote(inv *invocation, st *store.Store, preferred string, runtime timerRuntime) int {
	if inv.jsonOutput || !canAsk(inv) {
		return exitOK
	}
	reviews, err := st.PendingFocusNoteReviews(inv.ctx)
	if err != nil {
		return timerFailure(inv, err)
	}
	if len(reviews) == 0 {
		return exitOK
	}
	target := reviews[0]
	for _, review := range reviews {
		if review.Session.ID == preferred {
			target = review
			break
		}
	}
	task := "Task: none"
	if target.Session.TaskID != "" {
		task = "Task ID: " + target.Session.TaskID
		if cached, err := st.Task(inv.ctx, target.Session.TaskID); err == nil {
			task = "Task: " + cached.Title + " (ID: " + target.Session.TaskID + ")"
		}
	}
	for _, line := range []string{
		"Focus-history note for session " + target.Session.ID,
		"Kind: " + timerMode(target.Session.FocusType) + "; active: " + timerDuration(target.Session.ActiveDuration),
		"Started: " + target.Session.StartedAt.Format(time.RFC3339),
		"Ended: " + target.Session.EndedAt.Format(time.RFC3339),
		task,
		"Existing note: " + target.Session.Note,
		"Add a completion note (Enter skips; EOF defers):",
	} {
		for i, part := range strings.Split(ansi.Hardwrap(strconv.Quote(line), cli.Width-2, true), "\n") {
			if i != 0 {
				part = "  " + part
			}
			fmt.Fprintln(inv.stderr, part)
		}
	}
	addition, err := readTimerNoteAnswer(inv.ctx, inv.stdin)
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(inv.stderr, "Note review deferred; the saved session remains pending.")
		return exitOK
	}
	if err != nil {
		return timerFailure(inv, errors.Join(errors.New("session saved; note review remains pending"), err))
	}
	return resolveTimerNote(inv, st, target, addition, addition == "", runtime)
}

func readTimerNoteAnswer(ctx context.Context, input io.Reader) (string, error) {
	type answer struct {
		line string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(input, 20002)).ReadString('\n')
		if len(line) > 20001 {
			err = errors.New("completion note is too long")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		done <- answer{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-done:
		return result.line, result.err
	}
}
