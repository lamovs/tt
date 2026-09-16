package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
	tasksync "github.com/movsar/tt/internal/sync"
)

var openBrowser = defaultOpenBrowser

var loginAfterSave = func(ctx context.Context, token string, stdout, stderr io.Writer) int {
	return pullAfterLogin(ctx, token, stdout, stderr)
}

const loginUsage = "usage: tt login [--legacy] [--port N] [--same-account]\n       tt login --pkce --client-id ID [--port N] [--same-account]\n       tt login token [--same-account]\n       tt login web --same-account\n"

type sameAccountRecoveryKey struct{}

func cmdLogin(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	data, refinements := splitArgs(args)
	if !(len(data) == 1 && data[0] == "web") {
		var clean []string
		recovery := false
		for _, flag := range refinements {
			if flag != "--same-account" {
				clean = append(clean, flag)
				continue
			}
			if recovery {
				loginNote(stderr, "tt: login: --same-account was repeated")
				return exitUsage
			}
			recovery = true
		}
		if recovery {
			ctx = context.WithValue(ctx, sameAccountRecoveryKey{}, true)
			loginNote(stdout, "--same-account confirms that the replacement Open API credential belongs to this cache's TickTick account. This is your confirmation, not a provider identity check. Queued work is preserved; another account needs a separate XDG_DATA_HOME.")
		}
		refinements = clean
	}
	switch {
	case len(data) == 0:
		if slices.Contains(refinements, "--pkce") {
			clientID, port, err := parsePKCELogin(refinements)
			if err != nil {
				loginNote(stderr, "tt: login: "+err.Error())
				return exitUsage
			}
			return loginPKCE(ctx, stdout, stderr, clientID, port)
		}
		if slices.Contains(refinements, "--legacy") {
			var clean []string
			for _, value := range refinements {
				if value != "--legacy" {
					clean = append(clean, value)
				}
			}
			refinements = clean
		}
		port, ok := parseLoginPort(refinements, stderr)
		if !ok {
			return exitUsage
		}
		return loginOAuth(ctx, stdin, stdout, stderr, port)
	case len(data) == 1 && data[0] == "token":
		if len(refinements) != 0 {
			fmt.Fprint(stderr, loginUsage)
			return exitUsage
		}
		return loginToken(ctx, stdin, stdout, stderr)
	case len(data) == 1 && data[0] == "web":
		if len(refinements) != 1 || refinements[0] != "--same-account" {
			fmt.Fprint(stderr, loginUsage)
			return exitUsage
		}
		return loginWeb(ctx, stdin, stdout, stderr, true)
	default:
		fmt.Fprint(stderr, loginUsage)
		return exitUsage
	}
}

func parseLoginPort(refinements []string, stderr io.Writer) (port int, ok bool) {
	port = defaultLoginPort
	for i := 0; i < len(refinements); i++ {
		if refinements[i] != "--port" {
			fmt.Fprintln(stderr, quoteWord("tt: login: unknown option ", refinements[i]))
			fmt.Fprint(stderr, loginUsage)
			return 0, false
		}
		if i+1 >= len(refinements) {
			fmt.Fprint(stderr, "tt: login: --port needs a value\n")
			return 0, false
		}
		p, err := strconv.Atoi(refinements[i+1])
		if err != nil || p < 1 || p > 65535 {
			fmt.Fprintln(stderr, quoteWord("tt: login: invalid --port value ", refinements[i+1]))
			return 0, false
		}
		port = p
		i++
	}
	return port, true
}

func loginNote(stdout io.Writer, s string) {
	cli.WriteLines(stdout, cli.Wrap(s, cli.Width))
}

func loginOAuth(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, port int) int {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintf(stderr, "tt: listen on 127.0.0.1:%d: %v\n", port, err)
		return exitError
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	creds, hadCreds, err := loadCredentials()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}

	if creds.RedirectURI != "" && creds.RedirectURI != redirectURI {
		loginNote(stdout, fmt.Sprintf("note: the last login used the redirect URI %s, this one uses %s - the app registration has to list this one too",
			creds.RedirectURI, redirectURI))
	}
	clientID, clientSecret := creds.ClientID, creds.ClientSecret
	if !hadCreds || clientID == "" || clientSecret == "" {
		clientID, clientSecret, err = promptAppCredentials(ctx, stdin, stdout, stderr, redirectURI)
		if err != nil {
			fmt.Fprintf(stderr, "tt: %v\n", err)
			return exitError
		}
	}

	state, err := randomState()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	authURL := buildAuthorizeURL(clientID, redirectURI, state)

	fmt.Fprintf(stdout, "\nopen this URL to authorize tt:\n  %s\n", authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintln(stdout, "(could not open a browser automatically - use the link above)")
	}
	fmt.Fprintln(stdout, "waiting for the authorization...")

	waitCtx, cancel := context.WithTimeout(ctx, loginWaitTimeout)
	defer cancel()
	code, err := runCallback(waitCtx, listener, state)
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}

	tr, err := exchangeCode(ctx, clientID, clientSecret, redirectURI, code)
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}

	tokenPath, err := api.TokenPath()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	refreshToken := tr.RefreshToken
	droppedRefreshToken := false
	if !hasRefreshToken(refreshToken) && hasRefreshToken(creds.RefreshToken) {
		if creds.ClientID == clientID {
			refreshToken = creds.RefreshToken
		} else {
			droppedRefreshToken = true
		}
	}
	returnedScopes, scopeSafe := safeScopeWords(tr.Scope, tr.AccessToken, tr.RefreshToken, clientSecret)
	if err := saveLoginCredential(ctx, tr.AccessToken, &appCredentials{
		ClientID: clientID, ClientSecret: clientSecret,
		RedirectURI: redirectURI, RefreshToken: refreshToken,
		Method: "oauth_legacy", RequestedScopes: strings.Fields(oauthScope), ReturnedScopes: returnedScopes, ScopesKnown: tr.ScopeReturned && scopeSafe,
	}); err != nil {
		loginNote(stderr, "tt: login: authorization was not saved: "+api.OneLine(err.Error()))
		return exitError
	}

	fmt.Fprintf(stdout, "saved %s\n", tokenPath)
	if droppedRefreshToken {
		loginNote(stdout, "the refresh token saved for the previous client id was dropped: it belongs to that app, not this one")
	}
	switch {
	case !hasRefreshToken(refreshToken):
		loginNote(stdout, "TickTick did not return a refresh token; run \"tt login\" again once this one stops working")
	case !hasRefreshToken(tr.RefreshToken):
		loginNote(stdout, "TickTick did not return a refresh token this time; the one an earlier login saved is kept")
	}
	warnIfTokenEnvOverrides(stdout)
	return loginAfterSave(ctx, tr.AccessToken, stdout, stderr)
}

func parsePKCELogin(args []string) (string, int, error) {
	clientID, port := "", defaultLoginPort
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return "", 0, errors.New("login option was repeated")
		}
		seen[flag] = true
		switch flag {
		case "--pkce":
		case "--client-id", "--port":
			if i+1 >= len(args) {
				return "", 0, errors.New("login option needs a value")
			}
			i++
			if flag == "--client-id" {
				clientID = args[i]
				if strings.TrimSpace(clientID) != clientID || clientID == "" || strings.ContainsFunc(clientID, unicode.IsControl) {
					return "", 0, errors.New("client ID must be nonempty text without controls")
				}
			} else {
				value, err := strconv.Atoi(args[i])
				if err != nil || value < 1 || value > 65535 {
					return "", 0, errors.New("port must be from 1 to 65535")
				}
				port = value
			}
		default:
			return "", 0, errors.New("PKCE accepts only --client-id and --port")
		}
	}
	if !seen["--pkce"] || clientID == "" {
		return "", 0, errors.New("PKCE requires --client-id with your own public app registration")
	}
	return clientID, port, nil
}

func loginPKCE(ctx context.Context, stdout, stderr io.Writer, clientID string, port int) int {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		loginNote(stderr, "tt: login: cannot listen on the requested loopback port")
		return exitError
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	state, err := randomState()
	if err != nil {
		loginNote(stderr, "tt: login: cannot generate authorization state")
		return exitError
	}
	verifier, challenge, err := pkceVerifier()
	if err != nil {
		loginNote(stderr, "tt: login: cannot generate PKCE verifier")
		return exitError
	}
	loginNote(stdout, "PKCE needs your own public app registration and this exact callback: "+redirectURI)
	loginNote(stdout, "Registration support must be accepted by TickTick. No secret fallback is used.")
	authorizeURL := buildPKCEAuthorizeURL(clientID, redirectURI, state, challenge)
	fmt.Fprintf(stdout, "\nopen this URL to authorize tt:\n  %s\n", authorizeURL)
	if err := openBrowser(authorizeURL); err != nil {
		loginNote(stdout, "Could not open a browser; use the authorization URL above.")
	}
	waitCtx, cancel := context.WithTimeout(ctx, loginWaitTimeout)
	defer cancel()
	code, err := runPKCECallback(waitCtx, listener, state)
	if err != nil {
		loginNote(stderr, "tt: login: "+fullReportAtom(err.Error()))
		return exitError
	}
	response, err := exchangePKCECode(ctx, clientID, redirectURI, code, verifier)
	if err != nil {
		loginNote(stderr, "tt: login: "+err.Error())
		return exitError
	}
	returned, safe := safeScopeWords(response.Scope, response.AccessToken, response.RefreshToken)
	credentials := appCredentials{ClientID: clientID, RedirectURI: redirectURI, RefreshToken: response.RefreshToken, Method: "oauth_pkce", RequestedScopes: strings.Fields(oauthScope), ReturnedScopes: returned, ScopesKnown: response.ScopeReturned && safe}
	if err := saveLoginCredential(ctx, response.AccessToken, &credentials); err != nil {
		loginNote(stderr, "tt: login: "+fullReportAtom(err.Error()))
		return exitError
	}
	loginNote(stdout, "Saved PKCE authorization locally. Account identity remains unverified.")
	warnIfTokenEnvOverrides(stdout)
	return loginAfterSave(ctx, response.AccessToken, stdout, stderr)
}

func promptAppCredentials(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, redirectURI string) (clientID, clientSecret string, err error) {
	fmt.Fprintf(stdout, "tt needs a TickTick app registered to your account.\n\n"+
		"1. open %s and create an app (or reuse one)\n"+
		"2. set its redirect URI to EXACTLY:\n     %s\n\n", oauthPortalURL, redirectURI)

	br := bufio.NewReader(stdin)
	fmt.Fprint(stdout, "client id: ")
	clientID, err = readLine(br)
	if err != nil {
		return "", "", err
	}
	if clientID == "" {
		return "", "", errors.New("client id must not be empty")
	}

	clientSecret, err = readSecretNoEcho(ctx, stdin, br, stdout, stderr, func() {
		fmt.Fprint(stdout, "client secret: ")
	})
	if err != nil {
		return "", "", err
	}
	if clientSecret == "" {
		return "", "", errors.New("client secret must not be empty")
	}
	return clientID, clientSecret, nil
}

func loginToken(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	token, err := readSecretNoEcho(ctx, stdin, bufio.NewReader(stdin), stdout, stderr, func() {
		fmt.Fprint(stdout, "token: ")
	})
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	if token == "" {
		fmt.Fprint(stderr, "tt: no token entered\n")
		return exitError
	}

	path, err := api.TokenPath()
	if err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	if err := saveLoginCredential(ctx, token, nil); err != nil {
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "saved %s\n", path)
	warnIfTokenEnvOverrides(stdout)
	return loginAfterSave(ctx, token, stdout, stderr)
}

func pullAfterLogin(ctx context.Context, token string, stdout, stderr io.Writer, opts ...api.Option) int {
	st, err := store.Open(ctx, "")
	if err != nil {
		loginNote(stderr, "tt: login: token saved, but initial pull failed: "+fullReportAtom(err.Error()))
		return exitError
	}
	defer st.Close()

	unlock, err := acquireSyncLock(st)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			fmt.Fprintln(stderr, "tt: login: token saved, but initial pull did not run: another sync is already running")
		} else {
			fmt.Fprintln(stderr, "tt: login: token saved, but initial pull could not acquire the process lock")
		}
		return exitError
	}
	defer unlock()

	res, runErr := tasksync.New(st, managedAPIClient(token, opts...), tasksync.Options{}).Pull(ctx)
	fmt.Fprintf(stdout, "logged in; pulled %d task(s) from %d list(s), %d skipped, %d deleted, %d kept\n",
		res.Pulled, res.Projects, res.Skipped, res.Deleted, res.Kept)
	printSyncErrors(stderr, res.Errors, runErr, interrupted(ctx))
	if !app.TaskSyncSucceeded(res, runErr) {
		loginNote(stderr, "the token was saved, but the initial pull was incomplete; run \"tt sync\" to try again")
		return exitError
	}
	return exitOK
}

func warnIfTokenEnvOverrides(stdout io.Writer) {

	if strings.TrimSpace(os.Getenv(tokenEnvVar)) != "" {
		loginNote(stdout, fmt.Sprintf("note: $%s is set and overrides the token just saved; unset it to use the saved one", tokenEnvVar))
	}
}

const (
	secretDirPerm  = 0o700
	secretFilePerm = 0o600
)

func writeSecretFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := prepareSecretDir(dir); err != nil {
		return err
	}
	tmpPrefix := filepath.Base(path) + ".tmp"

	sweepWriteTemps(dir, tmpPrefix)

	f, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if renamed {
			return
		}
		f.Close()
		os.Remove(tmp)
	}()

	if err := f.Chmod(secretFilePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	renamed = true
	return nil
}

func sweepWriteTemps(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !isDecimalRun(name[len(prefix):]) {
			continue
		}

		if !e.Type().IsRegular() {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}

func isDecimalRun(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func prepareSecretDir(dir string) error {
	if err := os.MkdirAll(dir, secretDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, secretDirPerm); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	return strings.TrimSpace(line), ignoreEOF(err)
}

func isCharDevice(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func readSecretNoEcho(ctx context.Context, stdin io.Reader, br *bufio.Reader, stdout, stderr io.Writer, prompt func()) (string, error) {
	f, ok := stdin.(*os.File)
	if !ok || !isCharDevice(f) {
		prompt()
		return readLine(br)
	}
	echo := &echoSwitch{f: f}

	stopWatching := restoreEchoOnSignal(ctx, echo, stderr, os.Exit)
	defer stopWatching()
	offErr := echo.off()

	defer echo.restore()
	if offErr != nil {
		return "", fmt.Errorf("cannot hide what you type on this terminal, so nothing was read: %w", offErr)
	}

	prompt()
	line, err := readLine(br)
	fmt.Fprintln(stdout)
	return line, err
}

type pendingInput int

const (
	keepInput pendingInput = iota

	discardInput
)

var toggleEcho = defaultToggleEcho

type echoSwitch struct {
	f        *os.File
	mu       sync.Mutex
	restored bool
}

func (e *echoSwitch) off() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.restored {
		return nil
	}
	return toggleEcho(e.f, false, keepInput)
}

func (e *echoSwitch) restore() {
	e.restoreWith(keepInput)
}

func (e *echoSwitch) restoreDiscarding() {
	e.restoreWith(discardInput)
}

func (e *echoSwitch) restoreWith(pending pendingInput) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restored = true
	if pending == discardInput {
		_ = toggleEcho(e.f, true, keepInput)
	}
	_ = toggleEcho(e.f, true, pending)
}

// restoreEchoOnSignal gives echo back when a signal ends tt during a hidden
// prompt. Every signal that does reaches it through ctx, which ttMain cancels
// on ttSignals; it listens for none itself, so no signal has two handlers
// racing, and none that tt was started with ignored is taken back.
func restoreEchoOnSignal(ctx context.Context, echo *echoSwitch, stderr io.Writer, exit func(int)) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			echo.restoreDiscarding()

			reportInterrupted(stderr)
			exit(exitInterrupted)
		case <-done:
		}
	}()
	return func() { close(done) }
}

func ignoreEOF(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

func defaultOpenBrowser(url string) error {
	cmd, err := browserCommand(runtime.GOOS, wslDetected(), url)
	if err != nil {
		return err
	}
	return cmd.Start()
}

func browserCommand(goos string, wsl bool, url string) (*exec.Cmd, error) {
	switch {
	case goos == "darwin":
		return exec.Command("open", url), nil
	case wsl:
		script := "Start-Process '" + quotePowerShellSingleQuoted(url) + "'"
		return exec.Command("powershell.exe", "-NoProfile", "-Command", script), nil
	case goos == "linux":
		return exec.Command("xdg-open", url), nil
	default:
		return nil, fmt.Errorf("no known way to open a browser on %s", goos)
	}
}

var powerShellQuotes = []string{"'", "\u2018", "\u2019", "\u201a", "\u201b"}

func quotePowerShellSingleQuoted(s string) string {
	for _, q := range powerShellQuotes {
		s = strings.ReplaceAll(s, q, q+q)
	}
	return s
}
