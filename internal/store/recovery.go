package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/movsar/tt/internal/model"
)

var ErrConfirmationChanged = errors.New("confirmation is stale; review the exact targets again")

type QueueEntry struct {
	Item                                             OutboxItem
	Title, Version, TargetVersion, Recovery, Refusal string
	SentAt                                           sql.NullInt64
}

type FocusQueueEntry struct {
	SessionID, TaskID, Title, Outcome, Phase, LastError string
}

type RecoveryQueue struct {
	Tasks []QueueEntry
	Focus []FocusQueueEntry
}

func rowVersion(ctx context.Context, q execer, query string, args ...any) (string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	encoder := json.NewEncoder(h)
	if err := encoder.Encode(columns); err != nil {
		return "", err
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return "", err
		}
		if err := encoder.Encode(values); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), rows.Err()
}

func targetVersion(ctx context.Context, q execer, id string) (string, error) {
	h := sha256.New()
	epoch, err := readItemIdentityEpoch(ctx, q)
	if err != nil {
		return "", err
	}
	var event int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events`).Scan(&event); err != nil {
		return "", err
	}
	fmt.Fprintln(h, epoch, event)
	for _, query := range []string{
		`SELECT * FROM tasks WHERE id=?`,
		`SELECT * FROM items WHERE task_id=? ORDER BY id`,
		`SELECT * FROM item_identities WHERE task_id=? ORDER BY item_key`,
		`SELECT * FROM outbox WHERE task_id=? ORDER BY seq`,
	} {
		version, err := rowVersion(ctx, q, query, id)
		if err != nil {
			return "", err
		}
		fmt.Fprintln(h, version)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func queueEntries(ctx context.Context, q execer, condition string, args ...any) ([]QueueEntry, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE `+condition+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	entries := []QueueEntry{}
	for rows.Next() {
		item, err := scanOutbox(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, QueueEntry{Item: item})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range entries {
		e := &entries[i]
		e.Version, err = rowVersion(ctx, q, `SELECT * FROM outbox WHERE seq=?`, e.Item.Seq)
		if err != nil {
			return nil, err
		}
		e.TargetVersion, err = targetVersion(ctx, q, e.Item.TaskID)
		if err != nil {
			return nil, err
		}
		if err := q.QueryRowContext(ctx, `SELECT sent_at FROM outbox WHERE seq=?`, e.Item.Seq).Scan(&e.SentAt); err != nil {
			return nil, err
		}
		err := q.QueryRowContext(ctx, `SELECT title FROM tasks WHERE id=?`, e.Item.TaskID).Scan(&e.Title)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		e.Recovery, e.Refusal = queueRecovery(*e)
	}
	return entries, nil
}

func queueRecovery(e QueueEntry) (string, string) {
	i := e.Item
	if i.State != OutboxFailed {
		return "", "Only failed/parked operations can be returned to pending."
	}
	if i.Target != TargetOpenAPI {
		return "", "This task recovery action does not upload focus sessions."
	}
	send, versioned, err := decodeFeatureSend(i)
	if err != nil {
		return "", err.Error()
	}
	if versioned {
		switch send.Metadata.Phase {
		case FeatureArmed, FeatureAccepted, FeatureMismatch:
			if _, _, ok := send.FeatureAddress(); !ok {
				return "", "Uncertain allocation has no confirmed server address; retained without another POST."
			}
			return "Read-only confirmation on the next explicit sync; no allocating POST.", ""
		case FeaturePrepared:
			if i.Op == OpTaskCreate && e.SentAt.Valid {
				return "", "Create has a sent marker without confirmed rejection; no repeat POST."
			}
			return "Return the unsent operation to pending for explicit sync.", ""
		case FeatureRejected:
			return "Retry the exact frozen request after confirmed rejection; explicit sync sends it.", ""
		default:
			return "", "Unsupported feature phase; retained unchanged."
		}
	}
	switch i.Op {
	case OpTaskMoveNative:
		move, err := DecodeNativeMove(i)
		if err != nil {
			return "", err.Error()
		}
		if move.Phase == "armed" || move.Phase == "accepted" {
			return "Confirm the destination by read only; the move request will not be repeated.", ""
		}
		return "Return the unsent or definitely rejected move to pending.", ""
	case OpTaskCreate:
		if IsLocalID(i.TaskID) && (e.SentAt.Valid || i.Attempts != 0) {
			return "", "Legacy create has no durable rejection proof; no repeat POST."
		}
	case OpTaskUpdate, OpTaskMove:
		edit, _, err := DecodeTaskEditPayload(i.Payload)
		if err != nil {
			return "", err.Error()
		}
		if edit.Items != nil {
			return "", "Legacy checklist allocation has no durable retry proof; retained unchanged."
		}
	case OpTaskComplete, OpTaskDelete, OpTaskMoveDrop:
	default:
		return "", "Unsupported operation; retained unchanged."
	}
	return "Return this operation to pending for explicit sync.", ""
}

func (s *Store) RecoveryQueue(ctx context.Context) (RecoveryQueue, error) {
	var out RecoveryQueue
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out.Tasks, err = queueEntries(ctx, tx, "1=1")
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.id, COALESCE(f.task_id,''), COALESCE(t.title,''),
			COALESCE(f.outcome,''), COALESCE(u.phase,'not uploaded'), COALESCE(u.last_error,'')
			FROM focus_sessions f LEFT JOIN focus_uploads u ON u.session_id=f.id
			LEFT JOIN tasks t ON t.id=f.task_id
			WHERE f.ended_at IS NOT NULL AND f.synced_at IS NULL ORDER BY f.started_at,f.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e FocusQueueEntry
			if err := rows.Scan(&e.SessionID, &e.TaskID, &e.Title, &e.Outcome, &e.Phase, &e.LastError); err != nil {
				return err
			}
			out.Focus = append(out.Focus, e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) RetryQueueEntry(ctx context.Context, expected QueueEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := ctx
	ctx = context.WithoutCancel(ctx)
	return s.Tx(ctx, func(tx *sql.Tx) error {
		entries, err := queueEntries(ctx, tx, "seq=?", expected.Item.Seq)
		if err != nil {
			return err
		}
		if len(entries) != 1 || !reflect.DeepEqual(entries[0], expected) {
			return ErrConfirmationChanged
		}
		current := entries[0]
		if current.Refusal != "" {
			return errors.New(current.Refusal)
		}

		if err := request.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state='pending', inflight_at=NULL, lease_token=NULL, sent_at=NULL WHERE seq=?`, current.Item.Seq); err != nil {
			return err
		}
		registered, err := taskHasItemControl(ctx, tx, current.Item.TaskID)
		if err != nil {
			return err
		}
		if registered {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
}

type TaskOperationPreview struct {
	Native                      *NativeMovePreview
	Task                        model.Task
	Destination                 model.Project
	Version, DestinationVersion string
	Queue                       []QueueEntry
	RemoteDelete                bool
	RetainedCreates             int
}

func (s *Store) PreviewTaskOperation(ctx context.Context, original model.Task, destination string) (TaskOperationPreview, error) {
	var out TaskOperationPreview
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = taskOperationPreview(ctx, tx, original.Id, destination)
		if err == nil && !reflect.DeepEqual(out.Task, original) {
			return ErrConfirmationChanged
		}
		return err
	})
	return out, err
}

func taskOperationPreview(ctx context.Context, tx *sql.Tx, id, destination string) (TaskOperationPreview, error) {
	var p TaskOperationPreview
	var err error
	p.Task, err = loadTask(ctx, tx, id)
	if err != nil {
		return p, err
	}
	p.Version, err = targetVersion(ctx, tx, id)
	if err != nil {
		return p, err
	}
	p.Queue, err = queueEntries(ctx, tx, "task_id=?", id)
	if err != nil {
		return p, err
	}
	p.RemoteDelete, err = needsRemoteDelete(ctx, tx, id)
	if err != nil {
		return p, err
	}
	for _, e := range p.Queue {
		if IsLocalID(id) && e.Item.Op == OpTaskCreate && e.Item.State == OutboxInflight {
			p.RetainedCreates++
		}
	}
	if destination != "" {
		projects, err := queryProjects(ctx, tx, `SELECT `+projectColumns+` FROM projects WHERE id=?`, destination)
		if err != nil {
			return p, err
		}
		if len(projects) != 1 {
			return p, ErrNotFound
		}
		p.Destination = projects[0]
		p.DestinationVersion, err = rowVersion(ctx, tx, `SELECT * FROM projects WHERE id=?`, destination)
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Store) ApplyTaskOperation(ctx context.Context, expected TaskOperationPreview) (TaskMutationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return TaskMutationOutcome{}, err
	}
	request := ctx
	ctx = context.WithoutCancel(ctx)
	var out TaskMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := taskOperationPreview(ctx, tx, expected.Task.Id, expected.Destination.Id)
		if errors.Is(err, ErrNotFound) {
			return ErrConfirmationChanged
		}
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return ErrConfirmationChanged
		}
		if err := request.Err(); err != nil {
			return err
		}
		if expected.Destination.Id != "" {
			if expected.Destination.Closed {
				return errors.New("destination list is closed")
			}
			out.Task, err = moveTaskTx(ctx, tx, expected.Task.Id, expected.Destination.Id, MoveOptions{ByRecreate: true})
			out.Changed = err == nil && expected.Task.ProjectId != expected.Destination.Id
		} else {
			out.Task, err = deleteTaskTx(ctx, tx, expected.Task.Id, true)
			out.Changed = err == nil
		}
		return err
	})
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	return out, nil
}
