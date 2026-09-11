package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/model"
)

func (s *Store) DefaultFocusTask(ctx context.Context, id string) (model.Task, error) {
	return defaultFocusTask(ctx, s.db, id)
}

func defaultFocusTask(ctx context.Context, q execer, id string) (model.Task, error) {
	var task model.Task
	var project model.Project
	var status int
	err := q.QueryRowContext(ctx, `SELECT t.id, t.project_id, t.title, t.status,
		COALESCE(p.id,''), COALESCE(p.kind,''), COALESCE(p.closed,0)
		FROM tasks t LEFT JOIN projects p ON p.id=t.project_id WHERE t.id=?`, id).
		Scan(&task.Id, &task.ProjectId, &task.Title, &status, &project.Id, &project.Kind, &project.Closed)
	if errors.Is(err, sql.ErrNoRows) {
		return task, errors.New("default_focus task is not cached; sync or choose an explicit destination")
	}
	if err != nil {
		return task, err
	}
	task.Status = model.TaskStatus(status)
	if reason := DefaultFocusUnavailable(task, project); reason != "" {
		return task, fmt.Errorf("default_focus task is unavailable in the cache: %s", reason)
	}
	return task, nil
}

func DefaultFocusUnavailable(task model.Task, project model.Project) string {
	if !model.ValidProjectID(task.Id) {
		return "task ID is missing or unsupported"
	}
	if task.Status != model.TaskOpen {
		return "task is not open"
	}
	if task.ProjectId == "" || task.ProjectId != project.Id {
		return "list is not cached"
	}
	return project.CreateUnavailable()
}
