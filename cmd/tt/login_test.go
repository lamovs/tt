package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestLoginTokenSavesFileWithCorrectPermissions(t *testing.T) {
	isolate(t)
	var pulledToken string
	loginAfterSave = func(_ context.Context, token string, _, _ io.Writer) int {
		pulledToken = token
		return exitOK
	}
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"login", "token"}, strings.NewReader("my-secret-token\n"), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick", "token")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != "my-secret-token" {
		t.Fatalf("token file = %q, want %q", data, "my-secret-token")
	}
	assertFileMode(t, path, 0o600)
	assertFileMode(t, filepath.Dir(path), 0o700)
	if pulledToken != "my-secret-token" {
		t.Fatal("login token did not start the initial pull with the saved token")
	}
}

func TestPullAfterLoginCachesRemoteTasksWithoutSendingTheQueue(t *testing.T) {
	isolate(t)
	writeTokenFile(t, stage1CCommandToken)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	localTask, err := st.CreateTask(ctx, model.Task{ProjectId: "command-project", Title: "offline task"})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()

	remote := newStage1CCommandRemote(t)
	var stdout, stderr bytes.Buffer
	code := pullAfterLogin(ctx, stage1CCommandToken, &stdout, &stderr,
		api.WithHTTPClient(&http.Client{Transport: remote}), api.WithMaxRetries(1))
	if code != exitOK {
		t.Fatalf("pullAfterLogin = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "logged in; pulled 1 task(s) from 1 list(s)") {
		t.Fatalf("stdout = %q, want a successful login and pull summary", stdout.String())
	}
	remote.mu.Lock()
	requests := slices.Clone(remote.requests)
	remote.mu.Unlock()
	for _, request := range requests {
		if strings.HasPrefix(request, http.MethodPost+" ") || strings.HasPrefix(request, http.MethodDelete+" ") {
			t.Fatalf("initial pull sent a queued mutation: %v", requests)
		}
	}

	st, err = store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Task(ctx, stage1CCommandTaskID); err != nil {
		t.Fatalf("remote task was not cached: %v", err)
	}
	if _, err := st.Task(ctx, localTask.Id); err != nil {
		t.Fatalf("queued local task was not preserved: %v", err)
	}
}

func TestPullAfterLoginKeepsTheTokenAndNamesAnIncompletePull(t *testing.T) {
	isolate(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := pullAfterLogin(context.Background(), "secret-token", &stdout, &stderr,
		api.WithBaseURL(server.URL), api.WithMaxRetries(1))
	if code != exitError {
		t.Fatalf("pullAfterLogin = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr.String(), "token was saved, but the initial pull was incomplete") {
		t.Fatalf("stderr = %q, want the saved-token recovery state", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "secret-token") {
		t.Fatal("pull failure printed the token")
	}
}

func TestLoginTokenTightensExistingPermissions(t *testing.T) {
	isolate(t)
	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick", "token")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old-token"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"login", "token"}, strings.NewReader("new-secret\n"), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != "new-secret" {
		t.Fatalf("token file = %q, want %q", data, "new-secret")
	}
	assertFileMode(t, path, 0o600)
	assertFileMode(t, filepath.Dir(path), 0o700)
}

func TestLoginTokenReplacesASymlinkAtTheTokenPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privileged account there")
	}
	isolate(t)
	const victimContent = "someone else's file"
	victim := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(victim, []byte(victimContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick", "token")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"login", "token"}, strings.NewReader("new-secret\n"), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != victimContent {
		t.Fatalf("the file the symlink pointed at = %q, want it untouched", data)
	}
	assertFileMode(t, victim, 0o644)

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the token path is still a symlink, so the secret went wherever it pointed")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-secret" {
		t.Fatalf("token file = %q, want %q", data, "new-secret")
	}
	assertFileMode(t, path, 0o600)
	assertFileMode(t, filepath.Dir(path), 0o700)
}

func TestWriteSecretFileKeepsThePreviousSecretWhole(t *testing.T) {
	isolate(t)
	dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick")
	path := filepath.Join(dir, "token")
	if err := writeSecretFile(path, "old-token"); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := writeSecretFile(path, "new-token"); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("the new secret was written into the file the old one was in, where an interrupted write would have destroyed it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-token" {
		t.Fatalf("token file = %q, want %q", data, "new-token")
	}
	assertFileMode(t, path, 0o600)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "token" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("%s holds %v, want the token file alone", dir, names)
	}
}

func TestLoginTokenRejectsEmptyInput(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"login", "token"}, strings.NewReader("\n"), &stdout, &stderr)
	if got != exitError {
		t.Fatalf("run() = %d, want %d", got, exitError)
	}
}

func TestLoginTokenWarnsWhenEnvOverrides(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "env-token")
	var stdout, stderr bytes.Buffer
	got := run(context.Background(), []string{"login", "token"}, strings.NewReader("file-token\n"), &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), tokenEnvVar) {
		t.Fatalf("stdout = %q, want a note about %s overriding the saved token", stdout.String(), tokenEnvVar)
	}
}

func TestLoginRejectsUnknownForm(t *testing.T) {
	isolate(t)
	cases := [][]string{
		{"login", "bogus"},
		{"login", "token", "extra"},
		{"login", "--bogus"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
		if got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, got, exitUsage)
		}
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %04o, want %04o", path, got, want)
	}
}

func TestPromptAppCredentials(t *testing.T) {
	var stdout bytes.Buffer
	stdin := strings.NewReader("my-client-id\nmy-client-secret\n")
	id, secret, err := promptAppCredentials(context.Background(), stdin, &stdout, io.Discard, "http://127.0.0.1:9977/callback")
	if err != nil {
		t.Fatalf("promptAppCredentials: %v", err)
	}
	if id != "my-client-id" || secret != "my-client-secret" {
		t.Fatalf("id=%q secret=%q, want my-client-id / my-client-secret", id, secret)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:9977/callback") {
		t.Fatalf("stdout = %q, want it to show the exact redirect URI", stdout.String())
	}
	if !strings.Contains(stdout.String(), oauthPortalURL) {
		t.Fatalf("stdout = %q, want it to mention the developer portal", stdout.String())
	}
}

func TestPromptAppCredentialsRejectsEmptyClientID(t *testing.T) {
	var stdout bytes.Buffer
	stdin := strings.NewReader("\nsecret\n")
	if _, _, err := promptAppCredentials(context.Background(), stdin, &stdout, io.Discard, "http://x/callback"); err == nil {
		t.Fatal("promptAppCredentials accepted an empty client id")
	}
}

func TestLoginOAuthEndToEnd(t *testing.T) {
	for _, withRefresh := range []bool{true, false} {
		t.Run(map[bool]string{true: "with_refresh_token", false: "without_refresh_token"}[withRefresh], func(t *testing.T) {
			isolate(t)
			var pulledToken string
			loginAfterSave = func(_ context.Context, token string, _, _ io.Writer) int {
				pulledToken = token
				return exitOK
			}

			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatalf("parse form: %v", err)
				}
				assertOAuthClientAuth(t, r, "my-client-id", "my-client-secret")
				if r.PostForm.Get("code") != "the-auth-code" {
					t.Errorf("code = %q, want the-auth-code", r.PostForm.Get("code"))
				}
				w.Header().Set("Content-Type", "application/json")
				if withRefresh {
					w.Write([]byte(`{"access_token":"final-access-token","refresh_token":"final-refresh-token"}`))
				} else {
					w.Write([]byte(`{"access_token":"final-access-token"}`))
				}
			}))
			withOAuthURLs(t, tokenSrv)

			urlCh := make(chan string, 1)
			oldOpen := openBrowser
			openBrowser = func(u string) error { urlCh <- u; return nil }
			t.Cleanup(func() { openBrowser = oldOpen })

			stdin := strings.NewReader("my-client-id\nmy-client-secret\n")
			var stdout, stderr bytes.Buffer
			exitCh := make(chan int, 1)
			go func() {
				exitCh <- loginOAuth(context.Background(), stdin, &stdout, &stderr, 0)
			}()

			authURL := <-urlCh
			parsed, err := url.Parse(authURL)
			if err != nil {
				t.Fatalf("parse authorize URL: %v", err)
			}
			q := parsed.Query()
			redirectURI := q.Get("redirect_uri")
			state := q.Get("state")
			if redirectURI == "" || state == "" {
				t.Fatalf("authorize URL missing redirect_uri or state: %s", authURL)
			}

			resp, err := http.Get(redirectURI + "?code=the-auth-code&state=" + state)
			if err != nil {
				t.Fatalf("simulated browser GET: %v", err)
			}
			resp.Body.Close()

			got := <-exitCh
			if got != exitOK {
				t.Fatalf("run() = %d, want %d\nstdout: %s\nstderr: %s", got, exitOK, stdout.String(), stderr.String())
			}

			tokenPath := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick", "token")
			tok, err := os.ReadFile(tokenPath)
			if err != nil {
				t.Fatalf("read token file: %v", err)
			}
			if string(tok) != "final-access-token" {
				t.Fatalf("token = %q, want final-access-token", tok)
			}
			if pulledToken != "final-access-token" {
				t.Fatal("OAuth login did not start the initial pull with the saved token")
			}
			assertFileMode(t, tokenPath, 0o600)

			creds, ok, err := loadCredentials()
			if err != nil || !ok {
				t.Fatalf("loadCredentials: ok=%v err=%v", ok, err)
			}
			if creds.ClientID != "my-client-id" || creds.ClientSecret != "my-client-secret" {
				t.Fatalf("creds = %+v, want our client id/secret saved", creds)
			}
			wantRefresh := ""
			if withRefresh {
				wantRefresh = "final-refresh-token"
			}
			if creds.RefreshToken != wantRefresh {
				t.Fatalf("RefreshToken = %q, want %q", creds.RefreshToken, wantRefresh)
			}
			if !withRefresh && !strings.Contains(stdout.String(), "did not return a refresh token") {
				t.Fatalf("stdout = %q, want a note that no refresh token was issued", stdout.String())
			}
		})
	}
}

func TestLoginOAuthRejectsWrongState(t *testing.T) {
	isolate(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the token endpoint must not be called when the callback state did not match")
	}))
	withOAuthURLs(t, tokenSrv)

	urlCh := make(chan string, 1)
	oldOpen := openBrowser
	openBrowser = func(u string) error { urlCh <- u; return nil }
	t.Cleanup(func() { openBrowser = oldOpen })

	stdin := strings.NewReader("cid\ncsecret\n")
	var stdout, stderr bytes.Buffer
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- loginOAuth(context.Background(), stdin, &stdout, &stderr, 0)
	}()

	authURL := <-urlCh
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")

	resp, err := http.Get(redirectURI + "?code=whatever&state=not-the-real-state")
	if err != nil {
		t.Fatalf("simulated browser GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if got := <-exitCh; got != exitError {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitError, stderr.String())
	}
	if _, ok, _ := loadCredentials(); ok {
		t.Fatal("credentials were saved despite a state mismatch")
	}
}

func TestLoginOAuthHandlesUserDenial(t *testing.T) {
	isolate(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the token endpoint must not be called when the user declined")
	}))
	withOAuthURLs(t, tokenSrv)

	urlCh := make(chan string, 1)
	oldOpen := openBrowser
	openBrowser = func(u string) error { urlCh <- u; return nil }
	t.Cleanup(func() { openBrowser = oldOpen })

	stdin := strings.NewReader("cid\ncsecret\n")
	var stdout, stderr bytes.Buffer
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- loginOAuth(context.Background(), stdin, &stdout, &stderr, 0)
	}()

	authURL := <-urlCh
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")
	state := parsed.Query().Get("state")

	resp, err := http.Get(redirectURI + "?error=access_denied&state=" + state)
	if err != nil {
		t.Fatalf("simulated browser GET: %v", err)
	}
	resp.Body.Close()

	if got := <-exitCh; got != exitError {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitError, stderr.String())
	}
}

func TestLoginOAuthReusesSavedCredentials(t *testing.T) {
	isolate(t)
	if err := saveCredentials(appCredentials{ClientID: "saved-id", ClientSecret: "saved-secret"}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		assertOAuthClientAuth(t, r, "saved-id", "saved-secret")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok"}`))
	}))
	withOAuthURLs(t, tokenSrv)

	urlCh := make(chan string, 1)
	oldOpen := openBrowser
	openBrowser = func(u string) error { urlCh <- u; return nil }
	t.Cleanup(func() { openBrowser = oldOpen })

	var stdout, stderr bytes.Buffer
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- loginOAuth(context.Background(), strings.NewReader(""), &stdout, &stderr, 0)
	}()

	authURL := <-urlCh
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")
	state := parsed.Query().Get("state")

	resp, err := http.Get(redirectURI + "?code=abc&state=" + state)
	if err != nil {
		t.Fatalf("simulated browser GET: %v", err)
	}
	resp.Body.Close()

	if got := <-exitCh; got != exitOK {
		t.Fatalf("run() = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}
}

func runLoginOAuth(t *testing.T, port int, stdin string, token http.HandlerFunc) (code int, stdout, stderr string) {
	t.Helper()
	withOAuthURLs(t, httptest.NewServer(token))

	urlCh := make(chan string, 1)
	oldOpen := openBrowser
	openBrowser = func(u string) error { urlCh <- u; return nil }
	t.Cleanup(func() { openBrowser = oldOpen })

	var out, errOut bytes.Buffer
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- loginOAuth(context.Background(), strings.NewReader(stdin), &out, &errOut, port)
	}()

	var authURL string
	select {
	case authURL = <-urlCh:
	case got := <-exitCh:
		t.Fatalf("loginOAuth = %d before it opened a browser\nstdout: %s\nstderr: %s", got, out.String(), errOut.String())
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := parsed.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "?code=the-auth-code&state=" + q.Get("state"))
	if err != nil {
		t.Fatalf("simulated browser GET: %v", err)
	}
	resp.Body.Close()

	return <-exitCh, out.String(), errOut.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestLoginOAuthKeepsARefreshTokenTheExchangeDidNotReturn(t *testing.T) {
	isolate(t)
	if err := saveCredentials(appCredentials{
		ClientID: "saved-id", ClientSecret: "saved-secret", RefreshToken: "first-refresh-token",
	}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	code, stdout, stderr := runLoginOAuth(t, 0, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"second-access-token"}`))
	})
	if code != exitOK {
		t.Fatalf("loginOAuth = %d, want %d\nstderr: %s", code, exitOK, stderr)
	}

	creds, ok, err := loadCredentials()
	if err != nil || !ok {
		t.Fatalf("loadCredentials: ok=%v err=%v", ok, err)
	}
	if creds.RefreshToken != "first-refresh-token" {
		t.Fatalf("RefreshToken = %q, want the one the first login saved", creds.RefreshToken)
	}

	if !strings.Contains(collapsed(stdout), "the one an earlier login saved is kept") {
		t.Fatalf("stdout = %q, want it to say the saved refresh token was kept", stdout)
	}
	if strings.Contains(stdout, `run "tt login" again`) {
		t.Fatalf("stdout = %q, want no advice to log in again: the refresh token is still there", stdout)
	}
}

func TestLoginOAuthCarriesARefreshTokenOnlyUnderItsOwnClientID(t *testing.T) {
	for _, c := range []struct {
		name        string
		savedID     string
		typedID     string
		wantRefresh string
	}{
		{"the same app, secret rotated", "app-id", "app-id", "first-refresh-token"},
		{"another app", "old-app-id", "new-app-id", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)

			if err := saveCredentials(appCredentials{ClientID: c.savedID, RefreshToken: "first-refresh-token"}); err != nil {
				t.Fatalf("saveCredentials: %v", err)
			}

			code, stdout, stderr := runLoginOAuth(t, 0, c.typedID+"\nnew-secret\n", func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
				}
				assertOAuthClientAuth(t, r, c.typedID, "new-secret")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"an-access-token"}`))
			})
			if code != exitOK {
				t.Fatalf("loginOAuth = %d, want %d\nstderr: %s", code, exitOK, stderr)
			}

			creds, _, err := loadCredentials()
			if err != nil {
				t.Fatalf("loadCredentials: %v", err)
			}
			if creds.ClientID != c.typedID {
				t.Fatalf("ClientID = %q, want %q", creds.ClientID, c.typedID)
			}
			if creds.RefreshToken != c.wantRefresh {
				t.Fatalf("RefreshToken = %q, want %q", creds.RefreshToken, c.wantRefresh)
			}

			wantDrop := c.wantRefresh == ""
			if got := strings.Contains(stdout, "was dropped"); got != wantDrop {
				t.Fatalf("stdout = %q, want it to report a dropped refresh token: %v", stdout, wantDrop)
			}
		})
	}
}

func TestLoginOAuthNotesARedirectURIChange(t *testing.T) {
	const note = "note: the last login used the redirect URI"
	for _, c := range []struct {
		name     string
		saved    func(port int) string
		wantNote bool
	}{
		{"nothing saved", func(int) string { return "" }, false},

		{"another port", func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/callback", port+1) }, true},
		{"the same one", func(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/callback", port) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			port := freePort(t)
			if err := saveCredentials(appCredentials{
				ClientID: "saved-id", ClientSecret: "saved-secret", RedirectURI: c.saved(port),
			}); err != nil {
				t.Fatalf("saveCredentials: %v", err)
			}

			code, stdout, stderr := runLoginOAuth(t, port, "", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"an-access-token"}`))
			})
			if code != exitOK {
				t.Fatalf("loginOAuth = %d, want %d\nstderr: %s", code, exitOK, stderr)
			}
			if got := strings.Contains(stdout, note); got != c.wantNote {
				t.Fatalf("stdout = %q, want a note about the redirect URI: %v", stdout, c.wantNote)
			}
		})
	}
}

func TestBrowserCommand(t *testing.T) {
	const authURL = "https://ticktick.com/oauth/authorize?client_id=cid&scope=tasks%3Aread+tasks%3Awrite&state=st4te"
	for _, c := range []struct {
		name string
		goos string
		wsl  bool
		url  string
		want []string
	}{
		{"macos", "darwin", false, authURL, []string{"open", authURL}},
		{"linux", "linux", false, authURL, []string{"xdg-open", authURL}},
		{"wsl", "linux", true, authURL,
			[]string{"powershell.exe", "-NoProfile", "-Command", "Start-Process '" + authURL + "'"}},

		{"wsl, a url with a quote in it", "linux", true, "https://example.test/?q='x'",
			[]string{"powershell.exe", "-NoProfile", "-Command", "Start-Process 'https://example.test/?q=''x'''"}},

		{"wsl, a url with smart quotes in it", "linux", true, "https://example.test/?q=\u2018x\u2019&r=\u201ay\u201b",
			[]string{"powershell.exe", "-NoProfile", "-Command",
				"Start-Process 'https://example.test/?q=\u2018\u2018x\u2019\u2019&r=\u201a\u201ay\u201b\u201b'"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd, err := browserCommand(c.goos, c.wsl, c.url)
			if err != nil {
				t.Fatalf("browserCommand: %v", err)
			}
			if !slices.Equal(cmd.Args, c.want) {
				t.Fatalf("args = %q, want %q", cmd.Args, c.want)
			}
		})
	}
	if _, err := browserCommand("plan9", false, authURL); err == nil {
		t.Fatal("browserCommand on a system with no known way to open a browser returned no error")
	}
}

func TestRestoreEchoOnSignalCoversEveryWayOutOfThePrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("none of these signals is delivered to a process there")
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	// Every signal tt ends a run on, the ones it was not started with ignored.
	for _, s := range ttSignals() {
		sig := s.(syscall.Signal)
		ctx, stopSignals := signal.NotifyContext(context.Background(), ttSignals()...)
		exited := make(chan int, 1)
		stop := restoreEchoOnSignal(ctx, &echoSwitch{f: f}, io.Discard, func(code int) { exited <- code })
		if err := self.Signal(sig); err != nil {
			stop()
			stopSignals()
			t.Fatalf("send %v: %v", sig, err)
		}
		select {
		case got := <-exited:
			if got != exitInterrupted {
				t.Errorf("%v exits with %d, want %d", sig, got, exitInterrupted)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%v never reached the watch", sig)
		}
		stop()
		stopSignals()
	}
}

func TestRestoreEchoOnSignalLeavesTheRootSignalsToTheRootWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("none of these signals is delivered to a process there")
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	for _, s := range ttSignals() {
		sig := s.(syscall.Signal)
		ctx, stopSignals := signal.NotifyContext(context.Background(), ttSignals()...)
		exited := make(chan int, 1)
		stop := restoreEchoOnSignal(context.Background(), &echoSwitch{f: f}, io.Discard, func(code int) { exited <- code })

		if err := self.Signal(sig); err != nil {
			stop()
			stopSignals()
			t.Fatalf("send %v: %v", sig, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%v never reached the root watch, so this proves nothing about the prompt watch", sig)
		}

		select {
		case code := <-exited:
			t.Errorf("the prompt watch fired on %v with %d; it is registered for a signal ttMain already owns", sig, code)
		case <-time.After(200 * time.Millisecond):
		}
		stop()
		stopSignals()
	}
}

func TestRestoreEchoOnSignalSaysWhyItIsLeaving(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the signal is not delivered to a process there")
	}
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var stderr bytes.Buffer
	exited := make(chan int, 1)
	stop := restoreEchoOnSignal(ctx, &echoSwitch{f: f}, &stderr, func(code int) { exited <- code })
	defer stop()

	if err := self.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	select {
	case got := <-exited:
		if got != exitInterrupted {
			t.Errorf("exits with %d, want %d", got, exitInterrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT never reached the watch")
	}

	got := stderr.String()
	if !strings.Contains(got, "tt: interrupted") {
		t.Errorf("stderr = %q, want the same line every other interrupt prints", got)
	}

	if !strings.HasPrefix(got, "\n") {
		t.Errorf("stderr = %q, want it to close the prompt's line first", got)
	}
	if strings.Contains(got, "queue") {
		t.Errorf("stderr = %q, want nothing about a queue: an unanswered prompt leaves no work behind", got)
	}
}

type echoCall struct {
	on      bool
	pending pendingInput
}

func (c echoCall) String() string {
	state, queue := "off", "keepInput"
	if c.on {
		state = "on"
	}
	if c.pending == discardInput {
		queue = "discardInput"
	}
	return "echo " + state + "/" + queue
}

func recordEcho(t *testing.T) *[]echoCall {
	t.Helper()
	var calls []echoCall
	saved := toggleEcho
	toggleEcho = func(_ *os.File, on bool, pending pendingInput) error {
		calls = append(calls, echoCall{on: on, pending: pending})
		return nil
	}
	t.Cleanup(func() { toggleEcho = saved })
	return &calls
}

func refuseEchoOff(t *testing.T) error {
	t.Helper()
	refused := errors.New("inappropriate ioctl for device")
	saved := toggleEcho
	toggleEcho = func(_ *os.File, on bool, _ pendingInput) error {
		if on {
			return nil
		}
		return refused
	}
	t.Cleanup(func() { toggleEcho = saved })
	return refused
}

func charDevice(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestEchoSwitchAlwaysEndsWithEchoOn(t *testing.T) {
	t.Run("an off after a restore never reaches the terminal", func(t *testing.T) {
		calls := recordEcho(t)
		e := &echoSwitch{}
		e.restore()

		if err := e.off(); err != nil {
			t.Errorf("off() = %v, want no complaint about a terminal that is already echoing", err)
		}
		want := []echoCall{{on: true, pending: keepInput}}
		if !slices.Equal(*calls, want) {
			t.Fatalf("calls = %v, want %v: echo given back is given back for good", *calls, want)
		}
	})

	t.Run("the ordinary way through a prompt", func(t *testing.T) {
		calls := recordEcho(t)
		e := &echoSwitch{}
		e.off()
		e.restore()
		want := []echoCall{{on: false, pending: keepInput}, {on: true, pending: keepInput}}
		if !slices.Equal(*calls, want) {
			t.Fatalf("calls = %v, want %v", *calls, want)
		}
	})

	t.Run("a signal gives echo back and empties the queue with it", func(t *testing.T) {
		calls := recordEcho(t)
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan int, 1)
		stop := restoreEchoOnSignal(ctx, &echoSwitch{}, io.Discard, func(code int) { exited <- code })
		defer stop()

		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("the cancelled context never reached the watch")
		}
		want := []echoCall{{on: true, pending: keepInput}, {on: true, pending: discardInput}}
		if !slices.Equal(*calls, want) {
			t.Fatalf("calls = %v, want %v: what was typed and never read must not outlive tt, and echo must be back before anything that can block on the terminal", *calls, want)
		}
	})

	t.Run("neither order matters when the two race", func(t *testing.T) {
		calls := recordEcho(t)
		for round := range 200 {
			*calls = (*calls)[:0]
			e := &echoSwitch{}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); e.off() }()
			go func() { defer wg.Done(); e.restore() }()
			wg.Wait()
			got := *calls
			if len(got) == 0 || !got[len(got)-1].on {
				t.Fatalf("round %d: calls = %v, want the last of them to turn echo on", round, got)
			}
		}
	})
}

func TestSecretPromptRefusesATerminalThatWillNotHideInput(t *testing.T) {
	t.Run("the secret is left unread", func(t *testing.T) {
		refused := refuseEchoOff(t)

		br := bufio.NewReader(strings.NewReader("secr3t\n"))
		var stdout, stderr bytes.Buffer

		got, err := readSecretNoEcho(context.Background(), charDevice(t), br, &stdout, &stderr, func() {
			t.Fatal("prompt appeared before echo could be disabled")
		})
		if err == nil {
			t.Fatalf("read %q and reported nothing wrong; a prompt that cannot hide what is typed must not read a secret", got)
		}
		if got != "" {
			t.Errorf("returned %q, want nothing at all", got)
		}
		if !errors.Is(err, refused) {
			t.Errorf("err = %v, want it to carry what the terminal said (%v)", err, refused)
		}
		left, _ := br.ReadString('\n')
		if left != "secr3t\n" {
			t.Errorf("the reader is left holding %q, want the whole line still there", left)
		}
	})

	t.Run("and the run says why it stopped", func(t *testing.T) {
		refuseEchoOff(t)
		var stdout, stderr bytes.Buffer

		if code := loginToken(context.Background(), charDevice(t), &stdout, &stderr); code != exitError {
			t.Errorf("loginToken() = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr.String(), "tt: cannot hide what you type on this terminal") {
			t.Errorf("stderr = %q, want it to say the terminal would not hide the token", stderr.String())
		}
		if strings.Contains(stderr.String(), "no token entered") {
			t.Errorf("stderr = %q, want nothing about an empty answer: the prompt never asked", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want no prompt when input cannot be hidden", stdout.String())
		}
	})
}

func TestWriteSecretFileSweepsItsOwnLeftovers(t *testing.T) {
	isolate(t)
	dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	swept := write("token.tmp123456", "half of an older secret")
	kept := []string{
		write("token.tmp", "not a name CreateTemp ever produces"),
		write("oauth.json.tmp99", "the credentials file's leftover, not this one's"),
		write("tokens.txt", "someone's own file"),
	}

	if err := os.Mkdir(filepath.Join(dir, "token.tmp778899"), 0o700); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, filepath.Join(dir, "token.tmp778899"))

	victim := ""
	if runtime.GOOS != "windows" {
		victim = filepath.Join(t.TempDir(), "notes.txt")
		if err := os.WriteFile(victim, []byte("someone else's file"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "token.tmp654321")
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, link, victim)
	}

	path := filepath.Join(dir, "token")
	if err := writeSecretFile(path, "new-token"); err != nil {
		t.Fatalf("writeSecretFile: %v", err)
	}

	if _, err := os.Lstat(swept); !os.IsNotExist(err) {
		t.Errorf("%s is still there, want the leftover secret swept away", swept)
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is gone, want it left alone: %v", p, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-token" {
		t.Fatalf("token file = %q, want %q", data, "new-token")
	}
	assertFileMode(t, path, 0o600)
}

func loginLineFits(line string) bool {
	if utf8.RuneCountInString(line) <= cli.Width {
		return true
	}
	var breakable []string
	for _, word := range strings.Fields(line) {
		if utf8.RuneCountInString(word) > cli.Width {
			continue
		}
		breakable = append(breakable, word)
	}
	return utf8.RuneCountInString(strings.Join(breakable, " ")) <= cli.Width
}

func assertLoginFits(t *testing.T, what, out string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !loginLineFits(line) {
			t.Errorf("%s line %d is %d columns, want at most %d: %q",
				what, i+1, utf8.RuneCountInString(line), cli.Width, line)
		}
	}
}

func TestLoginProseStaysInsideTheWidth(t *testing.T) {
	t.Run("the token form, with the environment overriding what it saved", func(t *testing.T) {
		isolateInALongPath(t)
		t.Setenv(tokenEnvVar, "env-token")
		var stdout, stderr bytes.Buffer
		if got := run(context.Background(), []string{"login", "token"}, strings.NewReader("file-token\n"), &stdout, &stderr); got != exitOK {
			t.Fatalf("run(login token) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
		}
		if !strings.Contains(collapsed(stdout.String()), "unset it to use the saved one") {
			t.Fatalf("stdout = %q, want the note about the environment overriding the token", stdout.String())
		}
		assertLoginFits(t, "tt login token", stdout.String())
	})

	accessTokenOnly := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"an-access-token"}`))
	}
	for _, c := range []struct {
		name  string
		saved func(port int) appCredentials
		stdin string
		want  string
	}{
		{

			name: "a redirect URI the last login did not use",
			saved: func(port int) appCredentials {
				return appCredentials{
					ClientID: "saved-id", ClientSecret: "saved-secret",
					RedirectURI: fmt.Sprintf("http://127.0.0.1:%d/callback", port+1),
				}
			},
			want: "the app registration has to list this one too",
		},
		{

			name: "a refresh token that belongs to another client id",
			saved: func(int) appCredentials {
				return appCredentials{ClientID: "old-app-id", RefreshToken: "first-refresh-token"}
			},
			stdin: "new-app-id\nnew-secret\n",
			want:  "it belongs to that app, not this one",
		},
		{
			name: "a refresh token the exchange did not return again",
			saved: func(int) appCredentials {
				return appCredentials{ClientID: "saved-id", ClientSecret: "saved-secret", RefreshToken: "first-refresh-token"}
			},
			want: "the one an earlier login saved is kept",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			port := freePort(t)
			if err := saveCredentials(c.saved(port)); err != nil {
				t.Fatalf("saveCredentials: %v", err)
			}
			code, stdout, stderr := runLoginOAuth(t, port, c.stdin, accessTokenOnly)
			if code != exitOK {
				t.Fatalf("loginOAuth = %d, want %d\nstderr: %s", code, exitOK, stderr)
			}
			if !strings.Contains(collapsed(stdout), c.want) {
				t.Fatalf("stdout = %q, want it to carry %q", stdout, c.want)
			}
			assertLoginFits(t, "tt login", stdout)
		})
	}
}
