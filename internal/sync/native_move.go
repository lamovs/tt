package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

func (s *Syncer) sendNativeMove(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	move, err := store.DecodeNativeMove(item)
	if err != nil {
		return "", pushPark, err
	}
	if move.Phase == "prepared" || move.Phase == "rejected" {
		remote, err := s.api.GetTaskRaw(ctx, move.FromProjectID, move.TaskID)
		if err != nil {
			return "", classify(item.Op, err), err
		}
		if remote.Task.ID != move.TaskID || remote.Task.ProjectID != move.FromProjectID {
			return "", pushPark, errors.New("native move source address changed")
		}
		if remote.Task.ParentID != "" || len(remote.Task.ChildIDs) != 0 || remote.Task.ColumnID != "" {
			return "", pushPark, errors.New("native move source gained an unsupported parent, child, or column relation")
		}
		if out, err := s.renewBefore(ctx, item, "the native move request"); err != nil {
			return "", out, err
		}
		if err := s.store.RecordNativeMovePhase(ctx, item, "armed", ""); err != nil {
			return "", pushAbort, err
		}
		results, err := s.api.MoveTasks(ctx, []api.TaskMove{{FromProjectID: move.FromProjectID, ToProjectID: move.ToProjectID, TaskID: move.TaskID}})
		if err != nil {
			if definitelyRejected(err) {
				if saved := s.store.RecordNativeMovePhase(settleContext(ctx), item, "rejected", ""); saved != nil {
					return "", pushAbort, saved
				}
			}
			return "", classify(item.Op, err), err
		}
		if len(results) != 1 || results[0].ID != move.TaskID || results[0].Etag == "" {
			return "", pushPark, errors.New("native move response did not confirm its task ID and etag")
		}
		if err := s.store.RecordNativeMovePhase(settleContext(ctx), item, "accepted", results[0].Etag); err != nil {
			return "", pushAbort, err
		}
	}
	if out, err := s.renewBefore(ctx, item, "the native move destination confirmation"); err != nil {
		return "", out, err
	}
	confirmed, err := s.api.GetTaskRaw(ctx, move.ToProjectID, move.TaskID)
	if err != nil {
		outcome := classify(item.Op, err)
		if statusIs(err, http.StatusNotFound) {
			outcome = pushPark
		}
		return "", outcome, fmt.Errorf("confirm native move by read only: %w", err)
	}
	if confirmed.Task.ID != move.TaskID || confirmed.Task.ProjectID != move.ToProjectID {
		return "", pushPark, errors.New("native move destination address was not confirmed")
	}
	if err := s.store.ConfirmNativeMove(settleContext(ctx), item, json.RawMessage(confirmed.Raw)); err != nil {
		return "", pushPark, err
	}
	return "", pushSettled, nil
}
