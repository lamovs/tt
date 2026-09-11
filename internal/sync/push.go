package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type pushOutcome int

const (
	pushDone pushOutcome = iota
	pushSettled
	pushRetry
	pushSkip
	pushPark
	pushAbort

	pushGone
)

const RetryHint = `run "tt sync --retry-failed" to put parked entries back in line`

type ParkedError struct {
	Err error
}

func (e *ParkedError) Error() string { return e.Err.Error() }

func (e *ParkedError) Unwrap() error { return e.Err }

type unsentError struct{ Err error }

func (e *unsentError) Error() string { return e.Err.Error() }

func (e *unsentError) Unwrap() error { return e.Err }

func nothingSent(format string, a ...any) error {
	return &unsentError{Err: fmt.Errorf(format, a...)}
}

const releaseTimeout = 5 * time.Second

const settleTimeout = 30 * time.Second

func (s *Syncer) Push(ctx context.Context) (res Result, err error) {
	seen := map[int64]bool{}
	back := requeued{}

	var held []store.OutboxItem
	defer func() { s.release(ctx, &res, back, held) }()

	for {
		items, parked, err := s.store.Claim(ctx, 1, s.lease)
		if err != nil {
			return res, err
		}

		res.Failed += int(parked)
		res.ParkedUnsent += int(parked)
		if len(items) == 0 {

			return res, ctx.Err()
		}
		for i, item := range items {
			if err := ctx.Err(); err != nil {
				s.release(ctx, &res, back, items[i:])
				return res, err
			}
			if seen[item.Seq] {
				held = append(held, item)
				continue
			}
			seen[item.Seq] = true
			swap, err := s.pushOne(ctx, item, &res, back)
			if err != nil {

				items[i].LastError = cause(err)
				s.release(ctx, &res, back, releasable(items[i:], err))
				return res, err
			}

			if swap.serverID != "" {
				adoptServerID(items[i+1:], item.TaskID, swap)
			}
		}
	}
}

func adoptServerID(items []store.OutboxItem, localID string, swap idSwap) {
	for i := range items {
		items[i].Payload = bytes.ReplaceAll(items[i].Payload, []byte(localID), []byte(swap.serverID))
		if items[i].TaskID != localID {
			continue
		}
		items[i].TaskID = swap.serverID
		if swap.merged {
			items[i].Rev = 0
		}
	}
}

type idSwap struct {
	serverID string

	merged bool
}

func (s *Syncer) mergesIntoACachedRow(ctx context.Context, serverID string) bool {
	_, err := s.store.Task(ctx, serverID)
	return !errors.Is(err, store.ErrNotFound)
}

type requeued map[int64]bool

func (r requeued) count(res *Result, seq int64) {
	if r[seq] {
		return
	}
	r[seq] = true
	res.Requeued++
}

func (s *Syncer) release(ctx context.Context, res *Result, back requeued, items []store.OutboxItem) {
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	for _, item := range items {

		mine, _ := settle(res, item.Seq, s.store.Unclaim(ctx, item.Seq, item.LeaseToken, item.LastError))
		if mine {
			back.count(res, item.Seq)
		}
	}
}

func (s *Syncer) pushOne(ctx context.Context, item store.OutboxItem, res *Result, back requeued) (idSwap, error) {
	switch err := s.renewLease(ctx, item); {
	case err == nil:
	case notOurs(err):

		res.addError(fmt.Errorf("outbox %d: %w", item.Seq, err))
		return idSwap{}, nil
	default:

		return idSwap{}, fmt.Errorf("outbox %d: renew the lease of %s: %w", item.Seq, item.Op, err)
	}
	serverID, outcome, err := s.send(ctx, item)
	if outcome == pushPark && err != nil {

		err = &ParkedError{Err: fmt.Errorf("%w; %s", err, RetryHint)}
	}
	if outcome == pushAbort && errors.Is(err, api.ErrRateLimited) {

		err = fmt.Errorf("%w; %s", err, rateLimitWait(err))
	}
	var unsent *unsentError
	switch {
	case err == nil, outcome == pushAbort:

	case errors.As(err, &unsent):

		res.addUnsent(err)
	default:
		res.addError(err)
	}
	switch outcome {
	case pushSettled:
		res.Pushed++
		return idSwap{serverID: serverID}, nil
	case pushDone:
		swap := idSwap{serverID: serverID}
		if serverID != "" {
			swap.merged = s.mergesIntoACachedRow(ctx, serverID)
		}
		mine, serr := settle(res, item.Seq, s.commit(ctx, item, serverID))
		if serr != nil {

			return s.parkAnAcceptedPush(ctx, item, swap, res, serr)
		}
		if mine {
			res.Pushed++
		}

		return swap, nil
	case pushRetry:
		mine, serr := settle(res, item.Seq, s.store.Requeue(ctx, item.Seq, item.LeaseToken, cause(err)))
		if serr != nil {
			return idSwap{}, serr
		}
		if mine {
			back.count(res, item.Seq)
		}
	case pushSkip:

		mine, serr := settle(res, item.Seq, s.store.Unclaim(ctx, item.Seq, item.LeaseToken, cause(err)))
		if serr != nil {
			return idSwap{}, serr
		}
		if mine {
			back.count(res, item.Seq)
		}
	case pushPark:

		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		mine, serr := settle(res, item.Seq, s.store.MarkFailed(ctx, item.Seq, item.LeaseToken, cause(err)))
		if serr != nil {
			return idSwap{}, serr
		}
		if mine {
			res.Failed++
		}
	case pushAbort:

		return idSwap{}, err
	case pushGone:

	}
	return idSwap{}, nil
}

func (s *Syncer) renewLease(ctx context.Context, item store.OutboxItem) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return s.store.Renew(ctx, item.Seq, item.LeaseToken, s.lease)
}

func (s *Syncer) renewBefore(ctx context.Context, item store.OutboxItem, what string) (pushOutcome, error) {
	err := s.renewLease(ctx, item)
	switch {
	case err == nil:
		return pushDone, nil
	case notOurs(err):
		return pushGone, fmt.Errorf("outbox %d: %s of %s: %s was not made: %w",
			item.Seq, item.Op, item.TaskID, what, err)
	default:
		return pushAbort, fmt.Errorf("outbox %d: renew the lease of %s before %s: %w",
			item.Seq, item.Op, what, err)
	}
}

func (s *Syncer) markSent(ctx context.Context, item store.OutboxItem) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return s.store.MarkSent(ctx, item.Seq, item.LeaseToken)
}

func (s *Syncer) parkAnAcceptedPush(ctx context.Context, item store.OutboxItem, swap idSwap, res *Result, commitErr error) (idSwap, error) {
	if repeatable(item.Op) {
		return idSwap{}, commitErr
	}
	recorded := s.recordTheServerID(ctx, item.TaskID, swap.serverID)
	parked := &ParkedError{Err: acceptedPushError(item, swap.serverID, commitErr, recorded)}
	if !recorded {

		swap = idSwap{}
	}

	res.addError(parked)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	mine, serr := settle(res, item.Seq, s.store.MarkFailed(ctx, item.Seq, item.LeaseToken, cause(parked)))
	if serr != nil {
		return idSwap{}, commitErr
	}
	if mine {
		res.Failed++
	}
	return swap, nil
}

func acceptedPushError(item store.OutboxItem, serverID string, commitErr error, recorded bool) error {
	if recorded && serverID == "" {
		return fmt.Errorf(
			"outbox %d: create task %s was the server's already - the entry carries the id it gave the "+
				"task, so nothing was sent - but the entry could not be dropped from the queue: %w; it "+
				"is parked instead, and raising it costs nothing: a later pass finishes it without "+
				"sending anything",
			item.Seq, item.TaskID, commitErr)
	}
	if recorded {
		return fmt.Errorf(
			"outbox %d: create task %s reached the server, and the cache recorded the returned task id "+
				"but the entry could not be dropped from the queue: %w; it is parked "+
				"instead, and raising it costs nothing now: it carries the server's own id, so a later "+
				"pass finishes it without sending anything",
			item.Seq, item.TaskID, commitErr)
	}
	return fmt.Errorf(
		"outbox %d: create task %s reached the server, but its returned id could not be recorded and "+
			"cannot be recovered from this diagnostic: %w; "+
			"do not put this entry back in line - the task is on the server already, and sending the "+
			"create again would make a second copy; the server's own copy reaches the cache with the next pull",
		item.Seq, item.TaskID, commitErr)
}

func (s *Syncer) recordTheServerID(ctx context.Context, localID, serverID string) bool {
	if !store.IsLocalID(localID) {
		return true
	}
	if serverID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return s.store.ReplaceLocalID(ctx, localID, serverID) == nil
}

func settle(res *Result, seq int64, err error) (mine bool, fatal error) {
	switch {
	case err == nil:
		return true, nil
	case notOurs(err):
		res.addError(fmt.Errorf("outbox %d: %w", seq, err))
		return false, nil
	default:
		return false, &unsettledError{Err: err}
	}
}

type unsettledError struct{ Err error }

func (e *unsettledError) Error() string { return e.Err.Error() }

func (e *unsettledError) Unwrap() error { return e.Err }

func releasable(items []store.OutboxItem, err error) []store.OutboxItem {
	var unsettled *unsettledError
	if len(items) == 0 || repeatable(items[0].Op) || !errors.As(err, &unsettled) {
		return items
	}
	return items[1:]
}

func rateLimitWait(err error) string {
	var se *api.StatusError
	if !errors.As(err, &se) || !se.RetryAfterKnown {
		return "the server did not say when to try again"
	}
	d := se.RetryAfter.Round(time.Second)
	if d <= 0 {
		return "the server asks for no wait before the next request"
	}
	return fmt.Sprintf("the server asks for %s before the next request", d)
}

func (s *Syncer) commit(ctx context.Context, item store.OutboxItem, serverID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return s.store.Tx(ctx, func(tx *sql.Tx) error {

		if err := store.MarkDoneTx(ctx, tx, item.Seq, item.LeaseToken); err != nil {
			return err
		}

		if _, err := store.MarkPushedTx(ctx, tx, item.TaskID, item.Rev); err != nil {
			return err
		}
		if _, err := store.BumpItemIdentityEpochIfRegisteredTx(ctx, tx, item.TaskID); err != nil {
			return err
		}
		if serverID == "" {
			return nil
		}
		return store.ReplaceLocalIDTx(ctx, tx, item.TaskID, serverID)
	})
}

func (s *Syncer) send(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	if item.Target != store.TargetOpenAPI {

		return "", pushSkip, nothingSent("outbox %d: target %q is not supported yet", item.Seq, item.Target)
	}
	if item.TaskID == "" {
		return "", pushPark, fmt.Errorf("outbox %d: %s without a task id", item.Seq, item.Op)
	}
	if err := validateColumnProtocol(item); err != nil {
		return "", pushPark, err
	}
	if item.Op == store.OpTaskMoveNative {
		return s.sendNativeMove(ctx, item)
	}

	if store.IsLocalID(item.TaskID) && item.Op != store.OpTaskCreate && item.Op != store.OpTaskMoveDrop {
		return s.sendUnderALocalID(ctx, item)
	}
	parent, checkParent, err := store.TaskExtensionParent(item)
	if err != nil {
		return "", pushPark, err
	}
	if checkParent {
		if out, err := s.checkTaskParent(ctx, item, parent); err != nil {
			return "", out, err
		}
	}
	column, checkColumn, err := store.TaskExtensionColumn(item)
	if err != nil {
		return "", pushPark, err
	}
	if checkColumn {
		if out, err := s.checkTaskColumn(ctx, item, column); err != nil {
			return "", out, err
		}
	}
	baseline, err := store.TaskExtensionBaseline(item)
	if err != nil {
		return "", pushPark, err
	}
	if baseline != nil {
		if out, err := s.renewBefore(ctx, item, "the task extension conflict check"); err != nil {
			return "", out, err
		}
		remote, err := s.api.GetTaskRaw(ctx, item.ProjectID, item.TaskID)
		if err != nil {
			return "", classify(item.Op, err), err
		}
		if err := store.CheckTaskExtensionBaseline(remote.Raw, item.TaskID, item.ProjectID, *baseline); err != nil {
			return "", pushPark, err
		}
		if checkColumn {
			if err := checkColumnTaskStatus(remote.Raw); err != nil {
				return "", pushPark, err
			}
			if remote.Task.Status != 0 || remote.Task.ParentID != "" || len(remote.Task.ChildIDs) != 0 {
				return "", pushPark, errors.New("column move requires an open independent task; review changed server state")
			}
		}
	}
	feature, versioned, post, err := s.store.PrepareFeatureSend(ctx, item)
	if err != nil {
		if notOurs(err) {
			return "", pushGone, fmt.Errorf("outbox %d: prepare versioned mutation: %w", item.Seq, err)
		}
		return "", pushPark, fmt.Errorf("outbox %d: prepare versioned mutation: %w", item.Seq, err)
	}
	if versioned {
		return s.sendVersioned(ctx, item, feature, post)
	}

	switch item.Op {
	case store.OpTaskCreate:
		return s.sendCreate(ctx, item)
	case store.OpTaskUpdate, store.OpTaskMove:

		return s.sendEdit(ctx, item)
	case store.OpTaskComplete:
		return s.sendComplete(ctx, item)
	case store.OpTaskDelete:
		return s.sendDelete(ctx, item)
	case store.OpTaskMoveDrop:
		return s.sendMoveDrop(ctx, item)
	default:
		return "", pushPark, fmt.Errorf("outbox %d: unknown op %q", item.Seq, item.Op)
	}
}

func (s *Syncer) sendUnderALocalID(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	queued, err := s.store.HasQueuedCreate(ctx, item.TaskID)
	if err != nil {

		return "", pushAbort, fmt.Errorf("outbox %d: %s of %s: %w", item.Seq, item.Op, item.TaskID, err)
	}
	if queued {
		return "", pushSkip, nothingSent("outbox %d: %s of %s waits for the create of that task, still queued",
			item.Seq, item.Op, item.TaskID)
	}
	return "", pushPark, fmt.Errorf(
		"outbox %d: %s of %s: nothing is left in the queue to create that task, so there is no id "+
			"the server would take this under; whether it has the task all the same is not known here - "+
			"the create is parked or gone, and a park can be made over an answer that was lost as "+
			"readily as over one that arrived and could not be written down",
		item.Seq, item.Op, item.TaskID)
}

func (s *Syncer) sendCreate(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	if !store.IsLocalID(item.TaskID) {
		return "", pushDone, nothingSent(
			"outbox %d: create task %s: the server has this task already - the entry carries the id it "+
				"gave the task - so nothing was sent",
			item.Seq, item.TaskID)
	}
	var t model.Task
	if err := json.Unmarshal(item.Payload, &t); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: decode %s payload: %w", item.Seq, item.Op, err)
	}
	if t.ProjectId == "" {
		t.ProjectId = item.ProjectID
	}

	if err := s.markSent(ctx, item); err != nil {
		if notOurs(err) {
			return "", pushGone, fmt.Errorf("outbox %d: create task %s: %w", item.Seq, item.TaskID, err)
		}

		return "", pushSkip, fmt.Errorf(
			"outbox %d: create task %s: the queue could not be told the create is going out: %w",
			item.Seq, item.TaskID, err)
	}
	created, err := s.api.CreateTask(ctx, api.TaskCreateFrom(t))
	if err != nil {
		var guard *api.RequestGuardError
		if errors.As(err, &guard) {
			if recordErr := s.store.RecordTaskRequestUnsent(settleContext(ctx), item); recordErr != nil {
				return "", pushAbort, recordErr
			}
		}
		return "", classify(item.Op, err), fmt.Errorf("outbox %d: create task %s: %w", item.Seq, item.TaskID, err)
	}
	if store.IsLocalID(created.ID) {

		return "", pushDone, fmt.Errorf(
			"outbox %d: create task %s: the answer carried an unusable local-prefixed id",
			item.Seq, item.TaskID)
	}
	return created.ID, pushDone, nil
}

func (s *Syncer) sendEdit(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	var e model.TaskEdit
	if err := json.Unmarshal(item.Payload, &e); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: decode %s payload: %w", item.Seq, item.Op, err)
	}
	if e.ProjectId != nil && *e.ProjectId != item.ProjectID {
		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the entry asks for the task to be put in list %s, but this update endpoint "+
				"cannot move a task between lists - an update naming another list is answered "+
				"with a success and applied in no part, so sending this would lose the rest of the edit "+
				"without a word; tt mv uses its separate configured recreate workflow",
			item.Seq, item.Op, item.TaskID, *e.ProjectId)
	}
	if err := s.api.UpdateTask(ctx, api.TaskUpdateFrom(item.TaskID, item.ProjectID, e)); err != nil {
		return "", classify(item.Op, err), fmt.Errorf("outbox %d: update task %s: %w", item.Seq, item.TaskID, err)
	}
	return s.confirmTheUpdateLanded(ctx, item)
}

func (s *Syncer) confirmTheUpdateLanded(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {

	if out, err := s.renewBefore(ctx, item, "the read that confirms the update"); err != nil {
		return "", out, err
	}
	got, err := s.api.GetTask(ctx, item.ProjectID, item.TaskID)
	if err != nil {
		out := classify(item.Op, err)
		if out == pushAbort {
			return "", pushAbort, fmt.Errorf(
				"outbox %d: read task %s back after the %s: %w", item.Seq, item.TaskID, item.Op, err)
		}
		if statusIs(err, http.StatusNotFound) {
			return "", pushPark, fmt.Errorf(
				"outbox %d: %s of %s: the task is not in list %s any more - another client moved or "+
					"deleted it - and the server answers an update addressed to the wrong list with a "+
					"success and applies no part of it, so nothing was changed; the entry is kept rather "+
					"than counted as sent",
				item.Seq, item.Op, item.TaskID, item.ProjectID)
		}
		if out == pushRetry {

			if errors.Is(err, api.ErrServerError) {

				if out, err := s.renewBefore(ctx, item, "the question about the list"); err != nil {
					return "", out, err
				}
				if s.theProjectIsGone(ctx, item.ProjectID) {
					return "", pushPark, fmt.Errorf(
						"outbox %d: %s of %s: list %s is not on the server any more - another "+
							"client deleted it - so the update was addressed at a list the "+
							"server could not find, and such an update is answered with a success "+
							"and applied in no part; the entry is kept rather than counted as sent",
						item.Seq, item.Op, item.TaskID, item.ProjectID)
				}
			}

			return "", pushRetry, fmt.Errorf(
				"outbox %d: %s of %s: the server took the request, and the task could not be read back "+
					"from list %s to see that it is there: %w; the entry goes back in line, since an "+
					"update the server has already applied costs nothing to send again",
				item.Seq, item.Op, item.TaskID, item.ProjectID, err)
		}
		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the server took the request, but the task could not be read back from "+
				"list %s to see that it is there: %w; an update addressed to a list the task is not "+
				"in is answered with a success and applied in no part, so whether this one was applied is "+
				"not known and the entry is kept rather than counted as sent",
			item.Seq, item.Op, item.TaskID, item.ProjectID, err)
	}
	if got.ProjectID != "" && got.ProjectID != item.ProjectID {
		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the task the server answered with is in a different list from the %s the "+
				"request was addressed to, so no part of it was applied; the entry is kept rather than "+
				"counted as sent",
			item.Seq, item.Op, item.TaskID, item.ProjectID)
	}
	var edit model.TaskEdit
	if err := json.Unmarshal(item.Payload, &edit); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: decode confirmed update: %w", item.Seq, err)
	}
	if err := confirmScheduleEdit(*got, edit); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: %w; the entry is kept rather than counted as sent", item.Seq, err)
	}
	return "", pushDone, nil
}

func (s *Syncer) theProjectIsGone(ctx context.Context, projectID string) bool {
	_, err := s.api.GetProjectData(ctx, projectID)
	return errors.Is(err, api.ErrNoSuchProject)
}

func (s *Syncer) sendComplete(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	var e model.TaskEdit
	if err := json.Unmarshal(item.Payload, &e); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: decode %s payload: %w", item.Seq, item.Op, err)
	}
	asUpdate := e.Items != nil
	send := func() error {
		if asUpdate {
			return s.api.UpdateTask(ctx, api.TaskUpdateFrom(item.TaskID, item.ProjectID, e))
		}
		return s.api.CompleteTask(ctx, item.ProjectID, item.TaskID)
	}
	if err := send(); err != nil {
		return "", classify(item.Op, err), fmt.Errorf("outbox %d: complete task %s: %w", item.Seq, item.TaskID, err)
	}
	if asUpdate {
		return s.confirmTheUpdateLanded(ctx, item)
	}
	return "", pushDone, nil
}

func (s *Syncer) sendDelete(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	if err := s.api.DeleteTask(ctx, item.ProjectID, item.TaskID); err != nil {
		return "", classify(item.Op, err), fmt.Errorf("outbox %d: delete task %s: %w", item.Seq, item.TaskID, err)
	}
	return "", pushDone, nil
}

func (s *Syncer) sendMoveDrop(ctx context.Context, item store.OutboxItem) (string, pushOutcome, error) {
	var d store.MoveDrop
	if err := json.Unmarshal(item.Payload, &d); err != nil {
		return "", pushPark, fmt.Errorf("outbox %d: decode %s payload: %w", item.Seq, item.Op, err)
	}
	if store.IsLocalID(item.TaskID) {
		return s.dropUnderALocalOriginal(ctx, item, d)
	}
	if d.CopyID == "" {
		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the entry does not say which copy the move was made into, so there is "+
				"nothing to check the task against before deleting it",
			item.Seq, item.Op, item.TaskID)
	}
	if store.IsLocalID(d.CopyID) {
		queued, err := s.store.HasQueuedCreate(ctx, d.CopyID)
		if err != nil {

			return "", pushAbort, fmt.Errorf("outbox %d: %s of %s: %w", item.Seq, item.Op, item.TaskID, err)
		}
		if queued {
			return "", pushSkip, nothingSent(
				"outbox %d: %s of %s waits for the copy the task was moved into, whose create is still queued",
				item.Seq, item.Op, item.TaskID)
		}

		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: no server id was ever recorded for the copy this task was moved into, "+
				"and nothing is left in the queue to create it - the create is parked or gone, and a park "+
				"can be made over an answer that was lost as readily as over one that never arrived, so "+
				"the copy may be on the server all the same; the original is left where it was rather "+
				"than deleted",
			item.Seq, item.Op, item.TaskID)
	}
	if d.CopyProjectID == "" {

		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the entry says the task was moved into %s but not which list it was "+
				"moved to, and this build will not delete a task without finding the copy first; the "+
				"original is left where it was, and the move can be made again once it is",
			item.Seq, item.Op, item.TaskID, d.CopyID)
	}
	data, err := s.api.GetProjectData(ctx, d.CopyProjectID)
	switch {
	case errors.Is(err, api.ErrNoSuchProject):

		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: list %s, the one the task was moved into, is not on the server "+
				"any more, so there is nowhere to find the copy %s; the original is left where it was "+
				"rather than deleted, and the account holds the task once, in its old list",
			item.Seq, item.Op, item.TaskID, d.CopyProjectID, d.CopyID)
	case errors.Is(err, api.ErrIncompleteAnswer):

		return "", pushRetry, fmt.Errorf(
			"outbox %d: %s of %s: look for the copy %s in list %s: %w; the entry goes back in line, "+
				"since nothing was said about the copy and nothing was deleted",
			item.Seq, item.Op, item.TaskID, d.CopyID, d.CopyProjectID, err)
	case err != nil:

		return "", classify(item.Op, err), fmt.Errorf(
			"outbox %d: %s of %s: look for the copy %s in list %s: %w",
			item.Seq, item.Op, item.TaskID, d.CopyID, d.CopyProjectID, err)
	}
	if !holdsTask(data.Tasks, d.CopyID) {
		return "", pushPark, fmt.Errorf(
			"outbox %d: %s of %s: the copy %s is not in list %s - it was removed, or closed, since "+
				"the create was answered - so the original is left where it was rather than deleted; the "+
				"account holds the task once, in its old list",
			item.Seq, item.Op, item.TaskID, d.CopyID, d.CopyProjectID)
	}

	if out, err := s.renewBefore(ctx, item, "the delete of the original"); err != nil {
		return "", out, err
	}
	if err := s.api.DeleteTask(ctx, item.ProjectID, item.TaskID); err != nil {
		return "", classify(item.Op, err), fmt.Errorf(
			"outbox %d: delete task %s, moved into %s: %w", item.Seq, item.TaskID, d.CopyID, err)
	}
	return "", pushDone, nil
}

func (s *Syncer) dropUnderALocalOriginal(ctx context.Context, item store.OutboxItem, d store.MoveDrop) (string, pushOutcome, error) {
	queued, err := s.store.HasQueuedCreate(ctx, item.TaskID)
	if err != nil {
		return "", pushAbort, fmt.Errorf("outbox %d: %s of %s: %w", item.Seq, item.Op, item.TaskID, err)
	}
	if queued {
		return "", pushSkip, nothingSent(
			"outbox %d: %s of %s waits for the create of the task it is to delete, which is still queued - "+
				"the task was moved into %s, and the original cannot be deleted under an id the server "+
				"has never seen",
			item.Seq, item.Op, item.TaskID, d.CopyID)
	}
	return "", pushPark, fmt.Errorf(
		"outbox %d: %s of %s: nothing is left in the queue to create the task this delete addresses, so "+
			"there is no id the server would take it under; the task was moved into %s and the original "+
			"is left as it was - whether the server has it under an id nothing here knows is not known, "+
			"the create being parked or gone",
		item.Seq, item.Op, item.TaskID, d.CopyID)
}

func holdsTask(tasks []api.Task, id string) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

func classify(op string, err error) pushOutcome {
	var guard *api.RequestGuardError
	if errors.As(err, &guard) {
		return pushAbort
	}
	switch {
	case err == nil:
		return pushDone
	case errors.Is(err, api.ErrUnauthorized), statusIs(err, http.StatusForbidden):

		return pushAbort
	case errors.Is(err, api.ErrRateLimited):

		return pushAbort
	case statusIs(err, http.StatusRequestTimeout):

		return ambiguous(op)
	case errors.Is(err, api.ErrServerError):

		return ambiguous(op)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):

		return ambiguous(op)
	}
	var se *api.StatusError
	if errors.As(err, &se) {
		if se.StatusCode >= 400 && se.StatusCode < 500 {

			return pushPark
		}

		return ambiguous(op)
	}
	var te *api.TransportError
	if errors.As(err, &te) {

		return ambiguous(op)
	}

	return pushPark
}

func ambiguous(op string) pushOutcome {
	if !repeatable(op) {
		return pushPark
	}
	return pushRetry
}

func repeatable(op string) bool { return op != store.OpTaskCreate && op != store.OpTaskMoveNative }

func statusIs(err error, code int) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.StatusCode == code
}

func notOurs(err error) bool {
	return errors.Is(err, store.ErrLeaseLost) || errors.Is(err, store.ErrNoOutboxRow)
}

func cause(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
