package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/movsar/tt/internal/model"
)

const OpTaskMoveNative = "task.move_native"

type NativeMovePreview struct {
	Task               model.Task
	Destination        model.Project
	Version            string
	DestinationVersion string
}

type NativeMovePayload struct {
	Version       int    `json:"version"`
	TaskID        string `json:"task_id"`
	FromProjectID string `json:"from_project_id"`
	ToProjectID   string `json:"to_project_id"`
	Phase         string `json:"phase"`
	Etag          string `json:"etag,omitempty"`
}

func nativeMovePending(ctx context.Context, q execer, taskID string) error {
	var pending int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=? AND op=?`, taskID, OpTaskMoveNative).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("task has an unresolved native move; sync or recover it before editing")
	}
	return nil
}

func nativeMovePreview(ctx context.Context, tx *sql.Tx, id, destination string) (NativeMovePreview, error) {
	var preview NativeMovePreview
	var err error
	preview.Task, err = loadTask(ctx, tx, id)
	if err != nil {
		return preview, err
	}
	if IsLocalID(id) {
		return preview, errors.New("sync the task before moving it")
	}
	if preview.Task.Status != model.TaskOpen {
		return preview, ErrMoveCompleted
	}
	if preview.Task.ProjectId == destination {
		return preview, errors.New("task already belongs to the destination project")
	}
	if preview.Task.ParentId != "" || len(preview.Task.ChildIds) != 0 {
		return preview, errors.New("native move of a parent or child task requires verified relationship semantics")
	}
	if preview.Task.ColumnId != "" {
		return preview, errors.New("native move of a Kanban task requires verified column assignment semantics")
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id=?`, id).Scan(&queued); err != nil {
		return preview, err
	}
	if queued != 0 {
		return preview, errors.New("sync or recover queued task changes before moving")
	}
	projects, err := queryProjects(ctx, tx, `SELECT `+projectColumns+` FROM projects WHERE id=?`, destination)
	if err != nil {
		return preview, err
	}
	if len(projects) != 1 {
		return preview, ErrNotFound
	}
	preview.Destination = projects[0]
	if reason := preview.Destination.CreateUnavailable(); reason != "" {
		return preview, errors.New(reason)
	}
	preview.Version, err = targetVersion(ctx, tx, id)
	if err != nil {
		return preview, err
	}
	preview.DestinationVersion, err = rowVersion(ctx, tx, `SELECT * FROM projects WHERE id=?`, destination)
	return preview, err
}

func (s *Store) PreviewNativeMove(ctx context.Context, original model.Task, destination string) (NativeMovePreview, error) {
	var preview NativeMovePreview
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		preview, err = nativeMovePreview(ctx, tx, original.Id, destination)
		if err == nil && !reflect.DeepEqual(preview.Task, original) {
			return ErrTaskChanged
		}
		return err
	})
	return preview, err
}

func queueNativeMove(ctx context.Context, tx *sql.Tx, preview NativeMovePreview, recordUndo bool) (model.Task, error) {
	next := preview.Task
	next.ProjectId = preview.Destination.Id
	next.ModifiedTime = model.NewTime(time.Now())
	revision, err := writeLocalTask(ctx, tx, next, preview.Destination.Name)
	if err != nil {
		return model.Task{}, err
	}
	payload, err := json.Marshal(NativeMovePayload{Version: 1, TaskID: next.Id, FromProjectID: preview.Task.ProjectId, ToProjectID: next.ProjectId, Phase: "prepared"})
	if err != nil {
		return model.Task{}, err
	}
	seq, err := enqueue(ctx, tx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskMoveNative, TaskID: next.Id, ProjectID: preview.Task.ProjectId, Payload: payload, Rev: revision})
	if err != nil {
		return model.Task{}, err
	}
	if err := journal(ctx, tx, TaskMovedKind, next, &model.TaskEdit{ProjectId: model.Ptr(next.ProjectId)}); err != nil {
		return model.Task{}, err
	}
	if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
		return model.Task{}, err
	}
	if recordUndo {
		if err := pushUndo(ctx, tx, UndoAction{Op: OpTaskMoveNative, TaskID: next.Id, ProjectID: preview.Task.ProjectId, OperationSeq: seq}); err != nil {
			return model.Task{}, err
		}
	}
	return next, nil
}

func (s *Store) ApplyNativeMove(ctx context.Context, expected NativeMovePreview) (TaskMutationOutcome, error) {
	var out TaskMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := nativeMovePreview(ctx, tx, expected.Task.Id, expected.Destination.Id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return ErrConfirmationChanged
		}
		out.Task, err = queueNativeMove(ctx, tx, current, true)
		out.Changed = err == nil
		return err
	})
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	return out, nil
}

func DecodeNativeMove(item OutboxItem) (NativeMovePayload, error) {
	var payload NativeMovePayload
	if item.Op != OpTaskMoveNative {
		return payload, errors.New("not a native move operation")
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return payload, err
	}
	if payload.Version != 1 || payload.TaskID != item.TaskID || payload.TaskID == "" || IsLocalID(payload.TaskID) || payload.FromProjectID == "" || payload.ToProjectID == "" || payload.FromProjectID == payload.ToProjectID {
		return payload, errors.New("invalid native move payload")
	}
	switch payload.Phase {
	case "prepared", "armed", "accepted", "rejected":
	default:
		return payload, errors.New("invalid native move phase")
	}
	if payload.Phase == "accepted" && payload.Etag == "" {
		return payload, errors.New("accepted native move has no etag")
	}
	return payload, nil
}

func (s *Store) RecordNativeMovePhase(ctx context.Context, item OutboxItem, phase, etag string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := featureOutboxItem(ctx, tx, item.Seq, item.LeaseToken)
		if err != nil {
			return err
		}
		payload, err := DecodeNativeMove(current)
		if err != nil {
			return err
		}
		switch phase {
		case "armed":
			if payload.Phase != "prepared" && payload.Phase != "rejected" {
				return ErrEntityUncertain
			}
		case "accepted":
			if payload.Phase != "armed" || etag == "" {
				return ErrEntityUncertain
			}
		case "rejected":
			if payload.Phase != "armed" {
				return ErrEntityUncertain
			}
		default:
			return errors.New("invalid native move transition")
		}
		payload.Phase = phase
		if etag != "" {
			payload.Etag = etag
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE outbox SET payload=?,sent_at=CASE WHEN ?='rejected' THEN NULL ELSE ? END WHERE seq=?`, string(raw), phase, time.Now().Unix(), item.Seq)
		return err
	})
}

func (s *Store) ConfirmNativeMove(ctx context.Context, item OutboxItem, raw json.RawMessage) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := featureOutboxItem(ctx, tx, item.Seq, item.LeaseToken)
		if err != nil {
			return err
		}
		payload, err := DecodeNativeMove(current)
		if err != nil {
			return err
		}
		if payload.Phase != "armed" && payload.Phase != "accepted" {
			return errors.New("native move was not sent")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if err := exactRawString(fields, "id", payload.TaskID); err != nil {
			return err
		}
		if err := exactRawString(fields, "projectId", payload.ToProjectID); err != nil {
			return err
		}
		var previous string
		if err := tx.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id=?`, payload.TaskID).Scan(&previous); err != nil {
			return err
		}
		var merged map[string]json.RawMessage
		if json.Unmarshal([]byte(previous), &merged) != nil || merged == nil {
			merged = make(map[string]json.RawMessage)
		}
		for key, value := range fields {
			merged[key] = value
		}
		retained, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET dirty=0,raw=? WHERE id=? AND project_id=? AND dirty=?`, string(retained), payload.TaskID, payload.ToProjectID, current.Rev)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrTaskChanged
		}
		if err := markDone(ctx, tx, item.Seq, item.LeaseToken); err != nil {
			return err
		}
		return bumpItemIdentityEpoch(ctx, tx)
	})
}

func parkAmbiguousNativeMoves(ctx context.Context, tx *sql.Tx, cutoff int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET state='failed',last_error='native move outcome needs explicit read-only recovery',failed_at=?,inflight_at=NULL,lease_token=NULL
		WHERE op=? AND state='inflight' AND (inflight_at IS NULL OR inflight_at<=?)
		AND CASE WHEN json_valid(payload) THEN coalesce(json_extract(payload,'$.phase'),'') NOT IN ('prepared','rejected') ELSE 1 END`, time.Now().Unix(), OpTaskMoveNative, cutoff)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count != 0 {
		err = bumpItemIdentityEpoch(ctx, tx)
	}
	return count, err
}

func undoNativeMove(ctx context.Context, tx *sql.Tx, action UndoAction) (model.Task, error) {
	if action.OperationSeq <= 0 || action.ProjectID == "" {
		return model.Task{}, ErrUndoIncomplete
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE seq=?`, action.OperationSeq)
	if err != nil {
		return model.Task{}, err
	}
	if rows.Next() {
		item, err := scanOutbox(rows)
		rows.Close()
		if err != nil {
			return model.Task{}, err
		}
		payload, err := DecodeNativeMove(item)
		if err != nil {
			return model.Task{}, err
		}
		if item.State == OutboxInflight || payload.Phase != "prepared" && payload.Phase != "rejected" {
			return model.Task{}, ErrEntityUncertain
		}
		if payload.TaskID != action.TaskID || payload.FromProjectID != action.ProjectID {
			return model.Task{}, ErrUndoConflict
		}
		current, err := loadTask(ctx, tx, action.TaskID)
		if err != nil {
			return model.Task{}, err
		}
		if current.ProjectId != payload.ToProjectID {
			return model.Task{}, ErrTaskChanged
		}
		name, err := projectName(ctx, tx, action.ProjectID)
		if err != nil {
			return model.Task{}, err
		}
		current.ProjectId = action.ProjectID
		if _, err := writeLocalTask(ctx, tx, current, name); err != nil {
			return model.Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE seq=?`, action.OperationSeq); err != nil {
			return model.Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET dirty=0 WHERE id=?`, action.TaskID); err != nil {
			return model.Task{}, err
		}
		if err := journal(ctx, tx, TaskMovedKind, current, &model.TaskEdit{ProjectId: model.Ptr(current.ProjectId)}); err != nil {
			return model.Task{}, err
		}
		return current, bumpItemIdentityEpoch(ctx, tx)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return model.Task{}, err
	}
	rows.Close()
	preview, err := nativeMovePreview(ctx, tx, action.TaskID, action.ProjectID)
	if err != nil {
		return model.Task{}, fmt.Errorf("prepare reverse move: %w", err)
	}
	return queueNativeMove(ctx, tx, preview, false)
}
