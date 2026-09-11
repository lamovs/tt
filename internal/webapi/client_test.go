package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListTopicsUsesOnlyBrowserSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/timer" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Cookie") != "t=browser-session" || r.Header.Get("Authorization") != "" {
			t.Fatal("wrong authorization channel")
		}
		_, _ = w.Write([]byte(`[{"id":"study","name":"Study","status":0},{"id":"work","name":"Work","status":1}]`))
	}))
	defer server.Close()
	client, err := NewClient("browser-session", "test", WithBaseURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	topics, err := client.ListTopics(context.Background())
	if err != nil || len(topics) != 2 || topics[0].Name != "Study" || string(topics[0].Raw) == "" {
		t.Fatalf("topics: %+v, %v", topics, err)
	}
}

func TestCreateTopicFocusUsesExactBatch(t *testing.T) {
	const id = "0123456789abcdef01234567"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/batch/pomodoro/timing" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Add []FocusCreate `json:"add"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Add) != 1 || body.Add[0].Tasks[0].TimerID != "study" {
			t.Fatalf("body: %+v, %v", body, err)
		}
		_, _ = w.Write([]byte(`{"id2etag":{"` + id + `":"etag"},"id2error":{}}`))
	}))
	defer server.Close()
	client, _ := NewClient("browser-session", "test", WithBaseURL(server.URL))
	start := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(25 * time.Minute)
	from, to := start.Format("2006-01-02T15:04:05.000-0700"), end.Format("2006-01-02T15:04:05.000-0700")
	request := FocusCreate{ID: id, Type: 1, Status: 1, Added: true, StartTime: from, EndTime: to, Tasks: []FocusTopic{{TimerID: "study", TimerName: "Study", StartTime: from, EndTime: to}}}
	if _, err := client.CreateTopicFocus(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestTopicFocusIdentityCannotEscapeFixedPath(t *testing.T) {
	client, _ := NewClient("browser-session", "test")
	for _, id := range []string{"", "../timer", "0123456789ABCDEF01234567", "0123456789abcdef0123456z"} {
		if _, err := client.GetTopicFocus(context.Background(), id, 1); err == nil {
			t.Fatalf("accepted focus id %q", id)
		}
	}
}

func TestBrowserSessionValidation(t *testing.T) {
	for _, value := range []string{"", "space value", "value;other=secret", "value\nheader"} {
		if _, err := NewClient(value, "test"); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
