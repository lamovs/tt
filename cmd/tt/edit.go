package main

import (
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/editor"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func init() {
	register(command{help: cli.Help{
		Verb: "edit", Summary: "edit a task or note in a Markdown editor",
		Examples: []cli.Example{
			{Cmd: "tt edit 3 -e markdown", What: "open a prefilled task in VISUAL or EDITOR"},
			{Cmd: "tt edit meeting notes", What: "find one task or note by title and edit it"},
			{Cmd: "tt edit 3 --estimated-duration 30m --estimated-pomo 2 --preview", What: "preview estimates without an editor"},
			{Cmd: "tt edit 3 --parent none --preview", What: "preview unlinking a parent"},
			{Cmd: "tt edit 3 --column COLUMN_ID --preview", What: "preview a same-project column assignment"},
			{Cmd: "tt edit --accept PREVIEW_ID", What: "apply the exact field preview locally"},
		},
		Sections: []cli.HelpSection{
			{Title: "Editor behavior", Items: []string{
				"Save and close the editor to apply supported fields and the exact Markdown body locally, then run tt sync.",
				"Cancellation and failed edits preserve the draft. An unchanged document does nothing.",
				"Existing kind, id, project_id and checklist items remain unchanged.",
			}},
			{Title: "Dates and overlaps", Items: []string{
				"Changing due on an open non-recurring interval preserves its start and time zone. The new timed end must be later than the start.",
				"Cached overlaps are reviewed before saving. Use --allow-overlap when no interactive terminal is available.",
			}},
			{Title: "Additional fields", Items: []string{
				"--parent TASK|none, --estimated-duration, --estimated-pomo 0..60 and --sort-order INT64 use exact preview and accept steps.",
				"--parent takes an open parent of the same list, and one whose own creation is still queued counts: tt add A, tt add B and tt edit B --parent A need no tt sync in between. Any other queued change of the parent chain is refused.",
				"A link to a parent whose creation does not go through is parked and returns to the queue with tt sync --retry-failed; the child stays a task of its own until it is sent.",
				"Durations use whole-second Go duration syntax. Sort order has a separate preview from the other extension fields.",
			}},
			{Title: "Kanban", Items: []string{
				"--column COLUMN_ID previews assignment to an existing column in the same project. List IDs with tt project column ls --project PROJECT_ID.",
				"Column assignment cannot be combined with other fields. It does not complete the task or change its ordering.",
				"The column write uses a live-verified but undocumented Official Open API field.",
			}},
			{Title: "TUI", Items: []string{
				"In tt ui, Shift+E opens the selected record in the Markdown editor. Ctrl+E opens the current basic-form draft.",
			}},
		},
		SeeAlso: []string{"add", "ui", "show", "sync", "undo"},
	}, run: cmdEdit})
}

func cmdEdit(inv *invocation) int {
	if hasTaskFieldOptions(inv) {
		return cmdTaskFields(inv)
	}
	args := append([]string(nil), inv.refinements...)
	allow := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--allow-overlap" && !allow {
			allow = true
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if len(args) != 0 && (len(args) != 2 || args[0] != "-e" || args[1] != "markdown") {
		return inv.misuse("expected -e markdown")
	}
	if len(inv.data) == 0 {
		return inv.misuse("no task given")
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	id, code, ok := inv.oneFeatureTask(st, strings.Join(inv.data, " "))
	if !ok {
		return code
	}
	original, err := st.Task(inv.ctx, id)
	if err != nil {
		return inv.fail(err)
	}
	draft, err := inv.openTaskEditor(original)
	if err != nil {
		return inv.editorFailure(draft, err)
	}
	if !draft.Changed {
		return inv.editorDone(draft, "unchanged")
	}
	_, edit, err := editedTaskDocument(original, draft.Data, false)
	if err != nil {
		return inv.editorFailure(draft, err)
	}
	var outcome store.TaskMutationOutcome
	if schedule.ReviewsInterval(original, edit) {
		outcome, err = inv.saveInterval(st, &original, edit, model.Task{}, intervalOptions{allow: allow})
	} else {
		outcome, err = st.UpdateTaskIfUnchanged(inv.ctx, original, edit)
	}
	if err != nil {
		return inv.editorFailure(draft, err)
	}
	if !outcome.Changed {
		return inv.editorDone(draft, "unchanged")
	}
	return inv.editorDone(draft, cli.ReportLine("edited ", outcome.Task.Title, "", nil))
}

func (inv *invocation) openTaskEditor(task model.Task) (*editor.Draft, error) {
	cfg, err := inv.config()
	if err != nil {
		return nil, err
	}
	initial, err := editorDocument(task, cfg.EditorHints)
	if err != nil {
		return nil, err
	}
	return editor.Open(inv.ctx, initial, inv.stdin, inv.stdout, inv.stderr)
}

func (inv *invocation) editorFailure(draft *editor.Draft, err error) int {
	prefix := "tt: " + inv.verb + ": "
	fmt.Fprintln(inv.stderr, prefix+cli.Foreign(err.Error(), cli.Width-len(prefix)))
	if draft != nil {
		inv.editorDraftPath("draft kept at", draft.Path)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	return exitError
}

func (inv *invocation) editorDone(draft *editor.Draft, message string) int {
	fmt.Fprintln(inv.stdout, message)
	if err := draft.Discard(); err != nil {
		inv.editorDraftPath("could not remove editor draft:", draft.Path)
	}
	return exitOK
}

func (inv *invocation) editorDraftPath(message, path string) {
	fmt.Fprintln(inv.stderr, message)

	fmt.Fprintln(inv.stderr, cli.Foreign(path, 4*len(path)+2))
}
