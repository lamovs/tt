package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/model"
)

type TaskColumnPreview struct {
	Task               model.Task
	SourceName         string
	DestinationID      string
	DestinationName    string
	Version            string
	DestinationVersion string
}

func columnOnlyEdit(edit model.TaskEdit) bool {
	if edit.ColumnId == nil {
		return false
	}
	edit.ColumnId = nil
	return edit.IsEmpty()
}

// Retain column intent even when a valid JSON envelope uses decoder aliases or
// only its frozen snapshot carries the field. SQL aliases are internal constants.
func retainedColumnMutationSQL(prefix string) string {
	return `(` + prefix + `op IN ('task.update','task.move') AND EXISTS (
		SELECT 1 FROM json_tree(CASE WHEN json_valid(` + prefix + `payload)
		    THEN ` + prefix + `payload ELSE '{}' END) AS retained_column
		WHERE lower(retained_column.key)='column_id'))`
}

func validateTaskColumn(ctx context.Context, q execer, task model.Task, destinationID string) (ResourceEntity, error) {
	column, err := validateTaskColumnTarget(ctx, q, task, destinationID)
	if err != nil {
		return ResourceEntity{}, err
	}
	var dirty, pending int64
	if err := q.QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id=? AND project_id=?`, task.Id, task.ProjectId).Scan(&dirty); err != nil {
		return ResourceEntity{}, err
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=?`, task.Id).Scan(&pending); err != nil {
		return ResourceEntity{}, err
	}
	if dirty != 0 || pending != 0 {
		return ResourceEntity{}, errors.New("sync or recover pending task changes before moving a Kanban card")
	}
	return column, nil
}

func validateTaskColumnTarget(ctx context.Context, q execer, task model.Task, destinationID string) (ResourceEntity, error) {
	var empty ResourceEntity
	if task.Id == "" || IsLocalID(task.Id) {
		return empty, errors.New("sync the task before assigning a Kanban column; column assignment during creation is unsupported")
	}
	if task.Status != model.TaskOpen || task.ParentId != "" || len(task.ChildIds) != 0 {
		return empty, errors.New("Kanban movement requires an open task without parent or child task links")
	}
	if strings.TrimSpace(destinationID) != destinationID || destinationID == "" || IsLocalID(destinationID) {
		return empty, errors.New("choose a confirmed nonempty Kanban column ID")
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id=? AND project_id=?`, task.Id, task.ProjectId).Scan(&raw); err != nil {
		return empty, err
	}
	fields, err := entityObject(json.RawMessage(raw))
	if err != nil || exactRawString(fields, "id", task.Id) != nil || exactRawString(fields, "projectId", task.ProjectId) != nil {
		return empty, errors.New("refresh the confirmed task snapshot before moving a Kanban card")
	}
	projects, err := queryProjects(ctx, q, `SELECT `+projectColumns+` FROM projects WHERE id=?`, task.ProjectId)
	if err != nil {
		return empty, err
	}
	if len(projects) != 1 || projects[0].Closed || projects[0].Permission == "read" || projects[0].Permission == "comment" {
		return empty, errors.New("the task project is unavailable for Kanban writes")
	}
	column, err := scanEntity(q.QueryRowContext(ctx, `SELECT `+entityColumns+` FROM resource_entities WHERE kind='column' AND server_id=?`, destinationID))
	if err != nil {
		return empty, fmt.Errorf("refresh the destination columns before moving: %w", err)
	}
	if column.ProjectKey != task.ProjectId || column.Deleted || column.Dirty || column.ServerID != destinationID || !entityJSONEqual(column.Data, column.Base) {
		return empty, errors.New("destination must be a clean confirmed column in the same project")
	}
	var wire struct{ ID, ProjectID, Name string }
	if json.Unmarshal(column.Base, &wire) != nil || wire.ID != destinationID || wire.ProjectID != task.ProjectId || strings.TrimSpace(wire.Name) == "" {
		return empty, errors.New("destination column snapshot has invalid identity or name")
	}
	var pending int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM entity_operation_targets WHERE kind='column' AND entity_key=?`, column.Ref.Key).Scan(&pending); err != nil {
		return empty, err
	}
	if pending != 0 {
		return empty, errors.New("sync or recover changes to the destination column before moving a card")
	}
	return column, nil
}

func taskColumnPreview(ctx context.Context, tx *sql.Tx, id, destinationID string) (TaskColumnPreview, error) {
	var preview TaskColumnPreview
	var err error
	preview.Task, err = loadTask(ctx, tx, id)
	if err != nil {
		return preview, err
	}
	column, err := validateTaskColumn(ctx, tx, preview.Task, destinationID)
	if err != nil {
		return preview, err
	}
	var wire struct{ Name string }
	if err := json.Unmarshal(column.Data, &wire); err != nil {
		return preview, err
	}
	preview.SourceName = preview.Task.ColumnName
	if preview.SourceName == "" {
		preview.SourceName = preview.Task.ColumnId
		if preview.SourceName == "" {
			preview.SourceName = "No column"
		}
	}
	preview.DestinationID, preview.DestinationName = destinationID, wire.Name
	preview.Version, err = targetVersion(ctx, tx, id)
	if err != nil {
		return preview, err
	}
	preview.DestinationVersion, err = rowVersion(ctx, tx, `SELECT r.*,p.* FROM resource_entities r JOIN projects p ON p.id=r.project_key WHERE r.kind='column' AND r.entity_key=?`, column.Ref.Key)
	return preview, err
}

func (s *Store) PreviewTaskColumn(ctx context.Context, original model.Task, destinationID string) (TaskColumnPreview, error) {
	var preview TaskColumnPreview
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		preview, err = taskColumnPreview(ctx, tx, original.Id, destinationID)
		if err == nil && !samePulledTask(preview.Task, original) {
			return ErrTaskChanged
		}
		return err
	})
	return preview, err
}

func (s *Store) ApplyTaskColumn(ctx context.Context, expected TaskColumnPreview) (TaskMutationOutcome, error) {
	var outcome TaskMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := taskColumnPreview(ctx, tx, expected.Task.Id, expected.DestinationID)
		if err != nil {
			return err
		}
		// Private item keys are omitted from JSON previews. Version still fences
		// their database rows; apply only the freshly loaded task below.
		currentJSON, err := json.Marshal(current)
		if err != nil {
			return err
		}
		expectedJSON, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		if !bytes.Equal(currentJSON, expectedJSON) {
			return ErrConfirmationChanged
		}
		outcome, err = editTaskTxOutcome(ctx, tx, current.Task.Id, OpTaskUpdate, TaskUpdatedKind,
			func(context.Context, *sql.Tx, model.Task) (model.TaskEdit, []string, error) {
				return model.TaskEdit{ColumnId: model.Ptr(current.DestinationID)}, nil, nil
			}, true)
		if err != nil || !outcome.Changed {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET column_name=? WHERE id=? AND column_id=?`, current.DestinationName, current.Task.Id, current.DestinationID)
		outcome.Task.ColumnName = current.DestinationName
		return err
	})
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	return outcome, nil
}

func undoTaskColumn(ctx context.Context, tx *sql.Tx, action UndoAction) (model.Task, error) {
	if action.OperationSeq <= 0 || action.Before == nil || !columnOnlyEdit(*action.Before) || action.ColumnNameBefore == nil {
		return model.Task{}, ErrUndoIncomplete
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE seq=?`, action.OperationSeq)
	if err != nil {
		return model.Task{}, err
	}
	if !rows.Next() {
		err := rows.Err()
		rows.Close()
		if err != nil {
			return model.Task{}, err
		}
		return model.Task{}, errors.New("column move is already confirmed; prepare a new move instead of automatic undo")
	}
	item, err := scanOutbox(rows)
	rows.Close()
	if err != nil {
		return model.Task{}, err
	}
	edit, metadata, err := DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return model.Task{}, err
	}
	if item.State == OutboxInflight || metadata == nil || metadata.Phase != FeaturePrepared && metadata.Phase != FeatureRejected {
		return model.Task{}, ErrEntityUncertain
	}
	if item.Op != OpTaskUpdate || item.Target != TargetOpenAPI || item.TaskID != action.TaskID || item.ProjectID != action.ProjectID || !columnOnlyEdit(edit) || metadata.ExtensionBaseline == nil || metadata.ExtensionBaseline.ColumnId == nil || *metadata.ExtensionBaseline.ColumnId != *action.Before.ColumnId {
		return model.Task{}, ErrUndoConflict
	}
	var pending, dirty int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=?`, action.TaskID).Scan(&pending); err != nil {
		return model.Task{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id=?`, action.TaskID).Scan(&dirty); err != nil {
		return model.Task{}, err
	}
	current, err := loadTask(ctx, tx, action.TaskID)
	if err != nil {
		return model.Task{}, err
	}
	if pending != 1 || dirty != item.Rev || current.ProjectId != action.ProjectID || current.ColumnId != *edit.ColumnId {
		return model.Task{}, ErrUndoConflict
	}
	var sent sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT sent_at FROM outbox WHERE seq=?`, item.Seq).Scan(&sent); err != nil {
		return model.Task{}, err
	}
	if metadata.Phase == FeaturePrepared && sent.Valid {
		return model.Task{}, ErrEntityUncertain
	}
	current.ColumnId, current.ColumnName = *action.Before.ColumnId, *action.ColumnNameBefore
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET column_id=?,column_name=?,dirty=0 WHERE id=?`, current.ColumnId, current.ColumnName, current.Id); err != nil {
		return model.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE seq=?`, item.Seq); err != nil {
		return model.Task{}, err
	}
	if err := journal(ctx, tx, TaskUpdatedKind, current, action.Before); err != nil {
		return model.Task{}, err
	}
	return current, bumpItemIdentityEpoch(ctx, tx)
}
