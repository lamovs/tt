package sync

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func (s *Syncer) checkTaskParent(ctx context.Context, item store.OutboxItem, parent string) (pushOutcome, error) {
	seen := map[string]bool{item.TaskID: true}
	for parent != "" {
		if seen[parent] || store.IsLocalID(parent) {
			return pushPark, errors.New("remote parent chain is cyclic or has an unresolved identity")
		}
		if len(seen) > 100 {
			return pushPark, errors.New("remote parent chain exceeds the supported validation depth")
		}
		seen[parent] = true
		if out, err := s.renewBefore(ctx, item, "the remote parent-chain validation"); err != nil {
			return out, err
		}
		remote, err := s.api.GetTaskRaw(ctx, item.ProjectID, parent)
		if err != nil {
			return classify(item.Op, err), err
		}
		if remote.Task.ID != parent || remote.Task.ProjectID != item.ProjectID || remote.Task.Status != 0 {
			return pushPark, errors.New("remote parent must be an open task in the same project")
		}
		parent = remote.Task.ParentID
	}
	return pushDone, nil
}

func (s *Syncer) checkTaskColumn(ctx context.Context, item store.OutboxItem, columnID string) (pushOutcome, error) {
	if columnID == "" || store.IsLocalID(columnID) {
		return pushPark, errors.New("column move needs a confirmed destination column")
	}
	if out, err := s.renewBefore(ctx, item, "the destination column check"); err != nil {
		return out, err
	}
	columns, err := s.api.ListColumns(ctx, item.ProjectID)
	if err != nil {
		return classify(item.Op, err), err
	}
	for _, column := range columns {
		if column.ID == columnID && column.ProjectID == item.ProjectID {
			return pushDone, nil
		}
	}
	return pushPark, errors.New("destination column no longer belongs to the task project; prepare a new preview")
}

func validateColumnProtocol(item store.OutboxItem) error {
	switch item.Op {
	case store.OpTaskUpdate, store.OpTaskMove, store.OpTaskComplete:
	default:
		return nil
	}
	edit, metadata, err := store.DecodeTaskEditPayload(item.Payload)
	if err != nil {
		return err
	}
	if !hasColumnAssignment(&edit, metadata) {
		return nil
	}
	if item.Op != store.OpTaskUpdate || metadata == nil || metadata.Version != store.FeatureExtensionsPayloadVersion || !metadata.Fields.Extensions {
		return errors.New("column assignment requires a versioned column-only update; legacy payload was not sent")
	}
	if metadata.Phase != store.FeaturePrepared && metadata.Phase != store.FeatureRejected {
		// An armed request can only be read back. Do not change its uncertainty
		// into a rejection or authorize a new send from its retained root.
		return nil
	}
	if edit.ColumnId == nil || *edit.ColumnId == "" {
		return errors.New("column assignment requires a versioned column-only update; legacy payload was not sent")
	}
	destination := *edit.ColumnId
	edit.ColumnId = nil
	if !edit.IsEmpty() {
		return errors.New("column assignment cannot change other task fields in the same request")
	}
	if metadata.Snapshot != nil {
		frozen := metadata.Snapshot.Extensions
		if frozen == nil || frozen.ColumnId == nil || *frozen.ColumnId != destination {
			return errors.New("frozen column assignment does not match the reviewed destination; nothing was sent")
		}
		other := *frozen
		other.ColumnId = nil
		if !other.IsEmpty() {
			return errors.New("frozen column assignment contains other task fields; nothing was sent")
		}
	}
	return nil
}

func hasColumnAssignment(edit *model.TaskEdit, metadata *store.FeaturePayloadMetadata) bool {
	return edit != nil && edit.ColumnId != nil || metadata != nil && metadata.Snapshot != nil &&
		metadata.Snapshot.Extensions != nil && metadata.Snapshot.Extensions.ColumnId != nil
}

func checkColumnTaskStatus(raw []byte) error {
	var root map[string]json.RawMessage
	var status *int
	if json.Unmarshal(raw, &root) != nil || json.Unmarshal(root["status"], &status) != nil || status == nil || *status != 0 {
		return errors.New("column move requires an explicit open task status (status: 0); review current server state")
	}
	return nil
}
