package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

func (c *Client) GetTask(ctx context.Context, projectID, taskID string) (*Task, error) {
	raw, err := c.GetTaskRaw(ctx, projectID, taskID)
	if err != nil {
		return nil, err
	}
	return &raw.Task, nil
}

func (c *Client) GetTaskRaw(ctx context.Context, projectID, taskID string) (*RawTask, error) {
	path := fmt.Sprintf("/open/v1/project/%s/task/%s", url.PathEscape(projectID), url.PathEscape(taskID))
	var response rawTaskResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return &RawTask{Task: response.Task, Raw: response.Raw}, nil
}

type rawTaskResponse RawTask

func (r *rawTaskResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Task); err != nil {
		return err
	}
	r.Raw = append(r.Raw[:0], data...)
	return nil
}

func (c *Client) CreateTask(ctx context.Context, task TaskCreate) (*Task, error) {
	raw, err := c.CreateTaskRaw(ctx, task)
	if err != nil {
		return nil, err
	}
	return &raw.Task, nil
}

func (c *Client) CreateTaskRaw(ctx context.Context, task TaskCreate) (*RawTask, error) {
	var out rawTaskResponse
	if err := c.do(ctx, "POST", "/open/v1/task", task, &out); err != nil {
		return nil, err
	}
	if out.Task.ID == "" {
		return nil, fmt.Errorf("ticktick: POST /open/v1/task: %w: the created task came back without an id",
			ErrIncompleteAnswer)
	}
	return &RawTask{Task: out.Task, Raw: out.Raw}, nil
}

func (c *Client) UpdateTask(ctx context.Context, update TaskUpdate) error {
	path := fmt.Sprintf("/open/v1/task/%s", url.PathEscape(update.ID))
	return c.do(ctx, "POST", path, update, nil)
}

func (c *Client) CompleteTask(ctx context.Context, projectID, taskID string) error {
	path := fmt.Sprintf("/open/v1/project/%s/task/%s/complete", url.PathEscape(projectID), url.PathEscape(taskID))
	return c.do(ctx, "POST", path, nil, nil)
}

func (c *Client) DeleteTask(ctx context.Context, projectID, taskID string) error {
	path := fmt.Sprintf("/open/v1/project/%s/task/%s", url.PathEscape(projectID), url.PathEscape(taskID))
	return c.do(ctx, "DELETE", path, nil, nil)
}
