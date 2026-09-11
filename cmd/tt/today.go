package main

import (
	"fmt"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "today",
			Summary: "what is due today, plus anything overdue",
			Examples: []cli.Example{
				{Cmd: "tt today", What: "every open task due today or earlier"},
			},
			Sections: []cli.HelpSection{
				{Title: "Ordering", Items: []string{
					"Overdue tasks appear first, followed by tasks due today. The oldest due date is shown first.",
				}},
			},
			SeeAlso: []string{"ls", "due", "show"},
		},
		run: cmdToday,
	})
}

var todayNow = time.Now

func cmdToday(inv *invocation) int {
	if len(inv.data) > 0 {
		return inv.misuseWord("unexpected argument ", inv.data[0])
	}
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

	now := todayNow()

	tomorrow, err := dates.Parse("tmr", now)
	if err != nil {
		return inv.fail(err)
	}

	tasks, err := st.Tasks(inv.ctx, store.TaskFilter{
		Status: store.StatusOpen,
		DueTo:  tomorrow.Time,
		Order:  store.OrderDue,
	})
	if err != nil {
		return inv.fail(err)
	}

	if err := inv.listTasksAt(st, tasks, now); err != nil {
		return inv.fail(err)
	}
	if len(tasks) == 0 {

		fmt.Fprintln(inv.stdout, "nothing due today, and nothing overdue")
	}
	return exitOK
}
