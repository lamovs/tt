package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func focusCreateFixture() FocusCreate {
	return FocusCreate{
		Type: FocusTiming, TaskID: "task-1", Note: "measured session",
		StartTime: "2026-09-09T15:10:55.125+0300",
		EndTime:   "2026-09-09T15:10:59.125+0300",
		Duration:  4,
	}
}

const focusResponseFixture = `{"id":"focus-1","type":1,"note":"measured session","tasks":[{"taskId":"task-1","title":"test","projectName":"test list","tags":["test"],"startTime":"2026-09-09T12:10:55.125+0000","endTime":"2026-09-09T12:10:59.125+0000"}],"startTime":"2026-09-09T12:10:55.125+0000","endTime":"2026-09-09T12:10:59.125+0000","pauseDuration":700,"duration":4000,"relationType":[2],"future":{"preserve":true}}`

func TestCreateFocusWireAndRawResponse(t *testing.T) {
	for _, kind := range []FocusType{FocusPomodoro, FocusTiming} {
		t.Run(map[FocusType]string{FocusPomodoro: "pomodoro", FocusTiming: "timing"}[kind], func(t *testing.T) {
			request := focusCreateFixture()
			request.Type = kind
			request.Note = "quotes: \"text\", slash: \\, line\nnext, <tag>, заметка"
			request.TaskID = "task/with?characters&=+#"
			request.RelationType = []int{0}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/open/v1/focus" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing normal authenticated JSON request headers")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Error(err)
				}
				if _, sent := raw["id"]; sent {
					t.Error("allocating create sent an id")
				}
				if string(raw["type"]) != map[FocusType]string{FocusPomodoro: "0", FocusTiming: "1"}[kind] || string(raw["pauseDuration"]) != "0" || string(raw["duration"]) != "4" {
					t.Errorf("type or request units changed: %s", body)
				}
				var decoded FocusCreate
				if err := json.Unmarshal(body, &decoded); err != nil || !reflect.DeepEqual(decoded, request) {
					t.Errorf("request fields changed: %+v, %v", decoded, err)
				}
				_, _ = io.WriteString(w, focusResponseFixture)
			}))
			defer server.Close()

			got, err := testClient(t, server).CreateFocus(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || got.ID != "focus-1" || got.Type != FocusTiming || got.Duration != 4000 || got.PauseDuration != 700 || !reflect.DeepEqual(got.RelationType, []int{2}) {
				t.Fatalf("response values were normalized: calls=%d, %+v", calls.Load(), got)
			}
			if got.TaskID != "" || len(got.Tasks) != 1 || got.Tasks[0].TaskID != "task-1" || got.StartTime != "2026-09-09T12:10:55.125+0000" || string(got.Raw) != focusResponseFixture {
				t.Fatalf("response provenance or task association changed: %+v", got)
			}
		})
	}
}

func TestGetFocusEscapesIDAndPreservesPomodoroFields(t *testing.T) {
	id := "a/b?type=1&other=x#part %"
	body := `{"id":"returned-id","type":0,"taskId":"task-1","tasks":[{"taskId":"task-2"}],"status":1,"duration":3000,"pauseDuration":0,"startTime":"2026-09-09T12:31:56.000+0000","endTime":"2026-09-09T12:31:59.000+0000"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/open/v1/focus/"+id || !strings.Contains(r.URL.EscapedPath(), "a%2Fb%3F") || !strings.Contains(r.URL.EscapedPath(), "%23part%20%25") {
			t.Errorf("ID was not one escaped path segment: %s %s", r.Method, r.URL.RequestURI())
		}
		if query := r.URL.Query(); len(query) != 1 || query.Get("type") != "0" {
			t.Errorf("type missing or ID injected a query: %v", query)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	got, err := testClient(t, server).GetFocus(context.Background(), id, FocusPomodoro)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "returned-id" || got.Type != FocusPomodoro || got.TaskID != "task-1" || got.Tasks[0].TaskID != "task-2" || got.Status != 1 || got.Duration != 3000 || string(got.Raw) != body {
		t.Fatalf("raw Pomodoro fields changed: %+v", got)
	}
}

func TestFocusDurationAndPauseKeepTheirDifferentWireUnits(t *testing.T) {
	for _, kind := range []FocusType{FocusPomodoro, FocusTiming} {
		request := focusCreateFixture()
		request.Type, request.Duration, request.PauseDuration = kind, 3, 2
		request.EndTime = "2026-09-09T15:11:00.999+0300"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var got FocusCreate
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Duration != 3 || got.PauseDuration != 2 || got.StartTime != request.StartTime || got.EndTime != request.EndTime {
				t.Errorf("request seconds or fractional timestamps changed: %+v, %v", got, err)
			}
			_ = json.NewEncoder(w).Encode(Focus{ID: "f1", Type: kind, Duration: 3000, PauseDuration: 2})
		}))
		got, err := testClient(t, server).CreateFocus(context.Background(), request)
		server.Close()
		if err != nil || got.Duration != 3000 || got.PauseDuration != 2 || got.Type != kind {
			t.Fatalf("type=%d raw response=%+v err=%v", kind, got, err)
		}
	}
}

func TestGetFocusesEscapesRangeAndAllowsThirtyDays(t *testing.T) {
	from, to := "2026-09-01T15:00:00.125+0300", "2026-10-01T15:00:00.125+0300"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/open/v1/focus" || len(query) != 3 || query.Get("from") != from || query.Get("to") != to || query.Get("type") != "0" {
			t.Errorf("history request = %s %s", r.Method, r.URL.RequestURI())
		}
		if !strings.Contains(r.URL.RawQuery, "%2B0300") {
			t.Errorf("offset plus was not query escaped: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, "["+focusResponseFixture+"]")
	}))
	defer server.Close()
	got, err := testClient(t, server).GetFocuses(context.Background(), from, to, FocusPomodoro)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Duration != 4000 || string(got[0].Raw) != focusResponseFixture {
		t.Fatalf("history response = %+v", got)
	}
}

func TestFocusValidationPreventsRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, focusResponseFixture)
	}))
	defer server.Close()
	c := testClient(t, server)
	for _, tc := range []struct {
		name string
		edit func(*FocusCreate)
	}{
		{"negative type", func(r *FocusCreate) { r.Type = -1 }},
		{"unknown type", func(r *FocusCreate) { r.Type = 2 }},
		{"long note", func(r *FocusCreate) { r.Note = strings.Repeat("я", 5001) }},
		{"invalid UTF-8", func(r *FocusCreate) { r.Note = string([]byte{0xff}) }},
		{"empty start", func(r *FocusCreate) { r.StartTime = "" }},
		{"bad start", func(r *FocusCreate) { r.StartTime = "2026-09-09" }},
		{"unzoned start", func(r *FocusCreate) { r.StartTime = "2026-09-09T15:10:55" }},
		{"bad end", func(r *FocusCreate) { r.EndTime = "bad" }},
		{"whitespace date", func(r *FocusCreate) { r.EndTime += " " }},
		{"year zero", func(r *FocusCreate) { r.StartTime = "0000-01-01T00:00:00Z" }},
		{"zero timestamp", func(r *FocusCreate) { r.StartTime = "0001-01-01T00:00:00Z" }},
		{"extended year", func(r *FocusCreate) { r.EndTime = "10000-01-01T00:00:00Z" }},
		{"equal range", func(r *FocusCreate) { r.EndTime = r.StartTime }},
		{"reversed range", func(r *FocusCreate) { r.StartTime, r.EndTime = r.EndTime, r.StartTime }},
		{"negative pause", func(r *FocusCreate) { r.PauseDuration = -1 }},
		{"all paused", func(r *FocusCreate) { r.PauseDuration = 4 }},
		{"pause overflow", func(r *FocusCreate) { r.PauseDuration = math.MaxInt64 }},
		{"zero duration", func(r *FocusCreate) { r.Duration = 0 }},
		{"negative duration", func(r *FocusCreate) { r.Duration = -1 }},
		{"duration overflow", func(r *FocusCreate) { r.Duration = math.MaxInt64 }},
		{"too long", func(r *FocusCreate) { r.Duration = 5 }},
		{"too long after pause", func(r *FocusCreate) { r.PauseDuration = 1 }},
		{"fraction shorter than duration", func(r *FocusCreate) { r.EndTime = "2026-09-09T15:10:59.124+0300" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := focusCreateFixture()
			tc.edit(&request)
			if _, err := c.CreateFocus(context.Background(), request); err == nil {
				t.Fatal("invalid create accepted")
			}
		})
	}
	for _, tc := range []struct {
		name, from, to string
		kind           FocusType
	}{
		{"invalid type", "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", 2},
		{"empty start", "", "2026-09-02T00:00:00Z", FocusTiming},
		{"bad end", "2026-09-01T00:00:00Z", "invalid", FocusTiming},
		{"reverse range", "2026-09-02T00:00:00Z", "2026-09-01T00:00:00Z", FocusTiming},
		{"zero range", "2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z", FocusTiming},
		{"range over 30 days", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00.001Z", FocusTiming},
	} {
		t.Run("history/"+tc.name, func(t *testing.T) {
			if _, err := c.GetFocuses(context.Background(), tc.from, tc.to, tc.kind); err == nil {
				t.Fatal("invalid history request accepted")
			}
		})
	}
	for _, tc := range []struct {
		id   string
		kind FocusType
	}{{"", FocusTiming}, {" \t", FocusPomodoro}, {"f1", -1}, {"f1", 2}} {
		if _, err := c.GetFocus(context.Background(), tc.id, tc.kind); err == nil {
			t.Fatal("invalid addressed GET accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid input issued %d requests", calls.Load())
	}
}

func TestFocusCreateValidBoundaryValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, focusResponseFixture)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name string
		edit func(*FocusCreate)
	}{
		{"5000 runes", func(r *FocusCreate) { r.Note = strings.Repeat("я", 5000) }},
		{"pause", func(r *FocusCreate) { r.PauseDuration, r.Duration = 1, 3 }},
		{"RFC3339 instant", func(r *FocusCreate) { r.EndTime = "2026-09-09T12:10:59.125Z" }},
		{"fraction truncation", func(r *FocusCreate) { r.EndTime, r.Duration = "2026-09-09T15:10:59.124+0300", 3 }},
		{"long range without duration overflow", func(r *FocusCreate) {
			r.StartTime, r.EndTime, r.Duration = "1900-01-01T00:00:00Z", "2300-01-01T00:00:00Z", 10000000000
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := focusCreateFixture()
			tc.edit(&request)
			if _, err := testClient(t, server).CreateFocus(context.Background(), request); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFocusRejectsIncompleteAndMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		incomplete bool
	}{
		{"empty", "", true},
		{"null", "null", true},
		{"empty object", "{}", true},
		{"error object", `{"error":"failed"}`, true},
		{"empty id", `{"id":""}`, true},
		{"blank id", `{"id":" \t"}`, true},
		{"null id", `{"id":null}`, true},
		{"array id", `{"id":[]}`, false},
		{"array", "[]", false},
		{"string", `"focus"`, false},
		{"malformed", `{"id":`, false},
		{"wrong duration", `{"id":"f1","duration":"4000"}`, false},
		{"fractional duration", `{"id":"f1","duration":1.5}`, false},
		{"overflow duration", `{"id":"f1","duration":9223372036854775808}`, false},
		{"wrong type", `{"id":"f1","type":"0"}`, false},
	} {
		for _, method := range []string{"create", "get"} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, tc.body)
				}))
				defer server.Close()
				c := testClient(t, server)
				var err error
				if method == "create" {
					_, err = c.CreateFocus(context.Background(), focusCreateFixture())
				} else {
					_, err = c.GetFocus(context.Background(), "f1", FocusTiming)
				}
				var decoded *DecodeError
				if !errors.As(err, &decoded) || errors.Is(err, ErrIncompleteAnswer) != tc.incomplete || calls.Load() != 1 {
					t.Fatalf("calls=%d err=%T %v, incomplete=%v", calls.Load(), err, err, tc.incomplete)
				}
			})
		}
	}
	for _, tc := range []struct {
		body       string
		incomplete bool
	}{
		{"", true}, {"null", true}, {"{}", true},
		{`{"id":"f1"}`, false}, {`[null]`, true}, {`[{}]`, true},
		{`[{"id":""}]`, true}, {`[{"id":"f1","duration":false}]`, false},
	} {
		t.Run("history/"+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer server.Close()
			if _, err := testClient(t, server).GetFocuses(context.Background(), "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", FocusTiming); err == nil || errors.Is(err, ErrIncompleteAnswer) != tc.incomplete {
				t.Fatalf("history error=%v, want incomplete=%v", err, tc.incomplete)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "[]") }))
	defer server.Close()
	got, err := testClient(t, server).GetFocuses(context.Background(), "2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", FocusTiming)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("explicit empty history = %+v, %v", got, err)
	}
}

type focusRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn focusRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestCreateFocusNeverRetriesAnUncertainMutation(t *testing.T) {
	for _, mode := range []string{"transport", "server error", "rate limit", "lost response"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost {
					t.Errorf("method = %s", r.Method)
				}
				switch mode {
				case "server error":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "rate limit":
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
				case "lost response":
					w.Header().Set("Content-Length", "1000")
					_, _ = io.WriteString(w, `{"id":"allocated-but-incomplete`)
				}
			}))
			defer server.Close()
			c := testClient(t, server, WithMaxRetries(5))
			transportFailure := errors.New("synthetic transport failure")
			if mode == "transport" {
				c = testClient(t, server, WithMaxRetries(5), WithHTTPClient(&http.Client{Transport: focusRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, transportFailure
				})}))
			}
			_, err := c.CreateFocus(context.Background(), focusCreateFixture())
			if err == nil || calls.Load() != 1 {
				t.Fatalf("uncertain create calls=%d err=%v", calls.Load(), err)
			}
			switch mode {
			case "transport":
				if !errors.Is(err, transportFailure) {
					t.Fatalf("transport cause lost: %v", err)
				}
			case "server error":
				if !errors.Is(err, ErrServerError) {
					t.Fatalf("server classification lost: %v", err)
				}
			case "rate limit":
				if !errors.Is(err, ErrRateLimited) {
					t.Fatalf("rate classification lost: %v", err)
				}
			case "lost response":
				var transport *TransportError
				if !errors.As(err, &transport) || !transport.responseRead {
					t.Fatalf("lost response classification = %T %v", err, err)
				}
			}
		})
	}
}

func TestCreateFocusCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c := testClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CreateFocus(ctx, focusCreateFixture()); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("pre-canceled request: calls=%d err=%v", calls.Load(), err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := c.CreateFocus(ctx, focusCreateFixture())
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("in-flight cancellation: calls=%d err=%v", calls.Load(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop create")
	}
}

func TestFocusResponseLimitAndCredentialRedaction(t *testing.T) {
	const secret = "focus-secret-token"
	for _, mode := range []string{"limit", "decode", "status"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "limit":
					_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseBodyRead+1))
				case "decode":
					_, _ = io.WriteString(w, `{"error":"`+secret+`"}`)
				case "status":
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, secret)
				}
			}))
			defer server.Close()
			c := NewClient(secret, "test", WithBaseURL(server.URL))
			_, err := c.CreateFocus(context.Background(), focusCreateFixture())
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), secret) {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
			if mode == "limit" && !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("response cap lost: %v", err)
			}
			if mode == "decode" {
				var decoded *DecodeError
				if !errors.As(err, &decoded) || strings.Contains(decoded.Body, secret) || !strings.Contains(decoded.Body, "[REDACTED]") {
					t.Fatal("decode diagnostic lost credential redaction")
				}
			}
			if mode == "status" {
				var status *StatusError
				if !errors.As(err, &status) || strings.Contains(status.Body, secret) || status.Body != "[REDACTED]" {
					t.Fatal("status diagnostic lost credential redaction")
				}
			}
		})
	}
}
