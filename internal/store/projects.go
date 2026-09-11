package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

const projectColumns = `id, name, kind, sort_order, closed, color, group_id, view_mode, permission`

func (s *Store) ReplaceProjects(ctx context.Context, projects []model.Project) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		before, err := projectNames(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			return fmt.Errorf("replace lists: %w", err)
		}
		now := time.Now().Unix()
		seen := make(map[string]bool, len(projects))
		var renamed []string
		for _, p := range projects {
			if p.Id == "" {
				return errors.New("replace lists: empty list id")
			}
			if seen[p.Id] {

				continue
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO projects (id, name, kind, sort_order, closed, search, updated_at, color, group_id, view_mode, permission)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.Id, p.Name, p.Kind, p.SortOrder, p.Closed, searchText(p.Name), now, p.Color, p.GroupId, p.ViewMode, p.Permission)
			if err != nil {
				return fmt.Errorf("replace lists: %w", err)
			}
			seen[p.Id] = true
			if old, ok := before[p.Id]; !ok || old != p.Name {
				renamed = append(renamed, p.Id)
			}
		}
		var gone []string
		for id := range before {
			if !seen[id] {
				gone = append(gone, id)
			}
		}

		identityChanged := false
		for _, id := range gone {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE project_id = ? AND dirty = 0 AND local = 0`, id)
			if err != nil {
				return fmt.Errorf("replace lists: %w", err)
			}
			var taskIDs []string
			for rows.Next() {
				var taskID string
				if err := rows.Scan(&taskID); err != nil {
					rows.Close()
					return err
				}
				taskIDs = append(taskIDs, taskID)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, taskID := range taskIDs {
				registered, err := taskHasItemControl(ctx, tx, taskID)
				if err != nil {
					return err
				}
				if registered {
					identityChanged = true
					held, _, err := featureTaskQueueState(ctx, tx, taskID)
					if err != nil {
						return err
					}
					if held {
						continue
					}
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, taskID); err != nil {
					return fmt.Errorf("replace lists: %w", err)
				}
			}
		}
		if identityChanged {
			if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
				return err
			}
		}

		return refreshSearch(ctx, tx, append(renamed, gone...))
	})
}

func (s *Store) Projects(ctx context.Context) ([]model.Project, error) {
	return queryProjects(ctx, s.db, `SELECT `+projectColumns+` FROM projects ORDER BY sort_order, name`)
}

func (s *Store) Project(ctx context.Context, id string) (model.Project, error) {
	var p model.Project
	err := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id).
		Scan(&p.Id, &p.Name, &p.Kind, &p.SortOrder, &p.Closed, &p.Color, &p.GroupId, &p.ViewMode, &p.Permission)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Project{}, fmt.Errorf("the list %s: %w", id, ErrNotFound)
	case err != nil:
		return model.Project{}, fmt.Errorf("read list %s: %w", id, err)
	}
	return p, nil
}

func (s *Store) FindProjects(ctx context.Context, query string) ([]model.Project, error) {
	needle := searchNeedle(query)
	if needle == "" {
		return s.Projects(ctx)
	}
	return queryProjects(ctx, s.db,
		`SELECT `+projectColumns+` FROM projects WHERE instr(search, ?) > 0 ORDER BY sort_order, name`, needle)
}

func MatchNames(names []string, query string) []string {
	needle := searchNeedle(query)
	if needle == "" {
		return slices.Clone(names)
	}
	var out []string
	for _, name := range names {
		if strings.Contains(searchText(name), needle) {
			out = append(out, name)
		}
	}
	return out
}

func queryProjects(ctx context.Context, q execer, query string, args ...any) ([]model.Project, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read lists: %w", err)
	}
	defer rows.Close()
	var out []model.Project
	for rows.Next() {
		var p model.Project
		if err := rows.Scan(&p.Id, &p.Name, &p.Kind, &p.SortOrder, &p.Closed, &p.Color, &p.GroupId, &p.ViewMode, &p.Permission); err != nil {
			return nil, fmt.Errorf("read lists: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lists: %w", err)
	}
	return out, nil
}

func projectNames(ctx context.Context, q execer) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("read lists: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("read lists: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lists: %w", err)
	}
	return out, nil
}
