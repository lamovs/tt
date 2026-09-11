package app

import (
	"context"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func (a *Actions) PreviewTaskColumn(ctx context.Context, original model.Task, destinationID string) (store.TaskColumnPreview, error) {
	return a.store.PreviewTaskColumn(ctx, original, destinationID)
}

func (a *Actions) ApplyTaskColumn(ctx context.Context, preview store.TaskColumnPreview) (store.TaskMutationOutcome, error) {
	return a.store.ApplyTaskColumn(ctx, preview)
}
