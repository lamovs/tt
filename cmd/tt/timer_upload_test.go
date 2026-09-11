package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

func TestTimerSyncUploadsBothSavedModesAndDoesNotRepeat(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "synthetic-focus-token")
	writeTokenFile(t, "synthetic-focus-token")
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, mode := range []int{0, 1} {
		opts := store.TimerStartOptions{FocusType: mode, Note: "CLI focus proof"}
		if mode == 0 {
			opts.Planned = time.Minute
		}
		now := time.Date(2026, 9, 9, 12, mode, 0, 0, time.UTC)
		if _, err := st.StartTimer(ctx, opts, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.StopTimer(ctx, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	posts := 0
	records := map[string][]byte{}
	old := http.DefaultTransport
	http.DefaultTransport = oauthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var raw []byte
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/open/v1/focus":
			raw = []byte("[]")
		case req.Method == http.MethodPost && req.URL.Path == "/open/v1/focus":
			posts++
			var request api.FocusCreate
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			id := "timing"
			if request.Type == 0 {
				id = "pomodoro"
			}
			value := map[string]any{"id": id, "type": request.Type, "startTime": request.StartTime, "endTime": request.EndTime, "duration": request.Duration * 1000, "pauseDuration": request.PauseDuration, "note": request.Note, "tasks": []any{}}
			raw, _ = json.Marshal(value)
			records[id] = raw
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/open/v1/focus/"):
			raw = records[strings.TrimPrefix(req.URL.Path, "/open/v1/focus/")]
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	var out, errs bytes.Buffer
	if code := run(ctx, []string{"timer", "sync"}, strings.NewReader(""), &out, &errs); code != exitOK || posts != 2 || !strings.Contains(out.String(), "uploaded 2, held 0") {
		t.Fatalf("sync=%d posts=%d out=%q err=%q", code, posts, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := run(ctx, []string{"pomodoro", "sync"}, strings.NewReader(""), &out, &errs); code != exitOK || posts != 2 {
		t.Fatalf("repeat=%d posts=%d err=%q", code, posts, errs.String())
	}
}
