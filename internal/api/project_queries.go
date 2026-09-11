package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	path, err := resourcePath("/open/v1/project", id)
	if err != nil {
		return nil, err
	}
	out, err := resourceRequest[Project](ctx, c, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, fmt.Errorf("project response has no identifier: %w", ErrIncompleteAnswer)
	}
	return out, nil
}

func (c *Client) ListProjectsPage(ctx context.Context, offset, limit int) ([]Project, error) {
	if offset < 0 || limit < 1 {
		return nil, errors.New("ticktick: project page requires a nonnegative offset and positive limit")
	}
	query := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	out, err := resourceList[Project](ctx, c, "/open/v1/project?"+query.Encode())
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out))
	for _, project := range out {
		if strings.TrimSpace(project.ID) == "" || seen[project.ID] {
			return nil, fmt.Errorf("project page contains missing or duplicate identifiers: %w", ErrIncompleteAnswer)
		}
		seen[project.ID] = true
	}
	return out, nil
}
