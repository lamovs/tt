package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRequestGuardBlocksBeforeSendingAndRetainsCause(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `[]`) }))
	defer server.Close()
	cause := errors.New("synthetic credential change")
	client := NewClient("synthetic-token", "test", WithBaseURL(server.URL), WithRequestGuard(func(context.Context) (func(), error) { return nil, cause }))
	_, err := client.ListProjects(context.Background())
	var blocked *RequestGuardError
	if !errors.As(err, &blocked) || !errors.Is(err, cause) || requests.Load() != 0 {
		t.Fatalf("guard: %v requests=%d", err, requests.Load())
	}
}

func TestRequestGuardStaysHeldThroughResponseAndReleasesOnErrors(t *testing.T) {
	var held atomic.Bool
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !held.Load() {
			t.Error("guard released before request")
		}
		io.WriteString(w, `invalid response`)
	}))
	defer server.Close()
	client := NewClient("synthetic-token", "test", WithBaseURL(server.URL), WithRequestGuard(func(context.Context) (func(), error) {
		held.Store(true)
		return func() { held.Store(false); releases++ }, nil
	}))
	if _, err := client.ListProjects(context.Background()); err == nil {
		t.Fatal("expected response error")
	}
	if held.Load() || releases != 1 {
		t.Fatalf("guard not released: held=%v releases=%d", held.Load(), releases)
	}
	if err := client.do(context.Background(), http.MethodPost, "/test", make(chan int), nil); err == nil {
		t.Fatal("expected encoding error")
	}
	if held.Load() || releases != 2 {
		t.Fatalf("guard not released after encoding failure: %v %d", held.Load(), releases)
	}
}
