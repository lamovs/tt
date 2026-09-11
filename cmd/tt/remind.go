package main

import (
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/schedule"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "remind",
			Summary: "replace or clear a task's reminders",
			Examples: []cli.Example{
				{Cmd: "tt remind 3 at", What: "remind at the task's scheduled time"},
				{Cmd: "tt remind 3 -10min", What: "ten minutes before the scheduled time"},
				{Cmd: "tt remind 3 -1h at", What: "one hour before, then at the scheduled time"},
				{Cmd: "tt remind 3 none", What: "clear every reminder"},
				{Cmd: "tt remind 3 --explain --json", What: "inspect parsed offsets and local-delivery policy"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task and offsets", Items: []string{
					"TASK must be one shell argument: a number, ID or quoted title query.",
					"at means the task's scheduled time. Values such as -10min and -1h30min mean before that time, not delays from now.",
					"For a new task due and reminded in two hours, use tt add Call --in 2h. To move an existing task's due time, use tt due TASK in 2h.",
				}},
				{Title: "Replacement rules", Items: []string{
					"The supplied list replaces the complete reminder list and preserves order.",
					"Duplicate triggers are refused. none must stand alone and clears the list.",
				}},
				{Title: "Raw triggers", Items: []string{
					"Raw TRIGGER: and TRIGGER;RELATED=START|END: values pass through unchanged; quote them in the shell. at is stored as TRIGGER:PT0S.",
					"The parser accepts units from seconds through years. Calendar policy, all-day conventions and unspecified anchors may remain provider-defined.",
					"--explain only describes parsing and local delivery policy. Raw values must be nonempty and contain no control characters.",
				}},
				{Title: "Saving", Items: []string{
					"The change is local until tt sync. Saving it does not prove that a device notification was delivered.",
				}},
			},
			SeeAlso: []string{"add", "due", "repeat", "show", "undo"},
		},
		run: cmdRemind,
	})
}

func cmdRemind(inv *invocation) int {
	if len(inv.refinements) > 0 && inv.refinements[0] == "--explain" {
		return cmdExplainReminders(inv)
	}
	for _, value := range inv.refinements {
		if strings.HasPrefix(value, "-") {
			if _, err := schedule.ParseReminder(value); err != nil {
				return inv.misuseWord("unknown option ", value)
			}
		}
	}

	args := append(append([]string(nil), inv.data...), inv.refinements...)
	if len(inv.data) == 0 || len(args) < 2 {
		return inv.misuse("expected TASK and one or more reminders, or none")
	}
	values, err := schedule.ParseReminders(args[1:])
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
	outcome, err := st.SetTaskRemindersOutcome(inv.ctx, taskID, values)
	if err != nil {
		return inv.fail(err)
	}
	action := fmt.Sprintf(": %d reminders set", len(values))
	inv.recordTask(outcome.Task, outcome.Changed, false)
	if len(values) == 0 {
		action = ": reminders cleared"
	}
	if !outcome.Changed {
		action = ": reminders unchanged"
	}
	fmt.Fprintln(inv.stdout, cli.ReportLine("", outcome.Task.Title, action, nil))
	return exitOK
}

func validateReminderList(values []string) error { return schedule.ValidateReminders(values) }
