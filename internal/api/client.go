package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultTimeout    = 30 * time.Second
	DefaultMaxRetries = 3
)

const (
	defaultBaseURL        = "https://api.ticktick.com"
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 8 * time.Second
	maxResponseBodyRead   = 10 << 20
	maxRedirects          = 10
)

type Client struct {
	baseURL      string
	token        string
	userAgent    string
	httpClient   *http.Client
	maxRetries   int
	requestGuard RequestGuard
	errorAdvice  func(error) error

	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
}

type Option func(*Client)

func WithErrorAdvice(advice func(error) error) Option {
	return func(c *Client) { c.errorAdvice = advice }
}

func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		clone := *hc
		c.httpClient = &clone
	}
}

func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 1 {
			c.maxRetries = n
		}
	}
}

func NewClient(token, version string, opts ...Option) *Client {
	c := &Client{
		baseURL:        defaultBaseURL,
		token:          token,
		userAgent:      "tt/" + version,
		httpClient:     &http.Client{Timeout: DefaultTimeout},
		maxRetries:     DefaultMaxRetries,
		retryBaseDelay: defaultRetryBaseDelay,
		retryMaxDelay:  defaultRetryMaxDelay,
	}
	for _, opt := range opts {
		opt(c)
	}

	prev := c.httpClient.CheckRedirect
	c.httpClient.CheckRedirect = redirectPolicy(prev)
	return c
}

var errInsecureRedirect = errors.New("refused to follow a redirect away from https")

var errRedirectLimit = errors.New("redirect limit reached")

func redirectPolicy(prev func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if prev != nil {
			if err := prev(req, via); err != nil {
				return err
			}
			return refuseDowngrade(req, via)
		}
		if err := refuseDowngrade(req, via); err != nil {
			return err
		}
		return capRedirects(via)
	}
}

func refuseDowngrade(req *http.Request, via []*http.Request) error {

	if from := via[len(via)-1]; from.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: the bearer token this client sends with every request would go over %s unencrypted",
			errInsecureRedirect, req.URL.Scheme)
	}
	return nil
}

func capRedirects(via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w after %d hops", errRedirectLimit, maxRedirects)
	}
	return nil
}

func (c *Client) MaxCallDuration() time.Duration {
	timeout := c.httpClient.Timeout
	if timeout <= 0 {
		return 0
	}
	attempts := time.Duration(c.maxRetries)
	if attempts < 1 {
		attempts = 1
	}
	sending := attempts * timeout
	if sending <= 0 || sending/attempts != timeout {
		return 0
	}
	waiting := (attempts - 1) * c.backoffCap()
	if waiting < 0 || sending+waiting < sending {
		return 0
	}
	return sending + waiting
}

func (c *Client) do(ctx context.Context, method, path string, reqBody, out interface{}) (resultErr error) {
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
	var bodyBytes []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return &preparationError{Method: method, Path: path, Kind: encodeRequestFailure, Err: err}
		}
		bodyBytes = b
	}

	attempts := c.maxRetries
	if attempts < 1 {
		attempts = 1
	}
	if !safeToRepeat(method) {

		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return &preparationError{Method: method, Path: path, Kind: buildRequestFailure, Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", "application/json")
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = &TransportError{Method: method, Path: path, Err: err}
			if ctx.Err() != nil || attempt == attempts {
				return lastErr
			}
			if !c.sleepBackoff(ctx, attempt, "") {
				return lastErr
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyRead+1))
		resp.Body.Close()
		if readErr != nil {
			return &TransportError{Method: method, Path: path, Err: readErr, responseRead: true}
		}
		overLimit := int64(len(respBody)) > maxResponseBodyRead

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil {

				return nil
			}
			if overLimit {
				return &ResponseTooLargeError{
					StatusCode: resp.StatusCode, Method: method, Path: path, Limit: maxResponseBodyRead,
				}
			}
			if err := emptyAnswer(respBody); err != nil {
				return &DecodeError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: diagnosticBody(respBody, c.token), Err: err}
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return &DecodeError{StatusCode: resp.StatusCode, Method: method, Path: path, Body: diagnosticBody(respBody, c.token), Err: err}
			}
			return nil
		}

		retryAfter := resp.Header.Get("Retry-After")
		statusErr := newStatusError(method, path, resp.StatusCode, respBody, retryAfter, c.token)
		lastErr = statusErr
		if !isRetryableStatus(resp.StatusCode) || attempt == attempts {
			return statusErr
		}
		if !c.sleepBackoff(ctx, attempt, retryAfter) {
			return statusErr
		}
	}
	return lastErr
}

func emptyAnswer(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) == 0:
		return fmt.Errorf("%w: the response carried no body", ErrIncompleteAnswer)
	case bytes.Equal(trimmed, []byte("null")):
		return fmt.Errorf("%w: the response body was the JSON literal null", ErrIncompleteAnswer)
	case emptyObject(trimmed):
		return fmt.Errorf("%w: %w: the response body was a JSON object with no fields in it",
			ErrIncompleteAnswer, ErrNoSuchProject)
	}
	return nil
}

func emptyObject(trimmed []byte) bool {
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	return len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) == 0
}
