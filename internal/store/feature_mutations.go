package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/model"
)

var ErrUnsafeChecklist = errors.New("checklist cannot be replaced losslessly")

type featurePreparation struct {
	fields     FeatureFields
	adopt      []ItemIdentity
	newKeys    []string
	noop       bool
	registered bool
}

func featureFieldsOf(e model.TaskEdit) FeatureFields {
	_, intervalErr := intervalRoot(nil, &e)
	return FeatureFields{
		Extensions: hasTaskExtensions(e),
		Interval:   intervalErr == nil,
		Items:      e.Items != nil,
		RepeatFlag: e.RepeatFlag != nil,
		Reminders:  e.Reminders != nil,
		Kind:       e.Kind != nil,
	}
}

func featureFieldsOfTask(task model.Task) FeatureFields {
	return FeatureFields{
		Extensions: hasTaskExtensions(extensionEditOfTask(task)),
		Items:      task.Items != nil,
		RepeatFlag: task.RepeatFlag != "",
		Reminders:  task.Reminders != nil,
		Kind:       task.Kind != "",
	}
}

func featureEditNoop(cur model.Task, e model.TaskEdit) bool {
	next := applyEdit(cur, e)
	if e.Title != nil && next.Title != cur.Title ||
		e.Content != nil && next.Content != cur.Content ||
		e.Priority != nil && next.Priority != cur.Priority ||
		e.Status != nil && next.Status != cur.Status ||
		e.IsAllDay != nil && next.IsAllDay != cur.IsAllDay ||
		e.TimeZone != nil && next.TimeZone != cur.TimeZone ||
		e.RepeatFlag != nil && next.RepeatFlag != cur.RepeatFlag ||
		e.Kind != nil && next.Kind != cur.Kind ||
		e.SortOrder != nil && next.SortOrder != cur.SortOrder ||
		e.ParentId != nil && next.ParentId != cur.ParentId ||
		e.ColumnId != nil && next.ColumnId != cur.ColumnId ||
		e.EstimatedDuration != nil && next.EstimatedDuration != cur.EstimatedDuration ||
		e.EstimatedPomo != nil && next.EstimatedPomo != cur.EstimatedPomo ||
		e.ProjectId != nil && next.ProjectId != cur.ProjectId {
		return false
	}
	if e.DueDate != nil && !sameTime(next.DueDate, cur.DueDate) ||
		e.StartDate != nil && !sameTime(next.StartDate, cur.StartDate) ||
		e.CompletedTime != nil && !sameTime(next.CompletedTime, cur.CompletedTime) {
		return false
	}
	if e.Reminders != nil && !reflect.DeepEqual(next.Reminders, cur.Reminders) ||
		e.Tags != nil && !reflect.DeepEqual(next.Tags, cur.Tags) ||
		e.Items != nil && !sameItems(next.Items, cur.Items) {
		return false
	}
	return true
}

func sameTime(a, b model.Time) bool {
	return a.IsZero() && b.IsZero() || !a.IsZero() && !b.IsZero() && a.Equal(b.Time)
}

func sameItems(a, b []model.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Id != b[i].Id || a[i].Title != b[i].Title ||
			a[i].Status != b[i].Status || a[i].SortOrder != b[i].SortOrder ||
			a[i].IsAllDay != b[i].IsAllDay || a[i].TimeZone != b[i].TimeZone ||
			!sameTime(a[i].StartDate, b[i].StartDate) ||
			!sameTime(a[i].CompletedTime, b[i].CompletedTime) {
			return false
		}
	}
	return true
}

func prepareFeatureEdit(ctx context.Context, tx *sql.Tx, cur model.Task, e model.TaskEdit, newKeys []string) (featurePreparation, error) {
	p := featurePreparation{fields: featureFieldsOf(e), newKeys: newKeys}
	if p.fields.empty() {
		return p, nil
	}
	if p.fields.Items {
		adopt, registered, err := validateItemReplacement(ctx, tx, cur, []model.Item(*e.Items), newKeys)
		if err != nil {
			return featurePreparation{}, err
		}
		p.adopt = adopt
		p.registered = registered
	}
	p.noop = featureEditNoop(cur, e)
	if p.noop {
		return p, nil
	}
	registered, err := taskHasItemControl(ctx, tx, cur.Id)
	if err != nil {
		return featurePreparation{}, err
	}
	p.registered = registered
	return p, nil
}

func commitFeatureRegistration(ctx context.Context, tx *sql.Tx, taskID string, p featurePreparation) error {
	if err := ensureItemControl(ctx, tx, taskID); err != nil {
		return err
	}
	for _, identity := range p.adopt {
		if err := setItemIdentity(ctx, tx, identity); err != nil {
			return err
		}
	}
	for _, key := range p.newKeys {
		if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: taskID, ItemKey: key, State: ItemUnbound}); err != nil {
			return err
		}
	}
	return nil
}

func prepareCreatedTask(ctx context.Context, tx *sql.Tx, task *model.Task, preserveRanks bool) (FeatureFields, error) {
	_, intervalErr := intervalRoot(task, nil)
	fields := FeatureFields{
		Extensions: hasTaskExtensions(extensionEditOfTask(*task)),
		Interval:   intervalErr == nil,
		Items:      task.Items != nil,
		RepeatFlag: task.RepeatFlag != "",
		Reminders:  task.Reminders != nil,
	}
	if !fields.Items && !fields.RepeatFlag && !fields.Reminders && !fields.Interval && !fields.Extensions {
		return FeatureFields{}, nil
	}

	fields.Kind = task.Kind == "NOTE"
	if fields.Items {
		switch task.Kind {
		case "", "TEXT":
			if len(task.Items) != 0 {
				task.Kind = "CHECKLIST"
				fields.Kind = true
			}
		case "CHECKLIST":
			fields.Kind = true
		default:
			return FeatureFields{}, fmt.Errorf("%w: task kind %q is not an editable checklist", ErrUnsafeChecklist, task.Kind)
		}
		for i := range task.Items {
			key, err := newItemKey(ctx, tx, task.Id)
			if err != nil {
				return FeatureFields{}, err
			}
			task.Items[i].Key = key
			task.Items[i].Id = ""
			if !preserveRanks {
				task.Items[i].SortOrder = int64(i + 1)
			}
			if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: task.Id, ItemKey: key, State: ItemUnbound}); err != nil {
				return FeatureFields{}, err
			}
		}
	}
	if err := ensureItemControl(ctx, tx, task.Id); err != nil {
		return FeatureFields{}, err
	}
	return fields, nil
}

func encodeCreatedTask(task model.Task, fields FeatureFields) ([]byte, error) {
	if fields.empty() {
		return json.Marshal(task)
	}
	root := task
	if !fields.Items {
		root.Items = nil
	}
	if !fields.RepeatFlag {
		root.RepeatFlag = ""
	}
	if !fields.Reminders {
		root.Reminders = nil
	}
	if !fields.Kind {
		root.Kind = ""
	}
	keys := make([]string, len(root.Items))
	for i := range root.Items {
		keys[i] = root.Items[i].Key
	}
	return EncodeTaskPayload(root, FeaturePayloadMetadata{
		Version:  newFeaturePayloadVersion(fields),
		Fields:   fields,
		ItemKeys: keys,
		Phase:    FeaturePrepared,
	})
}

func newFeaturePayloadVersion(fields FeatureFields) int {
	if fields.Extensions {
		return FeatureExtensionsPayloadVersion
	}
	if fields.Interval {
		return FeatureIntervalPayloadVersion
	}
	if fields.Items {
		return FeatureReplacementPayloadVersion
	}
	return FeaturePayloadVersion
}

func taskHasItemControl(ctx context.Context, q execer, taskID string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM item_identities WHERE task_id = ? AND item_key = '')`, taskID).Scan(&n); err != nil {
		return false, fmt.Errorf("read item identity control for %s: %w", taskID, err)
	}
	return n != 0, nil
}

func bumpItemIdentityEpoch(ctx context.Context, tx *sql.Tx) error {
	const key = "item_identity_epoch"
	value, ok, err := getMeta(ctx, tx, key)
	if err != nil {
		return err
	}
	var epoch int64
	if ok {
		epoch, err = strconv.ParseInt(value, 10, 64)
		if err != nil || epoch < 0 || strconv.FormatInt(epoch, 10) != value {
			return fmt.Errorf("increment %s: invalid value %q", key, value)
		}
	}
	if epoch == math.MaxInt64 {
		return fmt.Errorf("increment %s: exhausted", key)
	}
	return SetMetaTx(ctx, tx, key, strconv.FormatInt(epoch+1, 10))
}

func BumpItemIdentityEpochIfRegisteredTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	registered, err := taskHasItemControl(ctx, tx, taskID)
	if err != nil || !registered {
		return false, err
	}
	return true, bumpItemIdentityEpoch(ctx, tx)
}

func validateItemReplacement(ctx context.Context, tx *sql.Tx, cur model.Task, desired []model.Item, newKeys []string) ([]ItemIdentity, bool, error) {
	switch cur.Kind {
	case "", "TEXT", "CHECKLIST":
	default:
		return nil, false, fmt.Errorf("%w: task kind %q is not an editable checklist", ErrUnsafeChecklist, cur.Kind)
	}
	identities, err := itemIdentities(ctx, tx, cur.Id)
	if err != nil {
		return nil, false, err
	}
	byKey := make(map[string]ItemIdentity, len(identities))
	for _, identity := range identities {
		byKey[identity.ItemKey] = identity
	}
	registered := len(identities) != 0
	var adopt []ItemIdentity
	if !registered {
		if !IsLocalID(cur.Id) {
			if err := validateImportedItemRaw(ctx, tx, cur); err != nil {
				return nil, false, err
			}
		} else if len(cur.Items) != 0 {
			return nil, false, fmt.Errorf("%w: legacy local checklist has no versioned provenance", ErrUnsafeChecklist)
		}
		for _, item := range cur.Items {
			if item.Key == "" || IsLocalID(item.Key) || item.Id == "" || item.Key != item.Id {
				return nil, false, fmt.Errorf("%w: legacy item %q has no proven server identity", ErrUnsafeChecklist, item.Title)
			}
			identity := ItemIdentity{TaskID: cur.Id, ItemKey: item.Key, ServerID: item.Id, State: ItemBound}
			adopt = append(adopt, identity)
			byKey[item.Key] = identity
		}
		registered = true
	} else if registered {
		if err := validateRetainedItemRaw(ctx, tx, cur.Id); err != nil {
			return nil, false, err
		}
		for _, item := range cur.Items {
			identity, ok := byKey[item.Key]
			if !ok {
				return nil, false, fmt.Errorf("%w: item key %q has no registry provenance", ErrUnsafeChecklist, item.Key)
			}
			if identity.State != ItemBound && identity.State != ItemUnbound && identity.State != ItemUncertain {
				return nil, false, fmt.Errorf("%w: item key %q is %s", ErrUnsafeChecklist, item.Key, identity.State)
			}
		}
	}

	allowedNew := make(map[string]bool, len(newKeys))
	for _, key := range newKeys {
		allowedNew[key] = true
	}
	seen := make(map[string]bool, len(desired))
	for i, item := range desired {
		if item.Key == "" || seen[item.Key] {
			return nil, false, fmt.Errorf("%w: desired item %d has a missing or duplicate key", ErrUnsafeChecklist, i+1)
		}
		seen[item.Key] = true
		if identity, ok := byKey[item.Key]; ok {
			if identity.State != ItemBound && identity.State != ItemUnbound && identity.State != ItemUncertain {
				return nil, false, fmt.Errorf("%w: item key %q is %s", ErrUnsafeChecklist, item.Key, identity.State)
			}
			continue
		}
		if !allowedNew[item.Key] || !IsLocalID(item.Key) {
			return nil, false, fmt.Errorf("%w: item key %q has no registry provenance", ErrUnsafeChecklist, item.Key)
		}
	}
	return adopt, registered, nil
}

func validateItemsForCopy(ctx context.Context, tx *sql.Tx, cur model.Task) error {
	if len(cur.Items) == 0 {
		return nil
	}
	identities, err := itemIdentities(ctx, tx, cur.Id)
	if err != nil {
		return err
	}
	if len(identities) == 0 {
		if IsLocalID(cur.Id) {
			return fmt.Errorf("%w: legacy local checklist has no versioned provenance", ErrUnsafeChecklist)
		}
		return validateImportedItemRaw(ctx, tx, cur)
	}
	byKey := make(map[string]ItemIdentity, len(identities))
	for _, identity := range identities {
		byKey[identity.ItemKey] = identity
	}
	if err := validateRetainedItemRaw(ctx, tx, cur.Id); err != nil {
		return err
	}
	for _, item := range cur.Items {
		identity, ok := byKey[item.Key]
		if !ok || identity.State != ItemBound && identity.State != ItemUnbound {
			return fmt.Errorf("%w: item key %q has no usable registry provenance", ErrUnsafeChecklist, item.Key)
		}
	}
	return nil
}

func validateRetainedItemRaw(ctx context.Context, q execer, taskID string) error {
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = ?`, taskID).Scan(&raw); err != nil {
		return fmt.Errorf("read raw checklist of %s: %w", taskID, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
		if !IsLocalID(taskID) {
			return fmt.Errorf("%w: task %s has no retained raw item array", ErrUnsafeChecklist, taskID)
		}
		return nil
	}
	_, present, err := decodeRawItems(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeChecklist, err)
	}
	if !present {
		if !IsLocalID(taskID) {
			current, err := loadTask(ctx, q, taskID)
			if err != nil {
				return err
			}
			identities, err := itemIdentities(ctx, q, taskID)
			if err != nil {
				return err
			}
			if confirmedEmptyChecklistRaw(current, raw, identities) {
				return nil
			}
			return fmt.Errorf("%w: task %s has no retained raw item array", ErrUnsafeChecklist, taskID)
		}
		return nil
	}
	return nil
}

func validateImportedItemRaw(ctx context.Context, q execer, cur model.Task) error {
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = ?`, cur.Id).Scan(&raw); err != nil {
		return fmt.Errorf("read raw checklist of %s: %w", cur.Id, err)
	}
	items, present, err := decodeRawItems(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeChecklist, err)
	}
	if !present {
		return fmt.Errorf("%w: task %s has no retained raw item array", ErrUnsafeChecklist, cur.Id)
	}
	if !sameItems(items, cur.Items) {
		return fmt.Errorf("%w: retained raw items do not match the imported checklist", ErrUnsafeChecklist)
	}
	return nil
}

func decodeRawItems(raw []byte) ([]model.Item, bool, error) {
	var root map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, errors.New("retained raw task is empty")
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, fmt.Errorf("retained raw task is invalid JSON: %w", err)
	}
	value, present := root["items"]
	if !present {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, false, errors.New("retained raw items are null")
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(value, &records); err != nil {
		return nil, false, errors.New("retained raw items are not an array of objects")
	}
	allowed := map[string]bool{
		"id": true, "title": true, "status": true, "sortOrder": true,
		"startDate": true, "isAllDay": true, "timeZone": true, "completedTime": true,
	}
	seenIDs := make(map[string]bool, len(records))
	out := make([]model.Item, 0, len(records))
	for i, record := range records {
		for name := range record {
			if !allowed[name] {
				return nil, false, fmt.Errorf("retained raw item %d has unknown field %q", i+1, name)
			}
		}
		var item model.Item
		if err := requiredRawString(record, "id", &item.Id); err != nil || item.Id == "" || IsLocalID(item.Id) {
			return nil, false, fmt.Errorf("retained raw item %d has invalid id", i+1)
		}
		if seenIDs[item.Id] {
			return nil, false, fmt.Errorf("retained raw item %d repeats id %q", i+1, item.Id)
		}
		seenIDs[item.Id] = true
		item.Key = item.Id
		if err := requiredRawString(record, "title", &item.Title); err != nil {
			return nil, false, fmt.Errorf("retained raw item %d has invalid title", i+1)
		}
		status, err := requiredRawInt(record, "status")
		if err != nil || status < 0 || status > 1 {
			return nil, false, fmt.Errorf("retained raw item %d has invalid status", i+1)
		}
		item.Status = model.ItemStatus(status)
		if raw, ok := record["sortOrder"]; ok {
			if item.SortOrder, err = rawInt(raw); err != nil {
				return nil, false, fmt.Errorf("retained raw item %d has invalid sortOrder", i+1)
			}
		}
		if raw, ok := record["isAllDay"]; ok {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &item.IsAllDay) != nil {
				return nil, false, fmt.Errorf("retained raw item %d has invalid isAllDay", i+1)
			}
		}
		if err := optionalRawString(record, "timeZone", &item.TimeZone); err != nil {
			return nil, false, fmt.Errorf("retained raw item %d has invalid timeZone", i+1)
		}
		if item.StartDate, err = rawItemTime(record["startDate"]); err != nil {
			return nil, false, fmt.Errorf("retained raw item %d has invalid startDate", i+1)
		}
		if item.CompletedTime, err = rawItemTime(record["completedTime"]); err != nil {
			return nil, false, fmt.Errorf("retained raw item %d has invalid completedTime", i+1)
		}
		out = append(out, item)
	}
	return out, true, nil
}

func rawItemTime(raw json.RawMessage) (model.Time, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return model.Time{}, nil
	}
	if raw[0] == '"' {
		var stamp string
		if err := json.Unmarshal(raw, &stamp); err != nil {
			return model.Time{}, err
		}
		return model.ParseTime(stamp)
	}
	var millis int64
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &millis) != nil {
		return model.Time{}, errors.New("item time is not a string or integral epoch milliseconds")
	}
	value := time.UnixMilli(millis).UTC()
	if value.Year() < 0 || value.Year() > 9999 || value.IsZero() {
		return model.Time{}, errors.New("item time milliseconds cannot be represented as a timestamp")
	}
	return model.NewTime(value), nil
}

func requiredRawString(record map[string]json.RawMessage, name string, dst *string) error {
	raw, ok := record[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("missing")
	}
	return json.Unmarshal(raw, dst)
}

func optionalRawString(record map[string]json.RawMessage, name string, dst *string) error {
	raw, ok := record[name]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null")
	}
	return json.Unmarshal(raw, dst)
}

func requiredRawInt(record map[string]json.RawMessage, name string) (int64, error) {
	raw, ok := record[name]
	if !ok {
		return 0, errors.New("missing")
	}
	return rawInt(raw)
}

func rawInt(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("not an integer")
	}
	return strconv.ParseInt(number.String(), 10, 64)
}

func (s *Store) AddTaskItem(ctx context.Context, taskID, title string) (model.Task, error) {
	outcome, err := s.AddTaskItemOutcome(ctx, taskID, title)
	return outcome.Task, err
}

func (s *Store) AddTaskItemOutcome(ctx context.Context, taskID, title string) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, taskID, nil, ItemChange{Action: ItemAdd, Title: title}, 0, 0)
}

func (s *Store) RenameTaskItem(ctx context.Context, taskID string, position int, title string) (model.Task, error) {
	outcome, err := s.RenameTaskItemOutcome(ctx, taskID, position, title)
	return outcome.Task, err
}

func (s *Store) RenameTaskItemOutcome(ctx context.Context, taskID string, position int, title string) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, taskID, nil, ItemChange{Action: ItemRename, Title: title}, position, 0)
}

func (s *Store) SetTaskItemDone(ctx context.Context, taskID string, position int, done bool) (model.Task, error) {
	outcome, err := s.SetTaskItemDoneOutcome(ctx, taskID, position, done)
	return outcome.Task, err
}

func (s *Store) SetTaskItemDoneOutcome(ctx context.Context, taskID string, position int, done bool) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, taskID, nil, ItemChange{Action: ItemSetDone, Done: done}, position, 0)
}

func (s *Store) RemoveTaskItem(ctx context.Context, taskID string, position int) (model.Task, error) {
	outcome, err := s.RemoveTaskItemOutcome(ctx, taskID, position)
	return outcome.Task, err
}

func (s *Store) RemoveTaskItemOutcome(ctx context.Context, taskID string, position int) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, taskID, nil, ItemChange{Action: ItemRemove}, position, 0)
}

func (s *Store) MoveTaskItem(ctx context.Context, taskID string, position, destination int) (model.Task, error) {
	outcome, err := s.MoveTaskItemOutcome(ctx, taskID, position, destination)
	return outcome.Task, err
}

func (s *Store) MoveTaskItemOutcome(ctx context.Context, taskID string, position, destination int) (TaskMutationOutcome, error) {
	return s.changeTaskItem(ctx, taskID, nil, ItemChange{Action: ItemMove}, position, destination)
}

func (s *Store) SetTaskRepeat(ctx context.Context, taskID, repeat string) (model.Task, error) {
	outcome, err := s.SetTaskRepeatOutcome(ctx, taskID, repeat)
	return outcome.Task, err
}

func (s *Store) SetTaskRepeatOutcome(ctx context.Context, taskID, repeat string) (TaskMutationOutcome, error) {
	return s.UpdateTaskOutcome(ctx, taskID, model.TaskEdit{RepeatFlag: &repeat})
}

func (s *Store) SetTaskReminders(ctx context.Context, taskID string, reminders []string) (model.Task, error) {
	outcome, err := s.SetTaskRemindersOutcome(ctx, taskID, reminders)
	return outcome.Task, err
}

func (s *Store) SetTaskRemindersOutcome(ctx context.Context, taskID string, reminders []string) (TaskMutationOutcome, error) {
	copyOfReminders := append([]string(nil), reminders...)
	if copyOfReminders == nil {
		copyOfReminders = []string{}
	}
	return s.UpdateTaskOutcome(ctx, taskID, model.TaskEdit{Reminders: model.NewEditList(copyOfReminders)})
}
