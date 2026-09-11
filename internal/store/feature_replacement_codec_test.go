package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestFeatureReplacementCodecKeepsVersionedFrozenMeaning(t *testing.T) {
	if FeaturePayloadVersion != 1 || FeatureReplacementPayloadVersion != 2 {
		t.Fatal("legacy/undo version must stay 1 and checklist replacement version must be 2")
	}
	for _, version := range []int{1, 2} {
		for _, phase := range []FeaturePhase{FeaturePrepared, FeatureArmed, FeatureRejected, FeatureAccepted, FeatureMismatch} {
			t.Run(fmt.Sprintf("v%d/%s", version, phase), func(t *testing.T) {
				task, edit, metadata := replacementCodecFixture()
				metadata.Version = version
				metadata.Phase = phase
				if version == 1 {
					(*metadata.Snapshot.Items)[0].ID = "old-1"
				}
				if phase == FeaturePrepared {
					metadata.Snapshot = nil
				}
				checkReplacementCodecs(t, task, edit, metadata, false)
			})
		}
	}
}

func TestFeatureReplacementCodecVersionRequiresItemsMask(t *testing.T) {
	for _, version := range []int{1, 2} {
		for mask := 0; mask < 16; mask++ {
			t.Run(fmt.Sprintf("v%d/mask%d", version, mask), func(t *testing.T) {
				fields := FeatureFields{
					Items: mask&1 != 0, RepeatFlag: mask&2 != 0,
					Reminders: mask&4 != 0, Kind: mask&8 != 0,
				}
				task := model.Task{Title: "Task"}
				edit := model.TaskEdit{}
				metadata := FeaturePayloadMetadata{Version: version, Fields: fields, Phase: FeaturePrepared}
				if fields.Items {

					task.Items = []model.Item{}
					edit.Items = model.NewEditList([]model.Item{})
				}
				if fields.RepeatFlag {
					task.RepeatFlag = "RRULE:FREQ=DAILY"
					edit.RepeatFlag = &task.RepeatFlag
				}
				if fields.Reminders {
					task.Reminders = []string{"TRIGGER:PT0S"}
					edit.Reminders = model.NewEditList(task.Reminders)
				}
				if fields.Kind {
					task.Kind = "CHECKLIST"
					edit.Kind = &task.Kind
				}
				wantError := fields.empty() || (version == 2 && !fields.Items)
				checkReplacementCodecs(t, task, edit, metadata, wantError)
			})
		}
	}
}

func TestFeatureReplacementCodecRejectsMalformedFrozenPayload(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*FeaturePayloadMetadata)
	}{
		{"known_retained_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.Items)[0].ID = "old-1" }},
		{"nonempty_new_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.Items)[1].ID = "new-server-id" }},
		{"local_frozen_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.Items)[0].ID = LocalIDPrefix + "item" }},
		{"empty_prior_key", func(m *FeaturePayloadMetadata) { (*m.Snapshot.PriorBindings)[0].Key = "" }},
		{"empty_prior_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.PriorBindings)[0].ID = "" }},
		{"local_prior_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.PriorBindings)[0].ID = LocalIDPrefix + "item" }},
		{"duplicate_prior_key", func(m *FeaturePayloadMetadata) { (*m.Snapshot.PriorBindings)[1].Key = "retained-key" }},
		{"duplicate_prior_id", func(m *FeaturePayloadMetadata) { (*m.Snapshot.PriorBindings)[1].ID = "old-1" }},
		{"missing_prior_bindings", func(m *FeaturePayloadMetadata) { m.Snapshot.PriorBindings = nil }},
		{"missing_frozen_items", func(m *FeaturePayloadMetadata) { m.Snapshot.Items = nil }},
		{"missing_snapshot", func(m *FeaturePayloadMetadata) { m.Snapshot = nil }},
		{"wrong_frozen_key", func(m *FeaturePayloadMetadata) { (*m.Snapshot.Items)[0].Key = "other-key" }},
		{"duplicate_item_key", func(m *FeaturePayloadMetadata) { m.ItemKeys[1] = m.ItemKeys[0] }},
		{"missing_item_key", func(m *FeaturePayloadMetadata) { m.ItemKeys = m.ItemKeys[:1] }},
		{"missing_items_mask", func(m *FeaturePayloadMetadata) { m.Fields.Items = false }},
		{"missing_marked_repeat", func(m *FeaturePayloadMetadata) { m.Fields.RepeatFlag = true }},
		{"unmarked_frozen_repeat", func(m *FeaturePayloadMetadata) {
			value := "RRULE:FREQ=DAILY"
			m.Snapshot.RepeatFlag = &value
		}},
		{"prepared_with_snapshot", func(m *FeaturePayloadMetadata) { m.Phase = FeaturePrepared }},
		{"unknown_version", func(m *FeaturePayloadMetadata) { m.Version = 4 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, edit, metadata := replacementCodecFixture()
			tc.mutate(&metadata)
			checkReplacementCodecs(t, task, edit, metadata, true)
		})
	}
}

func TestFeatureReplacementCodecUndoRemainsVersionOne(t *testing.T) {
	task, edit, payloadMetadata := replacementCodecFixture()
	actions := []struct {
		name   string
		action UndoAction
	}{
		{"edit", UndoAction{Op: OpTaskUpdate, TaskID: "task-1", Before: &edit}},
		{"task", UndoAction{Op: OpTaskCreate, Task: &task}},
	}
	for _, tc := range actions {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/v%d", tc.name, version), func(t *testing.T) {
				metadata := FeatureUndoMetadata{Version: version, Fields: payloadMetadata.Fields, ItemKeys: payloadMetadata.ItemKeys}
				encoded, encodeErr := EncodeUndoAction(tc.action, metadata)
				stored, err := json.Marshal(undoPayload{UndoAction: tc.action, Metadata: &metadata})
				if err != nil {
					t.Fatal(err)
				}
				decoded, gotMetadata, decodeErr := DecodeUndoAction(stored)
				if version == 2 {
					if encodeErr == nil || decodeErr == nil {
						t.Fatalf("undo v2 accepted: encode error=%v, decode error=%v", encodeErr, decodeErr)
					}
					return
				}
				if encodeErr != nil || decodeErr != nil {
					t.Fatalf("undo v1 refused: encode error=%v, decode error=%v", encodeErr, decodeErr)
				}
				if !reflect.DeepEqual(decoded, tc.action) || !reflect.DeepEqual(gotMetadata, &metadata) {
					t.Fatalf("undo v1 changed: action=%+v, metadata=%+v", decoded, gotMetadata)
				}
				if _, _, err := DecodeUndoAction(encoded); err != nil {
					t.Fatalf("encoded undo v1 refused: %v", err)
				}
			})
		}
	}
}

func replacementCodecFixture() (model.Task, model.TaskEdit, FeaturePayloadMetadata) {
	items := []model.Item{
		{Key: "retained-key", Id: "old-1", Title: "same", SortOrder: 2},
		{Key: "new-key", Title: "same", SortOrder: 4},
	}
	frozen := []FeatureItemSnapshot{
		{Key: "retained-key", Title: "same", SortOrder: 2},
		{Key: "new-key", Title: "same", SortOrder: 4},
	}
	prior := []FeatureItemBinding{
		{Key: "retained-key", ID: "old-1"},
		{Key: "removed-key", ID: "old-2"},
	}
	return model.Task{Title: "Task", Items: items}, model.TaskEdit{Items: model.NewEditList(items)}, FeaturePayloadMetadata{
		Version: 2, Fields: FeatureFields{Items: true}, ItemKeys: []string{"retained-key", "new-key"}, Phase: FeatureArmed,
		Snapshot: &FeatureSnapshot{Items: &frozen, PriorBindings: &prior},
	}
}

func checkReplacementCodecs(t *testing.T, task model.Task, edit model.TaskEdit, metadata FeaturePayloadMetadata, wantError bool) {
	t.Helper()
	for _, create := range []bool{true, false} {
		name := "edit"
		if create {
			name = "task"
		}
		t.Run(name, func(t *testing.T) {
			var encoded, stored []byte
			var encodeErr, marshalErr error
			if create {
				encoded, encodeErr = EncodeTaskPayload(task, metadata)
				stored, marshalErr = json.Marshal(taskPayload{Task: task, Metadata: &metadata})
			} else {
				encoded, encodeErr = EncodeTaskEditPayload(edit, metadata)
				stored, marshalErr = json.Marshal(taskEditPayload{TaskEdit: edit, Metadata: &metadata})
			}
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var gotMetadata *FeaturePayloadMetadata
			var decodeErr error
			var gotRoot, wantRoot any
			if create {
				gotRoot, gotMetadata, decodeErr = DecodeTaskPayload(stored)
				wantRoot = task
			} else {
				gotRoot, gotMetadata, decodeErr = DecodeTaskEditPayload(stored)
				wantRoot = edit
			}
			if wantError {
				if encodeErr == nil || decodeErr == nil {
					t.Fatalf("malformed payload accepted: encode error=%v, decode error=%v", encodeErr, decodeErr)
				}
				return
			}
			if encodeErr != nil || decodeErr != nil {
				t.Fatalf("valid payload refused: encode error=%v, decode error=%v", encodeErr, decodeErr)
			}
			if !reflect.DeepEqual(gotMetadata, &metadata) || !reflect.DeepEqual(gotRoot, wantRoot) {
				t.Fatalf("payload changed: root=%+v, metadata=%+v", gotRoot, gotMetadata)
			}
			if !reflect.DeepEqual(encoded, stored) {
				t.Fatal("encoding changed the stored payload")
			}
		})
	}
}
