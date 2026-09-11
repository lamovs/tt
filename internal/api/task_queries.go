package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const TaskQueryLimit = 200

type CompletedTaskFilter struct {
	ProjectIDs *[]string `json:"projectIds,omitempty"`
	StartDate  *string   `json:"startDate,omitempty"`
	EndDate    *string   `json:"endDate,omitempty"`
}

type TaskFilter struct {
	ProjectIDs *[]string `json:"projectIds,omitempty"`
	StartDate  *string   `json:"startDate,omitempty"`
	EndDate    *string   `json:"endDate,omitempty"`
	Priority   *[]int    `json:"priority,omitempty"`
	Tag        *[]string `json:"tag,omitempty"`
	Kind       *[]string `json:"kind,omitempty"`
	Status     *[]int    `json:"status,omitempty"`
}

type TaskSearch struct {
	Keywords   *string   `json:"keywords,omitempty"`
	ProjectIDs *[]string `json:"projectIds,omitempty"`
	Tags       *[]string `json:"tags,omitempty"`
	Status     *[]int    `json:"status,omitempty"`
	DueFrom    *string   `json:"dueFrom,omitempty"`
	DueTo      *string   `json:"dueTo,omitempty"`
}

type TaskMove struct {
	FromProjectID string `json:"fromProjectId"`
	ToProjectID   string `json:"toProjectId"`
	TaskID        string `json:"taskId"`
}

type TaskMoveResult struct {
	ID   string          `json:"id"`
	Etag string          `json:"etag"`
	Raw  json.RawMessage `json:"-"`
}

func (value *TaskMoveResult) UnmarshalJSON(data []byte) error {
	type wire TaskMoveResult
	var out wire
	raw, err := decodeResource(data, &out, "id")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Etag) == "" {
		return fmt.Errorf("move response has no etag: %w", ErrIncompleteAnswer)
	}
	out.Raw = raw
	*value = TaskMoveResult(out)
	return nil
}

func validateQueryRange(from, to *string) error {
	for _, value := range []*string{from, to} {
		if value != nil {
			if _, err := parseFocusTime(*value); err != nil {
				return fmt.Errorf("ticktick: task query date: %w", err)
			}
		}
	}
	if from != nil && to != nil {
		start, _ := parseFocusTime(*from)
		end, _ := parseFocusTime(*to)
		if end.Before(start) {
			return errors.New("ticktick: task query end must not be before its start")
		}
	}
	return nil
}

func (c *Client) queryTasks(ctx context.Context, path string, request any) ([]RawTask, error) {
	var rows []json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, request, &rows); err != nil {
		return nil, err
	}
	out := make([]RawTask, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, raw := range rows {
		var task Task
		if _, err := decodeResource(raw, &task, "id"); err != nil {
			return nil, &DecodeError{StatusCode: http.StatusOK, Method: http.MethodPost, Path: path, Err: err}
		}
		if strings.TrimSpace(task.ProjectID) == "" || seen[task.ID] {
			return nil, &DecodeError{StatusCode: http.StatusOK, Method: http.MethodPost, Path: path,
				Err: fmt.Errorf("task query contains missing project or duplicate id: %w", ErrIncompleteAnswer)}
		}
		seen[task.ID] = true
		out = append(out, RawTask{Task: task, Raw: append(json.RawMessage(nil), raw...)})
	}
	return out, nil
}

func (c *Client) GetCompletedTasks(ctx context.Context, input CompletedTaskFilter) ([]RawTask, error) {
	if err := validateQueryRange(input.StartDate, input.EndDate); err != nil {
		return nil, err
	}
	return c.queryTasks(ctx, "/open/v1/task/completed", input)
}

func (c *Client) FilterTasks(ctx context.Context, input TaskFilter) ([]RawTask, error) {
	if err := validateQueryRange(input.StartDate, input.EndDate); err != nil {
		return nil, err
	}
	if input.Priority != nil {
		for _, value := range *input.Priority {
			if value != 0 && value != 1 && value != 3 && value != 5 {
				return nil, errors.New("ticktick: task query priority must be 0, 1, 3, or 5")
			}
		}
	}
	return c.queryTasks(ctx, "/open/v1/task/filter", input)
}

func (c *Client) SearchTasks(ctx context.Context, input TaskSearch) ([]RawTask, error) {
	if err := validateQueryRange(input.DueFrom, input.DueTo); err != nil {
		return nil, err
	}
	if input.Keywords != nil && strings.TrimSpace(*input.Keywords) == "" {
		return nil, errors.New("ticktick: supplied search keywords must not be blank")
	}
	return c.queryTasks(ctx, "/open/v1/task/search", input)
}

func (c *Client) MoveTasks(ctx context.Context, input []TaskMove) ([]TaskMoveResult, error) {
	if len(input) == 0 {
		return nil, errors.New("ticktick: at least one task move is required")
	}
	wanted := make(map[string]bool, len(input))
	for _, move := range input {
		if strings.TrimSpace(move.TaskID) == "" || strings.TrimSpace(move.FromProjectID) == "" || strings.TrimSpace(move.ToProjectID) == "" {
			return nil, errors.New("ticktick: task move requires task, source, and destination identifiers")
		}
		if wanted[move.TaskID] || move.FromProjectID == move.ToProjectID {
			return nil, errors.New("ticktick: task moves must have unique tasks and different source and destination projects")
		}
		wanted[move.TaskID] = true
	}
	const path = "/open/v1/task/move"
	var out []TaskMoveResult
	if err := c.do(ctx, http.MethodPost, path, input, &out); err != nil {
		return nil, err
	}
	for _, result := range out {
		if !wanted[result.ID] {
			return nil, fmt.Errorf("move response contains an unexpected or duplicate task: %w", ErrIncompleteAnswer)
		}
		delete(wanted, result.ID)
	}
	if len(wanted) != 0 {
		return nil, fmt.Errorf("move response did not confirm every task: %w", ErrIncompleteAnswer)
	}
	return out, nil
}
