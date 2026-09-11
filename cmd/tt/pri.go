package main

import (
	"fmt"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "pri",
			Summary: "set a task's priority",
			Examples: []cli.Example{
				{Cmd: "tt pri 3 high", What: "the task numbered 3 in the last listing"},
				{Cmd: "tt pri call mom med", What: "search by title, then set it"},
				{Cmd: "tt pri 5 none", What: "take the priority off"},
			},
			Sections: []cli.HelpSection{
				{Title: "Values", Items: []string{
					"Priority is one of none, low, med or high.",
					"Title searches consider open tasks only.",
				}},
			},
		},
		run: cmdPri,
	})
}

func cmdPri(inv *invocation) int {
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	ids, levelArg, code, ok := inv.resolveTasksAndValue(st, inv.data, store.StatusOpen, "priority",
		func(s string) error {
			_, err := model.ParsePriority(s)
			return err
		})
	if !ok {
		return code
	}

	level, err := model.ParsePriority(levelArg)
	if err != nil {
		return inv.misuse("%v", err)
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		t, err := st.Task(inv.ctx, id)
		if err != nil {

			return inv.fail(err)
		}
		tasks = append(tasks, t)
	}

	edit := model.TaskEdit{Priority: model.Ptr(level)}
	written := make([]model.Task, 0, len(tasks))
	for _, t := range tasks {

		if inv.interrupted() {
			priReport(inv, level, written)
			return exitInterrupted
		}
		if t.Priority == level {
			inv.recordTask(t, false, false)

			fmt.Fprintln(inv.stdout, cli.ReportLine("", t.Title, ": priority already "+level.String(), nil))
			continue
		}
		updated, err := st.UpdateTask(inv.ctx, t.Id, edit)
		if err != nil {

			priReport(inv, level, written)
			return inv.fail(err)
		}
		written = append(written, updated)
		inv.recordTask(updated, true, false)
	}
	priReport(inv, level, written)
	return exitOK
}

func priReport(inv *invocation, level model.Priority, written []model.Task) {
	switch len(written) {
	case 0:
	case 1:
		fmt.Fprintln(inv.stdout, cli.ReportLine("", written[0].Title, ": priority set to "+level.String(), nil))
	default:
		fmt.Fprintf(inv.stdout, "priority set to %s on %d tasks\n", level, len(written))
	}
}
