package app

import (
	"context"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type FocusChoices struct {
	Projects         []model.Project
	Tasks            []model.Task
	InitialProjectID string
	Notice           string
}

func DefaultFocusChoices(ctx context.Context, cache CacheReader, defaultProject string) (FocusChoices, error) {
	var out FocusChoices
	projects, err := cache.Projects(ctx)
	if err != nil {
		return out, err
	}
	usable := make(map[string]model.Project)
	for _, p := range projects {
		if p.CreateUnavailable() == "" {
			out.Projects = append(out.Projects, p)
			usable[p.Id] = p
		}
	}
	if p, err := cli.ResolveDefaultProject(projects, defaultProject); err == nil {
		out.InitialProjectID = p.Id
	} else {
		out.Notice = "default_project is unavailable in the cache; choose a list explicitly."
	}
	tasks, err := cache.Tasks(ctx, store.TaskFilter{Status: store.StatusOpen, Order: store.OrderDue})
	if err != nil {
		return out, err
	}
	for _, task := range tasks {
		if store.DefaultFocusUnavailable(task, usable[task.ProjectId]) == "" {
			out.Tasks = append(out.Tasks, task)
		}
	}
	return out, nil
}

func BindDefaultFocus(ctx context.Context, st *store.Store, guard store.TimerGuard, ref config.FocusReference) (store.TimerGuard, error) {
	if ref.TaskID != "" {
		if _, err := st.DefaultFocusTask(ctx, ref.TaskID); err != nil {
			return guard, err
		}
	}
	bound, err := st.GuardTimerTask(ctx, guard, ref.TaskID)
	bound.DefaultTask = ref.TaskID != ""
	return bound, err
}
