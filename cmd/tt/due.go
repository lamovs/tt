package main

import (
	"fmt"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "due",
			Summary: "set or clear a task's due date",
			Examples: []cli.Example{
				{Cmd: "tt due 1 in 2h", What: "due two elapsed hours from now"},
				{Cmd: "tt due 1 +2h", What: "the same shortcut with +"},
				{Cmd: "tt due 1 +30min", What: "due in half an hour"},
				{Cmd: "tt due 1 fri", What: "the task numbered 1 in the last listing, this Friday"},
				{Cmd: "tt due 1 fri 18:00", What: "with a time of day"},
				{Cmd: "tt due 1 21.10.2026", What: "an explicit day, month and year"},
				{Cmd: "tt due 1 none", What: "take the date off"},
			},
			Sections: []cli.HelpSection{
				{Title: "Accepted dates", Items: []string{
					"Everything after TASK is read as one date expression.",
					"Accepted forms include weekdays, next mon, 25sep, sep25, 25.09, 21.10.2026, ISO dates, +3d, +2w, +1m, eow, eom, today, tmr and yst.",
					"in 2h and +2h are equal elapsed delays, including across clock changes. Use min for minutes; +1m means one calendar month.",
					"A date without a time is all-day. Timed dates use the local timezone.",
				}},
				{Title: "Intervals and overlaps", Items: []string{
					"For an open non-recurring timed interval, due changes only its end while preserving the start and valid stored time zone.",
					"The new end must remain after the start; conversion to an all-day date is refused.",
					"--preview reviews without saving. Overlaps require terminal confirmation or --allow-overlap.",
				}},
				{Title: "Reminders and clearing", Items: []string{
					"due changes the due date, not reminders. Use tt remind TASK at to add a reminder at that time, then run tt sync.",
					"none clears both start and due dates because the API otherwise restores due from start.",
				}},
				{Title: "Task selection", Items: []string{
					"due changes one task at a time. A range such as 1-3 is invalid here, although tt pri accepts ranges.",
				}},
			},
			SeeAlso: []string{"schedule", "add", "remind", "show"},
		},
		run: cmdDue,
	})
}

func cmdDue(inv *invocation) int {
	for _, arg := range inv.refinements {
		if arg != "--preview" && arg != "--allow-overlap" {
			return inv.misuseWord("unknown option ", arg)
		}
	}
	opt, err := parseIntervalOptions(inv.refinements)
	if err != nil || opt.zone != "" {
		if err == nil {
			err = fmt.Errorf("--zone belongs to tt schedule")
		}
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

	ids, dateExpr, code, ok := inv.resolveTasksAndValue(st, inv.data, store.StatusOpen, "date",
		func(s string) error {
			_, err := dates.ParseNow(s)
			return err
		})
	if !ok {
		return code
	}
	if len(ids) > 1 {

		return inv.misuse(oneTaskOnly, len(ids))
	}

	result, err := dates.ParseNow(dateExpr)
	if err != nil {
		return inv.misuse("%v", err)
	}

	edit := schedule.DueEdit(result)

	if inv.interrupted() {
		return exitInterrupted
	}

	cur, err := st.Task(inv.ctx, ids[0])
	if err != nil {

		return inv.fail(err)
	}
	if dueUnchanged(cur, result) {
		inv.recordTask(cur, false, false)

		fmt.Fprintln(inv.stdout, cli.ReportLine("", cur.Title, dueAlready(cur, result), nil))
		return exitOK
	}

	edit, err = schedule.ProtectIntervalEnd(cur, edit)
	if err != nil {
		return inv.fail(err)
	}
	if schedule.ReviewsInterval(cur, edit) {
		_, err = inv.saveInterval(st, &cur, edit, model.Task{}, opt)
		if err != nil {
			return inv.fail(err)
		}
		return exitOK
	}
	if opt.preview {
		return inv.misuse("--preview applies to an existing timed interval; use tt schedule")
	}
	out, err := st.UpdateTaskIfUnchanged(inv.ctx, cur, edit)
	t := out.Task
	if err != nil {
		return inv.fail(err)
	}

	fmt.Fprintln(inv.stdout, cli.ReportLine("", t.Title, ": due "+dueOf(t), nil))
	inv.recordTask(t, out.Changed, false)
	return exitOK
}

func dueAlready(t model.Task, r dates.Result) string {
	if r.Clear {
		return ": already has no due date"
	}
	return ": due already " + dueOf(t)
}

func dueOf(t model.Task) string {
	return dates.FormatNow(t.DueDate, t.IsAllDay, dates.Zone(t.TimeZone))
}

func dueUnchanged(t model.Task, r dates.Result) bool { return schedule.DueUnchanged(t, r) }
