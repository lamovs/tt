package main

import (
	"fmt"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "ls",
			Summary: "show every open task, from every list",
			Examples: []cli.Example{
				{Cmd: "tt ls", What: "everything still open, soonest due date first"},
				{Cmd: "tt ls --completed --remote", What: "fetch a read-only server completed query"},
				{Cmd: "tt ls --filter --tag work --priority 0,5", What: "read an exact cached server filter"},
			},
			Sections: []cli.HelpSection{
				{Title: "Local listing", Items: []string{
					"Open tasks are sorted by due date; undated tasks appear last.",
					"The printed numbers are used by commands such as tt done 2 and tt due 2 fri. Each new local listing replaces the previous numbered references.",
				}},
				{Title: "Server queries", Items: []string{
					"--completed and --filter use separate read-only server-query caches. Add --remote to fetch the exact query from the server.",
					"Server-query rows never replace local tasks or numbered references.",
					"--project takes comma-separated exact project IDs. --from and --to require RFC3339 timestamps with offsets: completion time for --completed and start time for --filter.",
				}},
				{Title: "Filters and coverage", Items: []string{
					"--filter also accepts --tag, --priority 0,1,3,5, --status 0,2 and --kind TEXT,NOTE,CHECKLIST.",
					"Server coverage is unknown or partial at 200 rows; no pagination cursor has been verified.",
				}},
			},
			SeeAlso: []string{"today", "show", "due"},
		},
		run: cmdLs,
	})
}

func cmdLs(inv *invocation) int {
	if hasServerTaskQuery(inv) {
		return inv.serverTaskQuery()
	}
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}
	if len(inv.data) > 0 {
		return inv.misuse("tt ls takes no arguments")
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	tasks, err := st.Tasks(inv.ctx, store.TaskFilter{})
	if err != nil {
		return inv.fail(err)
	}
	if err := inv.listTasks(st, tasks); err != nil {
		return inv.fail(err)
	}
	if len(tasks) == 0 {

		fmt.Fprintln(inv.stdout, "no open tasks: everything is done, or nothing was ever added")
	}
	return exitOK
}
