package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "rm",
			Summary: "delete tasks",
			Examples: []cli.Example{
				{Cmd: "tt rm 3", What: "delete the task numbered 3 in the last listing, after asking"},
				{Cmd: "tt rm 2-4", What: "delete a run of them, one question for all three"},
				{Cmd: "tt rm milk -y", What: "delete without the question"},
				{Cmd: "tt rm invoice -y -A", What: "delete every open task the word matches, not one of them"},
			},
			Sections: []cli.HelpSection{
				{Title: "Deletion safety", Items: []string{
					"rm asks before deleting because the API has no restore operation.",
					"tt undo can only recreate a deleted task with a new ID. References to the old ID cannot be restored.",
					"-y answers the confirmation in advance. -A is allowed only together with -y.",
					"Without terminal stdin and stderr, tt refuses instead of deleting because no confirmation can be given.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, Ctrl+D outside checklist mode previews deletion of one exact task and its queued changes. Enter confirms; stale previews are refused.",
				}},
			},
			SeeAlso: []string{"done", "undo", "ui"},
		},
		run: cmdRm,
	})
}

const (
	rmYesOption = "-y"
	rmAllOption = "-A"
)

func cmdRm(inv *invocation) int {
	var yes, all bool
	for _, r := range inv.refinements {
		switch r {
		case rmYesOption:
			yes = true
		case rmAllOption:
			all = true
		default:

			if !strings.HasPrefix(r, "-") {
				return inv.misuse("the tasks come before the options: %s", rmReordered(inv.args))
			}
			return inv.misuseWord("unknown option ", r)
		}
	}

	if all && !yes {
		return inv.misuse("%s deletes every task the words match, so it is only allowed with %s",
			rmAllOption, rmYesOption)
	}
	if len(inv.data) == 0 {
		return inv.misuse("%s", noTaskGivenFor(inv.verb))
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	ids, code, ok := rmTargets(inv, st, all)
	if !ok {
		return code
	}

	tasks, code, ok := rmRead(inv, st, ids)
	if !ok {
		return code
	}

	if !yes {
		if inv.jsonOutput {
			inv.resultData = map[string]any{"preview": true, "tasks": tasks, "requires": "-y", "restores_original_id": false}
			return exitOK
		}

		if !canAsk(inv) {

			return inv.fail(errors.New(rmNoTerminalRefusal(inv.verb)))
		}

		if err := cli.WriteLines(inv.stderr, rmPreview(tasks)); err != nil {
			return inv.fail(err)
		}
		fmt.Fprint(inv.stderr, rmQuestion(len(tasks)))
		if !confirm(inv.stdin) {

			fmt.Fprint(inv.stderr, "aborted, nothing deleted\n")
			return exitOK
		}
	}

	if inv.interrupted() {
		fmt.Fprint(inv.stderr, "interrupted, nothing deleted\n")
		return exitInterrupted
	}

	for i, t := range tasks {

		if inv.interrupted() {
			fmt.Fprintf(inv.stderr, "interrupted, %s not deleted\n", rmTaskCount(len(tasks)-i))
			return exitInterrupted
		}
		if err := st.DeleteTask(inv.ctx, t.Id); err != nil {
			return inv.fail(err)
		}
		inv.recordTask(t, true, true)
		fmt.Fprintln(inv.stdout, rmDeletedLine(t))
	}
	return exitOK
}

func rmNoTerminalRefusal(verb string) string {
	text := fmt.Sprintf("refusing to ask: stdin or stderr is not a terminal, so pass %s to delete without a question",
		rmYesOption)
	return strings.Join(cli.Wrap(text, cli.Width-len("tt: "+verb+": ")), "\n")
}

func rmReordered(args []string) string {
	line := make([]string, 0, len(args)+2)
	line = append(line, "tt", "rm")
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			line = append(line, a)
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			line = append(line, a)
		}
	}
	return strings.Join(line, " ")
}

func rmTargets(inv *invocation, st *store.Store, all bool) ([]string, int, bool) {
	if !all {
		return inv.resolveTasks(st, inv.data, store.StatusOpen)
	}
	r := inv.resolver(st)
	r.Ask = false
	ids, err := r.Tasks(inv.ctx, inv.data, store.StatusOpen)
	var ambiguous *cli.AmbiguousError
	if !errors.As(err, &ambiguous) {
		return inv.taskOutcome(ids, err)
	}
	found, err := st.Tasks(inv.ctx, store.TaskFilter{Search: ambiguous.Query, Status: store.StatusOpen})
	if err != nil {
		return nil, inv.fail(err), false
	}
	return cli.TaskIDs(found), exitOK, true
}

func rmRead(inv *invocation, st *store.Store, ids []string) ([]model.Task, int, bool) {
	if inv.interrupted() {
		return nil, exitInterrupted, false
	}
	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		t, err := st.Task(inv.ctx, id)
		if err != nil {
			return nil, inv.fail(err), false
		}
		tasks = append(tasks, t)
	}
	return tasks, exitOK, true
}

func rmPreview(tasks []model.Task) []string {
	lines := make([]string, 0, len(tasks)+2)
	lines = append(lines, fmt.Sprintf("this deletes %s:", rmTaskCount(len(tasks))))
	for _, t := range tasks {
		lines = append(lines, cli.ReportLine("  ", t.Title, "", nil))
	}

	lines = append(lines, "a deleted task cannot be restored; undo can only build it again under a new id")
	return lines
}

func rmQuestion(n int) string {
	if n == 1 {
		return "delete it? [y/N] "
	}
	return "delete them? [y/N] "
}

func rmDeletedLine(t model.Task) string {
	return cli.ReportLine("deleted ", t.Title, "", nil)
}

func rmTaskCount(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}
