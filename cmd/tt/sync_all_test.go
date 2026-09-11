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

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

func TestSyncCommandRefreshesCatalogsAndReportsPartialBrowserFailure(t *testing.T) {
	for _, mode := range []string{"ready", "expired", "absent"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			if err := saveLoginCredential(ctx, "open-token", nil); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if mode != "absent" {
				credentials := webCredentials{Version: 1, Session: "browser-token", OpenFingerprint: credentialFingerprint("open-token"), BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
				if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
					t.Fatal(err)
				}
			}
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			paths := map[string]int{}
			http.DefaultTransport = authTransportFunc(func(r *http.Request) (*http.Response, error) {
				paths[r.URL.Path]++
				if r.Method != "GET" {
					t.Fatal("sync without queued work made a write")
				}
				status, body := 200, "[]"
				if r.URL.Path == "/api/v2/timer" {
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "t=browser-token" {
						t.Fatal("Browser credential routing")
					}
					if mode == "expired" {
						status = 401
					} else {
						body = `[{"id":"work-id","name":"Work","type":"TIMER","status":0}]`
					}
				} else {
					if r.Header.Get("Authorization") != "Bearer open-token" || r.Header.Get("Cookie") != "" {
						t.Fatal("Open credential routing")
					}
					if strings.HasSuffix(r.URL.Path, "/data") {
						body = `{"project":{"id":"inbox","name":"Inbox","kind":"TASK"},"tasks":[],"columns":[]}`
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			var stdout, stderr bytes.Buffer
			code := run(ctx, []string{"sync", "--json"}, strings.NewReader(""), &stdout, &stderr)
			want := exitOK
			if mode == "expired" {
				want = exitError
			}
			if code != want {
				t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
			}
			var result struct {
				Data struct {
					Refreshes []app.SyncRefresh `json:"refreshes"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data.Refreshes) < 6 {
				t.Fatalf("missing report: %s", stdout.String())
			}
			state := result.Data.Refreshes[0]
			if mode == "expired" && (state.State != "failed" || !strings.Contains(state.Message, "tt login web --same-account")) {
				t.Fatalf("recovery guidance %+v", state)
			}
			if paths["/open/v1/tag"] != 1 || paths["/open/v1/project/group"] != 1 {
				t.Fatalf("Open catalogs not refreshed: %+v", paths)
			}
			if strings.Contains(stdout.String(), "browser-token") || strings.Contains(stdout.String(), "open-token") {
				t.Fatal("credential leaked")
			}
			if mode == "absent" && paths["/api/v2/timer"] != 0 {
				t.Fatal("requested Browser API without login")
			}
			for _, args := range [][]string{{"timer", "sync"}, {"pomodoro", "sync"}, {"timer", "topic", "ls"}} {
				before := len(paths)
				total := 0
				for _, n := range paths {
					total += n
				}
				stdout.Reset()
				stderr.Reset()
				if code := run(ctx, args, strings.NewReader(""), &stdout, &stderr); code != exitOK {
					t.Fatalf("individual %v: %d %s", args, code, stderr.String())
				}
				after := 0
				for _, n := range paths {
					after += n
				}
				if len(paths) != before || total != after {
					t.Fatal("individual local/empty sync refreshed unrelated catalogs")
				}
			}
		})
	}
}
