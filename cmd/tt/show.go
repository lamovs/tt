package main

import (
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "show",
			Summary: "print the full card of one task",
			Examples: []cli.Example{
				{Cmd: "tt show 3", What: "the card for task 3 from the last listing"},
				{Cmd: "tt show passport", What: "search for a task by title and show its card"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task selection", Items: []string{
					"show searches both open and completed tasks.",
					"It prints exactly one card and does not replace the current numbered listing.",
				}},
			},
			SeeAlso: []string{"ls", "done"},
		},
		run: cmdShow,
	})
}

func cmdShow(inv *invocation) int {
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	ids, code, ok := inv.resolveTasks(st, inv.data, store.StatusAll)
	if !ok {
		return code
	}

	if len(ids) > 1 {
		return inv.misuse(oneTaskOnly, len(ids))
	}

	t, err := st.Task(inv.ctx, ids[0])
	if err != nil {
		return inv.fail(err)
	}
	names, err := cli.ProjectNames(inv.ctx, st)
	if err != nil {
		return inv.fail(err)
	}
	if err := cli.WriteLines(inv.stdout, cli.CardLines(t, names[t.ProjectId], inv.outPalette(), time.Now())); err != nil {
		return inv.fail(err)
	}
	return exitOK
}
