package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

// newAIRunner builds what an assistant command calls. Tests replace it so
// that no test ever reaches a model.
var newAIRunner = func(cfg config.AI) app.AIRunner { return ai.New(cfg) }

var aiNow = time.Now

// aiApplyPlan applies an accepted plan. Tests replace it to stop a run in the
// middle of applying.
var aiApplyPlan = app.ApplyAIPlan

func init() {
	register(command{
		help: cli.Help{
			Verb:    "ai",
			Summary: "say what to change in your own words, review it, apply it as one change",
			Examples: []cli.Example{
				{Cmd: `tt ai "buy milk tomorrow at 10"`, What: "preview what the model proposes, then confirm it"},
				{Cmd: `tt ai "move the report to Work" --preview`, What: "only preview; prints the ID that accepts it"},
				{Cmd: "tt ai --accept PREVIEW_ID", What: "apply a preview you reviewed"},
				{Cmd: `tt ai "call mom fri 18:00, remind 10min before" -y`, What: "apply without a question"},
				{Cmd: `tt ai find "what to buy at the store"`, What: "search cached tasks with a filter the model writes"},
				{Cmd: `tt ai "plan a review tmr" --profile deep`, What: "use another configured profile"},
			},
			Sections: []cli.HelpSection{
				{Title: "How it works", Items: []string{
					"tt runs the agent CLI of the configured profile (claude, codex or your own command) headlessly. The model only proposes operations; tt checks every one against the cache before showing anything.",
					"The operations are add, edit, schedule, done, move and checklist_add. Nothing is ever deleted. Unknown lists and tags are refused, never created.",
					"The model writes dates in tt's own date words (tmr 10:00, fri, +3d); tt resolves them, so the preview shows exact times.",
					"Accepted changes are one undo group: a single tt undo reverses all of them. If one change fails while applying, the ones before it are reversed at once.",
				}},
				{Title: "Review", Items: []string{
					"In a terminal tt asks before applying. --preview only saves a preview; -y applies without asking. Without a terminal tt prints the preview and its ID.",
					"--accept ID applies a saved preview once, within 24 hours, and refuses if a task it changes has changed since.",
					"Overlapping planned intervals are shown; -y refuses them unless --allow-overlap is given as well.",
				}},
				{Title: "What the model sees", Items: []string{
					"The request, the current time and time zone, and the names of your lists and tags. With ai.context = today or all, also the titles, lists and due dates of today's and overdue, or of up to 200 open, tasks, under short local refs. tt puts no notes, checklists or server IDs in a prompt.",
					"A request that looks like it holds a password, PIN, card number, key or token is refused, and cached titles and names that look like one are left out; tt says how many. --allow-secrets sends both. The check matches patterns and can miss a secret.",
					"The agent CLI passes what tt sends to its own provider. codex also sends your global AGENTS.override.md or, without one, AGENTS.md with every call, which tt doctor points out; a claude profile sends no such file.",
				}},
				{Title: "Search", Items: []string{
					"tt ai find sends the query, time, list and tag names; the model returns keywords (with word forms and synonyms), a list, a status and date bounds. tt searches the cache with them.",
					"--rerank has a second call rank the matches by their titles; with ai.context = today or all it is also made for more than 15 matches, while under minimal no title is sent without --rerank. --no-ai-rerank skips it.",
					"The ranking sees only the query and the candidate titles. Candidates whose titles were left out as secrets follow the ranked ones.",
					"The results become the numbered listing, so tt done 1 acts on the first one.",
				}},
				{Title: "Profiles", Items: []string{
					"--profile NAME, --model and --effort override ai.default and the profile's own values for this call. ai.tasks.ai and ai.tasks.find pick a profile or a prompt file per command.",
				}},
			},
			SeeAlso: []string{"undo", "config", "doctor", "s"},
		},
		run: cmdAI,
	})
}

type aiArgs struct {
	words        []string
	preview      bool
	yes          bool
	allowOverlap bool
	allowSecrets bool
	rerank       bool
	noRerank     bool
	accept       string
	hasAccept    bool
	request      ai.Request
}

func parseAIArgs(inv *invocation, words []string, find bool) (aiArgs, int, bool) {
	out := aiArgs{words: words}
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		arg := inv.refinements[i]
		if arg == endOfOptions {
			out.words = append(out.words, inv.refinements[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			out.words = append(out.words, arg)
			continue
		}
		plan, search, value := false, false, false
		switch arg {
		case "--preview", "-y", "--allow-overlap":
			plan = true
		case "--accept":
			plan, value = true, true
		case "--rerank", "--no-ai-rerank":
			search = true
		case "--allow-secrets":
			plan, search = true, true
		case "--profile", "--model", "--effort":
			plan, search, value = true, true, true
		}
		if find && !search || !find && !plan {
			return out, inv.misuseWord("unknown option ", arg), false
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
		case "--preview":
			out.preview = true
		case "-y":
			out.yes = true
		case "--allow-overlap":
			out.allowOverlap = true
		case "--allow-secrets":
			out.allowSecrets = true
		case "--rerank":
			out.rerank = true
		case "--no-ai-rerank":
			out.noRerank = true
		case "--accept":
			out.accept, out.hasAccept = given, true
		case "--profile":
			out.request.Profile = given
		case "--model":
			out.request.Model = given
		case "--effort":
			out.request.Effort = given
		}
	}
	return out, exitOK, true
}

func cmdAI(inv *invocation) int {
	if len(inv.data) > 0 && inv.data[0] == "find" {
		return cmdAIFind(inv)
	}
	if inv.rawOutput {
		return inv.misuse("--raw is read-only")
	}
	args, code, ok := parseAIArgs(inv, inv.data, false)
	if !ok {
		return code
	}
	text := strings.TrimSpace(strings.Join(args.words, " "))
	if args.hasAccept {
		if text != "" || args.preview || args.yes || args.allowSecrets || args.request.Profile+args.request.Model+args.request.Effort != "" {
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
		return aiAccept(inv, st, args.accept, nil)
	}
	if text == "" {
		return inv.misuse(`say what to change, as in tt ai "buy milk tomorrow at 10"`)
	}
	if args.preview && args.yes {
		return inv.misuse("choose --preview or -y, not both")
	}
	if code, ok := aiRefuseSecrets(inv, text, args.allowSecrets); !ok {
		return code
	}
	cfg, err := inv.config()
	if err != nil {
		return inv.fail(err)
	}
	if code, ok := aiKnownProfile(inv, cfg, args.request.Profile); !ok {
		return code
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	now := aiNow()
	if err := app.PruneAIPreviews(inv.ctx, st, now); err != nil {
		return inv.fail(err)
	}
	planner := app.AIPlanner{Store: st, Runner: newAIRunner(cfg.AI), Config: cfg, Now: now, AllowSecrets: args.allowSecrets}
	proposal, err := planner.Propose(inv.ctx, text, args.request)
	if err != nil {
		if inv.interrupted() {
			return exitInterrupted
		}
		return aiCallFailure(inv, err, proposal.LeftOut)
	}
	if len(proposal.Issues) != 0 {
		return aiInvalid(inv, proposal, "the proposed changes cannot be applied; nothing was saved")
	}
	if len(proposal.Plan.Steps) == 0 {
		inv.resultData = map[string]any{"changed": false, "notes": proposal.Notes, "profile": proposal.Profile, "left_out": proposal.LeftOut}
		lines := []string{"the model proposed no changes; nothing was saved"}
		lines = append(lines, aiNotesLines(proposal.Notes)...)
		lines = append(lines, aiLeftOutLines(proposal.LeftOut)...)
		cli.WriteLines(inv.stdout, lines)
		return exitOK
	}
	id, err := app.SaveAIPreview(inv.ctx, st, proposal.Plan, now)
	if err != nil {
		return inv.fail(err)
	}
	preview := aiPreviewLines(proposal.Plan)
	preview = append(preview, aiNotesLines(proposal.Notes)...)
	preview = append(preview, aiLeftOutLines(proposal.LeftOut)...)
	ask := !args.preview && !args.yes && canAsk(inv)

	if args.preview || !args.yes && !ask {
		if inv.jsonOutput {
			result := app.Result(inv.verb, map[string]any{"id": id, "plan": proposal.Plan, "notes": proposal.Notes,
				"overlaps": proposal.Plan.HasOverlaps(), "profile": proposal.Profile, "undo": "one tt undo reverses every change of the bundle",
				"left_out": proposal.LeftOut},
				app.ResultMeta{Source: "local"})
			result.Status = "preview"
			return inv.writeResult(result)
		}
		cli.WriteLines(inv.stdout, preview)
		cli.WriteLines(inv.stdout, aiAcceptLines(id))
		return exitOK
	}
	if args.yes {
		if proposal.Plan.HasOverlaps() && !args.allowOverlap {
			cli.WriteLines(inv.stdout, preview)
			cli.WriteLines(inv.stdout, aiAcceptLines(id))
			inv.resultData = map[string]any{"id": id, "left_out": proposal.LeftOut}
			return inv.fail(aiError("the changes overlap planned intervals; review them, then accept the preview, or pass --allow-overlap with -y"))
		}
		if !inv.jsonOutput {
			cli.WriteLines(inv.stdout, preview)
		}
		return aiAccept(inv, st, id, &proposal)
	}

	cli.WriteLines(inv.stderr, preview)
	question := fmt.Sprintf("apply these %d changes together? [y/N] ", len(proposal.Plan.Steps))
	if len(proposal.Plan.Steps) == 1 {
		question = "apply this change? [y/N] "
	}
	if proposal.Plan.HasOverlaps() {
		question = "apply, overlaps included? [y/N] "
	}
	fmt.Fprint(inv.stderr, question)
	if !confirm(inv.stdin) {
		cli.WriteLines(inv.stderr, append([]string{"nothing was changed"}, aiAcceptLines(id)...))
		return exitOK
	}
	return aiAccept(inv, st, id, &proposal)
}

func aiRefuseSecrets(inv *invocation, text string, allowed bool) (int, bool) {
	if allowed {
		return exitOK, true
	}
	kind := app.AISecretKind(text)
	if kind == "" {
		return exitOK, true
	}
	return inv.fail(aiError("the text looks like it holds " + kind +
		", and tt does not send secrets to a model; take it out, or pass --allow-secrets")), false
}

// aiError folds a sentence tt wrote itself, never text from the model or the
// cache, to the width left after the "tt: ai: " prefix inv.fail puts in front.
func aiError(sentence string) error {
	return errors.New(strings.Join(cli.Wrap(sentence, cli.Width-len("tt: ai: ")), "\n"))
}

func aiKnownProfile(inv *invocation, cfg config.Config, name string) (int, bool) {
	if name == "" {
		return exitOK, true
	}
	if _, ok := cfg.AI.Profiles[name]; ok {
		return exitOK, true
	}
	return inv.misuseWord("no AI profile called ", name), false
}

// aiAccept claims the preview id and applies it. proposal is set when this run
// also made the preview: its ID has not been shown then, and a JSON result
// says what the call left out.
func aiAccept(inv *invocation, st *store.Store, id string, proposal *app.AIProposal) int {
	if inv.interrupted() {
		return exitInterrupted
	}
	plan, text, err := app.ClaimAIPreview(inv.ctx, st, id, aiNow())
	switch {
	case errors.Is(err, app.ErrAIPreviewInvalid), errors.Is(err, app.ErrAIPreviewUnavailable):
		return inv.misuse("%v", err)
	case err != nil:
		return inv.fail(err)
	}

	applied, err := aiApplyPlan(inv.ctx, st, plan)
	var failed *app.AIApplyError
	switch {
	case err == nil:
	case errors.Is(err, app.ErrAIPlanStale):
		return inv.fail(aiError("nothing was changed: a task this preview changes has changed since the preview; run tt ai again"))
	case errors.As(err, &failed):
		// A request reversed whole can be accepted again; one left partly
		// applied must not be.
		f := aiFailure{AIApplyError: failed, id: id, proposal: proposal}
		if failed.Rollback == nil {
			f.stale, f.keepErr = aiKeepPreview(inv, st, plan, id, text)
		}
		return aiApplyFailure(inv, st, f)
	default:
		// The plan stopped before its first change.
		stale, keepErr := aiKeepPreview(inv, st, plan, id, text)
		if keepErr != nil {
			return inv.fail(errors.New(err.Error() + "; the preview could not be kept either: " + keepErr.Error()))
		}
		inv.resultData = aiData(proposal, map[string]any{"id": id, "preview_kept": !stale})
		code := exitInterrupted
		if !inv.interrupted() {
			code = inv.fail(err)
		}
		switch {
		case inv.jsonOutput:
		case stale:
			cli.WriteLines(inv.stderr, aiStaleLines())
		case proposal != nil:
			cli.WriteLines(inv.stderr, aiKeptLines(id))
		}
		return code
	}

	inv.resultData = aiData(proposal, map[string]any{"id": id, "group_id": applied.GroupID, "changes": applied.Results})
	lines := []string{fmt.Sprintf(`applied %d changes together; one "tt undo" reverses them all, "tt sync" sends them`, len(applied.Results))}
	if len(applied.Results) == 1 {
		lines[0] = `applied 1 change; "tt undo" reverses it, "tt sync" sends it`
	}
	for i, result := range applied.Results {
		lines = append(lines, cli.ReportLine("  "+aiDone(plan.Steps[i], result.Changed), result.Task.Title, "", nil))
	}
	cli.WriteLines(inv.stdout, lines)
	return exitOK
}

func aiDone(step app.AIStep, changed bool) string {
	if !changed {
		return "no change to "
	}
	switch step.Op {
	case app.AIOpAdd:
		return "added "
	case app.AIOpEdit:
		return "edited "
	case app.AIOpSchedule:
		return "scheduled "
	case app.AIOpDone:
		return "completed "
	case app.AIOpMove:
		return "moved "
	default:
		return fmt.Sprintf("added %d checklist items to ", len(step.Checklist))
	}
}

func aiAcceptLines(id string) []string {
	return []string{"accept this preview with:", "tt ai --accept " + id}
}

// aiKeepPreview puts a claimed preview back after applying it left nothing
// applied, so that it can be accepted again - but only while the tasks its
// plan changes are as the preview saved them. Reversing an edit changes the
// task once more, and a preview that no longer matches would only be refused:
// stale reports one left deleted for that. It runs on a context an interrupt
// does not cancel, since the interrupt is often why applying stopped.
func aiKeepPreview(inv *invocation, st *store.Store, plan app.AIPlan, id, text string) (stale bool, err error) {
	ctx := context.WithoutCancel(inv.ctx)
	switch err := app.AIPlanFresh(ctx, st, plan); {
	case errors.Is(err, app.ErrAIPlanStale):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, app.RestoreAIPreview(ctx, st, id, text)
}

// aiStaleLines say why a preview was not put back after applying it failed:
// a task it changes, reversed or changed by another run meanwhile, is no
// longer as the preview saw it, so accepting it again would only be refused.
func aiStaleLines() []string {
	return cli.Wrap("the preview was not kept: a task it changes is no longer as the preview saw it; run tt ai again", cli.Width)
}

// aiKeptLines give the ID of a preview put back after applying it failed, to a
// run that made the preview and so has not shown its ID.
func aiKeptLines(id string) []string {
	return []string{"the preview is kept; accept it again with:", "tt ai --accept " + id}
}

// aiData is the data of a JSON result; when proposal is set, the run also
// called the model and the data says what the call left out.
func aiData(proposal *app.AIProposal, data map[string]any) map[string]any {
	if proposal != nil {
		data["left_out"] = proposal.LeftOut
	}
	return data
}

// aiFailure is a plan that failed while applying, and what became of its
// preview: stale is set when reversing the changes left the preview no longer
// matching the cache, and keepErr is why a request reversed whole could not
// put it back otherwise.
type aiFailure struct {
	*app.AIApplyError
	id       string
	proposal *app.AIProposal
	stale    bool
	keepErr  error
}

func (f aiFailure) kept() bool { return f.Rollback == nil && !f.stale && f.keepErr == nil }

// aiApplyFailure reports a plan that failed while applying: how far reversing
// the changes before the failure got, and what became of the preview.
func aiApplyFailure(inv *invocation, st *store.Store, f aiFailure) int {
	lead := fmt.Sprintf("change %d of the request failed; the changes before it were reversed, so nothing is left applied", f.Step)
	var blocked *app.AIRollbackBlockedError
	switch {
	case f.Rollback == nil:
	case errors.As(f.Rollback, &blocked):
		lead = fmt.Sprintf("change %d of the request failed, and reversing the changes before it stopped: another change was recorded in between, so %s",
			f.Step, aiStillApplied(blocked.Remaining))
	default:
		lead = fmt.Sprintf("change %d of the request failed, and reversing the changes before it failed too; run tt undo to reverse what is left", f.Step)
	}
	if inv.jsonOutput {
		inv.resultData = aiData(f.proposal, map[string]any{"step": f.Step, "op": f.Op, "rolled_back": f.Rollback == nil,
			"id": f.id, "preview_kept": f.kept()})
		message := lead + ": " + f.Error()
		if f.Rollback != nil {
			message += "; reversing failed: " + f.Rollback.Error()
		}
		if f.keepErr != nil {
			message += "; the preview could not be kept: " + f.keepErr.Error()
		}
		return inv.jsonFailure("apply_failed", message, exitError)
	}
	code := inv.fail(aiError(lead))
	lines := []string{"why:"}
	lines = append(lines, aiFold("  ", fixValue(aiReason(f.Err)))...)
	switch {
	case blocked != nil:
		lines = append(lines, aiBlockedLines(inv, st, blocked)...)
	case f.Rollback != nil:
		lines = append(lines, "why reversing failed:")
		lines = append(lines, aiFold("  ", fixValue(f.Rollback.Error()))...)
	}
	if f.keepErr != nil {
		lines = append(lines, "the preview could not be kept, so it cannot be accepted again:")
		lines = append(lines, aiFold("  ", fixValue(f.keepErr.Error()))...)
	}
	if f.stale {
		lines = append(lines, aiStaleLines()...)
	}
	if f.kept() && f.proposal != nil {
		lines = append(lines, aiKeptLines(f.id)...)
	}
	cli.WriteLines(inv.stderr, lines)
	if inv.interrupted() {
		return exitInterrupted
	}
	return code
}

func aiStillApplied(n int) string {
	if n == 1 {
		return "1 change of this request is still applied"
	}
	return fmt.Sprintf("%d changes of this request are still applied", n)
}

// aiBlockedLines name the tasks a blocked rollback left changed, and say how
// to reverse the rest only where that is sure: tt undo reverses the newest
// record first, and the newest is another run's.
func aiBlockedLines(inv *invocation, st *store.Store, blocked *app.AIRollbackBlockedError) []string {
	var lines []string
	for _, taskID := range blocked.TaskIDs {
		if task, err := st.Task(context.WithoutCancel(inv.ctx), taskID); err == nil {
			lines = append(lines, cli.ReportLine("  ", undoTitle(task), "", nil))
		}
	}
	if len(lines) != 0 {
		lines = append([]string{"still applied to:"}, lines...)
	}
	advice := `"tt undo" would first reverse changes that other runs recorded on top of them; review what is still ` +
		`applied with tt show and change it back by hand`
	switch {
	case blocked.Above == 0:
		advice = `the rest of this request is back on top of the undo history, so "tt undo" reverses it`
	case blocked.AboveTouches && blocked.Above == 1:
		advice = `"tt undo" would first reverse a change that another run recorded on top of them, and that change ` +
			`may touch the same tasks, so reversing this request's changes could undo it as well; review what is ` +
			`still applied with tt show and change it back by hand`
	case blocked.AboveTouches:
		advice = `"tt undo" would first reverse changes that other runs recorded on top of them, and those changes ` +
			`may touch the same tasks, so reversing this request's changes could undo them as well; review what is ` +
			`still applied with tt show and change it back by hand`
	case blocked.Above == 1 && blocked.Contiguous:
		advice = `"tt undo" would first reverse the change recorded on top of them; to keep that change and reverse ` +
			`the rest of this request, run "tt undo --skip", which drops the record of that change without ` +
			`reversing it, then "tt undo"`
	}
	return append(lines, cli.Wrap(advice, cli.Width)...)
}

func aiReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "tt was interrupted"
	case errors.Is(err, store.ErrTaskChanged):
		return "the task changed between two changes of this request"
	case errors.Is(err, store.ErrIntervalChanged), errors.Is(err, app.ErrAIOverlapChanged):
		return app.ErrAIOverlapChanged.Error()
	}
	return err.Error()
}

// aiCallFailure reports a model call that failed or answered with something
// unusable. leftOut is what the call was built without, since the prompt may
// have gone out before the failure.
func aiCallFailure(inv *invocation, err error, leftOut app.AILeftOut) int {
	message := "the model call failed"
	if errors.Is(err, app.ErrAIReplyShape) {
		message = "the model's reply could not be used"
	}
	if inv.jsonOutput {
		inv.resultData = map[string]any{"left_out": leftOut}
		return inv.jsonFailure("ai_failed", message+": "+err.Error(), exitError)
	}
	code := inv.fail(errors.New(message + "; nothing was changed"))
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(err.Error()), "\n") {
		lines = append(lines, aiFold("  ", fixValue(line))...)
	}
	lines = append(lines, aiLeftOutLines(leftOut)...)
	cli.WriteLines(inv.stderr, lines)
	return code
}

func aiInvalid(inv *invocation, proposal app.AIProposal, lead string) int {
	if inv.jsonOutput {
		inv.resultData = map[string]any{"issues": proposal.Issues, "notes": proposal.Notes, "profile": proposal.Profile, "left_out": proposal.LeftOut}
		return inv.jsonFailure("invalid_proposal", lead, exitError)
	}
	code := inv.fail(errors.New(lead))
	var lines []string
	for _, issue := range proposal.Issues {
		head := "the request as a whole:"
		if issue.Step > 0 {
			head = fmt.Sprintf("change %d:", issue.Step)
		}
		lines = append(lines, head)
		lines = append(lines, aiFold("  ", fixValue(issue.Reason))...)
		if issue.Value != "" {
			lines = append(lines, cli.ReportLine("  the model wrote ", issue.Value, "", nil))
		}
	}
	lines = append(lines, aiNotesLines(proposal.Notes)...)
	lines = append(lines, aiLeftOutLines(proposal.LeftOut)...)
	cli.WriteLines(inv.stderr, lines)
	return code
}

func aiNotesLines(notes string) []string {
	if strings.TrimSpace(notes) == "" {
		return nil
	}
	return append([]string{"note from the model:"}, aiFold("  ", fixValue(notes))...)
}

// aiLeftOutLines says how much of the cache a call kept from the model because
// it looks like a secret. It counts; it never repeats what was left out.
func aiLeftOutLines(left app.AILeftOut) []string {
	var parts []string
	for _, kind := range []struct {
		n         int
		one, many string
	}{{left.Tasks, "task", "tasks"}, {left.Lists, "list name", "list names"}, {left.Tags, "tag", "tags"}} {
		switch {
		case kind.n == 1:
			parts = append(parts, "1 "+kind.one)
		case kind.n > 1:
			parts = append(parts, strconv.Itoa(kind.n)+" "+kind.many)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	what := parts[len(parts)-1]
	if len(parts) > 1 {
		what = strings.Join(parts[:len(parts)-1], ", ") + " and " + what
	}
	sentence := what + " were left out because they look like they contain secrets; --allow-secrets sends them"
	if left.Tasks+left.Lists+left.Tags == 1 {
		sentence = what + " was left out because it looks like it contains a secret; --allow-secrets sends it"
	}
	return cli.Wrap(sentence, cli.Width)
}

// aiFold folds text that fixValue already escaped into lines that fit after
// indent. The quotes fixValue adds are dropped; a word wider than a line is
// cut, since escaped text has no break of its own to wait for.
func aiFold(indent, escaped string) []string {
	if len(escaped) >= 2 && escaped[0] == '"' && escaped[len(escaped)-1] == '"' {
		escaped = escaped[1 : len(escaped)-1]
	}
	width := cli.Width - cli.DisplayWidth(indent)
	var words []string
	for _, word := range strings.Fields(escaped) {
		for cli.DisplayWidth(word) > width {
			cut, used := 0, 0
			for i, r := range word {
				w := cli.DisplayWidth(string(r))
				if used+w > width {
					cut = i
					break
				}
				used += w
			}
			words = append(words, word[:cut])
			word = word[cut:]
		}
		words = append(words, word)
	}
	lines := cli.Wrap(strings.Join(words, " "), width)
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return lines
}

const aiDetail = "     "

func aiPreviewLines(plan app.AIPlan) []string {
	lines := []string{fmt.Sprintf(`%d proposed changes, applied together and reversed together by one "tt undo":`, len(plan.Steps))}
	if len(plan.Steps) == 1 {
		lines[0] = `1 proposed change; "tt undo" reverses it:`
	}
	for i, step := range plan.Steps {
		n := fmt.Sprintf("%2d. ", i+1)
		switch step.Op {
		case app.AIOpAdd:
			lines = append(lines, cli.ReportLine(n+"add ", step.Title, "", nil))
			lines = append(lines, cli.ReportLine(aiDetail+"to list ", step.Project.Name, "", nil))
		case app.AIOpEdit:
			lines = append(lines, cli.ReportLine(n+"edit ", step.Task.Title, "", nil))
			if step.Title != "" {
				lines = append(lines, cli.ReportLine(aiDetail+"new title ", step.Title, "", nil))
			}
		case app.AIOpSchedule:
			lines = append(lines, cli.ReportLine(n+"schedule ", step.Task.Title, "", nil))
		case app.AIOpDone:
			after := ""
			if step.Task.Status.Done() {
				after = " (already done)"
			}
			lines = append(lines, cli.ReportLine(n+"complete ", step.Task.Title, after, nil))
		case app.AIOpMove:
			lines = append(lines, cli.ReportLine(n+"move ", step.Task.Title, "", nil))
			lines = append(lines, cli.ReportLine(aiDetail+"to list ", step.Project.Name, "", nil))
		case app.AIOpChecklistAdd:
			lines = append(lines, cli.ReportLine(n+"add checklist items to ", step.Task.Title, "", nil))
		}
		lines = append(lines, aiStepDetails(step)...)
	}
	return lines
}

func aiStepDetails(step app.AIStep) []string {
	var lines []string
	if step.Content != "" {
		label := "notes "
		if step.Op == app.AIOpEdit {
			label = "append to notes "
		}
		lines = append(lines, cli.ReportLine(aiDetail+label, step.Content, "", nil))
	}
	for _, tag := range step.Tags {
		lines = append(lines, cli.ReportLine(aiDetail+"tag ", tag, "", nil))
	}
	for _, item := range step.Checklist {
		lines = append(lines, cli.ReportLine(aiDetail+"item ", item, "", nil))
	}
	if step.Due != nil {
		lines = append(lines, aiDetail+"due "+aiWhen(*step.Due))
	}
	if step.Interval != nil {
		lines = append(lines, aiDetail+"planned "+aiSpan(*step.Interval))
	}
	for _, trigger := range step.Reminders {
		lines = append(lines, cli.ReportLine(aiDetail+"reminder ", schedule.ReminderInput(trigger), "", nil))
	}
	if step.Priority != nil {
		lines = append(lines, aiDetail+"priority "+step.Priority.String())
	}
	for _, overlap := range step.Overlaps {
		lines = append(lines, cli.ReportLine(aiDetail+"overlaps ", overlap.Title, " "+aiClock(overlap.Start)+"-"+aiClock(overlap.End), nil))
	}
	for _, other := range step.Clashes {
		lines = append(lines, fmt.Sprintf("%soverlaps change %d of this request", aiDetail, other))
	}
	return lines
}

func aiWhen(r dates.Result) string {
	if r.Clear {
		return "none (the due date is removed)"
	}
	local := r.Time.In(time.Local)
	if r.AllDay {
		return local.Format("Mon 2006-01-02") + ", all day"
	}
	return local.Format("Mon 2006-01-02 15:04")
}

func aiSpan(i dates.Interval) string {
	loc, err := dates.IntervalZone(i.Zone)
	if err != nil {
		loc = time.Local
	}
	start, end := i.Start.In(loc), i.End.In(loc)
	layout := "15:04"
	if start.YearDay() != end.YearDay() || start.Year() != end.Year() {
		layout = "Mon 2006-01-02 15:04"
	}
	return start.Format("Mon 2006-01-02 15:04") + "-" + end.Format(layout) + " (" + dates.ElapsedLabel(end.Sub(start)) + ", " + loc.String() + ")"
}

func aiClock(t model.Time) string {
	return t.In(time.Local).Format("Mon 15:04")
}
