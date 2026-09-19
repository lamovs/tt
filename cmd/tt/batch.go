package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/editor"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/tasklist"
)

var batchNow = time.Now

// batchApplyPlan creates the tasks of an accepted batch. Tests replace it to
// stop a run in the middle of applying.
var batchApplyPlan = app.ApplyBatchPlan

func init() {
	register(command{
		help: cli.Help{
			Verb:    "batch",
			Summary: "write many tasks in your editor and create them as one change",
			Examples: []cli.Example{
				{Cmd: "tt batch", What: "write the document in VISUAL or EDITOR, review it, confirm it"},
				{Cmd: "tt batch -P work", What: "put the tasks written before the first heading in that list"},
				{Cmd: "tt batch --preview", What: "only preview; prints the ID that accepts it"},
				{Cmd: "tt batch --accept PREVIEW_ID", What: "create a batch you reviewed"},
				{Cmd: "tt batch -y", What: "create without a question"},
			},
			Sections: []cli.HelpSection{
				{Title: "The document", Items: []string{
					"One line is one task. A heading line without indentation, # Name, puts the tasks below it in that list; the tasks above the first heading go to -P, or to default_project when -P is absent.",
					"An indented line belongs to the task its block starts with, the nearest line above it without indentation. With [ ] or [x] in front it is a checklist item of that task, and [x] marks the item done. A [x] on a task line creates the task and completes it at once.",
					"Any other indented line is a child task of that same task: tt creates it on its own and then hangs it under its parent. That relationship is sent after the parent reaches the server; if the parent never does, the child stays a task of its own until tt sync --retry-failed.",
					"A task marked [x] may not have child tasks of its own: a child can only be hung under an open task. Remove the [x], or write the children as tasks of their own; tt refuses the document and keeps your draft.",
					"Blank lines and lines starting with // are ignored, so the hints tt prefills cost nothing. Leaving the document unchanged or empty creates nothing.",
					"Unknown list names are refused, never created: a list has no ID before tt sync, so it cannot be written to in the same batch.",
				}},
				{Title: "Review", Items: []string{
					"In a terminal tt shows the whole document as tt read it and asks before creating anything. --preview only saves a preview; -y creates without asking.",
					"--accept ID applies a saved preview once, within 24 hours. Without a terminal, pass --preview or -y; tt refuses rather than ask nobody.",
					"The tasks of one batch are one undo group: a single tt undo removes all of them. If one task fails while applying, every task of the batch is removed at once, unless a change of another run was recorded in between; tt says then how many are still there.",
					"Applying is not one transaction: the tasks are created one after another, so a kill in the middle leaves the ones created before it. They are in the group all the same, and one tt undo removes whatever part of the batch is there.",
					"A document tt cannot use leaves the editor draft on disk and names the file, so nothing you typed is lost.",
				}},
			},
			SeeAlso: []string{"add", "item", "project", "undo", "sync"},
		},
		run: cmdBatch,
	})
}

type batchArgs struct {
	project    string
	hasProject bool
	preview    bool
	yes        bool
	accept     string
	hasAccept  bool
}

func parseBatchArgs(inv *invocation) (batchArgs, int, bool) {
	var out batchArgs
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		arg := inv.refinements[i]
		value := arg == "-P" || arg == "--accept"
		switch arg {
		case "-P", "--accept", "--preview", "-y":
		default:
			return out, inv.misuse("unknown option; the options are -P, --preview, --accept and -y"), false
		}
		if seen[arg] {
			return out, inv.misuse("%s may be given only once", arg), false
		}
		seen[arg] = true
		given := ""
		if value {
			if i+1 >= len(inv.refinements) || strings.HasPrefix(inv.refinements[i+1], "-") {
				return out, inv.misuse("%s needs a value", arg), false
			}
			i++
			given = inv.refinements[i]
		}
		switch arg {
		case "-P":
			out.project, out.hasProject = given, true
		case "--accept":
			out.accept, out.hasAccept = given, true
		case "--preview":
			out.preview = true
		case "-y":
			out.yes = true
		}
	}
	return out, exitOK, true
}

func cmdBatch(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("--raw is read-only")
	}
	args, code, ok := parseBatchArgs(inv)
	if !ok {
		return code
	}
	if len(inv.data) != 0 {
		return inv.misuse("takes no words: the tasks come from the document it opens in your editor")
	}
	if args.hasAccept {
		if args.preview || args.yes || args.hasProject {
			return inv.misuse("--accept applies a saved preview and takes nothing else")
		}
		if inv.interrupted() {
			return exitInterrupted
		}
		st, err := inv.openStore()
		if err != nil {
			return inv.fail(err)
		}
		defer st.Close()
		return batchAccept(inv, st, args.accept, false, nil)
	}
	if args.preview && args.yes {
		return inv.misuse("choose --preview or -y, not both")
	}
	if inv.jsonOutput {
		return inv.jsonFailure("unsupported_mode",
			"an external editor requires interactive output; tt batch --accept ID applies a saved preview", exitUsage)
	}
	if !args.preview && !args.yes && !canAsk(inv) {
		return inv.misuse("without a terminal there is nobody to ask: pass --preview to save a preview, or -y to create the tasks")
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	if err := app.PruneBatchPreviews(inv.ctx, st, batchNow()); err != nil {
		return inv.fail(err)
	}
	// The list -P names is resolved before the editor opens, so a name that
	// matches nothing is refused before anything is typed.
	var chosen model.Project
	if args.hasProject {
		chosen, err = inv.resolver(st).Project(inv.ctx, args.project)
		if err != nil {
			return inv.fail(err)
		}
	}

	draft, err := editor.Open(inv.ctx, []byte(tasklist.Template()), inv.stdin, inv.stdout, inv.stderr)
	if err != nil {
		return inv.editorFailure(draft, err)
	}
	if !draft.Changed {
		return inv.editorDone(draft, "unchanged; no tasks created")
	}
	if strings.TrimSpace(string(draft.Data)) == "" {
		return inv.editorDone(draft, "the document is empty; no tasks created")
	}
	doc, err := tasklist.Parse(string(draft.Data))
	if err != nil {
		return inv.batchDocumentFailure(draft, err)
	}
	if doc.CountTasks() == 0 {
		return inv.editorDone(draft, "the document holds no tasks; nothing was created")
	}
	if !args.hasProject && batchNeedsDefaultList(doc) {
		chosen, err = batchDefaultList(inv, st)
		if err != nil {
			return inv.editorFailure(draft, err)
		}
	}
	// The clock starts once the document is read, not when tt was run: the
	// hours a preview may be accepted in are the reader's to spend after the
	// editor, and a session longer than that would else leave a preview that
	// expired while it was being written.
	now := batchNow()
	plan, err := app.PlanBatch(inv.ctx, st, doc, chosen, now)
	if err != nil {
		return inv.batchDocumentFailure(draft, err)
	}
	id, err := app.SaveBatchPreview(inv.ctx, st, plan, now)
	if err != nil {
		return inv.editorFailure(draft, err)
	}

	preview := batchPreviewLines(plan)
	if args.preview {
		batchDropDraft(inv, draft)
		cli.WriteLines(inv.stdout, preview)
		cli.WriteLines(inv.stdout, batchAcceptLines(id))
		return exitOK
	}
	if args.yes {
		cli.WriteLines(inv.stdout, preview)
		return batchAccept(inv, st, id, true, draft)
	}
	cli.WriteLines(inv.stderr, preview)
	question := fmt.Sprintf("create these %d tasks together? [y/N] ", len(plan.Steps))
	if len(plan.Steps) == 1 {
		question = "create this task? [y/N] "
	}
	fmt.Fprint(inv.stderr, question)
	if !confirm(inv.stdin) {
		batchDropDraft(inv, draft)
		cli.WriteLines(inv.stderr, append([]string{"nothing was created"}, batchAcceptLines(id)...))
		return exitOK
	}
	return batchAccept(inv, st, id, true, draft)
}

// batchDropDraft removes the editor draft once the document it holds is kept
// somewhere else: in a preview the reader was given the ID of, or in the
// tasks the batch created. It names the file when it cannot remove it.
func batchDropDraft(inv *invocation, draft *editor.Draft) {
	if draft == nil {
		return
	}
	if err := draft.Discard(); err != nil {
		inv.editorDraftPath("could not remove editor draft:", draft.Path)
	}
}

// batchDefaultList reads the list default_project names, the way tt add picks
// one without -P.
func batchDefaultList(inv *invocation, st *store.Store) (model.Project, error) {
	cfg, err := inv.config()
	if err != nil {
		return model.Project{}, err
	}
	return inv.resolver(st).DefaultProject(inv.ctx, cfg.DefaultProject)
}

// batchNeedsDefaultList reports whether doc has tasks written before its first
// list heading, which are the only ones a default list is needed for.
func batchNeedsDefaultList(doc tasklist.Document) bool {
	for _, group := range doc.Groups {
		if strings.TrimSpace(group.List) == "" && len(group.Tasks) != 0 {
			return true
		}
	}
	return false
}

// batchDocumentFailure reports a document tt could not read or could not
// resolve against the cache, and keeps the draft so the text is not lost.
func (inv *invocation) batchDocumentFailure(draft *editor.Draft, err error) int {
	if inv.jsonOutput {
		return inv.jsonFailure("invalid_document", "the document could not be used: "+err.Error(), exitError)
	}
	// A refusal that names lists of the cache reads as its own lines already,
	// every name escaped and the widths kept, so it goes out the way every
	// other command reports a list nothing matches.
	var plan *app.BatchPlanError
	var code int
	if errors.As(err, &plan) && plan.Rendered {
		code = inv.fail(err)
	} else {
		code = inv.fail(batchError("the document could not be used; nothing was created"))
		lines := []string{"why:"}
		lines = append(lines, aiFold("  ", fixValue(err.Error()))...)
		cli.WriteLines(inv.stderr, lines)
	}
	if draft != nil {
		inv.editorDraftPath("draft kept at", draft.Path)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	return code
}

// batchAccept claims the preview id and applies it. own is set when this run
// also made the preview: its ID has not been shown then. draft is the editor
// draft of that same run, still on disk until the preview is claimed, and nil
// for a preview accepted by a run of its own.
func batchAccept(inv *invocation, st *store.Store, id string, own bool, draft *editor.Draft) int {
	if inv.interrupted() {
		return exitInterrupted
	}
	plan, text, err := app.ClaimBatchPreview(inv.ctx, st, id, batchNow())
	if err != nil {
		// The document is nowhere but in the draft while the preview it was
		// saved as has not been taken out of the cache, so a claim that never
		// got it leaves the file and names it. A preview that went stale
		// between the question and the answer is the one way the run that
		// wrote the document reaches this with a draft in hand.
		var code int
		if errors.Is(err, app.ErrBatchPreviewInvalid) || errors.Is(err, app.ErrBatchPreviewUnavailable) {
			code = inv.misuse("%v", err)
		} else {
			code = inv.fail(err)
		}
		if draft != nil {
			inv.editorDraftPath("draft kept at", draft.Path)
		}
		return code
	}
	// The plan is in hand, so the document is no longer only in the draft.
	batchDropDraft(inv, draft)

	applied, err := batchApplyPlan(inv.ctx, st, plan)
	var failed *app.BatchApplyError
	switch {
	case err == nil:
	case errors.As(err, &failed):
		// A batch removed whole can be accepted again; one left partly
		// created must not be.
		f := batchFailure{BatchApplyError: failed, id: id, own: own}
		if failed.Rollback == nil {
			f.keepErr = app.RestoreBatchPreview(context.WithoutCancel(inv.ctx), st, id, text)
		}
		return batchApplyFailure(inv, f)
	default:
		// The batch stopped before its first task, so the preview goes back.
		keepErr := app.RestoreBatchPreview(context.WithoutCancel(inv.ctx), st, id, text)
		if inv.jsonOutput {
			inv.resultData = map[string]any{"id": id, "preview_kept": keepErr == nil}
			return inv.jsonFailure("apply_failed", "nothing was created: "+err.Error(), exitError)
		}
		code := inv.fail(batchError("nothing was created"))
		lines := []string{"why:"}
		lines = append(lines, aiFold("  ", fixValue(batchReason(err)))...)
		switch {
		case keepErr != nil:
			lines = append(lines, "the preview could not be kept, so it cannot be accepted again:")
			lines = append(lines, aiFold("  ", fixValue(keepErr.Error()))...)
		case own:
			lines = append(lines, batchKeptLines(id)...)
		}
		cli.WriteLines(inv.stderr, lines)
		if inv.interrupted() {
			return exitInterrupted
		}
		return code
	}

	inv.resultData = map[string]any{"id": id, "group_id": applied.GroupID, "tasks": applied.Results}
	lines := []string{fmt.Sprintf(`created %d tasks together; one "tt undo" reverses them all, "tt sync" sends them`, len(applied.Results))}
	if len(applied.Results) == 1 {
		lines[0] = `created 1 task; "tt undo" reverses it, "tt sync" sends it`
	}
	for i, result := range applied.Results {
		after := ""
		if plan.Steps[i].Complete {
			after = " (done)"
		}
		lines = append(lines, cli.ReportLine("  ", result.Task.Title, after, nil))
	}
	cli.WriteLines(inv.stdout, lines)
	return exitOK
}

// batchFailure is a batch that failed while applying, and what became of its
// preview: keepErr is why a batch removed whole could not put it back.
type batchFailure struct {
	*app.BatchApplyError
	id      string
	own     bool
	keepErr error
}

func (f batchFailure) kept() bool { return f.Rollback == nil && f.keepErr == nil }

// batchApplyFailure reports a batch that failed while applying: how far
// removing the tasks created before the failure got, and what became of the
// preview.
func batchApplyFailure(inv *invocation, f batchFailure) int {
	// The whole batch is taken back, not only the part of it that came
	// before the failure: a batch completes the tasks its document marked
	// done once every task is there, so a failure in that pass has tasks
	// created after the one that failed to remove as well.
	lead := fmt.Sprintf("task %d of the batch failed; every task of the batch was removed, so nothing is left", f.Step)
	var blocked *app.BatchRollbackBlockedError
	switch {
	case f.Rollback == nil:
	case errors.As(f.Rollback, &blocked):
		lead = fmt.Sprintf("task %d of the batch failed, and removing the tasks of the batch stopped: "+
			"another change was recorded in between, so %s", f.Step, batchStillThere(blocked.Remaining))
	default:
		lead = fmt.Sprintf("task %d of the batch failed, and removing the tasks of the batch failed too; "+
			"run tt undo to reverse what is left", f.Step)
	}
	if inv.jsonOutput {
		inv.resultData = map[string]any{"step": f.Step, "line": f.Line, "rolled_back": f.Rollback == nil,
			"id": f.id, "preview_kept": f.kept()}
		message := lead + ": " + f.Error()
		if f.Rollback != nil {
			message += "; removing them failed: " + f.Rollback.Error()
		}
		if f.keepErr != nil {
			message += "; the preview could not be kept: " + f.keepErr.Error()
		}
		return inv.jsonFailure("apply_failed", message, exitError)
	}
	code := inv.fail(batchError(lead))
	lines := []string{"why:"}
	lines = append(lines, aiFold("  ", fixValue(batchReason(f.Err)))...)
	if f.Rollback != nil {
		lines = append(lines, "why removing them failed:")
		lines = append(lines, aiFold("  ", fixValue(f.Rollback.Error()))...)
	}
	if f.keepErr != nil {
		lines = append(lines, "the preview could not be kept, so it cannot be accepted again:")
		lines = append(lines, aiFold("  ", fixValue(f.keepErr.Error()))...)
	}
	if f.kept() && f.own {
		lines = append(lines, batchKeptLines(f.id)...)
	}
	cli.WriteLines(inv.stderr, lines)
	if inv.interrupted() {
		return exitInterrupted
	}
	return code
}

func batchStillThere(n int) string {
	if n == 1 {
		return "1 task of this batch is still there"
	}
	return fmt.Sprintf("%d tasks of this batch are still there", n)
}

func batchReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "tt was interrupted"
	case errors.Is(err, store.ErrTaskChanged):
		return "a task of this batch changed while it was being created"
	}
	return err.Error()
}

// batchError folds a sentence tt wrote itself, never text from the document or
// the cache, to the width left after the "tt: batch: " prefix inv.fail puts in
// front.
func batchError(sentence string) error {
	return errors.New(strings.Join(cli.Wrap(sentence, cli.Width-len("tt: batch: ")), "\n"))
}

// The ID goes on a line of its own: with the verb in front of it the command
// would be wider than the report.
func batchAcceptLines(id string) []string {
	return []string{"accept this preview with tt batch --accept and this ID:", id}
}

func batchKeptLines(id string) []string {
	return []string{"the preview is kept; accept it again with tt batch --accept and this ID:", id}
}

const batchDetail = "     "

// batchPreviewLines reads the plan back as the document it came from: what
// every task is called, where it goes, and what hangs under it.
func batchPreviewLines(plan app.BatchPlan) []string {
	head := fmt.Sprintf(`%s in %s, %s; created together and reversed together by one "tt undo":`,
		batchCount(len(plan.Steps), "task", "tasks"),
		batchCount(len(plan.Lists()), "list", "lists"),
		batchCount(plan.CountItems(), "checklist item", "checklist items"))
	lines := cli.Wrap(head, cli.Width)
	for i, step := range plan.Steps {
		number := fmt.Sprintf("%2d. ", i+1)
		after := ""
		if step.Complete {
			after = " (done)"
		}
		lines = append(lines, cli.ReportLine(number+"add ", step.Task.Title, after, nil))
		if step.Parent >= 0 {
			lines = append(lines, fmt.Sprintf("%sunder task %d", batchDetail, step.Parent+1))
		}
		lines = append(lines, cli.ReportLine(batchDetail+"to list ", step.List, "", nil))
		for _, item := range step.Task.Items {
			label := "item "
			if item.Status.Done() {
				label = "item (done) "
			}
			lines = append(lines, cli.ReportLine(batchDetail+label, item.Title, "", nil))
		}
	}
	return lines
}

func batchCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
