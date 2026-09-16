package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const undoSkipOption = "--skip"

func init() {
	register(command{
		help: cli.Help{
			Verb:    "undo",
			Summary: "reverse the last change",
			Examples: []cli.Example{
				{Cmd: "tt undo", What: "put back what the last command changed; run it again for the one before"},
				{Cmd: "tt undo --skip", What: "drop the last change from the history and reverse nothing"},
			},
			Sections: []cli.HelpSection{
				{Title: "How undo works", Items: []string{
					"The reversal is queued like any other change and is sent by the next tt sync.",
					"A reversed entry is removed from undo history, so running tt undo again reaches the preceding change.",
					"Changes made together, such as everything one tt ai request applied, are reversed together by one tt undo, and --skip drops them together.",
					"A native move is reversed as well: an unsent move is cancelled, and a move sync already confirmed is moved back by a new queued move.",
				}},
				{Title: "Non-reversible records", Items: []string{
					"Legacy recreate/delete moves cannot be reversed because the original task ID no longer exists. tt refuses instead of guessing.",
					"Use --skip to discard such an undo record without changing a task.",
				}},
			},
			SeeAlso: []string{"sync"},
		},
		run: cmdUndo,
	})
}

type undoReversal struct {
	what       string
	reversible bool

	say func(a store.UndoAction, t model.Task, p cli.Palette) []string

	refuse func(name string) string
}

var errUndoIncomplete = store.ErrUndoIncomplete

var undoReversals = map[string]undoReversal{
	store.OpEntityMutation: {
		what:       "the resource change to",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {
			name := "resource"
			if a.EntityRef != nil {
				name = a.EntityRef.Kind + ":" + a.EntityRef.Key
			}
			return []string{cli.ReportLine("cancelled unsent change to ", name, "; nothing was sent", p.Bold)}
		},
	},

	store.OpTaskCreate: {
		what:       "adding",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {
			return []string{cli.ReportLine("undid adding ", undoTitle(t), ": the task is gone again", p.Bold)}
		},
	},

	store.OpTaskUpdate: {
		what:       "the change to",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {

			if undoWasReopen(a) {
				return []string{cli.ReportLine("undid reopening ", undoTitle(t), ": it is done again", p.Bold)}
			}
			return []string{cli.ReportLine("undid the last change to ", undoTitle(t), undoState(a, t), p.Bold)}
		},
	},

	store.OpTaskComplete: {
		what:       "completing",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {
			return []string{cli.ReportLine("undid completing ", undoTitle(t), undoState(a, t), p.Bold)}
		},
	},

	store.OpTaskDelete: {
		what:       "deleting",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {
			lines := []string{cli.ReportLine("undid deleting ", undoTitle(t), ": the task is back", p.Bold)}

			if t.Id != a.TaskID {
				lines = append(lines, cli.Wrap(undoRemade, cli.Width)...)
			}

			if t.Status.Done() {
				lines = append(lines, cli.Wrap(undoReopened, cli.Width)...)
			}
			return lines
		},
	},

	store.OpTaskMoveNative: {
		what:       "the move of",
		reversible: true,
		say: func(a store.UndoAction, t model.Task, p cli.Palette) []string {
			return []string{cli.ReportLine("undid the move of ", undoTitle(t), ": it is back in its old list", p.Bold)}
		},
	},

	store.OpTaskMoveRecreate: {
		what: "the move of",

		refuse: func(name string) string {
			return "it was the move of " + name + " - a move is a copy of the task made in the " +
				"other list and a delete of the original, so there is nothing left to move back, " +
				"and reversing it could only make a third copy, in the old list, under an id of " +
				"its own"
		},
	},
}

const undoStuck = `nothing was reversed, and the record stays on the undo stack so that the ` +
	`change before it is not reversed in its place - run "tt undo --skip" to drop this record ` +
	`and reach the one before it`

const undoRemade = `it comes back as a new task: the delete is on its way to the server and ` +
	`nothing puts a task back under an id the server has already been given, so the comments, ` +
	`the attachments and the history of the old one do not come with it`

const undoReopened = `the task was done and comes back as work to do: nothing can create a task ` +
	`already finished, so the copy the server makes is an open one and that is the copy the next ` +
	`sync brings back - finish it again once the sync has been through`

func cmdUndo(inv *invocation) int {
	skip := false
	for _, r := range inv.refinements {
		if r != undoSkipOption {
			return inv.misuseWord("unknown option ", r)
		}
		skip = true
	}
	if len(inv.data) > 0 {

		return inv.misuse("takes no arguments: it reverses the last change, whatever it was")
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	e, err := st.LastUndo(inv.ctx)
	switch {
	case errors.Is(err, store.ErrNoUndo):
		inv.resultData = map[string]any{"changed": false, "reason": "empty_undo_history"}

		fmt.Fprintln(inv.stderr, "nothing to undo")
		return exitOK
	case err != nil:
		return inv.fail(err)
	}
	if e.Group != "" {
		return undoGroup(inv, st, e, skip)
	}
	rev, known := undoReversals[e.Action.Op]

	if skip {
		return undoSkipEntry(inv, st, e, rev, known)
	}
	switch {
	case !known:

		return undoRefuse(inv, fmt.Sprintf("it was recorded as %s, an op this build has no reversal "+
			"for - the cache may have been written by another one",
			cli.ReportTitle(e.Action.Op, cli.Width-1)))
	case rev.refuse != nil:
		return undoRefuse(inv, rev.refuse(undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	t, err := st.ApplyUndo(inv.ctx, e)
	if why, ok := undoMoveRefusal(inv, st, e, err); ok {
		return undoRefuse(inv, why)
	}
	switch {
	case errors.Is(err, store.ErrEntityUndoUnavailable), errors.Is(err, store.ErrEntityUncertain):
		return undoRefuse(inv, "the resource change cannot be proven unsent and independent; inspect queue recovery before another action")
	case errors.Is(err, errUndoIncomplete):

		return undoRefuse(inv, fmt.Sprintf("the record of %s %s does not carry what reversing it needs",
			rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
	case errors.Is(err, store.ErrNotFound):

		return undoRefuse(inv, fmt.Sprintf("%s %s cannot be reversed: the task is not in the cache "+
			"any more, so there is nothing left to reverse it on",
			rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
	case errors.Is(err, store.ErrUnsafeChecklist):
		return undoRefuse(inv, fmt.Sprintf("%s: checklist cannot be replaced losslessly; %s %s cannot be reversed safely",
			unsafeUndoReason(err), rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
	case err != nil:
		return inv.fail(err)
	}

	cli.WriteLines(inv.stdout, rev.say(e.Action, t, inv.outPalette()))
	inv.resultData = map[string]any{"changed": true, "operation": e.Action.Op, "task": t}
	if e.Action.EntityRef != nil {
		inv.resultData = map[string]any{"changed": true, "operation": e.Action.Op, "resource_ref": e.Action.EntityRef, "operation_seq": e.Action.OperationSeq, "cancelled_unsent": true}
	}
	return exitOK
}

// undoMoveRefusal explains why a native move could not be reversed: the move
// may be on its way to the server, or the task has left the list the move put
// it in since.
func undoMoveRefusal(inv *invocation, st *store.Store, e store.UndoEntry, err error) (string, bool) {
	if e.Action.Op != store.OpTaskMoveNative || err == nil {
		return "", false
	}
	name := undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())
	switch {
	case errors.Is(err, store.ErrEntityUncertain):
		return "the move of " + name + " may already have reached the server, so it cannot be cancelled " +
			"from here - run tt sync, then tt undo again to move it back", true
	case errors.Is(err, store.ErrTaskChanged):
		return "the move of " + name + " cannot be reversed: the task is no longer in the list the move put it in", true
	}
	return "", false
}

func unsafeUndoReason(err error) string {
	switch text := err.Error(); {
	case strings.Contains(text, "no registry provenance"):
		return "no registry provenance"
	case strings.Contains(text, "uncertain"):
		return "uncertain item identity"
	case strings.Contains(text, "abandoned"):
		return "abandoned item identity"
	default:
		return "unsupported retained checklist data"
	}
}

func undoSkipEntry(inv *invocation, st *store.Store, e store.UndoEntry, rev undoReversal, known bool) int {

	name := undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())
	what := rev.what + " " + name
	if !known {

		what = "the change recorded as " + cli.ReportTitle(e.Action.Op, cli.Width) + " to " + name
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	if err := undoDropTop(inv.ctx, st, func(top store.UndoEntry) bool { return top.Seq == e.Seq }); err != nil {
		return inv.fail(err)
	}
	inv.resultData = map[string]any{"changed": true, "reversed": false, "dropped_record": e.Seq, "operation": e.Action.Op}

	cli.WriteLines(inv.stdout, append(cli.Wrap("dropped the record of "+what, cli.Width),
		`nothing was reversed; "tt undo" now reaches the change before it`))
	return exitOK
}

func undoRefuse(inv *invocation, why string) int {
	code := inv.fail(errors.New("the last change cannot be undone"))
	cli.WriteLines(inv.stderr, cli.Wrap(why, cli.Width))
	cli.WriteLines(inv.stderr, cli.Wrap(undoStuck, cli.Width))
	return code
}

const undoGroupStuck = `nothing was reversed, and every record of the group stays on the undo stack ` +
	`so that no part of it is reversed without the rest - run "tt undo --skip" to drop the ` +
	`whole group and reach the change before it`

func undoRefuseGroup(inv *invocation, count int, why string) int {
	if count == 1 {
		return undoRefuse(inv, why)
	}
	code := inv.fail(errors.New("the last change cannot be undone"))
	cli.WriteLines(inv.stderr, cli.Wrap(fmt.Sprintf("the last change is %d changes made together, "+
		"reversed together or not at all, and one of them stops the rest: %s", count, why), cli.Width))
	cli.WriteLines(inv.stderr, cli.Wrap(undoGroupStuck, cli.Width))
	return code
}

func undoGroup(inv *invocation, st *store.Store, top store.UndoEntry, skip bool) int {
	group, err := st.LastUndoGroup(inv.ctx)
	if err != nil {
		return inv.fail(err)
	}
	if group[0].Seq != top.Seq {
		return inv.fail(store.ErrUndoConflict)
	}
	if skip {
		return undoSkipGroup(inv, st, group)
	}
	for _, e := range group {
		rev, known := undoReversals[e.Action.Op]
		switch {
		case !known:
			return undoRefuseGroup(inv, len(group), fmt.Sprintf("it was recorded as %s, an op this build has no reversal "+
				"for - the cache may have been written by another one",
				cli.ReportTitle(e.Action.Op, cli.Width-1)))
		case rev.refuse != nil:
			return undoRefuseGroup(inv, len(group), rev.refuse(undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
		}
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	tasks, err := st.ApplyUndoGroup(inv.ctx, group)
	var failed *store.UndoGroupError
	if errors.As(err, &failed) {
		e, rev := failed.Entry, undoReversals[failed.Entry.Action.Op]
		if why, ok := undoMoveRefusal(inv, st, e, failed.Err); ok {
			return undoRefuseGroup(inv, len(group), why)
		}
		switch cause := failed.Err; {
		case errors.Is(cause, store.ErrEntityUndoUnavailable), errors.Is(cause, store.ErrEntityUncertain):
			return undoRefuseGroup(inv, len(group), "the resource change cannot be proven unsent and independent; inspect queue recovery before another action")
		case errors.Is(cause, errUndoIncomplete):
			return undoRefuseGroup(inv, len(group), fmt.Sprintf("the record of %s %s does not carry what reversing it needs",
				rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
		case errors.Is(cause, store.ErrNotFound):
			return undoRefuseGroup(inv, len(group), fmt.Sprintf("%s %s cannot be reversed: the task is not in the cache "+
				"any more, so there is nothing left to reverse it on",
				rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
		case errors.Is(cause, store.ErrUnsafeChecklist):
			return undoRefuseGroup(inv, len(group), fmt.Sprintf("%s: checklist cannot be replaced losslessly; %s %s cannot be reversed safely",
				unsafeUndoReason(cause), rev.what, undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())))
		}
	}
	if err != nil {
		return inv.fail(err)
	}

	// A group of one change reads like any single undo.
	var lines []string
	if len(group) > 1 {
		lines = append(lines, fmt.Sprintf("undid %d changes made together, newest first:", len(group)))
	}
	operations := make([]map[string]any, len(group))
	for i, e := range group {
		rev := undoReversals[e.Action.Op]
		lines = append(lines, rev.say(e.Action, tasks[i], inv.outPalette())...)
		operations[i] = map[string]any{"operation": e.Action.Op, "task": tasks[i]}
		if e.Action.EntityRef != nil {
			operations[i] = map[string]any{"operation": e.Action.Op, "resource_ref": e.Action.EntityRef, "operation_seq": e.Action.OperationSeq, "cancelled_unsent": true}
		}
	}
	cli.WriteLines(inv.stdout, lines)
	inv.resultData = map[string]any{"changed": true, "operation": "group", "group_id": group[0].Group, "count": len(group), "operations": operations}
	return exitOK
}

func undoSkipGroup(inv *invocation, st *store.Store, group []store.UndoEntry) int {
	var lines []string
	after := `nothing was reversed; "tt undo" now reaches the change before it`
	if len(group) > 1 {
		lines = append(lines, fmt.Sprintf("dropped the records of %d changes made together, newest first:", len(group)))
		after = `nothing was reversed; "tt undo" now reaches the change before them`
	}
	operations := make([]map[string]any, len(group))
	for i, e := range group {
		rev, known := undoReversals[e.Action.Op]
		name := undoSubject(inv.ctx, st, e.Action, cli.PlainPalette())
		what := rev.what + " " + name
		if !known {
			what = "the change recorded as " + cli.ReportTitle(e.Action.Op, cli.Width) + " to " + name
		}
		lines = append(lines, cli.Wrap("dropped the record of "+what, cli.Width)...)
		operations[i] = map[string]any{"dropped_record": e.Seq, "operation": e.Action.Op}
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	if err := st.DropUndoGroup(inv.ctx, group); err != nil {
		return inv.fail(err)
	}
	inv.resultData = map[string]any{"changed": true, "reversed": false, "operation": "group", "group_id": group[0].Group, "count": len(group), "operations": operations}

	cli.WriteLines(inv.stdout, append(lines, after))
	return exitOK
}

func undoDropTop(ctx context.Context, st *store.Store, is func(store.UndoEntry) bool) error {
	top, err := st.LastUndo(ctx)
	if err != nil {
		return err
	}
	if !is(top) {
		return errors.New("the undo stack moved: the record on top is not the one this run meant to drop")
	}
	_, err = st.PopUndo(ctx)
	return err
}

func undoWasReopen(a store.UndoAction) bool {
	return a.Before != nil && a.Before.Status != nil && a.Before.Status.Done()
}

func undoState(a store.UndoAction, t model.Task) string {
	if a.Before == nil || a.Before.Status == nil {
		return ""
	}
	if t.Status.Done() {
		return ": it is done again"
	}
	return ": it is open again"
}

func undoSubject(ctx context.Context, st *store.Store, a store.UndoAction, p cli.Palette) string {
	if a.EntityRef != nil {
		return undoName(model.Task{Id: a.EntityRef.Kind + ":" + a.EntityRef.Key}, p)
	}
	if a.Task != nil {
		return undoName(*a.Task, p)
	}
	if t, err := st.Task(ctx, a.TaskID); err == nil {
		return undoName(t, p)
	}
	return undoName(model.Task{Id: a.TaskID}, p)
}

func undoTitle(t model.Task) string {
	if strings.TrimSpace(t.Title) == "" {
		return t.Id
	}
	return t.Title
}

func undoName(t model.Task, p cli.Palette) string {
	return p.Bold(cli.ReportTitle(undoTitle(t), cli.Width))
}
