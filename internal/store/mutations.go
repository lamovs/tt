package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/model"
)

const (
	OpTaskCreate   = "task.create"
	OpTaskUpdate   = "task.update"
	OpTaskComplete = "task.complete"

	OpTaskMove   = "task.move"
	OpTaskDelete = "task.delete"

	OpTaskMoveDrop = "task.move_drop"

	OpTaskMoveRecreate = "task.move_recreate"
)

type MoveDrop struct {
	CopyID        string `json:"copy_id"`
	CopyProjectID string `json:"copy_project_id"`
}

const (
	TaskCreatedKind   = "task.created"
	TaskUpdatedKind   = "task.updated"
	TaskCompletedKind = "task.completed"
	TaskReopenedKind  = "task.reopened"
	TaskMovedKind     = "task.moved"
	TaskDeletedKind   = "task.deleted"
)

type CompleteOptions struct {
	KeepItems bool
}

type MoveOptions struct {
	ByRecreate bool
}

var ErrMoveNeedsRecreate = errors.New("move task: the task can only be moved by recreating it")

var ErrMoveCompleted = errors.New("move task: a completed task cannot be moved between lists")

var ErrMoveChained = errors.New("move task: this task is the copy of a move that has not been sent yet")

func (s *Store) CreateTask(ctx context.Context, t model.Task) (model.Task, error) {
	return s.createTask(ctx, t, false, true)
}

func (s *Store) createTask(ctx context.Context, t model.Task, preserveRanks, recordUndo bool) (model.Task, error) {
	if t.ProjectId == "" {
		return model.Task{}, errors.New("create task: list id is required")
	}
	if !IsLocalID(t.Id) {
		id, err := NewLocalID()
		if err != nil {
			return model.Task{}, err
		}
		t.Id = id
	}

	now := model.NewTime(time.Now())
	if t.CreatedTime.IsZero() {
		t.CreatedTime = now
	}
	if t.ModifiedTime.IsZero() {
		t.ModifiedTime = now
	}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		return createTaskTx(ctx, tx, &t, preserveRanks, recordUndo)
	})
	if err != nil {
		return model.Task{}, err
	}
	return t, nil
}

func createTaskTx(ctx context.Context, tx *sql.Tx, t *model.Task, preserveRanks, recordUndo bool) error {
	if err := validateTaskExtensions(ctx, tx, *t, extensionEditOfTask(*t)); err != nil {
		return err
	}
	if t.Items != nil && len(t.Items) != 0 && (t.Kind == "" || t.Kind == "TEXT") {
		t.Kind = "CHECKLIST"
	}
	name, err := projectName(ctx, tx, t.ProjectId)
	if err != nil {
		return err
	}
	rev, err := writeLocalTask(ctx, tx, *t, name)
	if err != nil {
		return err
	}
	fields, err := prepareCreatedTask(ctx, tx, t, preserveRanks)
	if err != nil {
		return err
	}
	if err := replaceItems(ctx, tx, t.Id, t.Items); err != nil {
		return err
	}
	payload, err := encodeCreatedTask(*t, fields)
	if err != nil {
		return fmt.Errorf("create task %s: %w", t.Id, err)
	}
	if err := queue(ctx, tx, OpTaskCreate, t.Id, t.ProjectId, rev, payload); err != nil {
		return err
	}
	if err := journal(ctx, tx, TaskCreatedKind, *t, nil); err != nil {
		return err
	}
	if !fields.empty() {
		if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
			return err
		}
	}
	if recordUndo {
		return pushUndo(ctx, tx, UndoAction{Op: OpTaskCreate, TaskID: t.Id, ProjectID: t.ProjectId})
	}
	return nil
}

type TaskMutationOutcome struct {
	Task    model.Task
	Changed bool
}

func (s *Store) UpdateTaskOutcome(ctx context.Context, id string, e model.TaskEdit) (TaskMutationOutcome, error) {
	return s.editTaskOutcome(ctx, id, OpTaskUpdate, TaskUpdatedKind,
		func(model.Task) (model.TaskEdit, error) { return e, nil })
}

func (s *Store) UpdateTask(ctx context.Context, id string, e model.TaskEdit) (model.Task, error) {
	outcome, err := s.UpdateTaskOutcome(ctx, id, e)
	return outcome.Task, err
}

func (s *Store) CompleteTask(ctx context.Context, id string, opts CompleteOptions) (model.Task, error) {
	return s.editTask(ctx, id, OpTaskComplete, TaskCompletedKind, func(cur model.Task) (model.TaskEdit, error) {
		return completionEdit(cur, opts), nil
	})
}

func completionEdit(cur model.Task, opts CompleteOptions) model.TaskEdit {
	now := model.NewTime(time.Now())
	var e model.TaskEdit
	if !cur.Status.Done() || cur.CompletedTime.IsZero() {
		e.Status = model.Ptr(model.TaskDone)
		e.CompletedTime = model.NewEditTime(now)
	}
	if opts.KeepItems || len(cur.Items) == 0 {
		return e
	}
	items := append([]model.Item(nil), cur.Items...)
	changed := false
	for i := range items {
		if items[i].Status.Done() && !items[i].CompletedTime.IsZero() {
			continue
		}
		items[i].Status = model.ItemDone
		items[i].CompletedTime = now
		changed = true
	}
	if changed {
		e.Items = model.NewEditList(items)
	}
	return e
}

func (s *Store) ReopenTask(ctx context.Context, id string) (model.Task, error) {
	return s.editTask(ctx, id, OpTaskUpdate, TaskReopenedKind, func(model.Task) (model.TaskEdit, error) {
		return model.TaskEdit{
			Status:        model.Ptr(model.TaskOpen),
			CompletedTime: model.NewEditTime(model.Time{}),
		}, nil
	})
}

func (s *Store) MoveTask(ctx context.Context, id, projectID string, opts MoveOptions) (model.Task, error) {
	if projectID == "" {
		return model.Task{}, errors.New("move task: list id is required")
	}
	var out model.Task
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = moveTaskTx(ctx, tx, id, projectID, opts)
		return err
	})
	if err != nil {
		return model.Task{}, err
	}
	return out, nil
}

func moveTaskTx(ctx context.Context, tx *sql.Tx, id, projectID string, opts MoveOptions) (model.Task, error) {
	if err := nativeMovePending(ctx, tx, id); err != nil {
		return model.Task{}, err
	}
	cur, err := loadTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, err
	}
	if cur.ProjectId == projectID {
		return cur, nil
	}
	if cur.Status != model.TaskOpen {
		return model.Task{}, ErrMoveCompleted
	}
	if !opts.ByRecreate {
		return model.Task{}, ErrMoveNeedsRecreate
	}
	chained, err := chainedMove(ctx, tx, cur.Id)
	if err != nil {
		return model.Task{}, err
	}
	if chained {
		return model.Task{}, ErrMoveChained
	}
	return moveByRecreate(ctx, tx, cur, projectID)
}

func chainedMove(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM outbox WHERE op = ?`, OpTaskMoveDrop)
	if err != nil {
		return false, fmt.Errorf("move task %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return false, fmt.Errorf("move task %s: %w", id, err)
		}
		var d MoveDrop
		if err := json.Unmarshal(payload, &d); err != nil {
			return false, fmt.Errorf("move task %s: decode a queued move: %w", id, err)
		}
		if d.CopyID == id {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("move task %s: %w", id, err)
	}

	return false, rows.Close()
}

func moveByRecreate(ctx context.Context, tx *sql.Tx, cur model.Task, projectID string) (model.Task, error) {
	if err := validateTaskMoveChildren(ctx, tx, cur.Id); err != nil {
		return model.Task{}, err
	}
	if cur.ParentId != "" || len(cur.ChildIds) != 0 || cur.ColumnId != "" || len(cur.FocusSummaries) != 0 {
		return model.Task{}, errors.New("recreate move cannot preserve task relationships; use a verified native move")
	}
	if err := validateItemsForCopy(ctx, tx, cur); err != nil {
		return model.Task{}, err
	}
	oldRegistered, err := taskHasItemControl(ctx, tx, cur.Id)
	if err != nil {
		return model.Task{}, err
	}
	copyID, err := NewLocalID()
	if err != nil {
		return model.Task{}, err
	}
	now := model.NewTime(time.Now())
	next := cur
	next.Id = copyID
	next.ProjectId = projectID
	next.CreatedTime = now
	next.ModifiedTime = now
	next.Items = itemsOfACopy(cur.Items)

	name, err := projectName(ctx, tx, projectID)
	if err != nil {
		return model.Task{}, err
	}
	rev, err := writeLocalTask(ctx, tx, next, name)
	if err != nil {
		return model.Task{}, err
	}
	fields, err := prepareCreatedTask(ctx, tx, &next, true)
	if err != nil {
		return model.Task{}, err
	}
	if err := replaceItems(ctx, tx, next.Id, next.Items); err != nil {
		return model.Task{}, err
	}
	payload, err := encodeCreatedTask(next, fields)
	if err != nil {
		return model.Task{}, fmt.Errorf("move task %s: %w", cur.Id, err)
	}
	if err := queue(ctx, tx, OpTaskCreate, next.Id, projectID, rev, payload); err != nil {
		return model.Task{}, err
	}

	send, err := needsRemoteDelete(ctx, tx, cur.Id)
	if err != nil {
		return model.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, cur.Id); err != nil {
		return model.Task{}, fmt.Errorf("move task %s: %w", cur.Id, err)
	}
	cond := `task_id = ?`
	args := []any{cur.Id}
	if IsLocalID(cur.Id) {
		cond += ` AND NOT (state = ? AND op = ?)`
		args = append(args, string(OutboxInflight), OpTaskCreate)
	}
	if _, _, err := discardQueued(ctx, tx, cond, args...); err != nil {
		return model.Task{}, fmt.Errorf("move task %s: %w", cur.Id, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET rev = NULL WHERE task_id = ?`, cur.Id); err != nil {
		return model.Task{}, fmt.Errorf("move task %s: %w", cur.Id, err)
	}
	if send {
		drop, err := json.Marshal(MoveDrop{CopyID: next.Id, CopyProjectID: projectID})
		if err != nil {
			return model.Task{}, fmt.Errorf("move task %s: %w", cur.Id, err)
		}

		if err := queue(ctx, tx, OpTaskMoveDrop, cur.Id, cur.ProjectId, 0, drop); err != nil {
			return model.Task{}, err
		}
	}
	if err := journalEvent(ctx, tx, TaskMovedKind, taskEvent{
		ID: next.Id, ProjectID: next.ProjectId, Title: next.Title, FromID: cur.Id,
	}); err != nil {
		return model.Task{}, err
	}
	if oldRegistered || !fields.empty() {
		if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
			return model.Task{}, err
		}
	}

	return next, pushUndo(ctx, tx, UndoAction{
		Op: OpTaskMoveRecreate, TaskID: next.Id, ProjectID: next.ProjectId,
	})
}

func itemsOfACopy(items []model.Item) []model.Item {
	if len(items) == 0 {
		return nil
	}
	out := make([]model.Item, len(items))
	copy(out, items)
	for i := range out {
		out[i].Id = ""
		out[i].Key = ""
	}
	return out
}

func (s *Store) DeleteTask(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		_, err := deleteTaskTx(ctx, tx, id, true)
		return err
	})
}

func deleteTaskTx(ctx context.Context, tx *sql.Tx, id string, recordUndo bool) (model.Task, error) {
	if err := nativeMovePending(ctx, tx, id); err != nil {
		return model.Task{}, err
	}
	if err := validateUnsentChildLinks(ctx, tx, id); err != nil {
		return model.Task{}, err
	}
	cur, err := loadTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, err
	}
	registered, err := taskHasItemControl(ctx, tx, id)
	if err != nil {
		return model.Task{}, err
	}
	certifyUndoItems := len(cur.Items) != 0 && validateItemsForCopy(ctx, tx, cur) == nil

	send, err := needsRemoteDelete(ctx, tx, id)
	if err != nil {
		return model.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
		return model.Task{}, fmt.Errorf("delete task %s: %w", id, err)
	}

	cond := `task_id = ?`
	args := []any{id}
	if IsLocalID(id) {
		cond += ` AND NOT (state = ? AND op = ?)`
		args = append(args, string(OutboxInflight), OpTaskCreate)
	}
	if _, _, err := discardQueued(ctx, tx, cond, args...); err != nil {
		return model.Task{}, fmt.Errorf("delete task %s: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET rev = NULL WHERE task_id = ?`, id); err != nil {
		return model.Task{}, fmt.Errorf("delete task %s: %w", id, err)
	}
	if send {

		if err := queue(ctx, tx, OpTaskDelete, id, cur.ProjectId, 0, nil); err != nil {
			return model.Task{}, err
		}
	}
	if err := journal(ctx, tx, TaskDeletedKind, cur, nil); err != nil {
		return model.Task{}, err
	}
	if registered {
		if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
			return model.Task{}, err
		}
	}
	if recordUndo {
		action := UndoAction{Op: OpTaskDelete, TaskID: id, ProjectID: cur.ProjectId, Task: &cur}
		var err error
		if certifyUndoItems {
			err = pushFeatureUndo(ctx, tx, action, featureFieldsOfTask(cur))
		} else {
			err = pushUndo(ctx, tx, action)
		}
		if err != nil {
			return model.Task{}, err
		}
	}
	return cur, nil
}

func needsRemoteDelete(ctx context.Context, q execer, id string) (bool, error) {
	if !IsLocalID(id) {
		return true, nil
	}
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE task_id = ? AND op = ? AND attempts > 0`,
		id, OpTaskCreate).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("delete task %s: %w", id, err)
	}
	return n > 0, nil
}

func (s *Store) editTask(ctx context.Context, id, op, kind string, build func(model.Task) (model.TaskEdit, error)) (model.Task, error) {
	outcome, err := s.editTaskOutcome(ctx, id, op, kind, build)
	return outcome.Task, err
}

func (s *Store) editTaskOutcome(ctx context.Context, id, op, kind string, build func(model.Task) (model.TaskEdit, error)) (TaskMutationOutcome, error) {
	return s.editTaskWithTxOutcome(ctx, id, op, kind,
		func(_ context.Context, _ *sql.Tx, cur model.Task) (model.TaskEdit, []string, error) {
			e, err := build(cur)
			return e, nil, err
		})
}

func (s *Store) editTaskWithTxOutcome(ctx context.Context, id, op, kind string, build func(context.Context, *sql.Tx, model.Task) (model.TaskEdit, []string, error)) (TaskMutationOutcome, error) {
	var out TaskMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = editTaskTxOutcome(ctx, tx, id, op, kind, build, true)
		return err
	})
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	return out, nil
}

func editTaskTx(ctx context.Context, tx *sql.Tx, id, op, kind string, build func(context.Context, *sql.Tx, model.Task) (model.TaskEdit, []string, error), recordUndo bool) (model.Task, error) {
	outcome, err := editTaskTxOutcome(ctx, tx, id, op, kind, build, recordUndo)
	return outcome.Task, err
}

func editTaskTxOutcome(ctx context.Context, tx *sql.Tx, id, op, kind string, build func(context.Context, *sql.Tx, model.Task) (model.TaskEdit, []string, error), recordUndo bool) (TaskMutationOutcome, error) {
	if err := nativeMovePending(ctx, tx, id); err != nil {
		return TaskMutationOutcome{}, err
	}
	cur, err := loadTask(ctx, tx, id)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	e, newKeys, err := build(ctx, tx, cur)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	if err := validateTaskExtensions(ctx, tx, cur, e); err != nil {
		return TaskMutationOutcome{}, err
	}
	// Every way a task is closed builds its edit here - tt done, the key of
	// the browser, an applied tt ai plan, a batch that marked a task [x] -
	// so the closing end of the parent rule is read once, in the same
	// transaction as the write it guards.
	if err := validateTaskClosure(ctx, tx, cur, e); err != nil {
		return TaskMutationOutcome{}, err
	}
	if e.IsEmpty() {
		return TaskMutationOutcome{Task: cur}, nil
	}
	feature, err := prepareFeatureEdit(ctx, tx, cur, e, newKeys)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	if feature.noop {
		return TaskMutationOutcome{Task: cur}, nil
	}
	if !feature.fields.empty() {
		if err := commitFeatureRegistration(ctx, tx, id, feature); err != nil {
			return TaskMutationOutcome{}, err
		}
	}
	next := applyEdit(cur, e)
	next.ModifiedTime = model.NewTime(time.Now())
	name, err := projectName(ctx, tx, next.ProjectId)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	rev, err := writeLocalTask(ctx, tx, next, name)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	if e.Items != nil {
		if err := replaceItems(ctx, tx, next.Id, next.Items); err != nil {
			return TaskMutationOutcome{}, err
		}
	}
	var payload []byte
	if feature.fields.empty() {
		payload, err = json.Marshal(e)
	} else {
		metadata := FeaturePayloadMetadata{
			Version:  newFeaturePayloadVersion(feature.fields),
			Fields:   feature.fields,
			ItemKeys: itemKeyVector(e),
			Phase:    FeaturePrepared,
		}
		if feature.fields.Extensions {
			baseline := taskExtensionsOnly(beforeEdit(cur, e))
			metadata.ExtensionBaseline = &baseline
		}
		payload, err = EncodeTaskEditPayload(e, metadata)
	}
	if err != nil {
		return TaskMutationOutcome{}, fmt.Errorf("edit task %s: %w", id, err)
	}
	if err := queue(ctx, tx, op, id, cur.ProjectId, rev, payload); err != nil {
		return TaskMutationOutcome{}, err
	}
	if err := journal(ctx, tx, kind, next, &e); err != nil {
		return TaskMutationOutcome{}, err
	}
	if recordUndo {
		before := beforeEdit(cur, e)
		action := UndoAction{Op: op, TaskID: id, ProjectID: cur.ProjectId, Before: &before}
		if e.ColumnId != nil {
			if err := tx.QueryRowContext(ctx, `SELECT max(seq) FROM outbox WHERE task_id=?`, id).Scan(&action.OperationSeq); err != nil {
				return TaskMutationOutcome{}, err
			}
			action.ColumnNameBefore = model.Ptr(cur.ColumnName)
		}
		if feature.fields.empty() {
			if err := pushUndo(ctx, tx, action); err != nil {
				return TaskMutationOutcome{}, err
			}
		} else if err := pushFeatureUndo(ctx, tx, action, feature.fields); err != nil {
			return TaskMutationOutcome{}, err
		}
	}
	registered, err := taskHasItemControl(ctx, tx, id)
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	if registered {
		if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
			return TaskMutationOutcome{}, err
		}
	}
	return TaskMutationOutcome{Task: next, Changed: true}, nil
}

func itemKeyVector(e model.TaskEdit) []string {
	if e.Items == nil {
		return nil
	}
	items := []model.Item(*e.Items)
	keys := make([]string, len(items))
	for i := range items {
		keys[i] = items[i].Key
	}
	return keys
}

func queue(ctx context.Context, tx *sql.Tx, op, taskID, projectID string, rev int64, payload json.RawMessage) error {
	_, err := EnqueueTx(ctx, tx, OutboxEntry{
		Target:    TargetOpenAPI,
		Op:        op,
		TaskID:    taskID,
		ProjectID: projectID,
		Payload:   payload,
		Rev:       rev,
	})
	return err
}

type taskEvent struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id,omitempty"`
	Title     string          `json:"title,omitempty"`
	Change    *model.TaskEdit `json:"change,omitempty"`

	FromID string `json:"from_id,omitempty"`
}

func journal(ctx context.Context, tx *sql.Tx, kind string, t model.Task, e *model.TaskEdit) error {
	return journalEvent(ctx, tx, kind, taskEvent{
		ID: t.Id, ProjectID: t.ProjectId, Title: t.Title, Change: e,
	})
}

func journalEvent(ctx context.Context, tx *sql.Tx, kind string, ev taskEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("journal %s: %w", kind, err)
	}
	_, err = AppendEventTx(ctx, tx, kind, payload)
	return err
}

func applyEdit(t model.Task, e model.TaskEdit) model.Task {
	if e.ParentId != nil {
		t.ParentId = *e.ParentId
	}
	if e.ColumnId != nil {
		t.ColumnId = *e.ColumnId
		t.ColumnName = ""
	}
	if e.EstimatedDuration != nil {
		t.EstimatedDuration = *e.EstimatedDuration
	}
	if e.EstimatedPomo != nil {
		t.EstimatedPomo = *e.EstimatedPomo
	}
	if e.Title != nil {
		t.Title = *e.Title
	}
	if e.Content != nil {
		t.Content = *e.Content
	}
	if e.Priority != nil {
		t.Priority = *e.Priority
	}
	if e.Status != nil {
		t.Status = *e.Status
	}
	if e.DueDate != nil {
		t.DueDate = e.DueDate.Time
	}
	if e.StartDate != nil {
		t.StartDate = e.StartDate.Time
	}
	if e.CompletedTime != nil {
		t.CompletedTime = e.CompletedTime.Time
	}
	if e.IsAllDay != nil {
		t.IsAllDay = *e.IsAllDay
	}
	if e.TimeZone != nil {
		t.TimeZone = *e.TimeZone
	}
	if e.RepeatFlag != nil {
		t.RepeatFlag = *e.RepeatFlag
	}
	if e.Reminders != nil {
		t.Reminders = append([]string(nil), *e.Reminders...)
	}
	if e.Tags != nil {
		t.Tags = append([]string(nil), *e.Tags...)
	}
	if e.Kind != nil {
		t.Kind = *e.Kind
	}
	if e.SortOrder != nil {
		t.SortOrder = *e.SortOrder
	}
	if e.Items != nil {
		t.Items = append([]model.Item(nil), *e.Items...)
	}
	if e.ProjectId != nil {
		t.ProjectId = *e.ProjectId
	}
	return t
}

func beforeEdit(t model.Task, e model.TaskEdit) model.TaskEdit {
	var b model.TaskEdit
	if e.ParentId != nil {
		b.ParentId = model.Ptr(t.ParentId)
	}
	if e.ColumnId != nil {
		b.ColumnId = model.Ptr(t.ColumnId)
	}
	if e.EstimatedDuration != nil {
		b.EstimatedDuration = model.Ptr(t.EstimatedDuration)
	}
	if e.EstimatedPomo != nil {
		b.EstimatedPomo = model.Ptr(t.EstimatedPomo)
	}
	if e.Title != nil {
		b.Title = model.Ptr(t.Title)
	}
	if e.Content != nil {
		b.Content = model.Ptr(t.Content)
	}
	if e.Priority != nil {
		b.Priority = model.Ptr(t.Priority)
	}
	if e.Status != nil {
		b.Status = model.Ptr(t.Status)
	}
	if e.DueDate != nil {
		b.DueDate = model.NewEditTime(t.DueDate)
	}
	if e.StartDate != nil {
		b.StartDate = model.NewEditTime(t.StartDate)
	}
	if e.CompletedTime != nil {
		b.CompletedTime = model.NewEditTime(t.CompletedTime)
	}
	if e.IsAllDay != nil {
		b.IsAllDay = model.Ptr(t.IsAllDay)
	}
	if e.TimeZone != nil {
		b.TimeZone = model.Ptr(t.TimeZone)
	}
	if e.RepeatFlag != nil {
		b.RepeatFlag = model.Ptr(t.RepeatFlag)
	}
	if e.Reminders != nil {
		b.Reminders = model.NewEditList(append([]string(nil), t.Reminders...))
	}
	if e.Tags != nil {
		b.Tags = model.NewEditList(append([]string(nil), t.Tags...))
	}
	if e.Kind != nil {
		b.Kind = model.Ptr(t.Kind)
	}
	if e.SortOrder != nil {
		b.SortOrder = model.Ptr(t.SortOrder)
	}
	if e.Items != nil {
		b.Items = model.NewEditList(append([]model.Item(nil), t.Items...))
	}
	if e.ProjectId != nil {
		b.ProjectId = model.Ptr(t.ProjectId)
	}
	return b
}
