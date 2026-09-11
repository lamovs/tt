package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type failingCredentialMarshaler struct{ err error }

func (f failingCredentialMarshaler) MarshalJSON() ([]byte, error) { return nil, f.err }

func credentialFixtureForms(token string) []string {
	jsonBytes, _ := json.Marshal(token)
	return []string{
		token,
		strings.Trim(string(jsonBytes), `"`),
		url.QueryEscape(token),
		base64.StdEncoding.EncodeToString([]byte(token)),
	}
}

func assertCredentialFormsAbsent(t *testing.T, text string, forms []string) {
	t.Helper()
	for i, form := range forms {
		if form != "" && strings.Contains(text, form) {
			t.Errorf("diagnostic contains credential fixture form %d", i)
		}
	}
}

func TestHTTPDiagnosticsDoNotEchoCredentialForms(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Client) error
	}{
		{
			name: "status error",
			run: func(c *Client) error {
				return c.UpdateTask(context.Background(), TaskUpdate{ID: "t1", ProjectID: "p1"})
			},
		},
		{
			name: "malformed success",
			run: func(c *Client) error {
				_, err := c.ListProjects(context.Background())
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics []string
			for _, token := range []string{"cred<fixture/+A=", "cred<fixture/+B="} {
				forms := credentialFixtureForms(token)
				body := []byte(strings.Join(forms, "|"))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.name == "status error" {
						w.WriteHeader(http.StatusBadRequest)
					}
					_, _ = w.Write(body)
				}))

				client := NewClient(token, "0.0.0-test", WithBaseURL(server.URL), WithMaxRetries(1))
				err := tc.run(client)
				server.Close()
				if err == nil {
					t.Fatal("request unexpectedly succeeded")
				}
				diagnostic := err.Error()
				assertCredentialFormsAbsent(t, diagnostic, forms)
				diagnostics = append(diagnostics, diagnostic)

				wantBody := diagnosticBody(body, token)
				switch tc.name {
				case "status error":
					var typed *StatusError
					if !errors.As(err, &typed) {
						t.Fatal("status failure lost its typed error")
					}
					if typed.Body != wantBody {
						t.Error("status error did not retain the redacted raw body")
					}
				case "malformed success":
					var typed *DecodeError
					if !errors.As(err, &typed) {
						t.Fatal("decode failure lost its typed error")
					}
					if typed.Body != wantBody {
						t.Error("decode error did not retain the redacted raw body")
					}
				}
			}
			if len(diagnostics) != 2 || diagnostics[0] != diagnostics[1] {
				t.Error("credential value changed the ordinary diagnostic")
			}
		})
	}
}

func TestManualTypedErrorsDoNotTrustDiagnosticFields(t *testing.T) {
	const poison = "cred<manual/+fixture="
	cause := errors.New(poison)
	tests := []struct {
		name string
		err  error
		is   error
	}{
		{"status body", &StatusError{StatusCode: 418, Method: http.MethodGet, Path: "/open/v1/task", Body: poison, Err: cause}, cause},
		{"status method", &StatusError{StatusCode: 418, Method: poison, Path: "/open/v1/task", Err: cause}, cause},
		{"status path", &StatusError{StatusCode: 418, Method: http.MethodGet, Path: "/" + poison, Err: cause}, cause},
		{"status cause", &StatusError{StatusCode: 418, Method: http.MethodGet, Path: "/open/v1/task", Err: cause}, cause},
		{"decode body", &DecodeError{StatusCode: 200, Method: http.MethodGet, Path: "/open/v1/task", Body: poison, Err: cause}, cause},
		{"decode method", &DecodeError{StatusCode: 200, Method: poison, Path: "/open/v1/task", Err: cause}, cause},
		{"decode path", &DecodeError{StatusCode: 200, Method: http.MethodGet, Path: "/" + poison, Err: cause}, cause},
		{"decode cause", &DecodeError{StatusCode: 200, Method: http.MethodGet, Path: "/open/v1/task", Err: cause}, cause},
		{"transport method", &TransportError{Method: poison, Path: "/open/v1/task", Err: cause}, cause},
		{"transport path", &TransportError{Method: http.MethodGet, Path: "/" + poison, Err: cause}, cause},
		{"transport cause", &TransportError{Method: http.MethodGet, Path: "/open/v1/task", Err: cause}, cause},
		{"response limit method", &ResponseTooLargeError{StatusCode: 200, Method: poison, Path: "/open/v1/task", Limit: 99}, ErrResponseTooLarge},
		{"response limit path", &ResponseTooLargeError{StatusCode: 200, Method: http.MethodGet, Path: "/" + poison, Limit: 99}, ErrResponseTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.err.Error(), poison) {
				t.Error("ordinary diagnostic echoed a hostile typed field")
			}
			if !errors.Is(tc.err, tc.is) {
				t.Error("typed error lost its unwrap identity")
			}
		})
	}

	value := &json.UnmarshalTypeError{Value: poison, Type: nil, Offset: 17}
	err := &DecodeError{StatusCode: 200, Method: http.MethodGet, Path: "/open/v1/task", Err: value}
	if strings.Contains(err.Error(), poison) || !strings.Contains(err.Error(), "unexpected JSON type at byte 17") {
		t.Error("decode diagnostic did not replace the JSON value with its safe category and offset")
	}
	var gotValue *json.UnmarshalTypeError
	if !errors.As(err, &gotValue) || gotValue != value {
		t.Error("decode diagnostic lost the original JSON error")
	}
}

func TestRequestPreparationDiagnosticsRetainCausesWithoutEchoingThem(t *testing.T) {
	const poison = "cred<prepare/+fixture="
	cause := errors.New(poison)
	client := NewClient("token", "0.0.0-test", WithMaxRetries(1))

	err := client.do(context.Background(), poison, "/"+poison, failingCredentialMarshaler{err: cause}, nil)
	if err == nil || strings.Contains(err.Error(), poison) {
		t.Fatal("request encoding diagnostic exposed a fixture")
	}
	if !errors.Is(err, cause) {
		t.Error("request encoding diagnostic lost the original cause")
	}
	var marshalerError *json.MarshalerError
	if !errors.As(err, &marshalerError) {
		t.Error("request encoding diagnostic lost the JSON wrapper")
	}

	client.baseURL = "://" + poison
	err = client.do(context.Background(), http.MethodGet, "/open/v1/project", nil, &[]Project{})
	if err == nil || strings.Contains(err.Error(), poison) {
		t.Fatal("request construction diagnostic exposed a fixture")
	}
	var urlError *url.Error
	if !errors.As(err, &urlError) {
		t.Error("request construction diagnostic lost the URL error")
	}
}

func TestCompleteTaskFailureUsesTheCompletionRequestCategory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := NewClient("token", "0.0.0-test", WithBaseURL(server.URL), WithMaxRetries(1))

	err := client.CompleteTask(context.Background(), "p1", "t1")
	if err == nil {
		t.Fatal("completion request unexpectedly succeeded")
	}
	if got, want := err.Error(), "ticktick: POST task completion request: http 400"; got != want {
		t.Error("completion failure did not retain its fixed request category")
	}
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Error("completion failure lost its status classification")
	}
	var typed *StatusError
	if !errors.As(err, &typed) || typed.StatusCode != http.StatusBadRequest {
		t.Error("completion failure lost its typed status metadata")
	}
}

func TestGetProjectDataRawFailuresUseSafeDiagnostics(t *testing.T) {
	token := "cred<project/+fixture="
	forms := credentialFixtureForms(token)

	t.Run("missing task list", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"project":{"name":"x"}}`))
		}))
		defer server.Close()
		client := NewClient(token, "0.0.0-test", WithBaseURL(server.URL), WithMaxRetries(1))
		_, err := client.GetProjectDataRaw(context.Background(), token)
		if !errors.Is(err, ErrIncompleteAnswer) {
			t.Fatal("missing task list lost the incomplete-answer classification")
		}
		var typed *DecodeError
		if !errors.As(err, &typed) {
			t.Fatal("missing task list did not return a typed decode error")
		}
		assertCredentialFormsAbsent(t, err.Error(), forms)
	})

	t.Run("malformed task", func(t *testing.T) {
		body := `{"tasks":[{"id":{"credential":"` + token + `"},"title":"` +
			forms[2] + `|` + forms[3] + `"}],"columns":[]}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()
		client := NewClient(token, "0.0.0-test", WithBaseURL(server.URL), WithMaxRetries(1))
		_, err := client.GetProjectDataRaw(context.Background(), "p1")
		var typed *DecodeError
		if !errors.As(err, &typed) {
			t.Fatal("malformed task did not return a typed decode error")
		}
		assertCredentialFormsAbsent(t, err.Error(), forms)
		if strings.Contains(typed.Body, token) {
			t.Error("manual task decode body retained the sent bearer")
		}
		if !strings.Contains(typed.Body, forms[2]) || !strings.Contains(typed.Body, forms[3]) {
			t.Error("manual task decode body did not retain the raw encoded fixture forms")
		}
	})
}

func TestDiagnosticBodyRedactsBeforeTakingTheBytePrefix(t *testing.T) {
	token := "cred<cut/+fixture="
	body := append([]byte(strings.Repeat("x", maxErrorBodyLen-4)), []byte(token+"tail")...)
	wantBytes := bytes.ReplaceAll(body, []byte(token), []byte("[REDACTED]"))
	wantBytes = wantBytes[:maxErrorBodyLen]
	got := diagnosticBody(body, token)
	if !bytes.Equal([]byte(got), wantBytes) {
		t.Error("diagnostic body did not redact before taking its byte prefix")
	}
	if strings.Contains(got, token) {
		t.Error("bounded diagnostic body retained the sent bearer")
	}
}
