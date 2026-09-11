package main

import (
	"context"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

func defaultFocusVerdict(ref config.FocusReference) (string, []string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	inspection, err := inspectFocusCache(ctx)
	if err != nil {
		return "default_focus not checked", doctorNote("The cache could not be inspected. This does not confirm server removal. Run tt sync, then choose with tt config default-focus."), true
	}
	defer inspection.Close()
	task, err := inspection.Store.DefaultFocusTask(ctx, ref.TaskID)
	if err != nil {
		return "default_focus is unavailable in the cache", doctorNote(cli.Foreign(err.Error(), 72) + ". The cache may be empty or stale; server removal is not confirmed. Use tt config default-focus task:ID or none."), true
	}
	return "default_focus matches one open task by ID", doctorNote("Cached task: " + cli.Foreign(task.Title, 48) + " [" + cli.Foreign(ref.String(), len(ref.String())*8+2) + "]. Server availability is not verified. New default starts use this ID; existing sessions and uploads keep their destinations."), false
}
