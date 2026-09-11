package app

import (
	"context"
	"errors"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func (a *Actions) RecoveryQueue(ctx context.Context) (store.RecoveryQueue, error) {
	return a.store.RecoveryQueue(ctx)
}

func (a *Actions) RetryQueueEntry(ctx context.Context, expected store.QueueEntry) error {
	return a.store.RetryQueueEntry(ctx, expected)
}

func (a *Actions) PreviewTaskOperation(ctx context.Context, original model.Task, destination string) (store.TaskOperationPreview, error) {
	if destination != "" {
		p, err := a.store.PreviewNativeMove(ctx, original, destination)
		return store.TaskOperationPreview{Native: &p, Task: p.Task, Destination: p.Destination, Version: p.Version, DestinationVersion: p.DestinationVersion}, err
	}
	return a.store.PreviewTaskOperation(ctx, original, destination)
}

func (a *Actions) ApplyTaskOperation(ctx context.Context, expected store.TaskOperationPreview) (store.TaskMutationOutcome, error) {
	if expected.Native != nil {
		return a.store.ApplyNativeMove(ctx, *expected.Native)
	}
	if expected.Destination.Id != "" {
		return store.TaskMutationOutcome{}, errors.New("move preview has no native identity contract; prepare it again")
	}
	return a.store.ApplyTaskOperation(ctx, expected)
}
