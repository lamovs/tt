package store

import (
	"context"
	"errors"
	"reflect"

	"github.com/movsar/tt/internal/model"
)

var ErrTaskChanged = errors.New("task changed while the editor was open; reopen it and apply the saved draft")

func (s *Store) UpdateTaskIfUnchanged(ctx context.Context, original model.Task, edit model.TaskEdit) (TaskMutationOutcome, error) {
	return s.editTaskOutcome(ctx, original.Id, OpTaskUpdate, TaskUpdatedKind, func(current model.Task) (model.TaskEdit, error) {
		if !reflect.DeepEqual(current, original) {
			return model.TaskEdit{}, ErrTaskChanged
		}
		if featureEditNoop(current, edit) {
			return model.TaskEdit{}, nil
		}
		return edit, nil
	})
}

func (s *Store) CompleteTaskIfUnchanged(ctx context.Context, original model.Task, opts CompleteOptions) (TaskMutationOutcome, error) {
	return s.editTaskOutcome(ctx, original.Id, OpTaskComplete, TaskCompletedKind, func(current model.Task) (model.TaskEdit, error) {
		if !reflect.DeepEqual(current, original) {
			return model.TaskEdit{}, ErrTaskChanged
		}
		if current.Status.Done() {
			return model.TaskEdit{}, nil
		}
		return completionEdit(current, opts), nil
	})
}
