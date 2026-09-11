package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryAfterIsCappedAtTheBackoffCeiling(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := testClient(t, server, WithMaxRetries(2), WithTimeout(5*time.Second))
	start := time.Now()
	var out map[string]any
	if err := c.do(context.Background(), "GET", "/open/v1/project", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("the wait between attempts was %v, past the %v ceiling the client keeps",
			elapsed, c.backoffCap())
	}
	if calls != 2 {
		t.Errorf("%d call(s), want the second attempt to have been made", calls)
	}
}

func TestRetryAfterUnderTheCeilingIsWaitedOut(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c := testClient(t, server, WithMaxRetries(2), WithTimeout(5*time.Second))

	c.retryMaxDelay = 30 * time.Second

	start := time.Now()
	var out map[string]any
	if err := c.do(context.Background(), "GET", "/open/v1/project", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("the second attempt came after %v, want the %v the server asked for", elapsed, time.Second)
	}
}

func TestMaxCallDurationCountsEveryAttemptAndWait(t *testing.T) {

	c := NewClient("tok", "0.0.0-test", WithTimeout(30*time.Second), WithMaxRetries(3))
	if got, want := c.MaxCallDuration(), 106*time.Second; got != want {
		t.Errorf("MaxCallDuration = %v, want %v", got, want)
	}

	c = NewClient("tok", "0.0.0-test", WithTimeout(time.Second), WithMaxRetries(1))
	if got, want := c.MaxCallDuration(), time.Second; got != want {
		t.Errorf("MaxCallDuration of a single attempt = %v, want %v", got, want)
	}

	c = NewClient("tok", "0.0.0-test", WithHTTPClient(&http.Client{}))
	if got := c.MaxCallDuration(); got != 0 {
		t.Errorf("MaxCallDuration without a timeout = %v, want 0", got)
	}
}
