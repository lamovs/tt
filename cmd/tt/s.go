package main

import (
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "s",
			Summary: "search titles, descriptions, tags and lists",
			Examples: []cli.Example{
				{Cmd: "tt s posylku", What: "open tasks whose title, description, tags or list mention posylku"},
				{Cmd: "tt s send the parcel -a", What: "the same search, finished tasks included"},
				{Cmd: "tt s parcel --remote", What: "fetch a separate read-only server search"},
				{Cmd: "tt s parcel --server", What: "read that exact cached server search offline"},
			},
			Sections: []cli.HelpSection{
				{Title: "Local search", Items: []string{
					"Words are matched together, in order, as one phrase. Matching covers title, description, tags and list name with case folding across alphabets.",
					"Place -a after the search words to include completed tasks.",
					"Results become the current numbered listing, so tt s posylku followed by tt done 1 acts on the first result.",
				}},
				{Title: "Server search", Items: []string{
					"--remote fetches a separate server search. --server reads only that exact cached query.",
					"Server rows are read-only and never replace local tasks or numbered references.",
					"Server search accepts comma-separated --project IDs, --tag and --status 0,2. --from and --to filter due time and require RFC3339 timestamps with offsets.",
					"Server keyword semantics and coverage may differ from local search.",
				}},
			},
			SeeAlso: []string{"ls", "show", "done"},
		},
		run: cmdSearch,
	})
}

func cmdSearch(inv *invocation) int {
	if hasServerTaskQuery(inv) {
		return inv.serverTaskQuery()
	}

	phrase := strings.Join(inv.data, " ")

	if strings.TrimSpace(phrase) == "" {
		if len(inv.refinements) > 0 {

			return inv.misuse("the words come before the options: tt s <words> -a")
		}
		return inv.misuse("search needs at least one word")
	}
	all := false
	for _, r := range inv.refinements {
		if r != "-a" {
			return inv.misuseWord("unknown option ", r)
		}
		all = true
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	status := store.StatusOpen
	if all {
		status = store.StatusAll
	}
	tasks, err := st.Tasks(inv.ctx, store.TaskFilter{Status: status, Search: phrase})
	if err != nil {
		return inv.fail(err)
	}
	if err := inv.listTasks(st, tasks); err != nil {
		return inv.fail(err)
	}
	if len(tasks) == 0 {

		fmt.Fprintln(inv.stdout, quoteWord("no tasks match ", phrase))
	}
	return exitOK
}
