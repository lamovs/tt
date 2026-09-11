package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{
		help: cli.Help{
			Verb:    "item",
			Summary: "edit a task's checklist",
			Examples: []cli.Example{
				{Cmd: `tt item 3 add "book hotel"`, What: "append an item"},
				{Cmd: `tt item 3 rename 2 "book train"`, What: "rename item 2"},
				{Cmd: "tt item 3 done 2", What: "complete item 2"},
				{Cmd: "tt item 3 undone 2", What: "reopen item 2"},
				{Cmd: "tt item 3 remove 2", What: "remove item 2"},
				{Cmd: "tt item 3 move 1 3", What: "move item 1 to final position 3"},
				{Cmd: `tt item 3 add -- "-call bank"`, What: "append a title beginning with a dash"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task and positions", Items: []string{
					"TASK must be one shell argument: a number, ID or quoted title query.",
					"Positions are 1-based. tt show prints every position, including completed items.",
				}},
				{Title: "Titles and task kinds", Items: []string{
					"For add and rename, all remaining words are joined with spaces to form the checklist title.",
					"NOTE tasks and unsupported task kinds are refused rather than converted.",
				}},
			},
			SeeAlso: []string{"show", "repeat", "remind", "undo"},
		},
		run: cmdItem,
	})
}

type itemRequest struct {
	task        string
	action      string
	position    int
	destination int
	title       string
}

func parseItemRequest(inv *invocation) (itemRequest, error) {
	if len(inv.refinements) != 0 {
		return itemRequest{}, addUnknownOption{opt: inv.refinements[0]}
	}
	if len(inv.data) < 2 {
		return itemRequest{}, itemShapeError()
	}
	req := itemRequest{task: inv.data[0], action: inv.data[1]}
	switch req.action {
	case "add":
		if len(inv.data) < 3 {
			return itemRequest{}, itemShapeError()
		}
		req.title = strings.TrimSpace(strings.Join(inv.data[2:], " "))
		if req.title == "" {
			return itemRequest{}, errors.New("add needs an item title")
		}
	case "rename":
		if len(inv.data) < 4 {
			return itemRequest{}, itemShapeError()
		}
		position, err := positivePosition(inv.data[2])
		if err != nil {
			return itemRequest{}, err
		}
		req.position = position
		req.title = strings.TrimSpace(strings.Join(inv.data[3:], " "))
		if req.title == "" {
			return itemRequest{}, errors.New("rename needs an item title")
		}
	case "done", "undone", "remove":
		if len(inv.data) != 3 {
			return itemRequest{}, itemShapeError()
		}
		position, err := positivePosition(inv.data[2])
		if err != nil {
			return itemRequest{}, err
		}
		req.position = position
	case "move":
		if len(inv.data) != 4 {
			return itemRequest{}, itemShapeError()
		}
		position, err := positivePosition(inv.data[2])
		if err != nil {
			return itemRequest{}, err
		}
		destination, err := positivePosition(inv.data[3])
		if err != nil {
			return itemRequest{}, err
		}
		req.position, req.destination = position, destination
	default:
		return itemRequest{}, &unknownItemAction{action: req.action}
	}
	return req, nil
}

type unknownItemAction struct {
	action string
}

func (*unknownItemAction) Error() string {
	return "unknown checklist action"
}

func itemShapeError() error {
	return errors.New("expected TASK and one of add, rename, done, undone, remove, or move with its arguments")
}

func cmdItem(inv *invocation) int {
	req, err := parseItemRequest(inv)
	if err != nil {
		var unknown addUnknownOption
		if errors.As(err, &unknown) {
			return inv.misuseWord("unknown option ", unknown.opt)
		}
		var action *unknownItemAction
		if errors.As(err, &action) {
			return inv.misuseWord("unknown action ", action.action)
		}
		var position *invalidItemPosition
		if errors.As(err, &position) {
			return inv.misuseWord("position must be a positive integer: ", position.value)
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
	taskID, code, ok := inv.oneFeatureTask(st, req.task)
	if !ok {
		return code
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	var outcome store.TaskMutationOutcome
	switch req.action {
	case "add":
		outcome, err = st.AddTaskItemOutcome(inv.ctx, taskID, req.title)
	case "rename":
		outcome, err = st.RenameTaskItemOutcome(inv.ctx, taskID, req.position, req.title)
	case "done":
		outcome, err = st.SetTaskItemDoneOutcome(inv.ctx, taskID, req.position, true)
	case "undone":
		outcome, err = st.SetTaskItemDoneOutcome(inv.ctx, taskID, req.position, false)
	case "remove":
		outcome, err = st.RemoveTaskItemOutcome(inv.ctx, taskID, req.position)
	case "move":
		outcome, err = st.MoveTaskItemOutcome(inv.ctx, taskID, req.position, req.destination)
	}
	if err != nil {
		return inv.fail(err)
	}
	action := ": checklist " + req.action + " applied"
	inv.recordTask(outcome.Task, outcome.Changed, false)
	if !outcome.Changed {
		action = ": checklist " + req.action + " unchanged"
	}
	fmt.Fprintln(inv.stdout, cli.ReportLine("", outcome.Task.Title, action, nil))
	return exitOK
}
