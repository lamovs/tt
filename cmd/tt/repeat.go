package main

import (
	"fmt"

	"github.com/movsar/tt/internal/cli"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "repeat",
			Summary: "set or clear a task's repeat rule",
			Examples: []cli.Example{
				{Cmd: "tt repeat 3 weekly", What: "repeat every week"},
				{Cmd: `tt repeat 3 'RRULE:FREQ=WEEKLY;INTERVAL=2'`, What: "pass a raw rule unchanged"},
				{Cmd: "tt repeat 3 none", What: "clear the repeat rule"},
			},
			Sections: []cli.HelpSection{
				{Title: "Input", Items: []string{
					"TASK must be one shell argument: a number, ID or quoted title query.",
					"Built-in aliases use INTERVAL=1. Raw RRULE values must be nonempty and contain no control characters; the server validates their grammar.",
					"Quote raw rules that contain semicolons.",
				}},
				{Title: "Provider behavior", Items: []string{
					"tt does not set repeatFrom. Recurrence therefore uses existing server state and server defaults.",
				}},
			},
			SeeAlso: []string{"show", "remind", "item", "undo"},
		},
		run: cmdRepeat,
	})
}

func cmdRepeat(inv *invocation) int {
	if len(inv.refinements) != 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}
	if len(inv.data) != 2 {
		return inv.misuse("expected TASK and one repeat rule")
	}
	repeat, err := parseRepeatRule(inv.data[1])
	if err != nil {
		return inv.misuse("%v", err)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	taskID, code, ok := inv.oneFeatureTask(st, inv.data[0])
	if !ok {
		return code
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	outcome, err := st.SetTaskRepeatOutcome(inv.ctx, taskID, repeat)
	if err != nil {
		return inv.fail(err)
	}
	action := ": repeat set"
	inv.recordTask(outcome.Task, outcome.Changed, false)
	if repeat == "" {
		action = ": repeat cleared"
	}
	if !outcome.Changed {
		action = ": repeat unchanged"
	}
	fmt.Fprintln(inv.stdout, cli.ReportLine("", outcome.Task.Title, action, nil))
	return exitOK
}
