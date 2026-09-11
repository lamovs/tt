package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/movsar/tt/internal/model"
)

var errUnsoundRecovery = errors.New("recovery response is not a sound complete checklist")

var ErrPullAddressMismatch = errors.New("addressed task response does not match its requested address")

type PullFence struct {
	ProjectID string
	Epoch     int64
	Tasks     map[string]PullTaskFence
}

type PullTaskFence struct {
	ProjectID        string
	Registered       bool
	Control          ItemIdentityState
	Done             bool
	Dirty            bool
	Local            bool
	Held             bool
	Eligible         bool
	RecoveryEligible bool
}

func (s *Store) CapturePullFence(ctx context.Context, projectID string) (PullFence, error) {
	fence := PullFence{ProjectID: projectID, Tasks: map[string]PullTaskFence{}}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		epoch, err := readItemIdentityEpoch(ctx, tx)
		if err != nil {
			return err
		}
		fence.Epoch = epoch
		rows, err := tx.QueryContext(ctx, `SELECT t.id, t.project_id, t.status, t.dirty, t.local,
			coalesce(c.state, '')
			FROM tasks t LEFT JOIN item_identities c
			  ON c.task_id = t.id AND c.item_key = ''
			WHERE t.project_id = ?`, projectID)
		if err != nil {
			return fmt.Errorf("capture pull fence for %s: %w", projectID, err)
		}
		type row struct {
			id, project, control string
			status               int
			dirty                int64
			local                bool
		}
		var found []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.project, &r.status, &r.dirty, &r.local, &r.control); err != nil {
				rows.Close()
				return err
			}
			found = append(found, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, r := range found {
			registered := r.control != ""
			held, outbox, err := featureTaskQueueState(ctx, tx, r.id)
			if err != nil {
				return err
			}
			state := PullTaskFence{
				ProjectID: r.project, Registered: registered, Control: ItemIdentityState(r.control),
				Done: r.status != model.TaskOpen.Wire(), Dirty: r.dirty != 0, Local: r.local, Held: held,
			}
			state.Eligible = !state.Dirty && !state.Local && !state.Held
			state.RecoveryEligible = registered && state.Control == RecoveryPending && state.Eligible && !outbox
			fence.Tasks[r.id] = state
		}
		return nil
	})
	return fence, err
}

func (s *Store) CaptureTaskPullFence(ctx context.Context, projectID, taskID string) (PullFence, error) {
	fence, err := s.CapturePullFence(ctx, projectID)
	if err != nil {
		return PullFence{}, err
	}
	state, ok := fence.Tasks[taskID]
	fence.Tasks = map[string]PullTaskFence{}
	if ok {
		fence.Tasks[taskID] = state
	}
	return fence, nil
}

func readItemIdentityEpoch(ctx context.Context, q execer) (int64, error) {
	value, ok, err := getMeta(ctx, q, "item_identity_epoch")
	if err != nil || !ok {
		return 0, err
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch < 0 || strconv.FormatInt(epoch, 10) != value {
		return 0, fmt.Errorf("read item_identity_epoch: invalid value %q", value)
	}
	return epoch, nil
}

func featureTaskQueueState(ctx context.Context, q execer, taskID string) (held bool, any bool, err error) {
	rows, err := q.QueryContext(ctx, `SELECT op, payload FROM outbox WHERE task_id = ?`, taskID)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		any = true
		var op string
		var payload []byte
		if err := rows.Scan(&op, &payload); err != nil {
			return false, false, err
		}
		if op == OpTaskMoveNative {
			held = true
			continue
		}
		entry := OutboxItem{OutboxEntry: OutboxEntry{Op: op, Payload: payload}}
		_, versioned, decodeErr := decodeFeatureSend(entry)
		if decodeErr != nil {
			continue
		}
		if versioned {
			held = true
		}
	}
	return held, any, rows.Err()
}

func (s *Store) SyncProjectFenced(ctx context.Context, projectID string, tasks []ServerTask, fence PullFence) (SyncResult, error) {
	return s.syncFenced(ctx, projectID, tasks, fence, true)
}

func (s *Store) SyncTaskFenced(ctx context.Context, task ServerTask, fence PullFence) (SyncResult, error) {
	if len(fence.Tasks) != 1 {
		return SyncResult{}, fmt.Errorf("%w: fence has %d tasks", ErrPullAddressMismatch, len(fence.Tasks))
	}
	var taskID string
	for taskID = range fence.Tasks {
	}
	if task.Task.Id != taskID || task.Task.ProjectId != fence.ProjectID {
		return SyncResult{}, fmt.Errorf("%w: got task %q in project %q, want task %q in project %q",
			ErrPullAddressMismatch, task.Task.Id, task.Task.ProjectId, taskID, fence.ProjectID)
	}
	return s.syncFenced(ctx, fence.ProjectID, []ServerTask{task}, fence, false)
}

func (s *Store) syncFenced(ctx context.Context, projectID string, tasks []ServerTask, fence PullFence, deleteAbsent bool) (SyncResult, error) {
	var res SyncResult
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res = SyncResult{}
		currentEpoch, err := readItemIdentityEpoch(ctx, tx)
		if err != nil {
			return err
		}
		if fence.ProjectID != projectID || currentEpoch != fence.Epoch {
			res.Skipped = len(tasks)
			res.Kept = len(fence.Tasks)
			res.Stale = true
			return nil
		}
		if stale, err := pullFenceChanged(ctx, tx, fence); err != nil {
			return err
		} else if stale {
			res.Skipped = len(tasks)
			res.Kept = len(fence.Tasks)
			res.Stale = true
			return nil
		}
		name, err := projectName(ctx, tx, projectID)
		if err != nil {
			return err
		}
		incoming := make(map[string]bool, len(tasks))
		changedRegistered := false
		for _, st := range tasks {
			if st.Task.Id == "" {
				return errors.New("cache the list: task without an id")
			}
			incoming[st.Task.Id] = true
			captured, existed := fence.Tasks[st.Task.Id]
			if !existed || !captured.Registered {
				ok, err := upsertServerTask(ctx, tx, st, name)
				if err != nil {
					return err
				}
				if !ok {
					res.Skipped++
					continue
				}
				if err := replaceItems(ctx, tx, st.Task.Id, st.Task.Items); err != nil {
					return err
				}
				res.Upserted++
				continue
			}
			if !captured.Eligible || captured.Control == RecoveryPending && !captured.RecoveryEligible {
				res.Skipped++
				continue
			}
			if captured.RecoveryEligible {
				if err := adoptRecoveredChecklist(ctx, tx, st, name); err != nil {
					if errors.Is(err, errUnsoundRecovery) {
						res.Skipped++
						continue
					}
					return err
				}
				changedRegistered = true
				res.Upserted++
				continue
			}
			changed, sound, err := reconcileRegisteredTask(ctx, tx, st, name)
			if err != nil {
				return err
			}
			if !sound {
				res.Skipped++
			} else {
				res.Upserted++
			}
			changedRegistered = changedRegistered || changed
		}
		if !deleteAbsent {
			if changedRegistered {
				return bumpItemIdentityEpoch(ctx, tx)
			}
			return nil
		}
		for id, captured := range fence.Tasks {
			if incoming[id] {
				continue
			}
			if captured.Registered {
				if captured.Done || !captured.Eligible || captured.RecoveryEligible {
					res.Kept++
					continue
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
					return err
				}
				changedRegistered = true
				res.Deleted++
				continue
			}
			var currentProject string
			var currentStatus int
			var currentDirty int64
			var currentLocal bool
			err := tx.QueryRowContext(ctx, `SELECT project_id, status, dirty, local FROM tasks WHERE id = ?`, id).
				Scan(&currentProject, &currentStatus, &currentDirty, &currentLocal)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if captured.Done || captured.Dirty || captured.Local || currentProject != projectID ||
				currentStatus != model.TaskOpen.Wire() || currentDirty != 0 || currentLocal {
				res.Kept++
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
				return err
			}
			res.Deleted++
		}
		if changedRegistered {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
	return res, err
}

func pullFenceChanged(ctx context.Context, q execer, fence PullFence) (bool, error) {
	for id, before := range fence.Tasks {
		if !before.Registered {
			continue
		}
		var project, control string
		var status int
		var dirty int64
		var local bool
		err := q.QueryRowContext(ctx, `SELECT t.project_id, t.status, t.dirty, t.local, coalesce(c.state, '')
			FROM tasks t LEFT JOIN item_identities c ON c.task_id=t.id AND c.item_key=''
			WHERE t.id=?`, id).Scan(&project, &status, &dirty, &local, &control)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		held, outbox, err := featureTaskQueueState(ctx, q, id)
		if err != nil {
			return false, err
		}
		now := PullTaskFence{ProjectID: project, Registered: control != "", Control: ItemIdentityState(control), Done: status != model.TaskOpen.Wire(), Dirty: dirty != 0, Local: local, Held: held}
		now.Eligible = !now.Dirty && !now.Local && !now.Held
		now.RecoveryEligible = now.Registered && now.Control == RecoveryPending && now.Eligible && !outbox
		if now != before {
			return true, nil
		}
	}
	return false, nil
}

func reconcileRegisteredTask(ctx context.Context, q execer, st ServerTask, projectName string) (bool, bool, error) {
	rawItems, present, err := decodeRawItems(st.Raw)
	if err == nil && !present {
		changed, sound, err := reconcileRegisteredScalarTask(ctx, q, st, projectName)
		if err != nil || sound {
			return changed, sound, err
		}
	}
	if err != nil || !present || !sameServerItems(rawItems, st.Task.Items) {
		res, writeErr := q.ExecContext(ctx, `UPDATE tasks SET raw=? WHERE id=? AND raw<>?`, string(st.Raw), st.Task.Id, string(st.Raw))
		if writeErr != nil {
			return false, false, writeErr
		}
		n, _ := res.RowsAffected()
		return n != 0, false, nil
	}
	identities, err := itemIdentities(ctx, q, st.Task.Id)
	if err != nil {
		return false, false, err
	}
	active := map[string]ItemIdentity{}
	tombID := map[string]bool{}
	usedKey := map[string]bool{}
	for _, identity := range identities {
		usedKey[identity.ItemKey] = true
		if identity.State == ItemAbandoned && identity.ServerID != "" {
			tombID[identity.ServerID] = true
		}
		if (identity.State == ItemBound || identity.State == ItemUncertain) && identity.ServerID != "" {
			active[identity.ServerID] = identity
		}
	}
	desired := st.Task
	desired.Items = append([]model.Item(nil), rawItems...)
	seenKeys := map[string]bool{}
	registryChanged := false
	for i := range desired.Items {
		id := desired.Items[i].Id
		if identity, ok := active[id]; ok {
			desired.Items[i].Key = identity.ItemKey
		} else if !usedKey[id] && !tombID[id] {
			desired.Items[i].Key = id
		} else {
			key, err := newItemKey(ctx, q, st.Task.Id)
			if err != nil {
				return false, false, err
			}
			desired.Items[i].Key = key
			usedKey[key] = true
		}
		seenKeys[desired.Items[i].Key] = true
		changed, err := setItemIdentityIfChanged(ctx, q, ItemIdentity{TaskID: st.Task.Id, ItemKey: desired.Items[i].Key, ServerID: id, State: ItemBound})
		if err != nil {
			return false, false, err
		}
		registryChanged = registryChanged || changed
	}
	for _, identity := range identities {
		if identity.State != ItemBound && identity.State != ItemUncertain || seenKeys[identity.ItemKey] {
			continue
		}
		changed, err := setItemIdentityIfChanged(ctx, q, ItemIdentity{TaskID: st.Task.Id, ItemKey: identity.ItemKey, State: ItemUnbound})
		if err != nil {
			return false, false, err
		}
		registryChanged = registryChanged || changed
	}
	current, err := loadTask(ctx, q, st.Task.Id)
	if err != nil {
		return false, false, err
	}
	var currentRaw []byte
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id=?`, st.Task.Id).Scan(&currentRaw); err != nil {
		return false, false, err
	}
	changed := registryChanged || !samePulledTask(current, desired) || !bytes.Equal(currentRaw, st.Raw)
	if changed {
		if _, err := upsertServerTask(ctx, q, ServerTask{Task: desired, Raw: st.Raw}, projectName); err != nil {
			return false, false, err
		}
		if err := replaceItems(ctx, q, desired.Id, desired.Items); err != nil {
			return false, false, err
		}
	}
	return changed, true, nil
}

func reconcileRegisteredScalarTask(ctx context.Context, q execer, st ServerTask, projectName string) (bool, bool, error) {
	if st.Task.Kind != "TEXT" && st.Task.Kind != "NOTE" || len(st.Task.Items) != 0 {
		return false, false, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(st.Raw, &root); err != nil {
		return false, false, nil
	}
	var kind string
	if err := json.Unmarshal(root["kind"], &kind); err != nil || kind != st.Task.Kind {
		return false, false, nil
	}
	current, err := loadTask(ctx, q, st.Task.Id)
	if err != nil {
		return false, false, err
	}
	if len(current.Items) != 0 {
		return false, false, nil
	}
	identities, err := itemIdentities(ctx, q, st.Task.Id)
	if err != nil {
		return false, false, err
	}
	if len(identities) == 0 {
		if current.Kind != "" && current.Kind != st.Task.Kind {
			return false, false, nil
		}
	} else {
		if st.Task.Kind != "TEXT" || !confirmedEmptyChecklistRaw(current, st.Raw, identities) {
			return false, false, nil
		}
	}
	var currentRaw []byte
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id=?`, st.Task.Id).Scan(&currentRaw); err != nil {
		return false, false, err
	}
	if samePulledTask(current, st.Task) && bytes.Equal(currentRaw, st.Raw) {
		return false, true, nil
	}
	written, err := upsertServerTask(ctx, q, st, projectName)
	return written, written, err
}

func confirmedEmptyChecklistRaw(current model.Task, raw []byte, identities []ItemIdentity) bool {
	if current.Id == "" || current.ProjectId == "" || len(identities) == 0 ||
		current.Kind != "" && current.Kind != "TEXT" && current.Kind != "CHECKLIST" {
		return false
	}
	keys := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if identity.State != ItemUnbound || identity.ServerID != "" {
			return false
		}
		keys[identity.ItemKey] = true
	}

	for _, item := range current.Items {
		if item.Key == "" || !keys[item.Key] {
			return false
		}
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	if _, present := root["items"]; present {
		return false
	}
	return exactRawString(root, "id", current.Id) == nil &&
		exactRawString(root, "projectId", current.ProjectId) == nil &&
		exactRawString(root, "kind", "TEXT") == nil
}

func setItemIdentityIfChanged(ctx context.Context, q execer, desired ItemIdentity) (bool, error) {
	current, ok, err := itemIdentity(ctx, q, desired.TaskID, desired.ItemKey)
	if err != nil {
		return false, err
	}
	if ok && current.ServerID == desired.ServerID && current.State == desired.State {
		return false, nil
	}
	return true, setItemIdentity(ctx, q, desired)
}

func samePulledTask(a, b model.Task) bool {
	return a.Id == b.Id && a.ProjectId == b.ProjectId && a.Title == b.Title && a.Content == b.Content &&
		a.ParentId == b.ParentId && a.ColumnId == b.ColumnId && a.ColumnName == b.ColumnName &&
		a.EstimatedDuration == b.EstimatedDuration && a.EstimatedPomo == b.EstimatedPomo &&
		reflect.DeepEqual(a.ChildIds, b.ChildIds) && reflect.DeepEqual(a.FocusSummaries, b.FocusSummaries) &&
		a.Status == b.Status && a.Priority == b.Priority && sameTime(a.DueDate, b.DueDate) &&
		sameTime(a.StartDate, b.StartDate) && a.IsAllDay == b.IsAllDay && a.TimeZone == b.TimeZone &&
		a.RepeatFlag == b.RepeatFlag && reflect.DeepEqual(a.Reminders, b.Reminders) &&
		reflect.DeepEqual(a.Tags, b.Tags) && a.Kind == b.Kind && a.SortOrder == b.SortOrder &&
		sameTime(a.CreatedTime, b.CreatedTime) && sameTime(a.ModifiedTime, b.ModifiedTime) &&
		sameTime(a.CompletedTime, b.CompletedTime) && sameItems(a.Items, b.Items)
}

func adoptRecoveredChecklist(ctx context.Context, q execer, st ServerTask, projectName string) error {
	rawItems, present, err := decodeRawItems(st.Raw)
	if err != nil || !present || !sameServerItems(rawItems, st.Task.Items) {
		return errUnsoundRecovery
	}
	identities, err := itemIdentities(ctx, q, st.Task.Id)
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if err := setItemIdentity(ctx, q, ItemIdentity{TaskID: st.Task.Id, ItemKey: identity.ItemKey, ServerID: identity.ServerID, State: ItemAbandoned}); err != nil {
			return err
		}
	}
	desired := st.Task
	desired.Items = append([]model.Item(nil), rawItems...)
	for i := range desired.Items {
		key, err := newItemKey(ctx, q, st.Task.Id)
		if err != nil {
			return err
		}
		desired.Items[i].Key = key
		if err := setItemIdentity(ctx, q, ItemIdentity{TaskID: st.Task.Id, ItemKey: key, ServerID: desired.Items[i].Id, State: ItemBound}); err != nil {
			return err
		}
	}
	if _, err := upsertServerTask(ctx, q, ServerTask{Task: desired, Raw: st.Raw}, projectName); err != nil {
		return err
	}
	if err := replaceItems(ctx, q, desired.Id, desired.Items); err != nil {
		return err
	}
	if err := setItemIdentity(ctx, q, ItemIdentity{TaskID: st.Task.Id, State: RecoveryIdle}); err != nil {
		return err
	}
	return nil
}

func sameServerItems(a, b []model.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Id != b[i].Id || a[i].Title != b[i].Title || a[i].Status != b[i].Status ||
			a[i].SortOrder != b[i].SortOrder || a[i].IsAllDay != b[i].IsAllDay ||
			a[i].TimeZone != b[i].TimeZone || !sameTime(a[i].StartDate, b[i].StartDate) ||
			!sameTime(a[i].CompletedTime, b[i].CompletedTime) {
			return false
		}
	}
	return true
}
