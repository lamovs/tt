package webapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultBaseURL      = "https://api.ticktick.com"
	maxResponseBodyRead = 2 << 20
)

type Topic struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Icon         string          `json:"icon,omitempty"`
	Color        string          `json:"color,omitempty"`
	Type         string          `json:"type,omitempty"`
	PomodoroTime int             `json:"pomodoroTime,omitempty"`
	Status       int             `json:"status,omitempty"`
	SortOrder    int64           `json:"sortOrder,omitempty"`
	Etag         string          `json:"etag,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

type FocusTopic struct {
	TimerID   string `json:"timerId"`
	TimerName string `json:"timerName"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}

type FocusCreate struct {
	ID            string       `json:"id"`
	Tasks         []FocusTopic `json:"tasks"`
	StartTime     string       `json:"startTime"`
	EndTime       string       `json:"endTime"`
	Status        int          `json:"status"`
	PauseDuration int64        `json:"pauseDuration"`
	AdjustTime    int64        `json:"adjustTime"`
	Type          int          `json:"type"`
	Added         bool         `json:"added"`
	Note          string       `json:"note,omitempty"`
}

type BatchResponse struct {
	ID2Etag  map[string]string `json:"id2etag"`
	ID2Error map[string]string `json:"id2error"`
}

type StatusError struct {
	Method, Path string
	StatusCode   int
}

type RequestGuardError struct{ Err error }

func (e *RequestGuardError) Error() string { return e.Err.Error() }
func (e *RequestGuardError) Unwrap() error { return e.Err }

func (e *StatusError) Error() string {
	return fmt.Sprintf("TickTick Browser API %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
}

type Option func(*Client)

func WithBaseURL(value string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(value, "/") }
}

func WithHTTPClient(value *http.Client) Option {
	return func(c *Client) {
		clone := *value
		c.httpClient = &clone
	}
}

type Client struct {
	baseURL, session, userAgent string
	httpClient                  *http.Client
	requestGuard                func(context.Context) (func(), error)
	errorAdvice                 func(error) error
}

func WithRequestGuard(guard func(context.Context) (func(), error)) Option {
	return func(c *Client) { c.requestGuard = guard }
}

func WithErrorAdvice(advice func(error) error) Option {
	return func(c *Client) { c.errorAdvice = advice }
}

func (c *Client) CredentialFingerprint() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(c.session)))
}

func NewClient(session, version string, options ...Option) (*Client, error) {
	session = strings.TrimSpace(session)
	if err := ValidateSession(session); err != nil {
		return nil, err
	}
	c := &Client{
		baseURL: defaultBaseURL, session: session, userAgent: "tt/" + version,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, option := range options {
		option(c)
	}
	c.httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("TickTick Browser API redirect refused")
	}
	return c, nil
}

func ValidateSession(value string) error {
	if value == "" {
		return errors.New("Browser session is empty")
	}
	if len(value) > 8192 || !utf8.ValidString(value) {
		return errors.New("Browser session has an invalid size or encoding")
	}
	for _, r := range value {
		if r <= 0x20 || r >= 0x7f || r == ';' || r == ',' {
			return errors.New("Browser session contains characters that cannot be sent safely")
		}
	}
	return nil
}

func (c *Client) ListTopics(ctx context.Context) ([]Topic, error) {
	var raw []json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/api/v2/timer", nil, &raw); err != nil {
		return nil, err
	}
	topics := make([]Topic, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		var topic Topic
		if err := json.Unmarshal(item, &topic); err != nil {
			return nil, errors.New("TickTick Browser API returned an invalid topic")
		}
		topic.ID, topic.Name = strings.TrimSpace(topic.ID), strings.TrimSpace(topic.Name)
		if !validTopicIdentity(topic.ID, topic.Name) || seen[topic.ID] {
			return nil, errors.New("TickTick Browser API returned a missing, duplicate or invalid topic identity")
		}
		seen[topic.ID] = true
		topic.Raw = append(json.RawMessage(nil), item...)
		topics = append(topics, topic)
	}
	return topics, nil
}

func (c *Client) CreateTopicFocus(ctx context.Context, request FocusCreate) (BatchResponse, error) {
	if err := ValidateFocus(request); err != nil {
		return BatchResponse{}, err
	}
	path := "/api/v2/batch/pomodoro"
	if request.Type == 1 {
		path += "/timing"
	}
	body := struct {
		Add    []FocusCreate `json:"add"`
		Update []any         `json:"update"`
		Delete []any         `json:"delete"`
	}{Add: []FocusCreate{request}, Update: []any{}, Delete: []any{}}
	var response BatchResponse
	if err := c.do(ctx, http.MethodPost, path, body, &response); err != nil {
		return response, err
	}
	if message := strings.TrimSpace(response.ID2Error[request.ID]); message != "" {
		return response, fmt.Errorf("TickTick rejected topic focus %s", request.ID)
	}
	if strings.TrimSpace(response.ID2Etag[request.ID]) == "" {
		return response, errors.New("TickTick did not confirm the created topic focus identity")
	}
	return response, nil
}

func (c *Client) GetTopicFocus(ctx context.Context, id string, focusType int) (FocusCreate, error) {
	if !validFocusID(id) || focusType < 0 || focusType > 1 {
		return FocusCreate{}, errors.New("invalid Browser topic focus identity")
	}
	path := "/api/v2/pomodoro/" + id
	if focusType == 1 {
		path = "/api/v2/pomodoro/timing/" + id
	}
	var record FocusCreate
	if err := c.do(ctx, http.MethodGet, path, nil, &record); err != nil {
		return record, err
	}
	if record.ID != id {
		return record, errors.New("TickTick Browser API returned a different focus identity")
	}
	return record, nil
}

func ValidateFocus(request FocusCreate) error {
	if !validFocusID(request.ID) || request.Type < 0 || request.Type > 1 || request.Status != 1 || !request.Added || len(request.Tasks) != 1 {
		return errors.New("invalid Browser topic focus request")
	}
	topic := request.Tasks[0]
	if !validTopicIdentity(topic.TimerID, topic.TimerName) || topic.StartTime != request.StartTime || topic.EndTime != request.EndTime {
		return errors.New("invalid Browser topic focus relation")
	}
	start, startErr := time.Parse("2006-01-02T15:04:05.000-0700", request.StartTime)
	end, endErr := time.Parse("2006-01-02T15:04:05.000-0700", request.EndTime)
	elapsed := end.Sub(start)
	if startErr != nil || endErr != nil || elapsed < time.Second || elapsed > 30*24*time.Hour-2*time.Second || request.PauseDuration < 0 || request.PauseDuration >= int64(elapsed/time.Second) || request.AdjustTime != 0 || !utf8.ValidString(request.Note) || utf8.RuneCountInString(request.Note) > 5000 {
		return errors.New("invalid Browser topic focus note or timing")
	}
	return nil
}

func validTopicIdentity(id, name string) bool {
	return strings.TrimSpace(id) != "" && strings.TrimSpace(name) != "" && utf8.ValidString(id) && utf8.ValidString(name) &&
		utf8.RuneCountInString(id) <= 500 && utf8.RuneCountInString(name) <= 500 &&
		!strings.ContainsFunc(id, unicode.IsControl) && !strings.ContainsFunc(name, unicode.IsControl)
}

func validFocusID(id string) bool {
	if len(id) != 24 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) (resultErr error) {
	defer func() {
		if resultErr != nil && c.errorAdvice != nil {
			resultErr = c.errorAdvice(resultErr)
		}
	}()
	if c.requestGuard != nil {
		release, err := c.requestGuard(ctx)
		if err != nil {
			return &RequestGuardError{Err: err}
		}
		if release != nil {
			defer release()
		}
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Browser API request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build Browser API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cookie", "t="+c.session)
	request.Header.Set("Origin", "https://ticktick.com")
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("x-device", `{"platform":"web","os":"","device":"","name":"","version":4531,"id":"","channel":"website","campaign":""}`)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("TickTick Browser API %s %s failed: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyRead+1))
	if err != nil {
		return fmt.Errorf("read TickTick Browser API response: %w", err)
	}
	if len(raw) > maxResponseBodyRead {
		return errors.New("TickTick Browser API response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &StatusError{Method: method, Path: path, StatusCode: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("TickTick Browser API returned an empty response")
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return errors.New("TickTick Browser API returned invalid JSON")
	}
	return nil
}
