package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/tasklist"
)

const (
	batchPlanVersion   = 1
	batchPreviewPrefix = "batch_preview_"

	// BatchPlanLifetime is how long a saved preview can be accepted. A plan
	// holds the lists it resolved against the cache, so an old one would
	// create tasks where the cache no longer points.
	BatchPlanLifetime = 24 * time.Hour
)

// The texts stay short enough to fit one line after the "tt: batch: " they
// are printed behind.
var (
	ErrBatchPreviewUnavailable = errors.New("preview is unavailable: never saved, already accepted, or expired")
	ErrBatchPreviewInvalid     = errors.New("preview failed validation")
	ErrBatchPreviewExpired     = errors.New("preview has expired; run tt batch again")
	ErrBatchPreviewListChanged = errors.New("a list this preview writes to is gone or closed; run tt batch again")
)

// ErrBatchParentTaken reports that the create of a parent of this batch is no
// longer the only change queued for that task, so a child may not be hung
// under it any more. Inside a batch only another tt run can have done that:
// the create was queued moments earlier. It stands apart from every other
// reason a step fails, because nothing is wrong with the batch itself - the
// preview goes back, and accepting it again creates it.
var ErrBatchParentTaken = errors.New("another tt run changed the queued create of a parent task while this batch was being applied")

// BatchPlan is a validated bundle of tasks to create. It is what a preview
// saves and what an accept applies; it never holds the document it was read
// from.
type BatchPlan struct {
	Version int         `json:"version"`
	Created time.Time   `json:"created"`
	Steps   []BatchStep `json:"steps"`
}

// BatchStep is one task to create. Task is the whole task as it will be
// written, its target list in ProjectId and its checklist in Items; List is
// that list's name, for the lines a user reads. Complete is set when the
// document marked the task done, so it is completed once the whole batch is
// there. Line is the line of the document the task was read from. Parent is
// the index in Steps of the task this one hangs under, and -1 when it hangs
// under none; a plan always lists a parent before its children.
type BatchStep struct {
	Task     model.Task `json:"task"`
	List     string     `json:"list"`
	Complete bool       `json:"complete,omitempty"`
	Line     int        `json:"line"`
	Parent   int        `json:"parent"`
}

// CountItems counts the checklist items of every task of the plan.
func (p BatchPlan) CountItems() int {
	n := 0
	for _, step := range p.Steps {
		n += len(step.Task.Items)
	}
	return n
}

// Lists names the lists the plan writes to, in the order it first writes to
// each of them.
func (p BatchPlan) Lists() []string {
	var out []string
	for _, step := range p.Steps {
		if !slices.Contains(out, step.List) {
			out = append(out, step.List)
		}
	}
	return out
}

// BatchPlanError says why a document cannot be turned into a plan, at the
// line it was read from. Line is 0 for the document as a whole. Msg carries
// text of the document, which a caller escapes before it prints it; Rendered
// says that this one reports names of the cache instead and was already
// rendered for reading, lines, escaping and widths included, so a caller
// prints it as it stands.
//
// A rendered one leaves Line at 0 and names the line it means inside Msg: the
// widths it was fitted to are those of a refusal the verb alone is printed in
// front of, and a "line N: " between the two would push it off the terminal.
type BatchPlanError struct {
	Line     int
	Msg      string
	Rendered bool
}

func (e *BatchPlanError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

// PlanBatch turns a parsed document into a plan: every list name resolved
// against the cache, every title and checklist item validated, and every task
// a step of its own, a parent before its children. defaultList is where the
// tasks written before the first heading go, the way tt add picks a list; it
// is only needed when the document has such tasks.
func PlanBatch(ctx context.Context, st *store.Store, doc tasklist.Document, defaultList model.Project, now time.Time) (BatchPlan, error) {
	if n := doc.CountTasks(); n > tasklist.MaxTasks {
		return BatchPlan{}, &BatchPlanError{Msg: fmt.Sprintf("the document holds %d tasks, more than the %d one batch may create", n, tasklist.MaxTasks)}
	}
	projects, err := st.Projects(ctx)
	if err != nil {
		return BatchPlan{}, err
	}
	plan := BatchPlan{Version: batchPlanVersion, Created: now.UTC(), Steps: []BatchStep{}}
	for _, group := range doc.Groups {
		// A heading with nothing under it writes nowhere, so the list it
		// names is never resolved: a document is not refused over a heading
		// none of its tasks go to.
		if len(group.Tasks) == 0 {
			continue
		}
		project, err := batchGroupList(projects, group, defaultList)
		if err != nil {
			return BatchPlan{}, err
		}
		for _, task := range group.Tasks {
			if task.Done && len(task.Children) != 0 {
				return BatchPlan{}, &BatchPlanError{Line: task.Line, Msg: batchDoneParentMsg}
			}
			parent := len(plan.Steps)
			step, err := batchStep(task, project, now, -1)
			if err != nil {
				return BatchPlan{}, err
			}
			plan.Steps = append(plan.Steps, step)
			for _, child := range task.Children {
				step, err := batchStep(child, project, now, parent)
				if err != nil {
					return BatchPlan{}, err
				}
				plan.Steps = append(plan.Steps, step)
			}
		}
	}
	if len(plan.Steps) == 0 {
		return BatchPlan{}, &BatchPlanError{Msg: "the document holds no tasks"}
	}
	return plan, nil
}

// batchDoneParentMsg refuses a task the document both marks done and hangs
// other tasks under. Neither order works: a child cannot be hung under a
// closed task, and completing the parent first only moves the refusal to the
// push, where the relationship would be parked with the same reason and never
// reach the server.
const batchDoneParentMsg = "a task marked [x] cannot have child tasks: a child cannot be hung under a task " +
	"that is already closed; remove the [x], or write the children as tasks of their own"

// batchGroupList picks the list one group of the document writes to: the one
// its heading names, or defaultList for the tasks written before the first
// heading.
func batchGroupList(projects []model.Project, group tasklist.Group, defaultList model.Project) (model.Project, error) {
	project := defaultList
	if name := strings.TrimSpace(group.List); name != "" {
		var matches []model.Project
		for _, candidate := range projects {
			if strings.EqualFold(strings.TrimSpace(candidate.Name), name) {
				matches = append(matches, candidate)
			}
		}
		switch len(matches) {
		case 1:
			project = matches[0]
		case 0:
			return model.Project{}, batchUnknownList(projects, group.Line, name)
		default:
			return model.Project{}, &BatchPlanError{Line: group.Line, Msg: "more than one cached list is called " + name}
		}
	} else if project.Id == "" {
		return model.Project{}, &BatchPlanError{Line: group.Line,
			Msg: "these tasks come before the first list heading and no default list was given; put them under a heading, or pass -P"}
	}
	if reason := project.CreateUnavailable(); reason != "" {
		return model.Project{}, &BatchPlanError{Line: group.Line, Msg: reason + "; sync or choose another list"}
	}
	return project, nil
}

// batchUnknownList refuses a heading no cached list is called, and names the
// lists whose names come close to it, or every cached list when none does. The
// list a heading names is still taken by an exact match alone: the heading
// stands over every task written under it, so a close name is a hint to read
// and never a list to write to. The refusal reports names of the cache rather
// than text of the document, so it is rendered here through the refusal every
// other command gives for a list nothing matches, which escapes each name and
// keeps the line it puts them on inside the terminal. That refusal is cut to
// the room a verb leaves in front of it, so the heading's line is named in a
// line of the block rather than in front of the block.
func batchUnknownList(projects []model.Project, line int, name string) error {
	names := make([]string, len(projects))
	for i, project := range projects {
		names[i] = project.Name
	}
	known, omitted := cli.Shortlist(names)
	if kind, closest := cli.MatchListName(names, name); kind != cli.ListNameNone {
		known, omitted = cli.Shortlist(closest)
	}
	unknown := &cli.NoMatchError{What: "list", Query: name, Known: known, Omitted: omitted}
	lines := append([]string{unknown.Error(), fmt.Sprintf("line %d of the document names it", line)}, batchNoListHint...)
	return &BatchPlanError{Msg: strings.Join(lines, "\n"), Rendered: true}
}

// batchNoListHint is broken into lines here rather than folded, so that no
// command a reader is meant to type is split across two of them.
var batchNoListHint = []string{
	"tt batch does not create lists, so nothing was created",
	"create the list with tt project add and tt sync, then run tt batch again",
}

// batchStep builds the step that creates one task of the document.
func batchStep(task tasklist.Task, project model.Project, now time.Time, parent int) (BatchStep, error) {
	title := strings.TrimSpace(task.Title)
	switch {
	case title == "":
		return BatchStep{}, &BatchPlanError{Line: task.Line, Msg: "title is required"}
	case strings.ContainsFunc(title, unicode.IsControl):
		return BatchStep{}, &BatchPlanError{Line: task.Line, Msg: "title must not contain control characters"}
	case len(task.Items) > tasklist.MaxItemsPerTask:
		return BatchStep{}, &BatchPlanError{Line: task.Line, Msg: fmt.Sprintf("the task has %d checklist items, more than the %d one task may take",
			len(task.Items), tasklist.MaxItemsPerTask)}
	}
	step := BatchStep{
		Task:     model.Task{Title: title, ProjectId: project.Id},
		List:     project.Name,
		Complete: task.Done,
		Line:     task.Line,
		Parent:   parent,
	}
	for _, item := range task.Items {
		if err := ValidateItemTitle(item.Title); err != nil {
			return BatchStep{}, &BatchPlanError{Line: item.Line, Msg: err.Error()}
		}
		next := model.Item{Title: strings.TrimSpace(item.Title)}
		if item.Done {
			next.Status, next.CompletedTime = model.ItemDone, model.NewTime(now)
		}
		step.Task.Items = append(step.Task.Items, next)
	}
	return step, nil
}

// SaveBatchPreview stores plan under the hex SHA-256 of its JSON and returns
// that id. Previews older than BatchPlanLifetime are removed first.
func SaveBatchPreview(ctx context.Context, st *store.Store, plan BatchPlan, now time.Time) (string, error) {
	if err := PruneBatchPreviews(ctx, st, now); err != nil {
		return "", err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	return id, st.SetMeta(ctx, batchPreviewPrefix+id, string(raw))
}

// ClaimBatchPreview takes a saved preview out of the cache and returns its
// plan, so one preview is applied at most once. The raw text lets a caller put
// it back with RestoreBatchPreview when applying failed and changed nothing.
// Every claim removes the previews older than BatchPlanLifetime, the one asked
// for included, which is still reported as expired.
func ClaimBatchPreview(ctx context.Context, st *store.Store, id string, now time.Time) (BatchPlan, string, error) {
	if len(id) != sha256.Size*2 {
		return BatchPlan{}, "", ErrBatchPreviewInvalid
	}
	if _, err := hex.DecodeString(id); err != nil {
		return BatchPlan{}, "", ErrBatchPreviewInvalid
	}
	text, exists, err := st.Meta(ctx, batchPreviewPrefix+id)
	if err != nil {
		return BatchPlan{}, "", err
	}
	if err := PruneBatchPreviews(ctx, st, now); err != nil {
		return BatchPlan{}, "", err
	}
	if !exists {
		return BatchPlan{}, "", ErrBatchPreviewUnavailable
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != id {
		return BatchPlan{}, "", ErrBatchPreviewInvalid
	}
	var plan BatchPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil || plan.Version != batchPlanVersion {
		return BatchPlan{}, "", ErrBatchPreviewInvalid
	}
	if now.Sub(plan.Created) > BatchPlanLifetime || plan.Created.After(now.Add(time.Minute)) {
		return BatchPlan{}, "", ErrBatchPreviewExpired
	}
	// The lists were resolved against the cache when the plan was made, and
	// a preview is accepted for hours after that. They are read again before
	// the preview is taken out of the cache, so a refusal leaves it where it
	// was rather than spending it on a list nothing can be written to.
	if err := batchPlanLists(ctx, st, plan); err != nil {
		return BatchPlan{}, "", err
	}
	res, err := st.DB().ExecContext(ctx, `DELETE FROM meta WHERE key = ? AND value = ?`, batchPreviewPrefix+id, text)
	if err != nil {
		return BatchPlan{}, "", fmt.Errorf("claim preview: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return BatchPlan{}, "", ErrBatchPreviewUnavailable
	}
	return plan, text, nil
}

// batchPlanLists reports whether every list the plan writes to is still in
// the cache and still takes new tasks. It names no list: the lines a claim
// prints carry no text of the cache, and the document the names came from is
// gone by then anyway.
func batchPlanLists(ctx context.Context, st *store.Store, plan BatchPlan) error {
	projects, err := st.Projects(ctx)
	if err != nil {
		return err
	}
	cached := make(map[string]model.Project, len(projects))
	for _, project := range projects {
		cached[project.Id] = project
	}
	for _, step := range plan.Steps {
		project, ok := cached[step.Task.ProjectId]
		if !ok || project.CreateUnavailable() != "" {
			return ErrBatchPreviewListChanged
		}
	}
	return nil
}

func RestoreBatchPreview(ctx context.Context, st *store.Store, id, text string) error {
	return st.SetMeta(ctx, batchPreviewPrefix+id, text)
}

// PruneBatchPreviews removes the saved previews older than BatchPlanLifetime,
// and any that no longer reads as one. A preview holds a copy of every task
// it would create, so it is not kept past the time it can be used.
func PruneBatchPreviews(ctx context.Context, st *store.Store, now time.Time) error {
	rows, err := st.DB().QueryContext(ctx, `SELECT key, value FROM meta WHERE key GLOB 'batch_preview_*'`)
	if err != nil {
		return fmt.Errorf("read previews: %w", err)
	}
	var stale []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return fmt.Errorf("read previews: %w", err)
		}
		var plan struct {
			Created time.Time `json:"created"`
		}
		if json.Unmarshal([]byte(value), &plan) != nil || now.Sub(plan.Created) > BatchPlanLifetime {
			stale = append(stale, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read previews: %w", err)
	}
	rows.Close()
	for _, key := range stale {
		if _, err := st.DB().ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key); err != nil {
			return fmt.Errorf("remove expired preview: %w", err)
		}
	}
	return nil
}

// BatchApplyError reports the task that failed, at the line of the document
// it came from. Rollback is nil when every task created before it was removed
// again; otherwise it says why that failed, and part of the batch may still
// be there.
type BatchApplyError struct {
	Step     int
	Line     int
	Err      error
	Rollback error
}

func (e *BatchApplyError) Error() string {
	return fmt.Sprintf("task %d (line %d): %v", e.Step, e.Line, e.Err)
}

func (e *BatchApplyError) Unwrap() error { return e.Err }

type BatchStepResult struct {
	Step int        `json:"step"`
	Task model.Task `json:"task"`
	Done bool       `json:"done"`
}

// BatchApplied is a plan that went through: every task carries GroupID in the
// undo history, so one undo removes all of them.
type BatchApplied struct {
	GroupID string            `json:"group_id"`
	Results []BatchStepResult `json:"tasks"`
}

// ErrBatchRollbackBlocked reports that a change of another run was recorded
// among or above this batch's changes, so the undo history cannot reverse them
// as one group. The error a rollback returns is a BatchRollbackBlockedError.
var ErrBatchRollbackBlocked = errors.New("another change was recorded in between this batch's changes")

// BatchRollbackBlockedError is what a rollback stopped by
// ErrBatchRollbackBlocked left applied; its fields say the same as those of
// AIRollbackBlockedError, which reports it for a plan of tt ai.
type BatchRollbackBlockedError struct {
	Remaining    int
	TaskIDs      []string
	Above        int
	Contiguous   bool
	AboveTouches bool
}

func (e *BatchRollbackBlockedError) Error() string {
	if e.Remaining == 1 {
		return ErrBatchRollbackBlocked.Error() + ", so 1 of them is still there"
	}
	return fmt.Sprintf("%s, so %d of them are still there", ErrBatchRollbackBlocked, e.Remaining)
}

func (e *BatchRollbackBlockedError) Is(target error) bool { return target == ErrBatchRollbackBlocked }

// ApplyBatchPlan creates every task of plan through the same store path tt add
// uses, all under one fresh undo group, in two passes over the steps. The
// first creates every task and hangs the ones the plan puts behind another
// under their parent; the second completes the ones the document marked done.
// Completing comes last because a child may only be hung under an open task,
// so a parent the document marked done has to stay open until its children are
// there. Both passes record under the same group, so one undo still reverses
// the whole batch. When a step fails it removes what was created before it.
func ApplyBatchPlan(ctx context.Context, st *store.Store, plan BatchPlan) (BatchApplied, error) {
	// A claim reads the version of the preview it takes out of the cache,
	// but a plan also reaches this straight from PlanBatch, so the shape it
	// was written in is checked where it is applied as well: another version
	// may give these fields another meaning.
	if plan.Version != batchPlanVersion {
		return BatchApplied{}, &BatchPlanError{Msg: fmt.Sprintf(
			"the plan was written for batch plan version %d, and this tt applies version %d",
			plan.Version, batchPlanVersion)}
	}
	for i, step := range plan.Steps {
		if step.Parent < -1 || step.Parent >= i {
			return BatchApplied{}, &BatchPlanError{Line: step.Line,
				Msg: "the plan hangs this task under a task it does not create before it"}
		}
	}
	floor, err := aiUndoFloor(ctx, st)
	if err != nil {
		return BatchApplied{}, err
	}
	group, err := store.NewUndoGroupID()
	if err != nil {
		return BatchApplied{}, err
	}
	grouped := store.WithUndoGroup(ctx, group)
	out := BatchApplied{GroupID: group, Results: make([]BatchStepResult, 0, len(plan.Steps))}
	created := make([]string, len(plan.Steps))
	failed := func(i int, err error) (BatchApplied, error) {
		return BatchApplied{}, &BatchApplyError{Step: i + 1, Line: plan.Steps[i].Line, Err: err,
			Rollback: batchRollback(context.WithoutCancel(ctx), st, group, floor)}
	}
	for i, step := range plan.Steps {
		err := ctx.Err()
		var task model.Task
		if err == nil {
			task, err = st.CreateTask(grouped, step.Task)
		}
		if err == nil && step.Parent >= 0 {
			task, err = batchAttach(grouped, st, task, created[step.Parent])
		}
		if err != nil {
			return failed(i, err)
		}
		created[i] = task.Id
		out.Results = append(out.Results, BatchStepResult{Step: i + 1, Task: task})
	}
	for i, step := range plan.Steps {
		if !step.Complete {
			continue
		}
		err := ctx.Err()
		var task model.Task
		var done bool
		if err == nil {
			task, done, err = batchComplete(grouped, st, created[i])
		}
		if err != nil {
			return failed(i, err)
		}
		out.Results[i].Task, out.Results[i].Done = task, done
	}
	return out, nil
}

// batchComplete completes a task the document marked done, once every task of
// the plan is there. The task is read back under the id the cache holds for it
// now, so it is compared with what the batch left in the cache and not with
// the copy the plan carries; a sync between two steps of the batch may already
// have swapped a local id for the one the server gave it.
//
// The checklist is left as the document wrote it: closing a task by hand ticks
// off what is still open under it, but here the document said which items are
// done and the preview showed exactly that, so a [x] on the task line may not
// tick items the reader left open behind their back.
func batchComplete(ctx context.Context, st *store.Store, stepID string) (model.Task, bool, error) {
	id, err := st.CurrentTaskID(ctx, stepID)
	if err != nil {
		return model.Task{}, false, err
	}
	stored, err := st.Task(ctx, id)
	if err != nil {
		return model.Task{}, false, err
	}
	outcome, err := st.CompleteTaskIfUnchanged(ctx, stored, store.CompleteOptions{KeepItems: true})
	if err != nil {
		return model.Task{}, false, err
	}
	return outcome.Task, outcome.Changed, nil
}

// batchAttach hangs a task of the plan under the task the plan put it behind.
// The relationship is a guarded edit of its own, never a field of the create:
// a create that names a parent the server has not confirmed yet is held back
// whole, while a separate update leaves the child a top-level task on the
// server and is retried on its own. Both ids are read as the cache holds them
// now, because a sync between two steps of the batch may already have swapped
// a local id for the one the server gave it.
func batchAttach(ctx context.Context, st *store.Store, task model.Task, parentStepID string) (model.Task, error) {
	parent, err := st.CurrentTaskID(ctx, parentStepID)
	if err != nil {
		return task, err
	}
	id, err := st.CurrentTaskID(ctx, task.Id)
	if err != nil {
		return task, err
	}
	stored, err := st.Task(ctx, id)
	if err != nil {
		return task, err
	}
	outcome, err := st.UpdateTaskIfUnchanged(ctx, stored, model.TaskEdit{ParentId: model.Ptr(parent)})
	if err != nil {
		// The store refuses a parent whose own create is no longer the only
		// change queued for it. Inside a batch the create was queued moments
		// ago, so it is a sync of another run that took it, and the refusal
		// it words for tt edit would tell the reader to sync while a sync is
		// what went wrong.
		if errors.Is(err, store.ErrParentUnsynced) {
			return task, ErrBatchParentTaken
		}
		return task, err
	}
	return outcome.Task, nil
}

// batchRollback removes the tasks recorded under group since floor, the same
// way a failed plan of tt ai is reversed, and reports what the undo history
// would not let go of as a BatchRollbackBlockedError.
func batchRollback(ctx context.Context, st *store.Store, group string, floor int64) error {
	err := rollbackAIGroup(ctx, st, group, floor)
	var blocked *AIRollbackBlockedError
	if errors.As(err, &blocked) {
		return &BatchRollbackBlockedError{Remaining: len(blocked.TaskIDs), TaskIDs: blocked.TaskIDs,
			Above: blocked.Above, Contiguous: blocked.Contiguous, AboveTouches: blocked.AboveTouches}
	}
	return err
}
