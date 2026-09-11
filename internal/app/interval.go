package app

import (
	"context"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

func (a *Actions) PreviewSchedule(ctx context.Context, original model.Task, change schedule.Change) (store.IntervalPreview, error) {
	edit, err := change.Edit(original)
	if err != nil {
		return store.IntervalPreview{}, err
	}
	return a.store.PrepareInterval(ctx, &original, edit, model.Task{})
}

func (a *Actions) PreviewCreateInterval(ctx context.Context, d Draft) (store.IntervalPreview, error) {
	if err := d.Validate(); err != nil {
		return store.IntervalPreview{}, err
	}
	kind := d.Kind
	if kind == "" {
		kind = "TEXT"
	}
	task := model.Task{ProjectId: d.ProjectID, Title: d.Title, Content: d.Body, Kind: kind, Priority: d.Priority,
		StartDate: d.Interval.Start, DueDate: d.Interval.End, TimeZone: d.Interval.Zone}
	return a.store.PrepareInterval(ctx, nil, model.TaskEdit{}, task)
}

func (a *Actions) ApplyInterval(ctx context.Context, preview store.IntervalPreview, allow bool) (store.TaskMutationOutcome, error) {
	return a.store.ApplyInterval(ctx, preview, allow)
}
