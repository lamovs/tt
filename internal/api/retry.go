package api

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (c *Client) sleepBackoff(ctx context.Context, attempt int, retryAfter string) bool {
	if ctx.Err() != nil {
		return false
	}

	delay := c.computeBackoff(attempt)
	if retryAfter != "" {
		if d, ok := parseRetryAfter(retryAfter); ok {

			delay = min(d, c.backoffCap())
		}
	}
	if delay <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) computeBackoff(attempt int) time.Duration {
	base := c.retryBaseDelay
	if base <= 0 {
		base = defaultRetryBaseDelay
	}
	maxDelay := c.backoffCap()

	var exp time.Duration
	if attempt <= 1 || attempt > 20 {
		exp = base
		if attempt > 20 {
			exp = maxDelay
		}
	} else {
		exp = base * time.Duration(int64(1)<<uint(attempt-1))
	}
	if exp > maxDelay || exp <= 0 {
		exp = maxDelay
	}
	if exp <= 0 {
		return 0
	}
	return rand.N(exp + 1)
}

func (c *Client) backoffCap() time.Duration {
	if c.retryMaxDelay > 0 {
		return c.retryMaxDelay
	}
	return defaultRetryMaxDelay
}

func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
