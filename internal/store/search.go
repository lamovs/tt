package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/model"
)

func searchText(parts ...string) string {
	return strings.ToLower(strings.Join(parts, "\n"))
}

func searchNeedle(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

func taskSearch(t model.Task, projectName string) string {
	return searchText(t.Title, t.Content, strings.Join(t.Tags, " "), projectName)
}

func projectName(ctx context.Context, q execer, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var name string
	err := q.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, id).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read list %s: %w", id, err)
	}
	return name, nil
}

func refreshSearch(ctx context.Context, q execer, projectIDs []string) error {
	for _, id := range projectIDs {
		name, err := projectName(ctx, q, id)
		if err != nil {
			return err
		}

		pending, err := searchRebuild(ctx, q, id, name)
		if err != nil {
			return err
		}
		for _, p := range pending {
			if _, err := q.ExecContext(ctx, `UPDATE tasks SET search = ? WHERE id = ?`, p.search, p.id); err != nil {
				return fmt.Errorf("refresh search of %s: %w", p.id, err)
			}
		}
	}
	return nil
}

type searchRow struct{ id, search string }

func searchRebuild(ctx context.Context, q execer, projectID, name string) ([]searchRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, title, content, tags FROM tasks WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("refresh search of list %s: %w", projectID, err)
	}
	defer rows.Close()
	var out []searchRow
	for rows.Next() {
		var id, title, content, tags string
		if err := rows.Scan(&id, &title, &content, &tags); err != nil {
			return nil, fmt.Errorf("refresh search of list %s: %w", projectID, err)
		}
		list, err := decodeStrings(tags)
		if err != nil {
			return nil, err
		}
		out = append(out, searchRow{id, searchText(title, content, strings.Join(list, " "), name)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("refresh search of list %s: %w", projectID, err)
	}
	return out, nil
}
