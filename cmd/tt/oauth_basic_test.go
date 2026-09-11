package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assertOAuthClientAuth(t *testing.T, r *http.Request, clientID, clientSecret string) {
	t.Helper()
	id, secret, ok := r.BasicAuth()
	if !ok || id != clientID || secret != clientSecret || len(r.Header.Values("Authorization")) != 1 {
		t.Error("token request must authenticate the expected client with one Basic Auth header")
	}
	if err := r.ParseForm(); err != nil {
		t.Error("token request form could not be parsed")
		return
	}
	for _, key := range []string{"client_id", "client_secret"} {
		if r.PostForm.Has(key) || r.URL.Query().Has(key) {
			t.Error("token request must not put client credentials in the form or URL")
		}
	}
}

func TestExchangeCodeBasicAuthRequest(t *testing.T) {
	for _, suffix := range []string{"", "+ &=%/"} {
		t.Run(map[bool]string{true: "plain", false: "reserved_characters"}[suffix == ""], func(t *testing.T) {
			clientID := "client-id" + suffix
			clientSecret := oauthCredentialFixture('N') + ":" + suffix
			code := "code+with &reserved=%characters"
			redirectURI := "http://127.0.0.1:9977/callback"
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" || r.URL.RawQuery != "" {
					t.Error("token exchange must POST to the fixed token path without a query")
				}
				if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
					t.Error("token exchange must send a form and accept JSON")
				}
				assertOAuthClientAuth(t, r, clientID, clientSecret)
				want := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}}
				if r.PostForm.Encode() != want.Encode() {
					t.Error("token form must contain exactly the unchanged grant, code and redirect URI")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"access_token":"synthetic-access-token"}`)
			}))
			withOAuthURLs(t, srv)
			tr, err := exchangeCode(context.Background(), clientID, clientSecret, redirectURI, code)
			if err != nil || tr.AccessToken != "synthetic-access-token" || calls.Load() != 1 {
				t.Fatal("authorization code exchange did not return the single token response")
			}
		})
	}
}

func TestExchangeCodeOmitsEchoedBasicAuth(t *testing.T) {
	for _, kind := range []string{"unknown_error", "known_error", "malformed_success", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			clientID, clientSecret := oauthCredentialFixture('O'), oauthCredentialFixture('P')
			encoded := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
			auth := "Basic " + encoded
			forms := append(oauthCredentialForms(clientID), oauthCredentialForms(clientSecret)...)
			forms = append(forms, oauthCredentialForms(auth)...)
			forms = append(forms, encoded)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 {
					t.Error("token redirect must not be followed")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assertOAuthClientAuth(t, r, clientID, clientSecret)
				echo := r.Header.Get("Authorization")
				switch kind {
				case "malformed_success":
					io.WriteString(w, "{"+echo)
				case "redirect":
					w.Header().Set("Location", "/collect?echo="+url.QueryEscape(echo))
					w.WriteHeader(http.StatusPermanentRedirect)
				default:
					w.WriteHeader(http.StatusBadRequest)
					errorCode := echo
					if kind == "known_error" {
						errorCode = "invalid_client"
					}
					json.NewEncoder(w).Encode(map[string]string{"error": errorCode, "error_description": echo})
				}
			}))
			withOAuthURLs(t, srv)
			_, err := exchangeCode(context.Background(), clientID, clientSecret, "http://localhost/callback", "synthetic-code")
			if err == nil {
				t.Fatal("credential-bearing failure was accepted")
			}
			want := map[string]string{
				"unknown_error":     "token request failed: http 400",
				"known_error":       "token request failed: http 400: oauth invalid_client",
				"malformed_success": "token endpoint: response decoding failed",
				"redirect":          "token endpoint: redirect refused",
			}[kind]
			if err.Error() != want {
				t.Error("token failure did not retain its fixed diagnostic")
			}
			assertCredentialMaterialAbsent(t, err.Error(), forms...)
		})
	}
}

func TestExchangeCodeRefusesEveryRedirect(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, destination := range []string{"same_host", "different_port"} {
			t.Run(strconv.Itoa(status)+"/"+destination, func(t *testing.T) {
				var targetCalls atomic.Int32
				targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetCalls.Add(1)
					io.WriteString(w, `{"access_token":"unexpected-token"}`)
				})
				target := httptest.NewServer(targetHandler)
				defer target.Close()
				location := "/collect"
				if destination == "different_port" {
					location = target.URL + location
				}
				mux := http.NewServeMux()
				mux.Handle("/collect", targetHandler)
				mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
					assertOAuthClientAuth(t, r, "synthetic-id", "synthetic-secret")
					w.Header().Set("Location", location)
					w.WriteHeader(status)
				})
				withOAuthURLs(t, httptest.NewServer(mux))
				_, err := exchangeCode(context.Background(), "synthetic-id", "synthetic-secret", "http://localhost/callback", "synthetic-code")
				if !errors.Is(err, errTokenRedirect) || targetCalls.Load() != 0 {
					t.Fatal("token redirect was not refused before contacting the target")
				}
			})
		}
	}
}

func TestExchangeCodeContextBounds(t *testing.T) {
	for _, kind := range []string{"default", "shorter_parent", "canceled", "expired"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var wantErr error
			var wantCategory string
			var cancel context.CancelFunc
			switch kind {
			case "shorter_parent":
				ctx, cancel = context.WithTimeout(ctx, time.Second)
			case "canceled":
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantErr, wantCategory = context.Canceled, "token endpoint: request canceled"
			case "expired":
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				wantErr, wantCategory = context.DeadlineExceeded, "token endpoint: request timed out"
			}
			if cancel != nil {
				defer cancel()
			}
			previousTransport, previousURL := http.DefaultTransport, oauthTokenURL
			oauthTokenURL = "https://example.invalid/token"
			started := time.Now()
			http.DefaultTransport = oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || deadline.After(time.Now().Add(oauthHTTPTimeout)) {
					t.Error("token exchange must carry its HTTP timeout")
				}
				if parent, ok := ctx.Deadline(); ok && !deadline.Equal(parent) {
					t.Error("token exchange changed the shorter parent deadline")
				}
				if kind == "default" && deadline.Before(started.Add(oauthHTTPTimeout)) {
					t.Error("token exchange shortened the default HTTP timeout")
				}
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-token"}`))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport, oauthTokenURL = previousTransport, previousURL })
			_, err := exchangeCode(ctx, "synthetic-id", "synthetic-secret", "http://localhost/callback", "synthetic-code")
			if !errors.Is(err, wantErr) || (wantErr != nil && err.Error() != wantCategory) {
				t.Fatal("token exchange lost cancellation identity or its fixed diagnostic")
			}
		})
	}
}
