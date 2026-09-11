package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestAuthTextBoundsAndEscapes(t *testing.T) {
	var output bytes.Buffer
	inv := &invocation{ctx: context.Background(), verb: "auth", stdout: &output, stderr: io.Discard}
	if code := authText(inv, []string{"Requested scopes: " + strings.Repeat("a", 250), "foreign\n\x1b[31m\ttext"}); code != exitOK {
		t.Fatal(code)
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\t") || !strings.Contains(output.String(), `\n`) {
		t.Fatalf("raw controls or missing escaped newline: %q", output.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		if cli.DisplayWidth(line) > cli.Width {
			t.Fatalf("auth line too wide: %q", line)
		}
	}
}

func TestAuthLegacyDataWithoutSavedBearerCannotAdoptEnvironmentOrLogin(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	seedProjects(t, model.Project{Id: "legacy-project", Name: "Legacy", Kind: "TASK"})
	t.Setenv(tokenEnvVar, "unknown-environment-bearer")
	if _, err := managedCredential(ctx, "unknown-environment-bearer"); !errors.Is(err, store.ErrUnboundCredentialData) {
		t.Fatalf("environment adopted legacy cache: %v", err)
	}
	if err := saveLoginCredential(ctx, "new-login-bearer", nil); !errors.Is(err, store.ErrUnboundCredentialData) {
		t.Fatalf("login adopted legacy cache: %v", err)
	}
	if err := logoutCredential(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	binding, ok, err := st.AuthBinding(ctx)
	if err != nil || !ok || !binding.SignedOut || binding.Fingerprint != "" {
		t.Fatalf("logout invented identity: %+v %v", binding, err)
	}
	if err := saveLoginCredential(ctx, "unknown-environment-bearer", nil); !errors.Is(err, store.ErrUnboundCredentialData) {
		t.Fatalf("logout allowed bypass: %v", err)
	}
	status, _, err := readAuthStatus(ctx)
	if err != nil || status.State != "signed_out" {
		t.Fatalf("local status failed: %+v %v", status, err)
	}
	projects, err := st.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("legacy cache changed: %+v %v", projects, err)
	}
}

func TestAuthStatusIsLocalAndDoesNotCreateCache(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"auth", "status", "--json"}, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("status: %d %s", code, stderr.String())
	}
	var result struct {
		Data authStatus `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.State != "missing" || result.Data.Checked || result.Data.Scopes.ReturnedKnown {
		t.Fatalf("unexpected local status: %+v", result.Data)
	}
	entries, err := os.ReadDir(os.Getenv("XDG_DATA_HOME"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("status wrote local data: %v %v", entries, err)
	}
}

type authTransportFunc func(*http.Request) (*http.Response, error)

func (transport authTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestAuthStatusCheckMakesOneReadWithoutInventingScopes(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	if err := saveLoginCredential(ctx, "synthetic-auth-token", nil); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	requests := 0
	http.DefaultTransport = authTransportFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/open/v1/project" {
			t.Error("auth check made a non-read request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[]`)), Request: request}, nil
	})
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"auth", "status", "--check", "--json"}, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("check: %d %s", code, stdout.String()+stderr.String())
	}
	var result struct {
		Data authStatus `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !result.Data.Checked || result.Data.Check != "read_accepted" || result.Data.Scopes.ReturnedKnown || result.Data.Identity != "unknown" {
		t.Fatalf("check claimed unproven capabilities: %+v requests=%d", result.Data, requests)
	}
	if strings.Contains(stdout.String()+stderr.String(), "synthetic-auth-token") {
		t.Fatal("status exposed credential")
	}
}

func TestAuthLogoutPreservesLocalWorkAndBlocksEnvironment(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	const token = "synthetic-auth-token"
	if err := saveLoginCredential(ctx, token, &appCredentials{ClientID: "own-id", ClientSecret: "own-secret", RefreshToken: "refresh-secret", Method: "oauth_legacy", RequestedScopes: []string{"tasks:read", "tasks:write"}, ReturnedScopes: []string{"tasks:read"}, ScopesKnown: true}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "offline task"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	timer, err := st.StartTimer(ctx, store.TimerStartOptions{FocusType: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := readAuthStatus(ctx)
	if err != nil || !status.Scopes.ReturnedKnown || len(status.Scopes.Returned) != 1 || status.Identity != "unknown" {
		t.Fatalf("scope status: %+v %v", status, err)
	}
	if err := logoutCredential(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LoadToken(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token remains: %v", err)
	}
	credentials, _, err := loadCredentials()
	if err != nil || credentials.RefreshToken != "" || credentials.ClientID != "own-id" {
		t.Fatalf("credentials after logout: %+v %v", credentials, err)
	}
	if _, err := st.Task(ctx, task.Id); err != nil {
		t.Fatal("logout removed task")
	}
	after, err := st.OutboxCounts(ctx)
	if err != nil || before != after {
		t.Fatalf("queue changed: %+v %+v %v", before, after, err)
	}
	active, err := st.ReadTimer(ctx, now)
	if err != nil || active.State == nil || active.State.SessionID != timer.State.SessionID {
		t.Fatalf("timer changed: %+v %v", active, err)
	}
	t.Setenv(tokenEnvVar, token)
	if _, err := syncToken(); !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("environment bypassed logout: %v", err)
	}
	if result, token := checkToken(); result.status != statusFail || token.value != "" {
		t.Fatal("doctor accepted signed-out credential")
	}
	if err := saveLoginCredential(ctx, "different-token", nil); !errors.Is(err, store.ErrCredentialChanged) {
		t.Fatalf("different login accepted: %v", err)
	}
	if err := saveLoginCredential(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := syncToken(); err != nil || got != token {
		t.Fatalf("same-token resume: %v", err)
	}
}

func TestManagedClientRejectsStaleGenerationAndTokenReplacement(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	const token = "synthetic-auth-token"
	if err := saveLoginCredential(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); io.WriteString(w, `[]`) }))
	defer server.Close()
	client := managedAPIClient(token, api.WithBaseURL(server.URL))
	if err := logoutCredential(ctx); err != nil {
		t.Fatal(err)
	}
	if err := saveLoginCredential(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	_, err := client.ListProjects(ctx)
	var blocked *api.RequestGuardError
	if !errors.As(err, &blocked) || requests.Load() != 0 {
		t.Fatalf("old generation sent a request: %v %d", err, requests.Load())
	}
	client = managedAPIClient(token, api.WithBaseURL(server.URL))
	t.Setenv(tokenEnvVar, "different-token")
	if _, err := client.ListProjects(ctx); !errors.As(err, &blocked) || requests.Load() != 0 {
		t.Fatalf("replaced credential sent: %v", err)
	}
}

func TestAuthLogoutWaitsForInFlightRequest(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const token = "synthetic-auth-token"
	if err := saveLoginCredential(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, `{"id":"created"}`)
	}))
	defer server.Close()
	client := managedAPIClient(token, api.WithBaseURL(server.URL))
	requestDone := make(chan error, 1)
	go func() { _, err := client.CreateProject(ctx, api.Project{Name: "project"}); requestDone <- err }()
	select {
	case <-started:
	case err := <-requestDone:
		t.Fatalf("request did not start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	logoutDone := make(chan error, 1)
	go func() { logoutDone <- logoutCredential(ctx) }()
	select {
	case err := <-logoutDone:
		close(release)
		t.Fatalf("logout passed in-flight request: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-logoutDone; err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListProjects(ctx); err == nil {
		t.Fatal("old client remained authorized")
	}
}

func TestPKCEOwnClientWireAndScopeEvidence(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got := pkceChallenge(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge = %s", got)
	}
	first, challenge, err := pkceVerifier()
	if err != nil || len(first) != 43 || len(challenge) != 43 {
		t.Fatalf("verifier generation: %v", err)
	}
	second, _, _ := pkceVerifier()
	if first == second {
		t.Fatal("verifier repeated")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "" || r.Form.Get("client_secret") != "" || r.Form.Get("client_id") != "own-id" || r.Form.Get("code_verifier") != verifier || r.Form.Get("grant_type") != "authorization_code" {
			t.Error("wrong public-client token exchange")
		}
		io.WriteString(w, `{"access_token":"synthetic-token","scope":"tasks:read"}`)
	}))
	withOAuthURLs(t, server)
	response, err := exchangePKCECode(context.Background(), "own-id", "http://127.0.0.1:9977/callback", "code", verifier)
	if err != nil || response.Scope != "tasks:read" || !response.ScopeReturned {
		t.Fatalf("response scopes: %+v %v", response, err)
	}
	parsed, err := url.Parse(buildPKCEAuthorizeURL("own-id", "http://127.0.0.1:9977/callback", "state", challenge))
	if err != nil || parsed.Query().Get("client_id") != "own-id" || parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("code_challenge") != challenge || strings.Contains(parsed.String(), verifier) {
		t.Fatalf("authorize URL: %v", err)
	}
	for _, raw := range []string{`{"access_token":"token"}`, `{"access_token":"token","scope":null}`} {
		var response tokenResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil || response.ScopeReturned {
			t.Fatalf("invented returned scopes: %+v %v", response, err)
		}
	}
}

func TestPKCELoginDoesNotReadOrPersistClientSecretOrVerifier(t *testing.T) {
	isolate(t)
	port := freePort(t)
	path, _ := credentialsPath()
	if err := writeSecretFile(path, "invalid existing credentials"); err != nil {
		t.Fatal(err)
	}
	verifierCh := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		verifierCh <- r.Form.Get("code_verifier")
		io.WriteString(w, `{"access_token":"pkce-token","scope":"tasks:read tasks:write"}`)
	}))
	withOAuthURLs(t, server)
	oldBrowser := openBrowser
	t.Cleanup(func() { openBrowser = oldBrowser })
	openBrowser = func(raw string) error {
		authorize, _ := url.Parse(raw)
		go func() {
			callback := authorize.Query().Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(authorize.Query().Get("state"))
			response, err := http.Get(callback)
			if err == nil {
				response.Body.Close()
			}
		}()
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := loginPKCE(context.Background(), &stdout, &stderr, "my-own-public-id", port); code != exitOK {
		t.Fatalf("login: %d %s", code, stderr.String())
	}
	credentials, _, err := loadCredentials()
	if err != nil || credentials.ClientID != "my-own-public-id" || credentials.ClientSecret != "" || credentials.Method != "oauth_pkce" {
		t.Fatalf("PKCE credentials: %+v %v", credentials, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verifier := <-verifierCh
	if verifier == "" || strings.Contains(string(data)+stdout.String()+stderr.String(), verifier) {
		t.Fatal("verifier absent or exposed")
	}
}

func TestPKCERejectsInvalidOptionsAndTokenRedirects(t *testing.T) {
	for _, args := range [][]string{{"--pkce"}, {"--pkce", "--client-id", ""}, {"--pkce", "--client-id", "own", "--legacy"}, {"--pkce", "--client-id", "own", "--port", "0"}, {"--pkce", "--client-id", "own", "--pkce"}} {
		if _, _, err := parsePKCELogin(args); err == nil {
			t.Fatalf("accepted options %v", args)
		}
	}
	var redirects atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	withOAuthURLs(t, server)
	_, err := exchangePKCECode(context.Background(), "own-id", "http://127.0.0.1:9977/callback", "private-code", "private-verifier")
	if err == nil || redirects.Load() != 0 || strings.Contains(err.Error(), "private-") {
		t.Fatalf("redirect or diagnostic: %v %d", err, redirects.Load())
	}
}

func TestAuthLockRejectsSymlinkWithoutTouchingTarget(t *testing.T) {
	isolate(t)
	path, _ := api.TokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".auth-lock"); err != nil {
		t.Fatal(err)
	}
	if lock, err := acquireAuthLock(context.Background(), path); err == nil {
		lock.Release()
		t.Fatal("accepted symlink lock")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("lock target changed: %v", err)
	}
}

func TestPKCECallbackRejectsWrongStateMethodAndHost(t *testing.T) {
	for _, test := range []struct{ name, method, host, query string }{
		{"state", http.MethodGet, "", "state=wrong&code=private-code"},
		{"duplicate-state", http.MethodGet, "", "state=expected&state=expected&code=private-code"},
		{"method", http.MethodPost, "", "state=expected&code=private-code"},
		{"host", http.MethodGet, "other.invalid", "state=expected&code=private-code"},
		{"error-state", http.MethodGet, "", "error=access_denied&state=wrong"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() { _, err := runPKCECallback(ctx, listener, "expected"); finished <- err }()
			request, _ := http.NewRequest(test.method, "http://"+listener.Addr().String()+"/callback?"+test.query, nil)
			if test.host != "" {
				request.Host = test.host
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if err := <-finished; err == nil || strings.Contains(err.Error(), "private-code") {
				t.Fatalf("callback = %v", err)
			}
		})
	}
}

func TestAuthExistingCacheRejectsCredentialChangeBeforeFileWrite(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path, _ := api.TokenPath()
	if err := writeSecretFile(path, "original-token"); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "queued"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := saveLoginCredential(ctx, "different-token", nil); !errors.Is(err, store.ErrCredentialChanged) {
		t.Fatalf("changed login: %v", err)
	}
	token, err := api.LoadToken()
	if err != nil || token.Value != "original-token" {
		t.Fatalf("original credential replaced: %v", err)
	}
}
