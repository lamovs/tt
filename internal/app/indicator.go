package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/movsar/tt/internal/store"
)

func ReadIndicator(ctx context.Context, path string, now time.Time) (store.TimerSnapshot, error) {
	inspection, err := store.Inspect(ctx, path)
	if errors.Is(err, store.ErrNoCache) {
		return store.TimerSnapshot{ObservedAt: now}, nil
	}
	if err != nil {
		return store.TimerSnapshot{}, err
	}
	defer inspection.Close()
	version, hasMeta, hasRow, err := inspection.Version(ctx)
	if err != nil {
		return store.TimerSnapshot{}, err
	}
	if !hasMeta || !hasRow || version != inspection.SchemaLatest {
		return store.TimerSnapshot{}, fmt.Errorf("indicator requires cache schema %d, found %d; use a compatible tt and run timer status explicitly to migrate an older cache", inspection.SchemaLatest, version)
	}
	return inspection.Store.ReadTimer(ctx, now)
}
