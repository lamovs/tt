package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/movsar/tt/internal/model"
)

type FeatureSend struct {
	TaskID    string
	ProjectID string
	Task      *model.Task
	Edit      *model.TaskEdit
	Metadata  FeaturePayloadMetadata
}

type FeatureConfirmation struct {
	TaskID    string
	ProjectID string
	Raw       []byte
	Bindings  []FeatureItemBinding
}

func (s *Store) PrepareFeatureSend(ctx context.Context, claimed OutboxItem) (FeatureSend, bool, bool, error) {
	var (
		out     FeatureSend
		present bool
		post    bool
	)
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		item, err := featureOutboxItem(ctx, tx, claimed.Seq, claimed.LeaseToken)
		if err != nil {
			return err
		}
		out, present, err = decodeFeatureSend(item)
		if err != nil {
			return err
		}
		if !present {
			return validateLegacyChecklistSend(item)
		}
		switch out.Metadata.Phase {
		case FeatureArmed, FeatureAccepted, FeatureMismatch:
			return nil
		case FeaturePrepared:
		case FeatureRejected:

			if out.Metadata.Snapshot == nil {
				return errors.New("rejected feature operation has no frozen snapshot")
			}
			out.Metadata.Phase = FeatureArmed
			if err := markFeatureSnapshotUncertain(ctx, tx, item.TaskID, out.Metadata); err != nil {
				return err
			}
			if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
				return err
			}
			post = true
			return writeFeatureMetadata(ctx, tx, item, out)
		default:
			return fmt.Errorf("unsupported feature phase %q", out.Metadata.Phase)
		}

		snapshot, err := freezeFeatureSnapshot(ctx, tx, item.TaskID, out)
		if err != nil {
			return err
		}
		out.Metadata.Snapshot = snapshot
		out.Metadata.Phase = FeatureArmed
		if err := markFeatureSnapshotUncertain(ctx, tx, item.TaskID, out.Metadata); err != nil {
			return err
		}
		if err := writeFeatureMetadata(ctx, tx, item, out); err != nil {
			return err
		}
		post = true
		return bumpItemIdentityEpoch(ctx, tx)
	})
	if err != nil {
		return FeatureSend{}, false, false, err
	}
	return out, present, post, nil
}

func validateLegacyChecklistSend(item OutboxItem) error {
	if item.Op != OpTaskCreate && item.Op != OpTaskUpdate && item.Op != OpTaskMove {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(item.Payload, &root); err != nil {
		return err
	}
	var raw json.RawMessage
	for name, value := range root {
		if !equalFoldASCII(name, "items") {
			continue
		}
		if raw != nil {
			return fmt.Errorf("%w: legacy checklist payload repeats items", ErrUnsafeChecklist)
		}
		raw = value
	}
	if raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("%w: legacy checklist payload items are not an array of objects", ErrUnsafeChecklist)
	}
	required := map[string]bool{
		"Id": true, "Title": true, "Status": true, "SortOrder": true,
		"StartDate": true, "IsAllDay": true, "TimeZone": true, "CompletedTime": true,
	}
	var items []model.Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("%w: legacy checklist payload has invalid item fields: %v", ErrUnsafeChecklist, err)
	}
	seen := make(map[string]bool, len(items))
	for i, record := range records {
		for name := range record {
			if !required[name] {
				return fmt.Errorf("%w: legacy checklist item %d has unknown field %q", ErrUnsafeChecklist, i+1, name)
			}
		}
		for name := range required {
			if _, ok := record[name]; !ok {
				return fmt.Errorf("%w: legacy checklist item %d is missing field %q", ErrUnsafeChecklist, i+1, name)
			}
		}
		if items[i].Id == "" || IsLocalID(items[i].Id) {
			return fmt.Errorf("%w: legacy checklist item %d has no usable server id", ErrUnsafeChecklist, i+1)
		}
		if seen[items[i].Id] {
			return fmt.Errorf("%w: legacy checklist item %d repeats server id %q", ErrUnsafeChecklist, i+1, items[i].Id)
		}
		seen[items[i].Id] = true
		for _, name := range []string{"Id", "Title", "Status", "SortOrder", "IsAllDay", "TimeZone"} {
			if bytes.Equal(bytes.TrimSpace(record[name]), []byte("null")) {
				return fmt.Errorf("%w: legacy checklist item %d field %s is null", ErrUnsafeChecklist, i+1, name)
			}
		}
	}
	return nil
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func featureOutboxItem(ctx context.Context, q execer, seq int64, token LeaseToken) (OutboxItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox
		WHERE seq = ? AND state = 'inflight' AND lease_token = ?`, seq, string(token))
	if err != nil {
		return OutboxItem{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return OutboxItem{}, err
		}
		return OutboxItem{}, notAffected(ctx, q, seq)
	}
	return scanOutbox(rows)
}

func decodeFeatureSend(item OutboxItem) (FeatureSend, bool, error) {
	out := FeatureSend{TaskID: item.TaskID, ProjectID: item.ProjectID}
	switch item.Op {
	case OpTaskCreate:
		task, metadata, err := DecodeTaskPayload(item.Payload)
		if err != nil {
			return FeatureSend{}, false, err
		}
		if metadata == nil {
			return FeatureSend{}, false, nil
		}
		if task.ProjectId == "" {
			task.ProjectId = item.ProjectID
		}
		out.Task, out.Metadata = &task, *metadata
	case OpTaskUpdate, OpTaskMove:
		edit, metadata, err := DecodeTaskEditPayload(item.Payload)
		if err != nil {
			return FeatureSend{}, false, err
		}
		if metadata == nil {
			return FeatureSend{}, false, nil
		}
		out.Edit, out.Metadata = &edit, *metadata
	default:
		return FeatureSend{}, false, nil
	}
	return out, true, nil
}

func freezeFeatureSnapshot(ctx context.Context, q execer, taskID string, send FeatureSend) (*FeatureSnapshot, error) {
	fields := send.Metadata.Fields
	snapshot := &FeatureSnapshot{}
	if send.Metadata.Fields.Extensions {
		var edit model.TaskEdit
		if send.Task != nil {
			edit = extensionEditOfTask(*send.Task)
		} else {
			edit = taskExtensionsOnly(*send.Edit)
		}
		current, err := loadTask(ctx, q, taskID)
		if err != nil {
			return nil, err
		}
		if edit.ColumnId != nil {
			if send.Task != nil || !columnOnlyEdit(edit) {
				return nil, errors.New("invalid column assignment payload")
			}
			if _, err := validateTaskColumnTarget(ctx, q, current, *edit.ColumnId); err != nil {
				return nil, err
			}
		} else if err := validateTaskExtensions(ctx, q, current, edit); err != nil {
			return nil, err
		}
		snapshot.Extensions = &edit
	}
	if fields.Interval {
		interval, err := intervalRoot(send.Task, send.Edit)
		if err != nil {
			return nil, err
		}
		snapshot.Interval = &interval
	}
	if fields.Items {
		var items []model.Item
		if send.Task != nil {
			items = send.Task.Items
		} else {
			items = []model.Item(*send.Edit.Items)
		}
		frozen := make([]FeatureItemSnapshot, len(items))
		for i, item := range items {
			identity, ok, err := itemIdentity(ctx, q, taskID, item.Key)
			if err != nil {
				return nil, err
			}
			if !ok || identity.State != ItemBound && identity.State != ItemUnbound {
				return nil, fmt.Errorf("%w: item key %q has no sendable registry provenance", ErrUnsafeChecklist, item.Key)
			}
			serverID := identity.ServerID
			if send.Metadata.ReplacesItems() {
				serverID = ""
			}
			frozen[i] = featureItemSnapshot(item, serverID)
		}
		bindings := []FeatureItemBinding{}
		identities, err := itemIdentities(ctx, q, taskID)
		if err != nil {
			return nil, err
		}
		for _, identity := range identities {
			if identity.State == ItemBound {
				bindings = append(bindings, FeatureItemBinding{Key: identity.ItemKey, ID: identity.ServerID})
			}
		}
		snapshot.Items = &frozen
		snapshot.PriorBindings = &bindings
	}
	if fields.RepeatFlag {
		if send.Task != nil {
			snapshot.RepeatFlag = model.Ptr(send.Task.RepeatFlag)
		} else {
			snapshot.RepeatFlag = model.Ptr(*send.Edit.RepeatFlag)
		}
	}
	if fields.Reminders {
		var reminders []string
		if send.Task != nil {
			reminders = append([]string(nil), send.Task.Reminders...)
		} else {
			reminders = append([]string(nil), []string(*send.Edit.Reminders)...)
		}
		if reminders == nil {
			reminders = []string{}
		}
		snapshot.Reminders = &reminders
	}
	if fields.Kind {
		if send.Task != nil {
			snapshot.Kind = model.Ptr(send.Task.Kind)
		} else {
			snapshot.Kind = model.Ptr(*send.Edit.Kind)
		}
	}
	return snapshot, nil
}

func featureItemSnapshot(item model.Item, serverID string) FeatureItemSnapshot {
	return FeatureItemSnapshot{
		Key: item.Key, ID: serverID, Title: item.Title, Status: item.Status.Wire(),
		SortOrder: item.SortOrder, StartDate: featureStamp(item.StartDate),
		IsAllDay: item.IsAllDay, TimeZone: item.TimeZone,
		CompletedTime: featureStamp(item.CompletedTime),
	}
}

func featureStamp(value model.Time) string {
	if value.IsZero() {
		return ""
	}
	stamp, _ := FormatStamp(value.Time).(string)
	return stamp
}

func markFeatureSnapshotUncertain(ctx context.Context, q execer, taskID string, metadata FeaturePayloadMetadata) error {
	snapshot := metadata.Snapshot
	if snapshot.Items == nil {
		return nil
	}
	prior := map[string]string{}
	if metadata.ReplacesItems() {
		for _, binding := range *snapshot.PriorBindings {
			prior[binding.Key] = binding.ID
		}
	}
	seen := map[string]bool{}
	for _, item := range *snapshot.Items {
		seen[item.Key] = true
		serverID := item.ID
		if metadata.ReplacesItems() {
			serverID = prior[item.Key]
		}
		if err := setItemIdentity(ctx, q, ItemIdentity{TaskID: taskID, ItemKey: item.Key, ServerID: serverID, State: ItemUncertain}); err != nil {
			return err
		}
	}
	for _, binding := range *snapshot.PriorBindings {
		if seen[binding.Key] {
			continue
		}
		if err := setItemIdentity(ctx, q, ItemIdentity{TaskID: taskID, ItemKey: binding.Key, ServerID: binding.ID, State: ItemUncertain}); err != nil {
			return err
		}
	}
	return nil
}

func writeFeatureMetadata(ctx context.Context, q execer, item OutboxItem, send FeatureSend) error {
	var payload []byte
	var err error
	if send.Task != nil {
		payload, err = EncodeTaskPayload(*send.Task, send.Metadata)
	} else {
		payload, err = EncodeTaskEditPayload(*send.Edit, send.Metadata)
	}
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx, `UPDATE outbox SET payload = ?, sent_at = ?
		WHERE seq = ? AND state = 'inflight' AND lease_token = ?`, string(payload), timeNowUnix(), item.Seq, string(item.LeaseToken))
	if err != nil {
		return fmt.Errorf("arm feature outbox %d: %w", item.Seq, err)
	}
	return affectedOne(ctx, q, res, item.Seq)
}

func timeNowUnix() int64 { return time.Now().Unix() }

func (s *Store) RecordFeatureEvidence(ctx context.Context, claimed OutboxItem, phase FeaturePhase, evidence FeatureConfirmation) error {
	if phase != FeatureAccepted && phase != FeatureMismatch {
		return fmt.Errorf("record feature evidence: invalid phase %q", phase)
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		item, err := featureOutboxItem(ctx, tx, claimed.Seq, claimed.LeaseToken)
		if err != nil {
			return err
		}
		send, present, err := decodeFeatureSend(item)
		if err != nil || !present {
			return err
		}
		send.Metadata.Phase = phase
		send.Metadata.AcceptedTaskID = model.Ptr(evidence.TaskID)
		send.Metadata.AcceptedProjectID = model.Ptr(evidence.ProjectID)
		send.Metadata.AcceptedResponse = append([]byte(nil), evidence.Raw...)
		if err := writeFeatureMetadata(ctx, tx, item, send); err != nil {
			return err
		}
		return bumpItemIdentityEpoch(ctx, tx)
	})
}

func (s *Store) RejectFeature(ctx context.Context, claimed OutboxItem) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		item, err := featureOutboxItem(ctx, tx, claimed.Seq, claimed.LeaseToken)
		if err != nil {
			return err
		}
		send, present, err := decodeFeatureSend(item)
		if err != nil || !present {
			return err
		}
		if send.Metadata.Snapshot == nil {
			return errors.New("reject feature operation without a frozen snapshot")
		}
		if send.Metadata.Snapshot.Items != nil {
			prior := make(map[string]string, len(*send.Metadata.Snapshot.PriorBindings))
			for _, binding := range *send.Metadata.Snapshot.PriorBindings {
				prior[binding.Key] = binding.ID
			}
			seen := map[string]bool{}
			for _, frozen := range *send.Metadata.Snapshot.Items {
				seen[frozen.Key] = true
				id, existed := prior[frozen.Key]
				state := ItemUnbound
				if existed {
					state = ItemBound
				}
				if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: item.TaskID, ItemKey: frozen.Key, ServerID: id, State: state}); err != nil {
					return err
				}
			}
			for _, binding := range *send.Metadata.Snapshot.PriorBindings {
				if seen[binding.Key] {
					continue
				}
				if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: item.TaskID, ItemKey: binding.Key, ServerID: binding.ID, State: ItemBound}); err != nil {
					return err
				}
			}
		}
		send.Metadata.Phase = FeatureRejected
		if err := writeFeatureMetadata(ctx, tx, item, send); err != nil {
			return err
		}
		return bumpItemIdentityEpoch(ctx, tx)
	})
}

func (s FeatureSend) FeatureAddress() (string, string, bool) {
	if s.Metadata.AcceptedTaskID != nil && s.Metadata.AcceptedProjectID != nil {
		return *s.Metadata.AcceptedTaskID, *s.Metadata.AcceptedProjectID, true
	}
	if !IsLocalID(s.TaskID) {
		return s.TaskID, s.ProjectID, true
	}
	return "", "", false
}

func ConfirmFeatureResponse(send FeatureSend, taskID, projectID string, raw []byte) (FeatureConfirmation, error) {
	confirmation := FeatureConfirmation{TaskID: taskID, ProjectID: projectID, Raw: append([]byte(nil), raw...)}
	if taskID == "" || IsLocalID(taskID) || projectID == "" {
		return confirmation, errors.New("feature confirmation has an invalid address")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return confirmation, fmt.Errorf("feature confirmation is invalid JSON: %w", err)
	}
	if err := exactRawString(root, "id", taskID); err != nil {
		return confirmation, err
	}
	if err := exactRawString(root, "projectId", projectID); err != nil {
		return confirmation, err
	}
	snapshot := send.Metadata.Snapshot
	if snapshot == nil {
		return confirmation, errors.New("feature confirmation has no frozen snapshot")
	}
	if snapshot.Extensions != nil {
		if err := confirmTaskExtensions(root, *snapshot.Extensions); err != nil {
			return confirmation, err
		}
	}
	if snapshot.Interval != nil {
		if err := confirmIntervalRoot(root, *snapshot.Interval); err != nil {
			return confirmation, err
		}
	}
	if snapshot.RepeatFlag != nil {
		if err := exactRawString(root, "repeatFlag", *snapshot.RepeatFlag); err != nil {
			return confirmation, err
		}
	}
	if snapshot.Kind != nil {
		if err := exactRawString(root, "kind", *snapshot.Kind); err != nil {
			return confirmation, err
		}
	}
	if snapshot.Reminders != nil {
		var got []string
		rawValue, ok := root["reminders"]
		var kind string
		_ = json.Unmarshal(root["kind"], &kind)

		omittedClear := !ok && len(*snapshot.Reminders) == 0 &&
			(kind == "TEXT" || kind == "NOTE" || kind == "CHECKLIST")
		if !omittedClear && (!ok || bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) || json.Unmarshal(rawValue, &got) != nil || !reflect.DeepEqual(got, *snapshot.Reminders)) {
			return confirmation, errors.New("feature confirmation does not contain the exact reminders value")
		}
	}
	if snapshot.Items == nil {
		return confirmation, nil
	}
	items, present, err := decodeCompleteConfirmationItems(raw)
	if err != nil {
		return confirmation, err
	}

	_, hasItems := root["items"]
	if !hasItems && send.Metadata.ReplacesItems() &&
		len(*snapshot.Items) == 0 &&
		exactRawString(root, "kind", "TEXT") == nil {
		items, present = []model.Item{}, true
	}
	if !present || len(items) != len(*snapshot.Items) {
		return confirmation, errors.New("feature confirmation does not contain the complete item replacement")
	}
	if send.Metadata.ReplacesItems() {
		priorIDs := make(map[string]bool, len(*snapshot.PriorBindings))
		for _, binding := range *snapshot.PriorBindings {
			priorIDs[binding.ID] = true
		}
		for i, item := range items {
			if priorIDs[item.Id] {
				return confirmation, fmt.Errorf("replacement confirmation item %d retained a prior bound id", i+1)
			}
		}
	}
	var parentTimeZone string
	if rawZone, ok := root["timeZone"]; ok {
		_ = json.Unmarshal(rawZone, &parentTimeZone)
	}
	used := make([]bool, len(items))
	bindings := make([]FeatureItemBinding, len(items))
	matched := make([]int, len(items))
	for i := range matched {
		matched[i] = -1
	}

	for i, want := range *snapshot.Items {
		if want.ID == "" {
			continue
		}
		for j, got := range items {
			if got.Id == want.ID {
				if used[j] || !featureWritableEqual(want, got, parentTimeZone) {
					return confirmation, fmt.Errorf("feature confirmation item %d does not match its known id", i+1)
				}
				used[j], matched[i] = true, j
				break
			}
		}
		if matched[i] < 0 {
			return confirmation, fmt.Errorf("feature confirmation item %d is missing its known id", i+1)
		}
	}
	for i, want := range *snapshot.Items {
		if want.ID != "" {
			continue
		}
		matches := []int{}
		for j, got := range items {
			if used[j] || !featureWritableEqual(want, got, parentTimeZone) {
				continue
			}
			matches = append(matches, j)
		}
		if len(matches) != 1 {
			return confirmation, fmt.Errorf("feature confirmation item %d has %d exact correspondences", i+1, len(matches))
		}
		j := matches[0]
		used[j], matched[i] = true, j
		bindings[i] = FeatureItemBinding{Key: want.Key, ID: items[j].Id}
	}
	for i, want := range *snapshot.Items {
		if matched[i] != i {
			return confirmation, errors.New("feature confirmation item order does not match the frozen replacement")
		}
		if want.ID != "" {
			bindings[i] = FeatureItemBinding{Key: want.Key, ID: want.ID}
		}
	}
	confirmation.Bindings = bindings
	return confirmation, nil
}

func exactRawString(root map[string]json.RawMessage, name, want string) error {
	raw, ok := root[name]
	var got string
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &got) != nil || got != want {
		return fmt.Errorf("feature confirmation does not contain the exact %s value", name)
	}
	return nil
}

func decodeCompleteConfirmationItems(raw []byte) ([]model.Item, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, err
	}
	value, present := root["items"]
	if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, false, nil
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(value, &records); err != nil {
		return nil, false, errors.New("feature confirmation items are not an array of objects")
	}

	required := []string{"id", "title", "status", "sortOrder", "isAllDay", "timeZone"}
	for i, record := range records {
		for _, name := range required {
			v, ok := record[name]
			if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return nil, false, fmt.Errorf("feature confirmation item %d lacks explicit %s", i+1, name)
			}
		}
	}
	items, _, err := decodeRawItems(raw)
	return items, true, err
}

func featureWritableEqual(want FeatureItemSnapshot, got model.Item, parentTimeZone string) bool {
	return want.Title == got.Title && want.Status == got.Status.Wire() && want.SortOrder == got.SortOrder &&
		want.IsAllDay == got.IsAllDay && featureTimeZoneEqual(want.TimeZone, got.TimeZone, parentTimeZone) &&
		sameStamp(want.StartDate, got.StartDate) && sameStamp(want.CompletedTime, got.CompletedTime)
}

func featureTimeZoneEqual(want, got, parent string) bool {
	if want == got {
		return true
	}
	if want != "" || parent == "" || parent == "Local" || got != parent {
		return false
	}
	_, err := time.LoadLocation(parent)
	return err == nil
}

func sameStamp(want string, got model.Time) bool {
	w, err := model.ParseTime(want)
	return err == nil && sameTime(w, got)
}

func (s *Store) SettleFeature(ctx context.Context, claimed OutboxItem, confirmation FeatureConfirmation) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		item, err := featureOutboxItem(ctx, tx, claimed.Seq, claimed.LeaseToken)
		if err != nil {
			return err
		}
		send, present, err := decodeFeatureSend(item)
		if err != nil || !present {
			return err
		}
		if err := MarkDoneTx(ctx, tx, item.Seq, item.LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, item.TaskID, item.Rev); err != nil {
			return err
		}
		if send.Metadata.Fields.Items {
			bound := make(map[string]string, len(confirmation.Bindings))
			for _, binding := range confirmation.Bindings {
				bound[binding.Key] = binding.ID
				if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: item.TaskID, ItemKey: binding.Key, ServerID: binding.ID, State: ItemBound}); err != nil {
					return err
				}
			}
			for _, prior := range *send.Metadata.Snapshot.PriorBindings {
				if _, kept := bound[prior.Key]; kept {
					continue
				}
				if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: item.TaskID, ItemKey: prior.Key, State: ItemUnbound}); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET raw = ? WHERE id = ?`, string(confirmation.Raw), item.TaskID); err != nil {
				return fmt.Errorf("refresh confirmed task raw: %w", err)
			}
		}
		if item.Op == OpTaskCreate && IsLocalID(item.TaskID) {
			twin, err := taskExists(ctx, tx, confirmation.TaskID)
			if err != nil {
				return err
			}
			if twin {
				return fmt.Errorf("accepted task %s already has a cached twin", confirmation.TaskID)
			}
			if err := ReplaceLocalIDTx(ctx, tx, item.TaskID, confirmation.TaskID); err != nil {
				return err
			}
		}
		return bumpItemIdentityEpoch(ctx, tx)
	})
}
