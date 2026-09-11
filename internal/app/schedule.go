package app

import (
	"context"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func (a *Actions) Schedule(ctx context.Context, original model.Task, change schedule.Change) (store.TaskMutationOutcome, error) {
	edit, err := change.Edit(original)
	if err != nil {
		return store.TaskMutationOutcome{}, err
	}
	if change.Interval != nil || schedule.ReviewsInterval(original, edit) {
		p, err := a.store.PrepareInterval(ctx, &original, edit, model.Task{})
		if err != nil {
			return store.TaskMutationOutcome{}, err
		}
		return a.ApplyInterval(ctx, p, false)
	}
	return a.store.UpdateTaskIfUnchanged(ctx, original, edit)
}
