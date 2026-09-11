package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

const FeaturePayloadVersion = 1

const FeatureReplacementPayloadVersion = 2

const FeatureIntervalPayloadVersion = 3

const FeatureExtensionsPayloadVersion = 4

type FeaturePhase string

const (
	FeaturePrepared FeaturePhase = "prepared"
	FeatureArmed    FeaturePhase = "armed"
	FeatureAccepted FeaturePhase = "accepted"
	FeatureRejected FeaturePhase = "rejected"
	FeatureMismatch FeaturePhase = "mismatch"
)

type FeatureFields struct {
	Extensions bool `json:"extensions,omitempty"`
	Interval   bool `json:"interval,omitempty"`
	Items      bool `json:"items,omitempty"`
	RepeatFlag bool `json:"repeat_flag,omitempty"`
	Reminders  bool `json:"reminders,omitempty"`
	Kind       bool `json:"kind,omitempty"`
}

func (f FeatureFields) empty() bool {
	return !f.Items && !f.RepeatFlag && !f.Reminders && !f.Kind && !f.Interval && !f.Extensions
}

type FeatureItemSnapshot struct {
	Key           string `json:"key"`
	ID            string `json:"id,omitempty"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	SortOrder     int64  `json:"sort_order"`
	StartDate     string `json:"start_date"`
	IsAllDay      bool   `json:"is_all_day"`
	TimeZone      string `json:"time_zone"`
	CompletedTime string `json:"completed_time"`
}

func (s *FeatureItemSnapshot) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	required := []string{"key", "title", "status", "sort_order", "start_date", "is_all_day", "time_zone", "completed_time"}
	allowed := map[string]bool{"id": true}
	for _, name := range required {
		allowed[name] = true
		raw, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("feature snapshot item field %s is missing or null", name)
		}
	}
	for name, raw := range fields {
		if !allowed[name] {
			return fmt.Errorf("unknown feature snapshot item field %q", name)
		}
		if name == "id" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("feature snapshot item field id is null")
		}
	}
	type plain FeatureItemSnapshot
	return json.Unmarshal(data, (*plain)(s))
}

type FeatureItemBinding struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type FeatureSnapshot struct {
	Extensions    *model.TaskEdit        `json:"extensions,omitempty"`
	Interval      *dates.Interval        `json:"interval,omitempty"`
	Items         *[]FeatureItemSnapshot `json:"items,omitempty"`
	PriorBindings *[]FeatureItemBinding  `json:"prior_bindings,omitempty"`
	RepeatFlag    *string                `json:"repeat_flag,omitempty"`
	Reminders     *[]string              `json:"reminders,omitempty"`
	Kind          *string                `json:"kind,omitempty"`
}

type FeaturePayloadMetadata struct {
	ExtensionBaseline *model.TaskEdit  `json:"extension_baseline,omitempty"`
	Version           int              `json:"version"`
	Fields            FeatureFields    `json:"fields"`
	ItemKeys          []string         `json:"item_keys,omitempty"`
	Phase             FeaturePhase     `json:"phase"`
	Snapshot          *FeatureSnapshot `json:"snapshot,omitempty"`
	AcceptedTaskID    *string          `json:"accepted_task_id,omitempty"`
	AcceptedProjectID *string          `json:"accepted_project_id,omitempty"`
	AcceptedResponse  []byte           `json:"accepted_response,omitempty"`
}

func (m FeaturePayloadMetadata) ReplacesItems() bool {
	return m.Fields.Items && (m.Version == FeatureReplacementPayloadVersion || m.Version == FeatureIntervalPayloadVersion || m.Version == FeatureExtensionsPayloadVersion)
}

type FeatureUndoMetadata struct {
	Version  int           `json:"version"`
	Fields   FeatureFields `json:"fields"`
	ItemKeys []string      `json:"item_keys,omitempty"`
}

type taskPayload struct {
	model.Task
	Metadata *FeaturePayloadMetadata `json:"_tt,omitempty"`
}

type taskEditPayload struct {
	model.TaskEdit
	Metadata *FeaturePayloadMetadata `json:"_tt,omitempty"`
}

type undoPayload struct {
	UndoAction
	Metadata *FeatureUndoMetadata `json:"_tt,omitempty"`
}

func EncodeTaskPayload(task model.Task, metadata FeaturePayloadMetadata) ([]byte, error) {
	if err := validateFeatureMetadata(&metadata, &task, nil); err != nil {
		return nil, fmt.Errorf("encode task feature payload: %w", err)
	}
	return json.Marshal(taskPayload{Task: task, Metadata: &metadata})
}

func DecodeTaskPayload(payload []byte) (model.Task, *FeaturePayloadMetadata, error) {
	var wrapped taskPayload
	present, err := decodePrivateMetadata(payload, &wrapped.Metadata)
	if err != nil {
		return model.Task{}, nil, fmt.Errorf("decode task feature payload: %w", err)
	}
	if err := json.Unmarshal(payload, &wrapped.Task); err != nil {
		return model.Task{}, nil, fmt.Errorf("decode task feature payload: %w", err)
	}
	if !present {
		return wrapped.Task, nil, nil
	}
	if err := validateTaskRootPresence(payload, wrapped.Metadata.Fields); err != nil {
		return model.Task{}, nil, fmt.Errorf("decode task feature payload: %w", err)
	}
	if err := validateFeatureMetadata(wrapped.Metadata, &wrapped.Task, nil); err != nil {
		return model.Task{}, nil, fmt.Errorf("decode task feature payload: %w", err)
	}
	if wrapped.Metadata.Fields.Items {
		attachItemKeys(wrapped.Task.Items, wrapped.Metadata.ItemKeys)
	}
	return wrapped.Task, wrapped.Metadata, nil
}

func validateTaskRootPresence(payload []byte, fields FeatureFields) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return err
	}
	present := func(name string) bool {
		for key := range root {
			if strings.EqualFold(key, name) {
				return true
			}
		}
		return false
	}
	checks := []struct {
		name   string
		marked bool
		array  bool
	}{
		{"Items", fields.Items, true},
		{"RepeatFlag", fields.RepeatFlag, false},
		{"Reminders", fields.Reminders, true},
		{"Kind", fields.Kind, false},
		{"StartDate", fields.Interval, false},
		{"DueDate", fields.Interval, false},
		{"IsAllDay", fields.Interval, false},
		{"TimeZone", fields.Interval, false},
	}
	for _, check := range checks {
		if !check.marked {
			continue
		}
		var raw json.RawMessage
		for key, value := range root {
			if strings.EqualFold(key, check.name) {
				raw = value
				break
			}
		}
		if !present(check.name) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("_tt marks %s but the root value is missing or null", check.name)
		}
		if check.array {
			var values []json.RawMessage
			if err := json.Unmarshal(raw, &values); err != nil {
				return fmt.Errorf("_tt marks %s but the root value is not an array", check.name)
			}
		}
	}
	return nil
}

func EncodeTaskEditPayload(edit model.TaskEdit, metadata FeaturePayloadMetadata) ([]byte, error) {
	if err := validateFeatureMetadata(&metadata, nil, &edit); err != nil {
		return nil, fmt.Errorf("encode task edit feature payload: %w", err)
	}
	return json.Marshal(taskEditPayload{TaskEdit: edit, Metadata: &metadata})
}

func DecodeTaskEditPayload(payload []byte) (model.TaskEdit, *FeaturePayloadMetadata, error) {
	var wrapped taskEditPayload
	present, err := decodePrivateMetadata(payload, &wrapped.Metadata)
	if err != nil {
		return model.TaskEdit{}, nil, fmt.Errorf("decode task edit feature payload: %w", err)
	}
	if err := json.Unmarshal(payload, &wrapped.TaskEdit); err != nil {
		return model.TaskEdit{}, nil, fmt.Errorf("decode task edit feature payload: %w", err)
	}
	if !present {
		return wrapped.TaskEdit, nil, nil
	}
	if err := validateFeatureMetadata(wrapped.Metadata, nil, &wrapped.TaskEdit); err != nil {
		return model.TaskEdit{}, nil, fmt.Errorf("decode task edit feature payload: %w", err)
	}
	if wrapped.Metadata.Fields.Items {
		attachItemKeys([]model.Item(*wrapped.TaskEdit.Items), wrapped.Metadata.ItemKeys)
	}
	return wrapped.TaskEdit, wrapped.Metadata, nil
}

func EncodeUndoAction(action UndoAction, metadata FeatureUndoMetadata) ([]byte, error) {
	if err := validateUndoMetadata(&metadata, &action, false); err != nil {
		return nil, fmt.Errorf("encode feature undo: %w", err)
	}
	return json.Marshal(undoPayload{UndoAction: action, Metadata: &metadata})
}

func DecodeUndoAction(payload []byte) (UndoAction, *FeatureUndoMetadata, error) {
	var wrapped undoPayload
	present, err := decodePrivateMetadata(payload, &wrapped.Metadata)
	if err != nil {
		return UndoAction{}, nil, fmt.Errorf("decode feature undo: %w", err)
	}
	if err := json.Unmarshal(payload, &wrapped.UndoAction); err != nil {
		return UndoAction{}, nil, fmt.Errorf("decode feature undo: %w", err)
	}
	if !present {
		return wrapped.UndoAction, nil, nil
	}
	if err := validateUndoMetadata(wrapped.Metadata, &wrapped.UndoAction, true); err != nil {
		return UndoAction{}, nil, fmt.Errorf("decode feature undo: %w", err)
	}
	return wrapped.UndoAction, wrapped.Metadata, nil
}

func decodePrivateMetadata[T any](payload []byte, dst **T) (bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return false, err
	}
	raw, present := root["_tt"]
	if !present {
		return false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, errors.New("_tt metadata is null")
	}
	var metadata T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return false, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("_tt metadata has trailing JSON")
		}
		return false, err
	}
	*dst = &metadata
	return true, nil
}

func validateFeatureMetadata(metadata *FeaturePayloadMetadata, task *model.Task, edit *model.TaskEdit) error {
	if metadata == nil {
		return errors.New("missing _tt metadata")
	}
	if metadata.Version != FeaturePayloadVersion && metadata.Version != FeatureReplacementPayloadVersion && metadata.Version != FeatureIntervalPayloadVersion && metadata.Version != FeatureExtensionsPayloadVersion {
		return fmt.Errorf("unsupported _tt version %d", metadata.Version)
	}
	if metadata.Version == FeatureReplacementPayloadVersion && !metadata.Fields.Items {
		return errors.New("replacement _tt version requires the items field")
	}
	if metadata.Version != FeatureExtensionsPayloadVersion && (metadata.Version == FeatureIntervalPayloadVersion) != metadata.Fields.Interval {
		return errors.New("interval field requires interval payload version")
	}
	if (metadata.Version == FeatureExtensionsPayloadVersion) != metadata.Fields.Extensions {
		return errors.New("extensions require payload version 4")
	}
	if metadata.Fields.Extensions && edit != nil {
		if metadata.ExtensionBaseline == nil || !hasTaskExtensions(*metadata.ExtensionBaseline) || !onlyTaskExtensions(*metadata.ExtensionBaseline) {
			return errors.New("extension edit requires an exact baseline")
		}
		baseline := metadata.ExtensionBaseline
		if (edit.ParentId != nil) != (baseline.ParentId != nil) || (edit.ColumnId != nil) != (baseline.ColumnId != nil) ||
			(edit.EstimatedDuration != nil) != (baseline.EstimatedDuration != nil) || (edit.EstimatedPomo != nil) != (baseline.EstimatedPomo != nil) {
			return errors.New("extension baseline fields do not match edit")
		}
	} else if metadata.ExtensionBaseline != nil {
		return errors.New("extension baseline outside an extension edit")
	}
	if metadata.Fields.empty() {
		return errors.New("_tt fields are empty")
	}
	switch metadata.Phase {
	case FeaturePrepared, FeatureArmed, FeatureAccepted, FeatureRejected, FeatureMismatch:
	default:
		return fmt.Errorf("invalid _tt phase %q", metadata.Phase)
	}

	if err := validateFeatureRoot(metadata.Fields, task, edit); err != nil {
		return err
	}
	var items []model.Item
	if edit != nil && edit.Items != nil {
		items = []model.Item(*edit.Items)
	} else if task != nil {
		items = task.Items
	}
	if metadata.Fields.Items {
		if err := validateKeyVector(metadata.ItemKeys, len(items)); err != nil {
			return err
		}
		for i, item := range items {
			if item.Key != "" && item.Key != metadata.ItemKeys[i] {
				return fmt.Errorf("root item %d key %q does not match item_keys", i+1, item.Key)
			}
		}
	} else if len(metadata.ItemKeys) != 0 {
		return errors.New("_tt has item_keys without the items field")
	}

	if metadata.Phase == FeaturePrepared {
		if metadata.Snapshot != nil {
			return errors.New("prepared _tt payload has a frozen snapshot")
		}
	} else {
		if metadata.Snapshot == nil {
			return fmt.Errorf("%s _tt payload has no frozen snapshot", metadata.Phase)
		}
		if err := validateFeatureSnapshot(metadata.Fields, metadata.ItemKeys, metadata.Snapshot); err != nil {
			return err
		}
		if metadata.ReplacesItems() {
			for i, item := range *metadata.Snapshot.Items {
				if item.ID != "" {
					return fmt.Errorf("replacement snapshot item %d has a server id", i+1)
				}
			}
		}
	}

	accepted := metadata.AcceptedTaskID != nil || metadata.AcceptedProjectID != nil || len(metadata.AcceptedResponse) != 0
	if accepted {
		if metadata.AcceptedTaskID == nil || metadata.AcceptedProjectID == nil || len(metadata.AcceptedResponse) == 0 {
			return errors.New("accepted _tt evidence is incomplete")
		}
		if *metadata.AcceptedTaskID == "" || IsLocalID(*metadata.AcceptedTaskID) {
			return errors.New("accepted _tt task id is not a server id")
		}
		if *metadata.AcceptedProjectID == "" {
			return errors.New("accepted _tt project id is empty")
		}
		if metadata.Phase != FeatureAccepted && metadata.Phase != FeatureMismatch {
			return fmt.Errorf("%s _tt payload carries accepted response evidence", metadata.Phase)
		}
	}
	return nil
}

func validateFeatureRoot(fields FeatureFields, task *model.Task, edit *model.TaskEdit) error {
	if task != nil && edit != nil {
		return errors.New("feature payload has two roots")
	}
	if task == nil && edit == nil {
		return errors.New("feature payload has no root")
	}
	if task != nil && fields.Extensions != hasTaskExtensions(extensionEditOfTask(*task)) {
		return errors.New("task extensions do not match field mask")
	}
	if edit != nil && fields.Extensions != hasTaskExtensions(*edit) {
		return errors.New("edit extensions do not match field mask")
	}
	if fields.Interval {
		if _, err := intervalRoot(task, edit); err != nil {
			return err
		}
	}
	if task != nil {
		if fields.Items && task.Items == nil {
			return errors.New("_tt marks items but the task has no explicit items array")
		}
		if !fields.Items && len(task.Items) != 0 {
			return errors.New("task has items outside the _tt field mask")
		}
		if fields.Reminders && task.Reminders == nil {
			return errors.New("_tt marks reminders but the task has no explicit reminders array")
		}
		if !fields.Reminders && len(task.Reminders) != 0 {
			return errors.New("task has reminders outside the _tt field mask")
		}
		if !fields.RepeatFlag && task.RepeatFlag != "" {
			return errors.New("task has repeat_flag outside the _tt field mask")
		}
		if !fields.Kind && task.Kind != "" {
			return errors.New("task has kind outside the _tt field mask")
		}
		return nil
	}
	present := FeatureFields{
		Items:      edit.Items != nil,
		RepeatFlag: edit.RepeatFlag != nil,
		Reminders:  edit.Reminders != nil,
		Kind:       edit.Kind != nil,
	}
	checks := []struct {
		name          string
		marked, found bool
	}{
		{"items", fields.Items, present.Items},
		{"repeat_flag", fields.RepeatFlag, present.RepeatFlag},
		{"reminders", fields.Reminders, present.Reminders},
		{"kind", fields.Kind, present.Kind},
	}
	for _, check := range checks {
		if check.marked != check.found {
			return fmt.Errorf("_tt field %s=%t but root presence is %t", check.name, check.marked, check.found)
		}
	}
	return nil
}

func validateFeatureSnapshot(fields FeatureFields, keys []string, snapshot *FeatureSnapshot) error {
	if (snapshot.Extensions != nil) != fields.Extensions {
		return errors.New("extensions snapshot does not match fields")
	}
	if snapshot.Extensions != nil && (!hasTaskExtensions(*snapshot.Extensions) || !onlyTaskExtensions(*snapshot.Extensions)) {
		return errors.New("invalid extensions snapshot")
	}
	if (snapshot.Interval != nil) != fields.Interval {
		return errors.New("interval snapshot does not match fields")
	}
	if snapshot.Interval != nil {
		if err := snapshot.Interval.Validate(); err != nil {
			return err
		}
	}
	if fields.Items {
		if snapshot.Items == nil || snapshot.PriorBindings == nil {
			return errors.New("item feature snapshot is incomplete")
		}
		items := *snapshot.Items
		if len(items) != len(keys) {
			return fmt.Errorf("feature snapshot has %d items for %d item_keys", len(items), len(keys))
		}
		for i, item := range items {
			if item.Key != keys[i] {
				return fmt.Errorf("feature snapshot item %d key %q does not match item_keys", i+1, item.Key)
			}
			if item.ID != "" && IsLocalID(item.ID) {
				return fmt.Errorf("feature snapshot item %d has local server id %q", i+1, item.ID)
			}
		}
		seenKeys := map[string]bool{}
		seenIDs := map[string]bool{}
		for i, binding := range *snapshot.PriorBindings {
			if binding.Key == "" || binding.ID == "" || IsLocalID(binding.ID) {
				return fmt.Errorf("feature snapshot prior binding %d is invalid", i+1)
			}
			if seenKeys[binding.Key] || seenIDs[binding.ID] {
				return fmt.Errorf("feature snapshot prior binding %d is duplicated", i+1)
			}
			seenKeys[binding.Key] = true
			seenIDs[binding.ID] = true
		}
	} else if snapshot.Items != nil || snapshot.PriorBindings != nil {
		return errors.New("feature snapshot has items without the items field")
	}
	if (snapshot.RepeatFlag != nil) != fields.RepeatFlag {
		return errors.New("feature snapshot repeat_flag does not match fields")
	}
	if (snapshot.Reminders != nil) != fields.Reminders {
		return errors.New("feature snapshot reminders does not match fields")
	}
	if (snapshot.Kind != nil) != fields.Kind {
		return errors.New("feature snapshot kind does not match fields")
	}
	return nil
}

func validateUndoMetadata(metadata *FeatureUndoMetadata, action *UndoAction, attach bool) error {
	if metadata == nil {
		return errors.New("missing _tt metadata")
	}
	if metadata.Version != FeaturePayloadVersion && metadata.Version != FeatureExtensionsPayloadVersion {
		return fmt.Errorf("unsupported _tt version %d", metadata.Version)
	}
	if (metadata.Version == FeatureExtensionsPayloadVersion) != metadata.Fields.Extensions {
		return errors.New("undo extensions require payload version 4")
	}
	if metadata.Fields.Interval {
		return errors.New("undo interval values use the ordinary field snapshot")
	}
	if metadata.Fields.empty() {
		return errors.New("_tt fields are empty")
	}
	if err := validateFeatureRoot(metadata.Fields, action.Task, action.Before); err != nil {
		return fmt.Errorf("undo root: %w", err)
	}
	beforeItems := action.Before != nil && action.Before.Items != nil
	taskItems := action.Task != nil && action.Task.Items != nil
	if beforeItems && taskItems {
		return errors.New("undo _tt has two item snapshots")
	}
	if !metadata.Fields.Items {
		if len(metadata.ItemKeys) != 0 {
			return errors.New("undo _tt has item_keys without the items field")
		}
		if beforeItems || taskItems {
			return errors.New("undo has an item snapshot outside the _tt field mask")
		}
		return nil
	}
	var items []model.Item
	var destination []model.Item
	hasDestination := false
	if beforeItems {
		items = []model.Item(*action.Before.Items)
		destination = items
		hasDestination = true
	}
	if taskItems {
		if hasDestination {
			return errors.New("undo _tt has two item snapshots")
		}
		items = action.Task.Items
		destination = action.Task.Items
		hasDestination = true
	}
	if !hasDestination {
		return errors.New("undo _tt marks items but the action has no item snapshot")
	}
	if err := validateKeyVector(metadata.ItemKeys, len(items)); err != nil {
		return err
	}
	for i, item := range items {
		if item.Key != "" && item.Key != metadata.ItemKeys[i] {
			return fmt.Errorf("undo item %d key %q does not match item_keys", i+1, item.Key)
		}
	}
	if attach {
		attachItemKeys(destination, metadata.ItemKeys)
		if action.Before != nil && action.Before.Items != nil {
			*action.Before.Items = model.EditList[model.Item](destination)
		}
	}
	return nil
}

func validateKeyVector(keys []string, itemCount int) error {
	if len(keys) != itemCount {
		return fmt.Errorf("_tt has %d item_keys for %d items", len(keys), itemCount)
	}
	seen := make(map[string]bool, len(keys))
	for i, key := range keys {
		if key == "" {
			return fmt.Errorf("_tt item key %d is empty", i+1)
		}
		if seen[key] {
			return fmt.Errorf("_tt item key %d repeats %q", i+1, key)
		}
		seen[key] = true
	}
	return nil
}

func attachItemKeys(items []model.Item, keys []string) {
	for i := range items {
		items[i].Key = keys[i]
	}
}
