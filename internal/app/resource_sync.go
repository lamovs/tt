package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

type ResourceSyncResult struct {
	Confirmed int      `json:"confirmed"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors"`
}

type resourceWriteError struct{ Err error }

func (e *resourceWriteError) Error() string { return e.Err.Error() }
func (e *resourceWriteError) Unwrap() error { return e.Err }

func (r *Resources) Sync(ctx context.Context) ResourceSyncResult {
	out := ResourceSyncResult{Errors: []string{}}
	if r.client == nil {
		out.Errors = append(out.Errors, "resource client is unavailable")
		return out
	}
	client, err := r.client()
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return out
	}
	for batch := 0; batch < 20; batch++ {
		operations, err := r.store.ClaimEntityOperations(ctx, 20, 5*time.Minute)
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
			return out
		}
		if len(operations) == 0 {
			break
		}
		for _, operation := range operations {
			err := r.syncResourceOperation(ctx, client, operation)
			if err == nil {
				out.Confirmed++
				continue
			}
			out.Failed++
			message := fmt.Sprintf("resource operation %d: %s", operation.Item.Seq, err.Error())
			out.Errors = append(out.Errors, message)
			var status *api.StatusError
			var guard *api.RequestGuardError
			var write *resourceWriteError
			definite := errors.As(err, &write) && (errors.As(write.Err, &guard) || errors.As(write.Err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout)
			if failErr := r.store.FailEntityOperation(context.WithoutCancel(ctx), operation.Item.Seq, operation.Item.LeaseToken, message, definite); failErr != nil {
				out.Errors = append(out.Errors, failErr.Error())
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

func (r *Resources) syncResourceOperation(ctx context.Context, c *api.Client, op store.EntityOperation) error {
	if err := validateResourceMutation(op.Mutation); err != nil {
		return err
	}
	if op.Phase == "prepared" {
		if op.Mutation.Ref.Kind == "checkin" && op.Mutation.Action == "create" {
			_, err := r.readRemoteResource(ctx, c, op.Mutation, op.Mutation.Ref.Key)
			if err == nil {
				return store.ErrEntityChanged
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if op.Mutation.Action != "create" {
			entity, err := r.store.Entity(ctx, op.Mutation.Ref)
			if err != nil {
				return err
			}
			fence, err := r.store.CaptureEntityCollectionFence(ctx, op.Mutation.Ref.Kind, op.Mutation.ProjectKey)
			if err != nil {
				return err
			}
			before, err := r.readRemoteResource(ctx, c, op.Mutation, entity.ServerID)
			if err != nil {
				return err
			}
			if err := r.store.MergeEntitiesFenced(ctx, []store.ResourceEntity{{Ref: op.Mutation.Ref, ServerID: entity.ServerID, ProjectKey: op.Mutation.ProjectKey, Data: before}}, false, fence); err != nil {
				return err
			}
		}
		var err error
		op, err = r.store.ArmEntityOperation(ctx, op.Item.Seq, op.Item.LeaseToken)
		if err != nil {
			return err
		}
		id, response, err := r.sendResource(ctx, c, op)
		if err != nil {
			return &resourceWriteError{Err: err}
		}
		if err := r.store.AcceptEntityOperation(context.WithoutCancel(ctx), op.Item.Seq, op.Item.LeaseToken, id, response); err != nil {
			return err
		}
		op.RemoteID, op.Response, op.Phase = id, response, "accepted"
		if op.Mutation.Action == "delete" {
			if err := r.suppressDeletedFocus(ctx, op); err != nil {
				return err
			}
			return r.store.ConfirmEntityOperation(context.WithoutCancel(ctx), op.Item.Seq, op.Item.LeaseToken, id, nil)
		}
	}
	if op.RemoteID == "" {
		return errors.New("the server may have allocated an object; no proven ID is available, so the write will not be repeated")
	}
	if op.Mutation.Action == "delete" {
		if op.Mutation.Ref.Kind == "focus" {
			_, err := r.readRemoteResource(ctx, c, op.Mutation, op.RemoteID)
			var status *api.StatusError
			if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
				if err := r.suppressDeletedFocus(ctx, op); err != nil {
					return err
				}
				return r.store.ConfirmEntityOperation(context.WithoutCancel(ctx), op.Item.Seq, op.Item.LeaseToken, op.RemoteID, nil)
			}
		}
		return errors.New("deletion outcome is uncertain; no write will be repeated without exact absence proof")
	}
	raw, err := r.readRemoteResource(ctx, c, op.Mutation, op.RemoteID)
	if err != nil {
		return err
	}
	return r.store.ConfirmEntityOperation(context.WithoutCancel(ctx), op.Item.Seq, op.Item.LeaseToken, op.RemoteID, raw)
}

func (r *Resources) readRemoteResource(ctx context.Context, c *api.Client, m store.EntityMutation, id string) (json.RawMessage, error) {
	switch m.Ref.Kind {
	case "project":
		item, err := c.GetProject(ctx, id)
		if err != nil {
			return nil, err
		}
		if item.ID != id {
			return nil, errors.New("remote project identity mismatch")
		}
		return item.Raw, nil
	case "habit":
		item, err := c.GetHabit(ctx, id)
		if err != nil {
			return nil, err
		}
		if item.ID != id {
			return nil, errors.New("remote habit identity mismatch")
		}
		return item.Raw, nil
	case "focus":
		kind, key, err := focusResourceID(id)
		if err != nil {
			return nil, err
		}
		item, err := c.GetFocus(ctx, key, kind)
		if err != nil {
			return nil, err
		}
		if item.ID != key || item.Type != kind {
			return nil, errors.New("remote focus identity mismatch")
		}
		return item.Raw, nil
	}
	q := ResourceQuery{Kind: m.Ref.Kind}
	switch m.Ref.Kind {
	case "column":
		q.ProjectID = m.ProjectKey
	case "comment":
		task, err := r.store.Task(ctx, m.ProjectKey)
		if err != nil {
			return nil, err
		}
		q.ProjectID, q.TaskID = task.ProjectId, task.Id
	case "checkin":
		parent, stamp, valid := strings.Cut(id, "/")
		if !valid || parent != m.ProjectKey {
			return nil, errors.New("check-in identity mismatch")
		}
		q.HabitID, q.From, q.To = parent, stamp, stamp
	}
	items, _, err := fetchResources(ctx, c, q)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ServerID == id {
			return item.Data, nil
		}
	}
	if m.Ref.Kind == "checkin" {
		return nil, store.ErrNotFound
	}
	return nil, errors.New("the exact remote resource was not found; refresh or recover this operation")
}

func (r *Resources) sendResource(ctx context.Context, c *api.Client, op store.EntityOperation) (string, json.RawMessage, error) {
	id := op.RemoteID
	creating := op.Mutation.Action == "create"
	deleting := op.Mutation.Action == "delete"
	switch op.Mutation.Ref.Kind {
	case "project":
		if deleting {
			return "", nil, errors.New("container cascade is not verified")
		}
		if creating {
			var input api.Project
			if err := json.Unmarshal(op.Request, &input); err != nil {
				return "", nil, err
			}
			item, err := c.CreateProject(ctx, input)
			if err != nil {
				return "", nil, err
			}
			return item.ID, item.Raw, nil
		}
		var input api.ProjectUpdate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		input.ID = id
		item, err := c.UpdateProject(ctx, input)
		if err != nil {
			return "", nil, err
		}
		return item.ID, item.Raw, nil
	case "folder":
		if creating {
			var input api.ProjectGroupCreate
			if err := decodeResourceInput(op.Request, &input); err != nil {
				return "", nil, err
			}
			item, err := c.CreateProjectGroup(ctx, input)
			if err != nil {
				return "", nil, err
			}
			return item.ID, item.Raw, nil
		}
		var input api.ProjectGroupUpdate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		item, err := c.UpdateProjectGroup(ctx, id, input)
		if err != nil {
			return "", nil, err
		}
		return item.ID, item.Raw, nil
	case "column":
		if creating {
			var input api.ColumnCreate
			if err := decodeResourceInput(op.Request, &input); err != nil {
				return "", nil, err
			}
			item, err := c.CreateColumn(ctx, op.Mutation.ProjectKey, input)
			if err != nil {
				return "", nil, err
			}
			if item.ProjectID != op.Mutation.ProjectKey {
				return "", nil, api.ErrIncompleteAnswer
			}
			return item.ID, item.Raw, nil
		}
		var input api.ColumnUpdate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		item, err := c.UpdateColumn(ctx, op.Mutation.ProjectKey, id, input)
		if err != nil {
			return "", nil, err
		}
		if item.ID != id || item.ProjectID != op.Mutation.ProjectKey {
			return "", nil, api.ErrIncompleteAnswer
		}
		return item.ID, item.Raw, nil
	case "tag":
		var input api.TagCreate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		item, err := c.CreateTag(ctx, input)
		if err != nil {
			return "", nil, err
		}
		return item.Name, item.Raw, nil
	case "habit":
		var input api.HabitUpdate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		var item *api.Habit
		var err error
		if creating {
			item, err = c.CreateHabit(ctx, api.HabitCreate(input))
		} else {
			item, err = c.UpdateHabit(ctx, id, input)
		}
		if err != nil {
			return "", nil, err
		}
		return item.ID, item.Raw, nil
	case "comment":
		task, err := r.store.Task(ctx, op.Mutation.ProjectKey)
		if err != nil {
			return "", nil, err
		}
		if deleting {
			return id, nil, c.DeleteTaskComment(ctx, task.ProjectId, task.Id, id)
		}
		var input api.CommentCreate
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		item, err := c.AddTaskComment(ctx, task.ProjectId, task.Id, input)
		if err != nil {
			return "", nil, err
		}
		return item.ID, item.Raw, nil
	case "checkin":
		var input api.HabitCheckinInput
		if err := decodeResourceInput(op.Request, &input); err != nil {
			return "", nil, err
		}
		group, err := c.UpsertHabitCheckin(ctx, op.Mutation.ProjectKey, input)
		if err != nil {
			return "", nil, err
		}
		if group.HabitID != op.Mutation.ProjectKey {
			return "", nil, api.ErrIncompleteAnswer
		}
		for _, item := range group.Checkins {
			if item.Stamp == input.Stamp {
				return op.Mutation.ProjectKey + "/" + strconv.Itoa(input.Stamp), item.Raw, nil
			}
		}
		return "", nil, api.ErrIncompleteAnswer
	case "focus":
		kind, key, err := focusResourceID(id)
		if err != nil {
			return "", nil, err
		}
		item, err := c.DeleteFocus(ctx, key, kind)
		if err != nil {
			return "", nil, err
		}
		if item.ID != key || item.Type != kind {
			return "", nil, api.ErrIncompleteAnswer
		}
		return id, item.Raw, nil
	}
	return "", nil, errors.New("unsupported resource mutation")
}

func focusResourceID(id string) (api.FocusType, string, error) {
	kind, key, ok := strings.Cut(id, "/")
	if !ok || key == "" || kind != "0" && kind != "1" {
		return 0, "", errors.New("focus identity needs type/id")
	}
	value, _ := strconv.Atoi(kind)
	return api.FocusType(value), key, nil
}

func (r *Resources) suppressDeletedFocus(ctx context.Context, op store.EntityOperation) error {
	if op.Mutation.Ref.Kind != "focus" {
		return nil
	}
	kind, id, err := focusResourceID(op.RemoteID)
	if err != nil {
		return err
	}
	return r.store.ConfirmRemoteFocusDeletion(context.WithoutCancel(ctx), id, int(kind))
}

func resourceIdentity(raw json.RawMessage) (string, error) {
	var value struct {
		ID   string `json:"id"`
		Type *int   `json:"type"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	if value.ID == "" {
		return "", errors.New("missing resource ID")
	}
	if value.Type != nil {
		return strconv.Itoa(*value.Type) + "/" + value.ID, nil
	}
	return strings.TrimSpace(value.ID), nil
}
