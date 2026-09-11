package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/model"
)

type TaskExtensionsPreview struct {
	Task model.Task
	Edit model.TaskEdit
}

func hasTaskExtensions(edit model.TaskEdit) bool {
	return edit.ParentId != nil || edit.ColumnId != nil || edit.EstimatedDuration != nil || edit.EstimatedPomo != nil
}

func taskExtensionsOnly(edit model.TaskEdit) model.TaskEdit {
	var out model.TaskEdit
	if edit.ParentId != nil {
		out.ParentId = model.Ptr(*edit.ParentId)
	}
	if edit.ColumnId != nil {
		out.ColumnId = model.Ptr(*edit.ColumnId)
	}
	if edit.EstimatedDuration != nil {
		out.EstimatedDuration = model.Ptr(*edit.EstimatedDuration)
	}
	if edit.EstimatedPomo != nil {
		out.EstimatedPomo = model.Ptr(*edit.EstimatedPomo)
	}
	return out
}

func onlyTaskExtensions(edit model.TaskEdit) bool {
	edit.ParentId, edit.ColumnId, edit.EstimatedDuration, edit.EstimatedPomo = nil, nil, nil, nil
	return edit.IsEmpty()
}

func extensionEditOfTask(task model.Task) model.TaskEdit {
	var edit model.TaskEdit
	if task.ParentId != "" {
		edit.ParentId = model.Ptr(task.ParentId)
	}
	if task.ColumnId != "" {
		edit.ColumnId = model.Ptr(task.ColumnId)
	}
	if task.EstimatedDuration != 0 {
		edit.EstimatedDuration = model.Ptr(task.EstimatedDuration)
	}
	if task.EstimatedPomo != 0 {
		edit.EstimatedPomo = model.Ptr(task.EstimatedPomo)
	}
	return edit
}

func validateTaskExtensions(ctx context.Context, q execer, task model.Task, edit model.TaskEdit) error {
	if edit.EstimatedDuration != nil || edit.EstimatedPomo != nil {
		if _, err := model.ParseTaskEstimates(task.FocusSummaries); err != nil {
			return fmt.Errorf("cannot edit task estimates: %w", err)
		}
	}
	if edit.EstimatedDuration != nil && *edit.EstimatedDuration < 0 {
		return errors.New("estimated duration must be a nonnegative number of seconds")
	}
	if edit.EstimatedPomo != nil && (*edit.EstimatedPomo < 0 || *edit.EstimatedPomo > 60) {
		return errors.New("estimated Pomodoros must be between 0 and 60")
	}
	if edit.ParentId != nil && *edit.ParentId != "" {
		id := *edit.ParentId
		if IsLocalID(id) {
			return errors.New("sync the parent task before attaching a child")
		}
		seen := map[string]bool{task.Id: true}
		for id != "" {
			if seen[id] {
				return errors.New("parent relationship would create a task cycle")
			}
			seen[id] = true
			parent, err := loadTask(ctx, q, id)
			if err != nil {
				return fmt.Errorf("parent task must be cached before attaching a child: %w", err)
			}
			if parent.ProjectId != task.ProjectId {
				return errors.New("parent and child must belong to the same project")
			}
			if parent.Status != model.TaskOpen {
				return errors.New("parent task must be open")
			}
			var pending int
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=?`, id).Scan(&pending); err != nil {
				return err
			}
			if pending != 0 {
				return errors.New("sync or recover changes to the parent chain before attaching a child")
			}
			if len(seen) > 1000 {
				return errors.New("parent relationship exceeds the supported depth")
			}
			id = parent.ParentId
		}
	}
	if edit.ColumnId != nil {
		if !columnOnlyEdit(edit) {
			return errors.New("move a Kanban card separately from other task edits")
		}
		_, err := validateTaskColumn(ctx, q, task, *edit.ColumnId)
		return err
	}
	return nil
}

func (s *Store) PreviewTaskExtensions(ctx context.Context, original model.Task, edit model.TaskEdit) (TaskExtensionsPreview, error) {
	if !hasTaskExtensions(edit) || !onlyTaskExtensions(edit) {
		return TaskExtensionsPreview{}, errors.New("expected a task extension edit")
	}
	edit = taskExtensionsOnly(edit)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := loadTask(ctx, tx, original.Id)
		if err != nil {
			return err
		}
		if !samePulledTask(current, original) {
			return ErrTaskChanged
		}
		return validateTaskExtensions(ctx, tx, current, edit)
	})
	return TaskExtensionsPreview{Task: original, Edit: edit}, err
}

func (s *Store) ApplyTaskExtensions(ctx context.Context, preview TaskExtensionsPreview) (TaskMutationOutcome, error) {
	if !hasTaskExtensions(preview.Edit) || !onlyTaskExtensions(preview.Edit) {
		return TaskMutationOutcome{}, errors.New("expected a task extension edit")
	}
	return s.UpdateTaskIfUnchanged(ctx, preview.Task, taskExtensionsOnly(preview.Edit))
}

func confirmTaskExtensions(root map[string]json.RawMessage, edit model.TaskEdit) error {
	return matchTaskExtensions(root, edit, false)
}

func matchTaskExtensions(root map[string]json.RawMessage, edit model.TaskEdit, allowAbsent bool) error {
	for _, field := range []struct {
		name  string
		value *string
	}{{"parentId", edit.ParentId}, {"columnId", edit.ColumnId}} {
		if field.value != nil {
			if err := exactRawString(root, field.name, *field.value); err != nil {
				return err
			}
		}
	}
	var estimates model.TaskEstimateFields
	if edit.EstimatedDuration != nil || edit.EstimatedPomo != nil {
		var err error
		estimates, err = model.ParseTaskEstimates(root["focusSummaries"])
		if err != nil {
			return err
		}
	}
	if edit.EstimatedDuration != nil {
		if estimates.EstimatedDuration == nil && allowAbsent && *edit.EstimatedDuration == 0 {
			estimates.EstimatedDuration = model.Ptr(int64(0))
		}
		if estimates.EstimatedDuration == nil || *estimates.EstimatedDuration != *edit.EstimatedDuration {
			return errors.New("estimated duration was not confirmed")
		}
	}
	if edit.EstimatedPomo != nil {
		if estimates.EstimatedPomo == nil && allowAbsent && *edit.EstimatedPomo == 0 {
			estimates.EstimatedPomo = model.Ptr(0)
		}
		if estimates.EstimatedPomo == nil || *estimates.EstimatedPomo != *edit.EstimatedPomo {
			return errors.New("estimated Pomodoros were not confirmed")
		}
	}
	return nil
}

func TaskExtensionBaseline(item OutboxItem) (*model.TaskEdit, error) {
	if item.Op != OpTaskUpdate && item.Op != OpTaskMove {
		return nil, nil
	}
	_, metadata, err := DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return nil, err
	}
	if metadata == nil || !metadata.Fields.Extensions || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected {
		return nil, nil
	}
	baseline := taskExtensionsOnly(*metadata.ExtensionBaseline)
	return &baseline, nil
}

func TaskExtensionParent(item OutboxItem) (string, bool, error) {
	var parent *string
	var metadata *FeaturePayloadMetadata
	var err error
	switch item.Op {
	case OpTaskCreate:
		var task model.Task
		task, metadata, err = DecodeTaskPayload(item.Payload)
		if task.ParentId != "" {
			parent = model.Ptr(task.ParentId)
		}
	case OpTaskUpdate, OpTaskMove:
		var edit model.TaskEdit
		edit, metadata, err = DecodeTaskEditPayload(item.Payload)
		parent = edit.ParentId
	default:
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if metadata == nil || !metadata.Fields.Extensions || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected || parent == nil || *parent == "" {
		return "", false, nil
	}
	return *parent, true, nil
}

func TaskExtensionColumn(item OutboxItem) (string, bool, error) {
	if item.Op != OpTaskUpdate && item.Op != OpTaskMove {
		return "", false, nil
	}
	edit, metadata, err := DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return "", false, err
	}
	if metadata == nil || !metadata.Fields.Extensions || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected || edit.ColumnId == nil {
		return "", false, nil
	}
	if !columnOnlyEdit(edit) || *edit.ColumnId == "" {
		return "", false, errors.New("invalid Kanban column mutation")
	}
	return *edit.ColumnId, true, nil
}

func CheckTaskExtensionBaseline(raw []byte, taskID, projectID string, baseline model.TaskEdit) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if err := exactRawString(root, "id", taskID); err != nil {
		return err
	}
	if err := exactRawString(root, "projectId", projectID); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"parentId", baseline.ParentId}, {"columnId", baseline.ColumnId}} {
		if field.value == nil {
			continue
		}
		if _, present := root[field.name]; !present && *field.value == "" {
			root[field.name] = json.RawMessage(`""`)
		}
	}
	if err := matchTaskExtensions(root, baseline, true); err != nil {
		return fmt.Errorf("task extension conflict; review current server values: %w", err)
	}
	return nil
}

func (s *Store) RecordTaskRequestUnsent(ctx context.Context, item OutboxItem) error {
	result, err := s.db.ExecContext(ctx, `UPDATE outbox SET sent_at=NULL WHERE seq=? AND state='inflight' AND lease_token=?`, item.Seq, string(item.LeaseToken))
	if err != nil {
		return err
	}
	return affectedOne(ctx, s.db, result, item.Seq)
}
