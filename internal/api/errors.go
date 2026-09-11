package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrUnauthorized     = errors.New("ticktick: unauthorized")
	ErrNotFound         = errors.New("ticktick: not found")
	ErrRateLimited      = errors.New("ticktick: rate limited")
	ErrServerError      = errors.New("ticktick: server error")
	ErrUnexpectedStatus = errors.New("ticktick: unexpected status")

	ErrIncompleteAnswer = errors.New("ticktick: incomplete answer")

	ErrNoSuchProject = errors.New("ticktick: no such project")

	ErrResponseTooLarge = errors.New("ticktick: response too large")
)

const maxErrorBodyLen = 2048

func truncateBody(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	if len(b) > maxErrorBodyLen {
		b = b[:maxErrorBodyLen]
	}
	return string(b)
}

func diagnosticBody(b []byte, bearer string) string {
	if bearer != "" {
		b = bytes.ReplaceAll(b, []byte(bearer), []byte("[REDACTED]"))
	}
	return truncateBody(b)
}

func OneLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {

			fmt.Fprintf(&b, `\x%02x`, s[i])
			i++
			continue
		}
		i += size
		if strconv.IsPrint(r) {
			b.WriteRune(r)
			continue
		}

		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

type StatusError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string

	RetryAfter      time.Duration
	RetryAfterKnown bool

	Err error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ticktick: %s: http %d", diagnosticRequest(e.Method, e.Path), e.StatusCode)
}

func (e *StatusError) Unwrap() error { return e.Err }

func newStatusError(method, path string, status int, body []byte, retryAfter, bearer string) *StatusError {
	var sentinel error
	switch {
	case status == 401:
		sentinel = ErrUnauthorized
	case status == 404:
		sentinel = ErrNotFound
	case status == 429:
		sentinel = ErrRateLimited
	case status >= 500:
		sentinel = ErrServerError
	default:
		sentinel = ErrUnexpectedStatus
	}
	e := &StatusError{StatusCode: status, Method: method, Path: path, Body: diagnosticBody(body, bearer), Err: sentinel}
	if d, ok := parseRetryAfter(retryAfter); ok {
		e.RetryAfter, e.RetryAfterKnown = d, true
	}
	return e
}

func isRetryableStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

func safeToRepeat(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

type TransportError struct {
	Method string
	Path   string
	Err    error

	responseRead bool
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("ticktick: %s: %s", diagnosticRequest(e.Method, e.Path), transportFailure(e.Err, e.responseRead))
}

func (e *TransportError) Unwrap() error { return e.Err }

type ResponseTooLargeError struct {
	StatusCode int
	Method     string
	Path       string
	Limit      int64
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("ticktick: %s: http %d: response body exceeds %d byte limit",
		diagnosticRequest(e.Method, e.Path), e.StatusCode, e.Limit)
}

func (e *ResponseTooLargeError) Unwrap() error { return ErrResponseTooLarge }

type DecodeError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
	Err        error
}

func (e *DecodeError) Error() string {
	kind, offset := decodeFailure(e.Err)
	if offset > 0 {
		return fmt.Sprintf("ticktick: %s: decode http %d response: %s at byte %d",
			diagnosticRequest(e.Method, e.Path), e.StatusCode, kind, offset)
	}
	return fmt.Sprintf("ticktick: %s: decode http %d response: %s",
		diagnosticRequest(e.Method, e.Path), e.StatusCode, kind)
}

func (e *DecodeError) Unwrap() error { return e.Err }

type preparationFailureKind uint8

const (
	encodeRequestFailure preparationFailureKind = iota + 1
	buildRequestFailure
)

type preparationError struct {
	Method string
	Path   string
	Kind   preparationFailureKind
	Err    error
}

func (e *preparationError) Error() string {
	kind := "request preparation failed"
	switch e.Kind {
	case encodeRequestFailure:
		kind = "request body encoding failed"
	case buildRequestFailure:
		kind = "request construction failed"
	}
	return fmt.Sprintf("ticktick: %s: %s", diagnosticRequest(e.Method, e.Path), kind)
}

func (e *preparationError) Unwrap() error { return e.Err }

func diagnosticRequest(method, path string) string {
	category := requestCategory(path)
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete:
		return method + " " + category
	default:
		return category
	}
}

func requestCategory(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "open" || parts[1] != "v1" {
		return "API request"
	}
	switch {
	case len(parts) == 3 && parts[2] == "project":
		return "projects request"
	case len(parts) == 4 && parts[2] == "project" && parts[3] != "":
		return "project request"
	case len(parts) == 5 && parts[2] == "project" && parts[3] != "" && parts[4] == "data":
		return "project data request"
	case len(parts) == 6 && parts[2] == "project" && parts[3] != "" && parts[4] == "task" && parts[5] != "":
		return "task request"
	case len(parts) == 3 && parts[2] == "task":
		return "tasks request"
	case len(parts) == 4 && parts[2] == "task" && parts[3] != "":
		return "task request"
	case len(parts) == 7 && parts[2] == "project" && parts[3] != "" && parts[4] == "task" && parts[5] != "" && parts[6] == "complete":
		return "task completion request"
	default:
		return "API request"
	}
}

func decodeFailure(err error) (string, int64) {
	if errors.Is(err, ErrIncompleteAnswer) {
		return "incomplete answer", 0
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "malformed JSON", syntax.Offset
	}
	var value *json.UnmarshalTypeError
	if errors.As(err, &value) {
		return "unexpected JSON type", value.Offset
	}
	return "response decode failed", 0
}

func transportFailure(err error, responseRead bool) string {
	if responseRead {
		return "response read failed"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, errInsecureRedirect):
		return "redirect away from https refused"
	case errors.Is(err, errRedirectLimit):
		return fmt.Sprintf("stopped after %d redirects", maxRedirects)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "request timed out"
	}
	return "transport failure"
}
