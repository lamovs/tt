package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out []Project
	if err := c.do(ctx, "GET", "/open/v1/project", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetProjectData(ctx context.Context, projectID string) (*ProjectData, error) {
	raw, err := c.GetProjectDataRaw(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := ProjectData{
		Project: raw.Project,
		Tasks:   make([]Task, 0, len(raw.Tasks)),
		Columns: raw.Columns,
	}
	for _, t := range raw.Tasks {
		out.Tasks = append(out.Tasks, t.Task)
	}
	return &out, nil
}

func (c *Client) GetProjectDataRaw(ctx context.Context, projectID string) (*ProjectDataRaw, error) {
	var body projectDataRaw
	path := fmt.Sprintf("/open/v1/project/%s/data", url.PathEscape(projectID))
	if err := c.do(ctx, "GET", path, nil, &body); err != nil {
		return nil, err
	}
	if body.Tasks == nil {

		return nil, &DecodeError{
			StatusCode: http.StatusOK, Method: http.MethodGet, Path: path,
			Err: fmt.Errorf("%w: response carried no task list", ErrIncompleteAnswer),
		}
	}
	out := ProjectDataRaw{
		Project: body.Project,
		Tasks:   make([]RawTask, 0, len(*body.Tasks)),
		Columns: body.Columns,
	}
	for _, raw := range *body.Tasks {
		var t Task
		if err := json.Unmarshal(raw, &t); err != nil {

			return nil, &DecodeError{
				StatusCode: http.StatusOK, Method: http.MethodGet, Path: path,
				Body: diagnosticBody(raw, c.token), Err: err,
			}
		}

		out.Tasks = append(out.Tasks, RawTask{Task: t, Raw: raw})
	}
	return &out, nil
}

func (c *Client) CreateProject(ctx context.Context, project Project) (*Project, error) {
	var out Project
	if err := c.do(ctx, "POST", "/open/v1/project", project, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateProject(ctx context.Context, update ProjectUpdate) (*Project, error) {
	var out Project
	path := fmt.Sprintf("/open/v1/project/%s", url.PathEscape(update.ID))
	if err := c.do(ctx, "POST", path, update, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteProject(ctx context.Context, projectID string) error {
	path := fmt.Sprintf("/open/v1/project/%s", url.PathEscape(projectID))
	return c.do(ctx, "DELETE", path, nil, nil)
}
