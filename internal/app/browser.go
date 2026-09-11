package app

import (
	"context"
	"errors"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type CacheReader interface {
	Projects(context.Context) ([]model.Project, error)
	Tasks(context.Context, store.TaskFilter) ([]model.Task, error)
	Task(context.Context, string) (model.Task, error)
}

type BrowseView int

const (
	TodayView BrowseView = iota
	OpenView
	CompletedView
	ProjectView
)

type BrowseQuery struct {
	View      BrowseView
	ProjectID string
	Search    string
}

type Browser struct{ cache CacheReader }

func NewBrowser(cache CacheReader) *Browser { return &Browser{cache: cache} }

func (b *Browser) Projects(ctx context.Context) ([]model.Project, error) {
	return b.cache.Projects(ctx)
}

func (b *Browser) OpenTasks(ctx context.Context, projectID string) ([]model.Task, error) {
	return b.Tasks(ctx, BrowseQuery{View: ProjectView, ProjectID: projectID}, time.Time{})
}

func (b *Browser) Tasks(ctx context.Context, query BrowseQuery, now time.Time) ([]model.Task, error) {
	f := store.TaskFilter{Status: store.StatusOpen, Search: query.Search, Order: store.OrderDue}
	switch query.View {
	case TodayView:
		tomorrow, err := dates.Parse("tmr", now)
		if err != nil {
			return nil, err
		}
		f.DueTo = tomorrow.Time
	case OpenView:
	case CompletedView:
		f.Status = store.StatusDone
	case ProjectView:
		if query.ProjectID == "" {
			return nil, errors.New("select a cached list first")
		}
		f.ProjectID = query.ProjectID
	default:
		return nil, errors.New("unknown cached view")
	}
	return b.cache.Tasks(ctx, f)
}

func (b *Browser) Task(ctx context.Context, id string) (model.Task, error) {
	if id == "" {
		return model.Task{}, errors.New("select a cached task first")
	}
	return b.cache.Task(ctx, id)
}
