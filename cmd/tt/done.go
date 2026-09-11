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

func init() {
	register(command{
		help: cli.Help{
			Verb:    "done",
			Summary: "complete one or more open tasks",
			Examples: []cli.Example{
				{Cmd: "tt done 3", What: "complete the task numbered 3 in the last listing"},
				{Cmd: "tt done 1 3 5", What: "complete three tasks from the last listing at once"},
				{Cmd: "tt done groceries", What: "complete the one open task matching \"groceries\""},
				{Cmd: "tt done milk -A", What: "complete every open task matching \"milk\", no prompt"},
				{Cmd: "tt done 3 --keep-subs", What: "close the task but leave its checklist items open"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task selection", Items: []string{
					"Title searches consider open tasks only. A number or exact ID may still refer to an already completed task from a listing such as tt s -a.",
					"An already completed task is reported and left unchanged, including its original completion time.",
				}},
				{Title: "Checklist behavior", Items: []string{
					"Completing a task also completes its open checklist items. Use --keep-subs to leave those items open.",
				}},
				{Title: "Multiple matches", Items: []string{
					"A text search with several matches normally asks you to choose. -A skips that prompt and completes every match.",
				}},
			},
			SeeAlso: []string{"undo", "show"},
		},
		run: cmdDone,
	})
}

func cmdDone(inv *invocation) int {
	var all, keepSubs bool
	for _, r := range inv.refinements {
		switch r {
		case "-A":
			all = true
		case "--keep-subs":
			keepSubs = true
		default:
			return inv.misuseWord("unknown option ", r)
		}
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

	ids, code, ok := inv.taskOutcome(resolveDoneTasks(inv, st, all))
	if !ok {
		return code
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

	opts := store.CompleteOptions{KeepItems: keepSubs}
	for _, t := range tasks {
		if inv.interrupted() {
			return exitInterrupted
		}
		if t.Status.Done() {
			inv.recordTask(t, false, false)

			fmt.Fprintln(inv.stdout, cli.ReportLine("already done: ", t.Title, "", nil))
			continue
		}

		updated, line, err := completeOneResult(inv.ctx, st, t, opts)
		if err != nil {
			return inv.fail(err)
		}
		inv.recordTask(updated, true, false)
		fmt.Fprintln(inv.stdout, line)
	}
	return exitOK
}

func resolveDoneTasks(inv *invocation, st *store.Store, all bool) ([]string, error) {
	r := inv.resolver(st)
	if all {
		r.Ask = false
	}
	ids, err := r.Tasks(inv.ctx, inv.data, store.StatusOpen)
	if !all {
		return ids, err
	}
	var ambiguous *cli.AmbiguousError
	if !errors.As(err, &ambiguous) {
		return ids, err
	}
	return searchOpenTasks(inv.ctx, st, inv.data)
}

func searchOpenTasks(ctx context.Context, st *store.Store, args []string) ([]string, error) {
	var words []string
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			words = append(words, a)
		}
	}
	found, err := st.Tasks(ctx, store.TaskFilter{
		Search: strings.Join(words, " "),
		Status: store.StatusOpen,
	})
	if err != nil {
		return nil, err
	}
	return cli.TaskIDs(found), nil
}

func completeOne(ctx context.Context, st *store.Store, cur model.Task, opts store.CompleteOptions) (string, error) {
	_, line, err := completeOneResult(ctx, st, cur, opts)
	return line, err
}

func completeOneResult(ctx context.Context, st *store.Store, cur model.Task, opts store.CompleteOptions) (model.Task, string, error) {
	updated, err := st.CompleteTask(ctx, cur.Id, opts)
	if err != nil {
		return model.Task{}, "", err
	}
	closed := 0
	if !opts.KeepItems {
		closed = openItemCount(cur.Items)
	}
	if closed > 0 {
		return updated, cli.ReportLine("done: ", cur.Title, fmt.Sprintf(" (+%d subtasks)", closed), nil), nil
	}
	return updated, cli.ReportLine("done: ", cur.Title, "", nil), nil
}

func openItemCount(items []model.Item) int {
	n := 0
	for _, it := range items {
		if !it.Status.Done() {
			n++
		}
	}
	return n
}
