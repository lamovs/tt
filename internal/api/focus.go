package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type FocusType int

const (
	FocusPomodoro FocusType = 0
	FocusTiming   FocusType = 1
)

type FocusCreate struct {
	Type          FocusType `json:"type"`
	TaskID        string    `json:"taskId,omitempty"`
	Note          string    `json:"note,omitempty"`
	StartTime     string    `json:"startTime"`
	EndTime       string    `json:"endTime"`
	PauseDuration int64     `json:"pauseDuration"`
	Duration      int64     `json:"duration"`
	RelationType  []int     `json:"relationType,omitempty"`
}

type Focus struct {
	ID            string          `json:"id"`
	Type          FocusType       `json:"type"`
	TaskID        string          `json:"taskId,omitempty"`
	Note          string          `json:"note,omitempty"`
	Tasks         []FocusTask     `json:"tasks,omitempty"`
	Status        int             `json:"status,omitempty"`
	StartTime     string          `json:"startTime"`
	EndTime       string          `json:"endTime"`
	PauseDuration int64           `json:"pauseDuration"`
	Duration      int64           `json:"duration"`
	RelationType  []int           `json:"relationType,omitempty"`
	Added         bool            `json:"added,omitempty"`
	CreatedTime   string          `json:"createdTime,omitempty"`
	ModifiedTime  string          `json:"modifiedTime,omitempty"`
	Etimestamp    int64           `json:"etimestamp,omitempty"`
	Etag          string          `json:"etag,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

type FocusTask struct {
	TaskID      string   `json:"taskId"`
	Title       string   `json:"title,omitempty"`
	ProjectName string   `json:"projectName,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	StartTime   string   `json:"startTime"`
	EndTime     string   `json:"endTime"`
}

func (focus *Focus) UnmarshalJSON(data []byte) error {
	type wire Focus
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if strings.TrimSpace(decoded.ID) == "" {
		return fmt.Errorf("focus response has no id: %w", ErrIncompleteAnswer)
	}
	decoded.Raw = append(json.RawMessage(nil), data...)
	*focus = Focus(decoded)
	return nil
}

func (c *Client) CreateFocus(ctx context.Context, request FocusCreate) (*Focus, error) {
	if err := validateFocusCreate(request); err != nil {
		return nil, err
	}
	var out Focus
	if err := c.do(ctx, http.MethodPost, "/open/v1/focus", request, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetFocus(ctx context.Context, id string, kind FocusType) (*Focus, error) {
	if err := validateFocusType(kind); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("ticktick: focus id is required")
	}
	query := url.Values{"type": {strconv.Itoa(int(kind))}}
	path := "/open/v1/focus/" + url.PathEscape(id) + "?" + query.Encode()
	var out Focus
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteFocus(ctx context.Context, id string, kind FocusType) (*Focus, error) {
	if err := validateFocusType(kind); err != nil {
		return nil, err
	}
	path, err := resourcePath("/open/v1/focus", id)
	if err != nil {
		return nil, err
	}
	query := url.Values{"type": {strconv.Itoa(int(kind))}}
	return resourceRequest[Focus](ctx, c, http.MethodDelete, path+"?"+query.Encode(), nil)
}

func (c *Client) GetFocuses(ctx context.Context, from, to string, kind FocusType) ([]Focus, error) {
	if err := validateFocusType(kind); err != nil {
		return nil, err
	}
	start, end, err := focusTimeRange(from, to)
	if err != nil {
		return nil, err
	}
	if end.After(start.Add(30 * 24 * time.Hour)) {
		return nil, errors.New("ticktick: focus history range exceeds 30 days")
	}
	query := url.Values{"from": {from}, "to": {to}, "type": {strconv.Itoa(int(kind))}}
	var out []Focus
	if err := c.do(ctx, http.MethodGet, "/open/v1/focus?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateFocusCreate(request FocusCreate) error {
	if err := validateFocusType(request.Type); err != nil {
		return err
	}
	if !utf8.ValidString(request.Note) || utf8.RuneCountInString(request.Note) > 5000 {
		return errors.New("ticktick: focus note must be valid UTF-8 with at most 5000 characters")
	}
	start, end, err := focusTimeRange(request.StartTime, request.EndTime)
	if err != nil {
		return err
	}
	if request.PauseDuration < 0 {
		return errors.New("ticktick: focus pause duration must not be negative")
	}
	if request.Duration <= 0 {
		return errors.New("ticktick: focus duration must be positive")
	}

	elapsed := end.Unix() - start.Unix()
	if end.Nanosecond() < start.Nanosecond() {
		elapsed--
	}
	if request.PauseDuration >= elapsed || request.Duration > elapsed-request.PauseDuration {
		return errors.New("ticktick: focus duration exceeds the elapsed time minus pauses")
	}
	return nil
}

func validateFocusType(kind FocusType) error {
	if kind != FocusPomodoro && kind != FocusTiming {
		return errors.New("ticktick: focus type must be Pomodoro (0) or Timing (1)")
	}
	return nil
}

func focusTimeRange(from, to string) (time.Time, time.Time, error) {
	start, err := parseFocusTime(from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("ticktick: focus start time: %w", err)
	}
	end, err := parseFocusTime(to)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("ticktick: focus end time: %w", err)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("ticktick: focus end time must be after start time")
	}
	return start, end, nil
}

func parseFocusTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		parsed, err := time.Parse(layout, value)
		if err == nil && parsed.Year() >= 1 && !parsed.IsZero() {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("must be a valid timestamp with a time zone")
}
