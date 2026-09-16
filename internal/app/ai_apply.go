package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

// ErrAIPlanStale refuses a plan whose tasks changed after the preview. Nothing
// was applied.
var ErrAIPlanStale = errors.New("a task this preview changes has changed since the preview")

// ErrAIOverlapChanged refuses a planned interval that now overlaps a task the
// preview did not show.
var ErrAIOverlapChanged = errors.New("the planned interval now overlaps a task the preview did not show")

// AIApplyError reports the step that failed. Rollback is nil when every change
// made before it was reversed again; otherwise it says why that failed, and
// part of the plan may still be applied.
type AIApplyError struct {
	Step     int
	Op       string
	Err      error
	Rollback error
}

func (e *AIApplyError) Error() string {
	return fmt.Sprintf("change %d (%s): %v", e.Step, e.Op, e.Err)
}

func (e *AIApplyError) Unwrap() error { return e.Err }

type AIStepResult struct {
	Step    int        `json:"step"`
	Op      string     `json:"op"`
	Task    model.Task `json:"task"`
	Changed bool       `json:"changed"`
}

// AIApplied is a plan that went through: every change carries GroupID in the
// undo history, so one undo reverses all of them.
type AIApplied struct {
	GroupID string         `json:"group_id"`
	Results []AIStepResult `json:"changes"`
}

type aiApply struct {
	st      *store.Store
	ctx     context.Context
	current map[string]model.Task
	touched map[string]bool
}

// aiSameTask compares a cached task with the copy a preview saved. The saved
// copy went through JSON, which keeps every value but not the time.Location
// of its times, so the comparison is made on the JSON of both.
func aiSameTask(cached, saved model.Task) bool {
	a, errA := json.Marshal(cached)
	b, errB := json.Marshal(saved)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// AIPlanFresh reports whether plan can still be applied as previewed: nil
// when every task it changes is in the cache as the preview saved it, and
// ErrAIPlanStale when one is not. Reversing a failed apply changes the tasks
// it edited, so a preview whose plan edits any is stale after that.
func AIPlanFresh(ctx context.Context, st *store.Store, plan AIPlan) error {
	_, err := aiPlanTasks(ctx, st, plan)
	return err
}

// aiPlanTasks returns the cached copies of the tasks plan changes, refusing a
// plan with ErrAIPlanStale when one of them changed since the preview.
func aiPlanTasks(ctx context.Context, st *store.Store, plan AIPlan) (map[string]model.Task, error) {
	current := map[string]model.Task{}
	for _, step := range plan.Steps {
		if step.Task == nil {
			continue
		}
		task, err := st.Task(ctx, step.Task.Id)
		if errors.Is(err, store.ErrNotFound) || err == nil && !aiSameTask(task, *step.Task) {
			return nil, ErrAIPlanStale
		}
		if err != nil {
			return nil, err
		}
		current[task.Id] = task
	}
	return current, nil
}

// ApplyAIPlan applies every step of plan through the same store paths the
// matching tt commands use, all under one fresh undo group. When a step fails
// it reverses what the steps before it did.
func ApplyAIPlan(ctx context.Context, st *store.Store, plan AIPlan) (AIApplied, error) {
	current, err := aiPlanTasks(ctx, st, plan)
	if err != nil {
		return AIApplied{}, err
	}

	floor, err := aiUndoFloor(ctx, st)
	if err != nil {
		return AIApplied{}, err
	}
	group, err := store.NewUndoGroupID()
	if err != nil {
		return AIApplied{}, err
	}
	a := aiApply{st: st, ctx: store.WithUndoGroup(ctx, group), current: current, touched: map[string]bool{}}
	out := AIApplied{GroupID: group, Results: make([]AIStepResult, 0, len(plan.Steps))}
	for i, step := range plan.Steps {
		err := ctx.Err()
		var outcome store.TaskMutationOutcome
		if err == nil {
			outcome, err = a.step(step)
		}
		if err != nil {
			return AIApplied{}, &AIApplyError{Step: i + 1, Op: step.Op, Err: err,
				Rollback: rollbackAIGroup(context.WithoutCancel(ctx), st, group, floor)}
		}
		a.touched[outcome.Task.Id] = true
		out.Results = append(out.Results, AIStepResult{Step: i + 1, Op: step.Op, Task: outcome.Task, Changed: outcome.Changed})
	}
	return out, nil
}

// original is the task a step starts from: the copy checked against the
// preview the first time the plan touches it, so a change made since is
// refused, and the cache's copy after an earlier step of the plan changed it.
func (a aiApply) original(step AIStep) (model.Task, error) {
	if !a.touched[step.Task.Id] {
		return a.current[step.Task.Id], nil
	}
	return a.st.Task(a.ctx, step.Task.Id)
}

func (a aiApply) step(step AIStep) (store.TaskMutationOutcome, error) {
	if step.Op == AIOpAdd {
		task := aiNewTask(step)
		if step.Interval != nil {
			return a.interval(step, nil, model.TaskEdit{}, task)
		}
		created, err := a.st.CreateTask(a.ctx, task)
		return store.TaskMutationOutcome{Task: created, Changed: err == nil}, err
	}
	original, err := a.original(step)
	if err != nil {
		return store.TaskMutationOutcome{}, err
	}
	switch step.Op {
	case AIOpEdit:
		edit, err := aiEdit(original, step)
		if err != nil {
			return store.TaskMutationOutcome{}, err
		}
		if schedule.ReviewsInterval(original, edit) {
			return a.interval(step, &original, edit, model.Task{})
		}
		return a.st.UpdateTaskIfUnchanged(a.ctx, original, edit)
	case AIOpSchedule:
		edit, err := schedule.IntervalEdit(original, *step.Interval)
		if err != nil {
			return store.TaskMutationOutcome{}, err
		}
		return a.interval(step, &original, edit, model.Task{})
	case AIOpDone:
		return a.st.CompleteTaskIfUnchanged(a.ctx, original, store.CompleteOptions{})
	case AIOpMove:
		preview, err := a.st.PreviewNativeMove(a.ctx, original, step.Project.Id)
		if err != nil {
			return store.TaskMutationOutcome{}, err
		}
		return a.st.ApplyNativeMove(a.ctx, preview)
	case AIOpChecklistAdd:
		var outcome store.TaskMutationOutcome
		for i, title := range step.Checklist {
			if i > 0 {
				a.touched[original.Id] = true
				if original, err = a.original(step); err != nil {
					return store.TaskMutationOutcome{}, err
				}
			}
			next, err := a.st.ChangeTaskItemIfUnchanged(a.ctx, original, store.ItemChange{Action: store.ItemAdd, Title: title})
			if err != nil {
				return store.TaskMutationOutcome{}, err
			}
			outcome.Task, outcome.Changed = next.Task, outcome.Changed || next.Changed
		}
		return outcome, nil
	}
	return store.TaskMutationOutcome{}, fmt.Errorf("unsupported operation %s", step.Op)
}

// interval saves a planned interval the way tt add --schedule and tt schedule
// do. The overlaps the preview showed were accepted with it, and so are tasks
// this plan itself touched; any other overlap refuses the step.
func (a aiApply) interval(step AIStep, original *model.Task, edit model.TaskEdit, create model.Task) (store.TaskMutationOutcome, error) {
	preview, err := a.st.PrepareInterval(a.ctx, original, edit, create)
	if err != nil {
		return store.TaskMutationOutcome{}, err
	}
	for _, overlap := range preview.Overlaps {
		shown := slices.ContainsFunc(step.Overlaps, func(o AIOverlap) bool { return o.TaskID == overlap.Task.Id })
		if !shown && !a.touched[overlap.Task.Id] {
			return store.TaskMutationOutcome{}, ErrAIOverlapChanged
		}
	}
	return a.st.ApplyInterval(a.ctx, preview, len(preview.Overlaps) != 0)
}

func aiUndoFloor(ctx context.Context, st *store.Store) (int64, error) {
	top, err := st.LastUndo(ctx)
	switch {
	case errors.Is(err, store.ErrNoUndo):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return top.Seq, nil
}

// ErrAIRollbackBlocked reports that a change of another run was recorded among
// or above this plan's changes, so the undo history cannot reverse them as one
// group. The error a rollback returns is an AIRollbackBlockedError.
var ErrAIRollbackBlocked = errors.New("another change was recorded in between this request's changes")

// AIRollbackBlockedError is what a rollback stopped by ErrAIRollbackBlocked
// left applied: Remaining changes of the request, to the tasks TaskIDs names.
// Above counts the undo steps recorded on top of the newest of them, each one
// change or one group as tt undo takes them; Contiguous is false when changes
// of other runs sit among them as well. AboveTouches is true when a change on
// top of them may have changed one of their tasks - it names one, or names no
// task at all - so reversing them would put back what that change replaced.
type AIRollbackBlockedError struct {
	Remaining    int
	TaskIDs      []string
	Above        int
	Contiguous   bool
	AboveTouches bool
}

func (e *AIRollbackBlockedError) Error() string {
	if e.Remaining == 1 {
		return ErrAIRollbackBlocked.Error() + ", so 1 of them is still applied"
	}
	return fmt.Sprintf("%s, so %d of them are still applied", ErrAIRollbackBlocked, e.Remaining)
}

func (e *AIRollbackBlockedError) Is(target error) bool { return target == ErrAIRollbackBlocked }

// rollbackAIGroup reverses the changes recorded under group since floor, the
// top of the undo history before the plan started. The history reverses a
// group only from its top, so when changes of another run were recorded among
// or above the plan's, it reverses what lies above them and returns an
// AIRollbackBlockedError for the rest. The history may end below floor:
// another tt undo can reverse older changes meanwhile.
func rollbackAIGroup(ctx context.Context, st *store.Store, group string, floor int64) error {
	entries, err := st.LastUndoGroup(ctx)
	switch {
	case errors.Is(err, store.ErrNoUndo):
		return nil
	case err != nil:
		return err
	}
	if entries[0].Group == group {
		if _, err := st.ApplyUndoGroup(ctx, entries); err != nil {
			return err
		}
	}
	return aiRollbackLeft(ctx, st, group, floor)
}

// aiRollbackLeft reads what of group the undo history still holds above
// floor. Nothing left is a finished rollback.
func aiRollbackLeft(ctx context.Context, st *store.Store, group string, floor int64) error {
	rows, err := st.DB().QueryContext(ctx, `SELECT payload, coalesce(group_id, '') FROM undo_log WHERE seq > ? ORDER BY seq DESC`, floor)
	if err != nil {
		return fmt.Errorf("read undo log: %w", err)
	}
	defer rows.Close()
	left := &AIRollbackBlockedError{Contiguous: true}
	last, passed, unnamed := "", false, false
	above := map[string]bool{}
	for rows.Next() {
		var payload []byte
		var owner string
		if err := rows.Scan(&payload, &owner); err != nil {
			return fmt.Errorf("read undo log: %w", err)
		}
		action, _, err := store.DecodeUndoAction(payload)
		named := err == nil && action.TaskID != ""
		switch {
		case owner == group:
			left.Contiguous = left.Contiguous && !passed
			left.Remaining++
			unnamed = unnamed || !named
			if named && !slices.Contains(left.TaskIDs, action.TaskID) {
				left.TaskIDs = append(left.TaskIDs, action.TaskID)
			}
		case left.Remaining > 0:
			passed = true
		default:
			if owner == "" || owner != last {
				left.Above++
			}
			unnamed = unnamed || !named
			above[action.TaskID] = true
		}
		last = owner
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read undo log: %w", err)
	}
	if left.Remaining == 0 {
		return nil
	}
	if left.Above > 0 {
		left.AboveTouches = unnamed || slices.ContainsFunc(left.TaskIDs, func(id string) bool { return above[id] })
	}
	return left
}
