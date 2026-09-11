package app

import (
	"context"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	tasksync "github.com/movsar/tt/internal/sync"
)

type SyncOutcome struct {
	Tasks                    tasksync.Result
	Resources                ResourceSyncResult
	Focus                    focus.Result
	FocusAttempted, Canceled bool
	Err                      error
	Refreshes                []SyncRefresh
	FocusSkipped             string
}

func (o SyncOutcome) RefreshFailed() bool {
	for _, refresh := range o.Refreshes {
		if refresh.State == "failed" {
			return true
		}
	}
	return false
}

type SyncOptions struct {
	Tasks    tasksync.Options
	Catalogs bool
}

func TaskSyncSucceeded(result tasksync.Result, err error) bool {
	return err == nil && result.ErrorCount() == 0
}

func UploadPendingFocus(ctx context.Context, st *store.Store, client func() (*api.Client, error), includeAborted, retryRejected bool, topics ...focus.TopicClientFactory) (focus.Result, bool, error) {
	ids, err := st.PendingFocusSessionIDs(ctx, includeAborted)
	if err != nil || len(ids) == 0 {
		return focus.Result{}, false, err
	}
	c, err := client()
	if err != nil {
		return focus.Result{}, true, err
	}
	return focus.Upload(ctx, st, c, includeAborted, retryRejected, topics...), true, nil
}

func RunSync(ctx context.Context, st *store.Store, cfg config.Config, client func() (*api.Client, error), phase func(string), topics ...focus.TopicClientFactory) (out SyncOutcome) {
	return RunSyncWithOptions(ctx, st, cfg, client, phase, SyncOptions{Catalogs: true}, topics...)
}

func RunSyncWithOptions(ctx context.Context, st *store.Store, cfg config.Config, client func() (*api.Client, error), phase func(string), opts SyncOptions, topics ...focus.TopicClientFactory) (out SyncOutcome) {
	defer func() { out.Canceled = ctx.Err() != nil }()
	if err := ctx.Err(); err != nil {
		out.Err = err
		return
	}
	c, err := client()
	if err != nil {
		out.Err = err
		return
	}
	if opts.Catalogs {
		if phase != nil {
			phase("Refreshing Timer topics")
		}
		var factory focus.TopicClientFactory
		if len(topics) != 0 {
			factory = topics[0]
		}
		refresh, resolved := RefreshTimerTopics(ctx, st, factory)
		out.Refreshes = append(out.Refreshes, refresh)
		topics = []focus.TopicClientFactory{resolved}
	}
	out.Resources = NewResources(st, func() (*api.Client, error) { return c, nil }).Sync(ctx)
	out.Tasks, out.Err = tasksync.New(st, c, opts.Tasks).RunWithProgress(ctx, phase)
	if !TaskSyncSucceeded(out.Tasks, out.Err) || ctx.Err() != nil {
		out.FocusSkipped = "task sync did not finish successfully; queued sessions preserved"
		return
	}
	if opts.Catalogs {
		if phase != nil {
			phase("Refreshing resource catalogs")
		}
		out.Refreshes = append(out.Refreshes, NewResources(st, func() (*api.Client, error) { return c, nil }).RefreshCatalogs(ctx)...)
	}
	if !cfg.FocusUpload.Enabled {
		out.FocusSkipped = "automatic focus upload is disabled; use tt timer sync for an explicit upload"
		return
	}
	if phase != nil {
		phase("Uploading optional focus history")
	}
	out.Focus, out.FocusAttempted, out.Err = UploadPendingFocus(ctx, st, client, cfg.Timer.UploadAborted, false, topics...)
	return
}
