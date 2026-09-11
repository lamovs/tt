package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func resourcePath(prefix string, ids ...string) (string, error) {
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || id == "." || id == ".." || !utf8.ValidString(id) {
			return "", errors.New("ticktick: resource identifier is required and must be a valid path segment")
		}
		prefix += "/" + url.PathEscape(id)
	}
	return prefix, nil
}

func resourceRequest[T any](ctx context.Context, c *Client, method, path string, request any) (*T, error) {
	var out T
	if err := c.do(ctx, method, path, request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func resourceList[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out))
	for _, value := range out {
		key := resourceIdentity(value)
		if strings.TrimSpace(key) == "" || seen[key] {
			return nil, &DecodeError{StatusCode: http.StatusOK, Method: http.MethodGet, Path: path,
				Err: fmt.Errorf("resource list contains missing or duplicate identifiers: %w", ErrIncompleteAnswer)}
		}
		seen[key] = true
	}
	return out, nil
}

func resourceIdentity(value any) string {
	switch value := value.(type) {
	case Project:
		return value.ID
	case ProjectGroup:
		return value.ID
	case Column:
		return value.ID
	case Tag:
		return value.Name
	case Habit:
		return value.ID
	case HabitSection:
		return value.ID
	case HabitCheckin:
		return value.HabitID + "/" + strconv.Itoa(value.Year)
	case Comment:
		return value.ID
	case Countdown:
		return value.ID
	default:
		return ""
	}
}

func validateResourceName(name string, limit int) error {
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > limit {
		return fmt.Errorf("ticktick: name must be nonblank valid UTF-8 with at most %d characters", limit)
	}
	return nil
}

func (c *Client) ListProjectGroups(ctx context.Context) ([]ProjectGroup, error) {
	return resourceList[ProjectGroup](ctx, c, "/open/v1/project/group")
}

func (c *Client) CreateProjectGroup(ctx context.Context, input ProjectGroupCreate) (*ProjectGroup, error) {
	if err := validateResourceName(input.Name, 64); err != nil {
		return nil, err
	}
	return resourceRequest[ProjectGroup](ctx, c, http.MethodPost, "/open/v1/project/group", input)
}

func (c *Client) UpdateProjectGroup(ctx context.Context, id string, input ProjectGroupUpdate) (*ProjectGroup, error) {
	path, err := resourcePath("/open/v1/project/group", id)
	if err != nil {
		return nil, err
	}
	if input.Name == nil {
		return nil, errors.New("ticktick: project group update requires a name")
	}
	if err := validateResourceName(*input.Name, 64); err != nil {
		return nil, err
	}
	return resourceRequest[ProjectGroup](ctx, c, http.MethodPost, path, input)
}

func (c *Client) DeleteProjectGroup(ctx context.Context, id string) error {
	path, err := resourcePath("/open/v1/project/group", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func columnPath(projectID string, columnID ...string) (string, error) {
	path, err := resourcePath("/open/v1/project", projectID)
	if err != nil {
		return "", err
	}
	return resourcePath(path+"/column", columnID...)
}

func validateColumnResponse(value Column) error {
	if strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.ProjectID) == "" {
		return fmt.Errorf("column response is missing an identifier: %w", ErrIncompleteAnswer)
	}
	return nil
}

func (c *Client) ListColumns(ctx context.Context, projectID string) ([]Column, error) {
	path, err := columnPath(projectID)
	if err != nil {
		return nil, err
	}
	out, err := resourceList[Column](ctx, c, path)
	if err != nil {
		return nil, err
	}
	for _, column := range out {
		if err := validateColumnResponse(column); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Client) CreateColumn(ctx context.Context, projectID string, input ColumnCreate) (*Column, error) {
	path, err := columnPath(projectID)
	if err != nil {
		return nil, err
	}
	if err := validateResourceName(input.Name, 1000); err != nil {
		return nil, err
	}
	out, err := resourceRequest[Column](ctx, c, http.MethodPost, path, input)
	if err != nil {
		return nil, err
	}
	if err := validateColumnResponse(*out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) UpdateColumn(ctx context.Context, projectID, columnID string, input ColumnUpdate) (*Column, error) {
	path, err := columnPath(projectID, columnID)
	if err != nil {
		return nil, err
	}
	if input.Name == nil {
		return nil, errors.New("ticktick: column update requires a name")
	}
	if err := validateResourceName(*input.Name, 1000); err != nil {
		return nil, err
	}
	out, err := resourceRequest[Column](ctx, c, http.MethodPost, path, input)
	if err != nil {
		return nil, err
	}
	if err := validateColumnResponse(*out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) ListTags(ctx context.Context) ([]Tag, error) {
	return resourceList[Tag](ctx, c, "/open/v1/tag")
}

func (c *Client) CreateTag(ctx context.Context, input TagCreate) (*Tag, error) {
	if err := validateResourceName(input.Name, 64); err != nil {
		return nil, err
	}
	if err := validateResourceName(input.Label, 64); err != nil {
		return nil, err
	}
	if input.Name != strings.TrimSpace(input.Name) || input.Name != strings.ToLower(input.Name) || strings.ToLower(input.Label) != input.Name {
		return nil, errors.New("ticktick: tag name must be lowercase and trimmed, and match the lowercase label")
	}
	return resourceRequest[Tag](ctx, c, http.MethodPost, "/open/v1/tag", input)
}

func (c *Client) ListHabits(ctx context.Context) ([]Habit, error) {
	return resourceList[Habit](ctx, c, "/open/v1/habit")
}

func (c *Client) GetHabit(ctx context.Context, id string) (*Habit, error) {
	path, err := resourcePath("/open/v1/habit", id)
	if err != nil {
		return nil, err
	}
	return resourceRequest[Habit](ctx, c, http.MethodGet, path, nil)
}

func validateHabitInput(input HabitUpdate, create bool) error {
	if create && input.Name == nil {
		return errors.New("ticktick: habit name is required")
	}
	if input.Name != nil {
		if err := validateResourceName(*input.Name, 1000); err != nil {
			return err
		}
	}
	for _, value := range []*float64{input.Goal, input.Step} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("ticktick: habit goal and step must be finite numbers")
		}
	}
	if input.TargetStartDate != nil && *input.TargetStartDate != 0 {
		return validateDateStamp(*input.TargetStartDate)
	}
	return nil
}

func (c *Client) CreateHabit(ctx context.Context, input HabitCreate) (*Habit, error) {
	if err := validateHabitInput(HabitUpdate(input), true); err != nil {
		return nil, err
	}
	return resourceRequest[Habit](ctx, c, http.MethodPost, "/open/v1/habit", input)
}

func (c *Client) UpdateHabit(ctx context.Context, id string, input HabitUpdate) (*Habit, error) {
	path, err := resourcePath("/open/v1/habit", id)
	if err != nil {
		return nil, err
	}
	if err := validateHabitInput(input, false); err != nil {
		return nil, err
	}
	return resourceRequest[Habit](ctx, c, http.MethodPost, path, input)
}

func (c *Client) ListHabitSections(ctx context.Context) ([]HabitSection, error) {
	return resourceList[HabitSection](ctx, c, "/open/v1/habit/sections")
}

func validateDateStamp(stamp int) error {
	text := strconv.Itoa(stamp)
	if len(text) != 8 {
		return errors.New("ticktick: date stamp must be a valid YYYYMMDD date")
	}
	if _, err := time.Parse("20060102", text); err != nil {
		return errors.New("ticktick: date stamp must be a valid YYYYMMDD date")
	}
	return nil
}

func (c *Client) UpsertHabitCheckin(ctx context.Context, id string, input HabitCheckinInput) (*HabitCheckin, error) {
	path, err := resourcePath("/open/v1/habit", id)
	if err != nil {
		return nil, err
	}
	if err := validateDateStamp(input.Stamp); err != nil {
		return nil, err
	}
	for _, value := range []*float64{input.Value, input.Goal} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return nil, errors.New("ticktick: check-in value and goal must be finite numbers")
		}
	}
	for _, value := range []*string{input.Time, input.OpTime} {
		if value != nil {
			if _, err := parseFocusTime(*value); err != nil {
				return nil, fmt.Errorf("ticktick: check-in timestamp: %w", err)
			}
		}
	}
	return resourceRequest[HabitCheckin](ctx, c, http.MethodPost, path+"/checkin", input)
}

func (c *Client) GetHabitCheckins(ctx context.Context, input HabitCheckinQuery) ([]HabitCheckin, error) {
	if len(input.HabitIDs) == 0 {
		return nil, errors.New("ticktick: at least one habit identifier is required")
	}
	for _, id := range input.HabitIDs {
		if strings.TrimSpace(id) == "" || strings.Contains(id, ",") || !utf8.ValidString(id) {
			return nil, errors.New("ticktick: habit identifiers must be nonblank and cannot contain commas")
		}
	}
	if err := validateDateStamp(input.From); err != nil {
		return nil, err
	}
	if err := validateDateStamp(input.To); err != nil {
		return nil, err
	}
	if input.From > input.To {
		return nil, errors.New("ticktick: check-in range must not end before it starts")
	}
	query := url.Values{
		"habitIds": {strings.Join(input.HabitIDs, ",")},
		"from":     {strconv.Itoa(input.From)},
		"to":       {strconv.Itoa(input.To)},
	}
	return resourceList[HabitCheckin](ctx, c, "/open/v1/habit/checkins?"+query.Encode())
}

func commentPath(projectID, taskID string) (string, error) {
	path, err := resourcePath("/open/v1/project", projectID)
	if err != nil {
		return "", err
	}
	return resourcePath(path+"/task", taskID)
}

func (c *Client) ListTaskComments(ctx context.Context, projectID, taskID string) ([]Comment, error) {
	path, err := commentPath(projectID, taskID)
	if err != nil {
		return nil, err
	}
	return resourceList[Comment](ctx, c, path+"/comments")
}

func (c *Client) AddTaskComment(ctx context.Context, projectID, taskID string, input CommentCreate) (*Comment, error) {
	path, err := commentPath(projectID, taskID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Title) == "" || !utf8.ValidString(input.Title) {
		return nil, errors.New("ticktick: comment text must be nonblank valid UTF-8")
	}
	return resourceRequest[Comment](ctx, c, http.MethodPost, path+"/comment", input)
}

func (c *Client) DeleteTaskComment(ctx context.Context, projectID, taskID, commentID string) error {
	path, err := commentPath(projectID, taskID)
	if err != nil {
		return err
	}
	path, err = resourcePath(path+"/comment", commentID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) ListCountdowns(ctx context.Context) ([]Countdown, error) {
	return resourceList[Countdown](ctx, c, "/open/v1/countdown")
}
