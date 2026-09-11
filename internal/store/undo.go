package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/model"
)

type UndoAction struct {
	ColumnNameBefore *string    `json:"column_name_before,omitempty"`
	OperationSeq     int64      `json:"operation_seq,omitempty"`
	EntityRef        *EntityRef `json:"entity_ref,omitempty"`
	Op               string     `json:"op"`
	TaskID           string     `json:"task_id"`
	ProjectID        string     `json:"project_id,omitempty"`

	Before *model.TaskEdit `json:"before,omitempty"`

	Task *model.Task `json:"task,omitempty"`
}

type UndoEntry struct {
	Seq     int64
	At      time.Time
	Action  UndoAction
	Feature *FeatureUndoMetadata
	payload []byte
}

var ErrUndoIncomplete = errors.New("undo record does not carry what its reversal needs")
var ErrUndoConflict = errors.New("undo stack changed before the reversal")

func (s *Store) LastUndo(ctx context.Context) (UndoEntry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT seq, at, payload FROM undo_log ORDER BY seq DESC LIMIT 1`)
	return scanUndo(row)
}

func (s *Store) PopUndo(ctx context.Context) (UndoEntry, error) {
	return popUndo(ctx, s.db)
}

func PopUndoTx(ctx context.Context, tx *sql.Tx) (UndoEntry, error) {
	return popUndo(ctx, tx)
}

func popUndo(ctx context.Context, q execer) (UndoEntry, error) {
	row := q.QueryRowContext(ctx,
		`DELETE FROM undo_log WHERE seq = (SELECT max(seq) FROM undo_log)
		 RETURNING seq, at, payload`)
	return scanUndo(row)
}

func scanUndo(row *sql.Row) (UndoEntry, error) {
	var (
		e       UndoEntry
		at      int64
		payload []byte
	)
	err := row.Scan(&e.Seq, &at, &payload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return UndoEntry{}, ErrNoUndo
	case err != nil:
		return UndoEntry{}, fmt.Errorf("read undo log: %w", err)
	}
	action, feature, err := DecodeUndoAction(payload)
	if err != nil {
		return UndoEntry{}, fmt.Errorf("read undo log: %w", err)
	}
	e.Action = action
	e.Feature = feature
	e.payload = append([]byte(nil), payload...)
	e.At = time.Unix(at, 0)
	return e, nil
}

func pushUndo(ctx context.Context, q execer, a UndoAction) error {
	payload, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("record undo of %s: %w", a.Op, err)
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), a.Op, payload)
	if err != nil {
		return fmt.Errorf("record undo of %s: %w", a.Op, err)
	}
	return nil
}

func pushFeatureUndo(ctx context.Context, q execer, a UndoAction, fields FeatureFields) error {
	fields.Interval = false
	if fields.empty() {
		return pushUndo(ctx, q, a)
	}
	keys := []string(nil)
	if fields.Items {
		var items []model.Item
		if a.Before != nil && a.Before.Items != nil {
			items = []model.Item(*a.Before.Items)
		} else if a.Task != nil {
			items = a.Task.Items
		}
		keys = make([]string, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
	}
	version := FeaturePayloadVersion
	if fields.Extensions {
		version = FeatureExtensionsPayloadVersion
	}
	payload, err := EncodeUndoAction(a, FeatureUndoMetadata{
		Version:  version,
		Fields:   fields,
		ItemKeys: keys,
	})
	if err != nil {
		return fmt.Errorf("record undo of %s: %w", a.Op, err)
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO undo_log (at, kind, payload) VALUES (?, ?, ?)`,
		time.Now().Unix(), a.Op, string(payload))
	if err != nil {
		return fmt.Errorf("record undo of %s: %w", a.Op, err)
	}
	return nil
}

func (s *Store) ApplyUndo(ctx context.Context, expected UndoEntry) (model.Task, error) {
	var out model.Task
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := scanUndo(tx.QueryRowContext(ctx,
			`SELECT seq, at, payload FROM undo_log ORDER BY seq DESC LIMIT 1`))
		if err != nil {
			return err
		}
		if current.Seq != expected.Seq {
			return ErrUndoConflict
		}
		action := current.Action
		switch action.Op {
		case OpEntityMutation:
			err = undoEntityMutation(ctx, tx, action)
		case OpTaskMoveNative:
			out, err = undoNativeMove(ctx, tx, action)
		case OpTaskCreate:
			out, err = deleteTaskTx(ctx, tx, action.TaskID, false)
		case OpTaskUpdate, OpTaskComplete:
			if action.Before == nil || action.Before.IsEmpty() {
				return ErrUndoIncomplete
			}
			if action.Before.ColumnId != nil {
				out, err = undoTaskColumn(ctx, tx, action)
				break
			}
			out, err = editTaskTx(ctx, tx, action.TaskID, OpTaskUpdate, TaskUpdatedKind,
				func(ctx context.Context, tx *sql.Tx, cur model.Task) (model.TaskEdit, []string, error) {
					before := *action.Before
					if current.Feature == nil && before.Items != nil {
						items, err := resolveLegacyUndoItemKeys(ctx, tx, cur, current.payload)
						if err != nil {
							return model.TaskEdit{}, nil, err
						}
						before.Items = model.NewEditList(items)
					}
					return before, nil, nil
				}, false)
		case OpTaskDelete:
			if action.Task == nil {
				return ErrUndoIncomplete
			}
			if len(action.Task.Items) != 0 && (current.Feature == nil || !current.Feature.Fields.Items) {
				return fmt.Errorf("%w: deleted checklist has no retained versioned provenance", ErrUnsafeChecklist)
			}
			out = restoredTaskForCreate(*action.Task)
			out.Id, err = NewLocalID()
			if err == nil {
				err = createTaskTx(ctx, tx, &out, true, false)
			}
		default:
			return fmt.Errorf("undo %s: unsupported operation", action.Op)
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM undo_log WHERE seq = ? AND seq = (SELECT max(seq) FROM undo_log)`, current.Seq)
		if err != nil {
			return fmt.Errorf("consume undo record %d: %w", current.Seq, err)
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("consume undo record %d: %w", current.Seq, err)
		}
		if changed != 1 {
			return ErrUndoConflict
		}
		return nil
	})
	if err != nil {
		return model.Task{}, err
	}
	return out, nil
}

func resolveLegacyUndoItemKeys(ctx context.Context, tx *sql.Tx, cur model.Task, payload []byte) ([]model.Item, error) {
	items, err := decodeLegacyUndoItems(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeChecklist, err)
	}
	identities, err := itemIdentities(ctx, tx, cur.Id)
	if err != nil {
		return nil, err
	}
	byServerID := make(map[string]string, len(identities))
	ambiguousServerID := make(map[string]bool)
	abandonedServerID := make(map[string]bool)
	if len(identities) == 0 {
		if IsLocalID(cur.Id) {
			return nil, fmt.Errorf("%w: legacy local checklist has no versioned provenance", ErrUnsafeChecklist)
		}
		if err := validateImportedItemRaw(ctx, tx, cur); err != nil {
			return nil, err
		}
		for _, item := range cur.Items {
			if item.Id == "" || item.Key == "" {
				return nil, fmt.Errorf("%w: current item has no proven server identity", ErrUnsafeChecklist)
			}
			if _, duplicate := byServerID[item.Id]; duplicate {
				ambiguousServerID[item.Id] = true
			}
			byServerID[item.Id] = item.Key
		}
	} else {
		if err := validateLegacyUndoRawBaseline(ctx, tx, cur.Id); err != nil {
			return nil, err
		}
		for _, identity := range identities {
			if identity.ServerID == "" {
				continue
			}
			if identity.State == ItemAbandoned {
				abandonedServerID[identity.ServerID] = true
				continue
			}
			if identity.State != ItemBound && identity.State != ItemUncertain {
				continue
			}
			if _, duplicate := byServerID[identity.ServerID]; duplicate {
				ambiguousServerID[identity.ServerID] = true
			}
			byServerID[identity.ServerID] = identity.ItemKey
		}
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		if seen[items[i].Id] {
			return nil, fmt.Errorf("%w: legacy undo item %d repeats server id %q", ErrUnsafeChecklist, i+1, items[i].Id)
		}
		seen[items[i].Id] = true
		if abandonedServerID[items[i].Id] {
			return nil, fmt.Errorf("%w: legacy undo item %d refers to abandoned server id %q", ErrUnsafeChecklist, i+1, items[i].Id)
		}
		if ambiguousServerID[items[i].Id] {
			return nil, fmt.Errorf("%w: legacy undo item %d has ambiguous server id %q", ErrUnsafeChecklist, i+1, items[i].Id)
		}
		key, ok := byServerID[items[i].Id]
		if !ok {
			return nil, fmt.Errorf("%w: legacy undo item %d has no exact identity for server id %q", ErrUnsafeChecklist, i+1, items[i].Id)
		}
		items[i].Key = key
	}
	return items, nil
}

func validateLegacyUndoRawBaseline(ctx context.Context, q execer, taskID string) error {
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = ?`, taskID).Scan(&raw); err != nil {
		return fmt.Errorf("read raw checklist of %s: %w", taskID, err)
	}
	_, present, err := decodeRawItems(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeChecklist, err)
	}
	if !present {
		return fmt.Errorf("%w: task %s has no retained raw item array", ErrUnsafeChecklist, taskID)
	}
	return nil
}

func decodeLegacyUndoItems(payload []byte) ([]model.Item, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("legacy undo is invalid JSON: %w", err)
	}
	beforeRaw, ok := root["before"]
	if !ok || bytes.Equal(bytes.TrimSpace(beforeRaw), []byte("null")) {
		return nil, errors.New("legacy undo has no before object")
	}
	var before map[string]json.RawMessage
	if err := json.Unmarshal(beforeRaw, &before); err != nil {
		return nil, errors.New("legacy undo before is not an object")
	}
	itemsRaw, ok := before["items"]
	if !ok || bytes.Equal(bytes.TrimSpace(itemsRaw), []byte("null")) {
		return nil, errors.New("legacy undo has no complete item snapshot")
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(itemsRaw, &records); err != nil {
		return nil, errors.New("legacy undo items are not an array of objects")
	}
	required := map[string]bool{
		"Id": true, "Title": true, "Status": true, "SortOrder": true,
		"StartDate": true, "IsAllDay": true, "TimeZone": true, "CompletedTime": true,
	}
	items := make([]model.Item, len(records))
	if err := json.Unmarshal(itemsRaw, &items); err != nil {
		return nil, fmt.Errorf("legacy undo items have invalid fields: %w", err)
	}
	for i, record := range records {
		for name := range record {
			if !required[name] {
				return nil, fmt.Errorf("legacy undo item %d has unknown field %q", i+1, name)
			}
		}
		for name := range required {
			if _, ok := record[name]; !ok {
				return nil, fmt.Errorf("legacy undo item %d is missing field %q", i+1, name)
			}
		}
		if items[i].Id == "" || IsLocalID(items[i].Id) {
			return nil, fmt.Errorf("legacy undo item %d has invalid server id", i+1)
		}
		for _, name := range []string{"Id", "Title", "Status", "SortOrder", "IsAllDay", "TimeZone"} {
			if bytes.Equal(bytes.TrimSpace(record[name]), []byte("null")) {
				return nil, fmt.Errorf("legacy undo item %d field %s is null", i+1, name)
			}
		}
	}
	return items, nil
}

func restoredTaskForCreate(task model.Task) model.Task {
	if len(task.Items) == 0 {
		return task
	}
	items := append([]model.Item(nil), task.Items...)
	for i := range items {
		items[i].Id = ""
		items[i].Key = ""
	}
	task.Items = items
	return task
}
