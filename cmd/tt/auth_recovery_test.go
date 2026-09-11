package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

func TestOpenRecoveryRequiresConsentAndSuccessfulReadBeforeReplacement(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	if err := saveLoginCredential(ctx, "old-token", nil); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	started, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StopTimer(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadFocusTarget(ctx, started.State.SessionID)
	oldClient := managedAPIClient("old-token", api.WithMaxRetries(1))
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	requests, status := 0, http.StatusUnauthorized
	http.DefaultTransport = authTransportFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/open/v1/project" || r.Header.Get("Authorization") != "Bearer new-token" {
			t.Fatal("unexpected recovery request")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]")), Request: r}, nil
	})
	if err := saveLoginCredential(ctx, "new-token", nil); !errors.Is(err, store.ErrCredentialChanged) || requests != 0 {
		t.Fatalf("replaced without consent: %v", err)
	}
	recovery := context.WithValue(ctx, sameAccountRecoveryKey{}, true)
	if err := saveLoginCredential(recovery, "new-token", nil); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("invalid replacement: %v", err)
	}
	loaded, _ := api.LoadToken()
	if loaded.Value != "old-token" {
		t.Fatal("failed validation replaced token")
	}
	status = http.StatusOK
	if err := saveLoginCredential(recovery, "new-token", nil); err != nil {
		t.Fatal(err)
	}
	loaded, _ = api.LoadToken()
	if loaded.Value != "new-token" {
		t.Fatal("replacement not saved")
	}
	after, _ := st.ReadFocusTarget(ctx, started.State.SessionID)
	if after.Version != before.Version {
		t.Fatal("recovery changed queued focus")
	}
	if _, err := oldClient.ListProjects(ctx); err == nil || requests != 2 {
		t.Fatalf("stale client sent a request: %v; calls=%d", err, requests)
	}
	path, _ := api.TokenPath()
	info, _ := api.ReadRegularFile(path)
	if info.Mode.Perm() != 0o600 {
		t.Fatal("replacement permissions")
	}
}

func TestAuthorizationAdviceIdentifiesChannelWithoutInventingExpiry(t *testing.T) {
	isolate(t)
	unauthorized := &api.StatusError{StatusCode: 401, Err: api.ErrUnauthorized}
	advised := openAuthAdvice(unauthorized)
	if !errors.Is(advised, api.ErrUnauthorized) || !strings.Contains(advised.Error(), "Open API") || !strings.Contains(advised.Error(), "tt login token --same-account") {
		t.Fatal(advised)
	}
	if !strings.Contains(browserAuthAdvice(&webapi.StatusError{StatusCode: 401}).Error(), "tt login web --same-account") {
		t.Fatal("Browser recovery command missing")
	}
	for _, advice := range []func(error) error{openAuthAdvice, browserAuthAdvice} {
		network := errors.New("connection unavailable")
		if advice(network) != network {
			t.Fatal("network error misclassified")
		}
	}
	if text := openAuthAdvice(&api.StatusError{StatusCode: 403}).Error(); !strings.Contains(text, "permissions") || strings.Contains(text, "may have expired") {
		t.Fatal(text)
	}
	if text := browserAuthAdvice(&webapi.StatusError{StatusCode: 403}).Error(); !strings.Contains(text, "does not prove") {
		t.Fatal(text)
	}
	if err := saveCredentials(appCredentials{Method: "oauth_legacy", RedirectURI: "http://127.0.0.1:8080/callback"}); err != nil {
		t.Fatal(err)
	}
	if text := openAuthAdvice(unauthorized).Error(); !strings.Contains(text, "tt login --same-account --port 8080") {
		t.Fatal(text)
	}
	if err := saveCredentials(appCredentials{Method: "oauth_pkce", ClientID: "public$(unsafe)'id", RedirectURI: "http://127.0.0.1:8081/callback"}); err != nil {
		t.Fatal(err)
	}
	if text := openAuthAdvice(unauthorized).Error(); !strings.Contains(text, "--pkce --client-id 'public$(unsafe)'\"'\"'id' --same-account --port 8081") {
		t.Fatal(text)
	}
	t.Setenv(tokenEnvVar, "private-env-token")
	if text := openAuthAdvice(unauthorized).Error(); !strings.Contains(text, "Unset TT_TOKEN") || strings.Contains(text, "private-env-token") {
		t.Fatal(text)
	}
}

func TestAuthorizationAdviceRemainsReadableAndComplete(t *testing.T) {
	isolate(t)
	err := browserAuthAdvice(&webapi.StatusError{StatusCode: 401, Path: "/api/v2/timer\n\x1b[31m"})
	var output bytes.Buffer
	if !writeAuthorizationAdvice(&output, "tt: timer: ", err) {
		t.Fatal("advice not rendered")
	}
	text := output.String()
	if !strings.Contains(text, "tt login web --same-account") || strings.Contains(text, "\x1b") || strings.Contains(text, `\x20`) {
		t.Fatalf("unreadable or incomplete guidance: %q", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if cli.DisplayWidth(line) > cli.Width {
			t.Fatalf("advice too wide: %q", line)
		}
	}
}

func TestBrowserRecoveryPreservesSessionsAndFencesOldClient(t *testing.T) {
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
	credentials := webCredentials{Version: 1, Session: "browser-old", OpenFingerprint: credentialFingerprint("open-token"), BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
		t.Fatal(err)
	}
	oldClient, _, err := webClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credentials.Session = "browser-new"
	if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
		t.Fatal(err)
	}
	if allowed, err := st.BrowserCredentialAllows(ctx, credentialFingerprint("browser-old"), credentialFingerprint("browser-new")); err != nil || !allowed {
		t.Fatalf("continuity lost: %v %v", allowed, err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = authTransportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("old Browser client reached network")
		return nil, errors.New("unexpected request")
	})
	if _, err := oldClient.ListTopics(ctx); err == nil {
		t.Fatal("stale Browser client accepted")
	}
}

func TestBrowserRecoveryAfterInterruptedSecretSave(t *testing.T) {
	for _, action := range []string{"renew", "logout"} {
		t.Run(action, func(t *testing.T) {
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
			credentials := webCredentials{Version: 1, Session: "browser-old", OpenFingerprint: credentialFingerprint("open-token"), BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
				t.Fatal(err)
			}
			if err := st.RecoverBrowserCredential(ctx, credentialFingerprint("browser-old"), credentialFingerprint("browser-unsaved")); err != nil {
				t.Fatal(err)
			}
			if action == "logout" {
				path, err := webCredentialsPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := logoutWebCredential(ctx, path); err != nil {
					t.Fatal(err)
				}
				if _, exists, err := loadWebCredentials(); err != nil || exists {
					t.Fatalf("logout did not remove the interrupted credential: %v", err)
				}
				return
			}
			credentials.Session = "browser-next"
			if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
				t.Fatal(err)
			}
			if saved, exists, err := loadWebCredentials(); err != nil || !exists || saved != credentials {
				t.Fatalf("replacement not saved: %v", err)
			}
			if allowed, err := st.BrowserCredentialAllows(ctx, credentialFingerprint("browser-old"), credentialFingerprint("browser-next")); err != nil || !allowed {
				t.Fatalf("frozen work cannot use the confirmed replacement: %v %v", allowed, err)
			}
		})
	}
}

func TestSameAccountTokenLoginRoutesRecoveryWithoutSendingQueue(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	if err := saveLoginCredential(ctx, "original", nil); err != nil {
		t.Fatal(err)
	}
	previousTransport, previousAfterSave := http.DefaultTransport, loginAfterSave
	t.Cleanup(func() { http.DefaultTransport = previousTransport; loginAfterSave = previousAfterSave })
	reads := 0
	http.DefaultTransport = authTransportFunc(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != "GET" || r.URL.Path != "/open/v1/project" {
			t.Fatal("login sent a queued write")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]")), Request: r}, nil
	})
	loginAfterSave = func(ctx context.Context, token string, stdout, stderr io.Writer) int {
		if ctx.Value(sameAccountRecoveryKey{}) != true || token != "renewed" {
			t.Fatal("recovery option not forwarded")
		}
		return exitOK
	}
	var output, diagnostic bytes.Buffer
	code := run(ctx, []string{"login", "token", "--same-account"}, strings.NewReader("renewed\n"), &output, &diagnostic)
	if code != exitOK || reads != 1 {
		t.Fatalf("recovery login %d (%d reads): %s", code, reads, diagnostic.String())
	}
	if strings.Contains(output.String(), "renewed") {
		t.Fatal("replacement token printed")
	}
}
