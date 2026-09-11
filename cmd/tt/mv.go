package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "mv",
			Summary: "move a task to another list",
			Examples: []cli.Example{
				{Cmd: "tt mv 3 Work --preview", What: "preview one native move with unchanged task ID"},
				{Cmd: "tt mv --accept PREVIEW_ID", What: "queue the exact reviewed native move"},
				{Cmd: "tt mv passport Errands", What: "name the task by a word out of its title instead"},
				{Cmd: "tt mv passport Office Hub", What: "a list whose name is two words needs no quoting"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task and destination", Items: []string{
					"The destination list comes last; everything before it identifies the task. tt uses the longest trailing words that match one of your lists.",
					"Use id:PROJECT_ID to select one exact destination.",
				}},
				{Title: "Native move", Items: []string{
					"A native move previews one synchronized task at a time. --accept queues the reviewed move locally; sync confirms the unchanged task ID by read-back.",
					"Pending task edits and unverified parent, child or column semantics refuse the move. Global undo checks whether it was sent.",
				}},
				{Title: "Legacy fallback", Items: []string{
					"Only explicit --recreate uses the legacy copy/delete workflow and sync.move_by_recreate setting.",
					"That fallback creates a new ID, loses comments, attachments and history, and has no move undo.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, press m to preview a move, then Enter to confirm it.",
				}},
			},
			SeeAlso: []string{"ls", "config", "ui"},
		},
		run: cmdMv,
	})
}

func cmdMv(inv *invocation) int {
	if !legacyMoveRequested(inv) {
		for _, flag := range inv.refinements {
			if flag == "--preview" || flag == "--accept" {
				return cmdNativeMv(inv)
			}
		}
		if len(inv.refinements) == 0 {
			return cmdNativeMv(inv)
		}
	} else {
		copy := *inv
		copy.refinements = nil
		removed := false
		for _, flag := range inv.refinements {
			if flag == "--recreate" && !removed {
				removed = true
				continue
			}
			copy.refinements = append(copy.refinements, flag)
		}
		inv = &copy
	}
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}

	if len(inv.data) < 2 {
		return inv.misuse("name a task and the list to move it to, as in \"tt mv 3 Work\"")
	}

	cfg, err := inv.config()
	if err != nil {
		return inv.fail(err)
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	r := inv.resolver(st)
	var project model.Project
	var projectErr error
	namesAList := func(query string) error {
		p, err := r.Project(inv.ctx, query)
		var unknown *cli.NoMatchError
		if errors.As(err, &unknown) {

			return err
		}

		project, projectErr = p, err
		return nil
	}
	ids, _, code, ok := inv.resolveTasksAndValue(st, inv.data, store.StatusOpen, "list", namesAList)
	if !ok {
		return code
	}

	if inv.interrupted() {
		return exitInterrupted
	}

	if projectErr != nil {
		return inv.fail(projectErr)
	}

	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		t, err := st.Task(inv.ctx, id)
		if err != nil {

			return inv.fail(err)
		}
		tasks = append(tasks, t)
	}

	recreate := cfg.Sync.MoveByRecreate.Recreates()
	var moved []model.Task
	for i, t := range tasks {

		if inv.interrupted() {
			moveReport(inv, st, moved)
			return exitInterrupted
		}
		got, err := st.MoveTask(inv.ctx, t.Id, project.Id, store.MoveOptions{ByRecreate: recreate})
		if errors.Is(err, store.ErrMoveNeedsRecreate) {

			granted, cerr := moveConsent(inv, cfg.Sync.MoveByRecreate, canAsk(inv), tasks[i:], project)
			if cerr != nil {
				moveReport(inv, st, moved)
				return inv.fail(cerr)
			}
			if !granted {

				moveReport(inv, st, moved)

				if len(moved) == 0 {
					fmt.Fprint(inv.stderr, "nothing moved\n")
				}
				return exitOK
			}
			recreate = true

			if inv.interrupted() {
				moveReport(inv, st, moved)

				fmt.Fprintln(inv.stderr, cli.ReportLine(
					fmt.Sprintf("tt: %s: interrupted, ", inv.verb), t.Title, " was left where it is", nil))
				return exitInterrupted
			}
			got, err = st.MoveTask(inv.ctx, t.Id, project.Id, store.MoveOptions{ByRecreate: true})
		}
		if err != nil {

			moveReport(inv, st, moved)
			return inv.fail(moveFailure(t, project, err))
		}
		moved = append(moved, got)
		inv.recordTask(got, got.Id != t.Id || got.ProjectId != t.ProjectId, false)
	}

	if err := moveListing(inv, st, moved); err != nil {
		return inv.fail(err)
	}
	return exitOK
}

func moveConsent(inv *invocation, mode config.MoveByRecreate, ask bool, pending []model.Task, project model.Project) (bool, error) {
	if mode == config.MoveNever {
		return false, errors.New(mode.Refusal())
	}
	if !ask {
		return false, errors.New(movePromptImpossible)
	}

	notice := fmt.Sprintf("Moving %s to %s recreates it: the copy is a new task with an id of its own, "+
		"and the comments, attachments and history of the original do not come across. "+
		"Nothing puts a move back afterwards, undo included.",
		moveName(pending[0].Title, cli.Width), moveName(project.Name, cli.Width))
	question := "move it? [y/N] "
	if len(pending) > 1 {
		notice = fmt.Sprintf("Moving %d tasks to %s recreates each of them: every copy is a new task with "+
			"an id of its own, and the comments, attachments and history of the originals do not come "+
			"across. Nothing puts a move back afterwards, undo included.",
			len(pending), moveName(project.Name, cli.Width))
		question = "move them? [y/N] "
	}

	if err := cli.WriteLines(inv.stderr, cli.Wrap(notice, cli.Width)); err != nil {
		return false, err
	}
	fmt.Fprint(inv.stderr, question)
	return confirm(inv.stdin), nil
}

var movePromptImpossible = moveParagraph("moving a task to another list is not something tt does on its own.") +
	"\n" + config.MoveByRecreateNotice + "\n" +
	moveParagraph("sync.move_by_recreate is 'ask' and stdin or stderr is not a terminal, so there is "+
		"nobody to put the question to; set the key to 'always' to let tt make the move unasked.")

const moveMessageWidth = cli.Width - len("tt: mv: ")

func moveParagraph(s string) string {
	return strings.Join(cli.Wrap(s, moveMessageWidth), "\n")
}

func moveName(s string, w int) string {
	return cli.ReportTitle(s, w-1)
}

func moveFailure(t model.Task, project model.Project, err error) error {
	name := moveName(t.Title, moveMessageWidth)
	switch {
	case errors.Is(err, store.ErrMoveCompleted):
		return errors.New(moveParagraph(fmt.Sprintf("%s is done, and a completed task cannot be moved "+
			"to another list: the copy would come back open, since the server ignores the status a "+
			"create carries", name)))
	case errors.Is(err, store.ErrMoveChained):
		return errors.New(moveParagraph(fmt.Sprintf("%s is itself the copy of a move that has not been "+
			"sent yet; run \"tt sync\" and move it again once the copy is on the server", name)))
	case errors.Is(err, store.ErrNotFound):
		return errors.New(moveParagraph(fmt.Sprintf("%s is no longer in the local cache", name)))
	}

	return &moveError{
		text: moveParagraph(fmt.Sprintf("moving %s to %s: %v",
			name, moveName(project.Name, moveMessageWidth), err)),
		err: err,
	}
}

type moveError struct {
	text string
	err  error
}

func (e *moveError) Error() string { return e.text }
func (e *moveError) Unwrap() error { return e.err }

func moveReport(inv *invocation, st *store.Store, moved []model.Task) {
	if len(moved) == 0 {
		return
	}
	if err := moveListing(inv, st, moved); err != nil {
		fmt.Fprintf(inv.stderr, "tt: %s: %v\n", inv.verb, err)
	}
}

func moveListing(inv *invocation, st *store.Store, moved []model.Task) error {
	report := *inv
	report.ctx = context.WithoutCancel(inv.ctx)
	return report.listTasks(st, moved)
}
