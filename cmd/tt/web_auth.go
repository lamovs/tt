package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

const webCredentialsFileName = "web-auth.json"

type webCredentials struct {
	Version         int    `json:"version"`
	Session         string `json:"session"`
	OpenFingerprint string `json:"open_fingerprint"`
	BoundAt         string `json:"bound_at"`
}

func webCredentialsPath() (string, error) {
	tokenPath, err := api.TokenPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(tokenPath), webCredentialsFileName), nil
}

func loadWebCredentials() (webCredentials, bool, error) {
	path, err := webCredentialsPath()
	if err != nil {
		return webCredentials{}, false, err
	}
	file, err := api.ReadRegularFile(path)
	if err != nil {
		if os.IsNotExist(err) && !errors.Is(err, api.ErrUnusableLink) {
			return webCredentials{}, false, nil
		}
		return webCredentials{}, false, errors.New("read Browser credential file: " + fullReportAtom(err.Error()))
	}
	if file.Mode&0o077 != 0 {
		return webCredentials{}, false, errors.New("Browser credential file permissions are too broad; expected 0600")
	}
	var credentials webCredentials
	if err := json.Unmarshal(file.Data, &credentials); err != nil {
		return webCredentials{}, false, errors.New("Browser credential file is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, credentials.BoundAt); credentials.Version != 1 || !validCredentialFingerprint(credentials.OpenFingerprint) || err != nil {
		return webCredentials{}, false, errors.New("Browser credential file is invalid")
	}
	if err := webapi.ValidateSession(credentials.Session); err != nil {
		return webCredentials{}, false, err
	}
	return credentials, true, nil
}

func validCredentialFingerprint(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func saveWebCredentials(credentials webCredentials) error {
	path, err := webCredentialsPath()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return errors.New("encode Browser credentials")
	}
	return writeSecretFile(path, string(encoded)+"\n")
}

func currentOpenCredential(ctx context.Context) (string, string, error) {
	status, token, err := readAuthStatus(ctx)
	if err != nil {
		return "", "", err
	}
	if status.State != "available" || status.Binding != "bound" || strings.TrimSpace(token) == "" {
		return "", "", errors.New("Open API authorization is unavailable; run tt auth status and restore the same account with tt login token --same-account or your OAuth login first")
	}
	return token, credentialFingerprint(token), nil
}

func webClient(ctx context.Context) (*webapi.Client, string, error) {
	token, fingerprint, err := currentOpenCredential(ctx)
	if err != nil {
		return nil, "", err
	}
	credentials, ok, err := loadWebCredentials()
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", focus.ErrBrowserNotConfigured
	}
	if credentials.OpenFingerprint != fingerprint {
		return nil, "", errors.New("Browser session binding does not match the current Open API credential; run tt login web --same-account with the same account")
	}
	openSession, err := managedCredential(ctx, token)
	if err != nil {
		return nil, "", openAuthAdvice(err)
	}
	guard := func(ctx context.Context) (func(), error) {
		release, err := openSession.guard(ctx)
		if err != nil {
			return nil, openAuthAdvice(err)
		}
		current, exists, err := loadWebCredentials()
		if err != nil || !exists || current != credentials {
			release()
			return nil, errors.New("Browser authorization changed; retry with the current session or run tt login web --same-account")
		}
		return release, nil
	}
	client, err := webapi.NewClient(credentials.Session, version, webapi.WithRequestGuard(guard), webapi.WithErrorAdvice(browserAuthAdvice))
	return client, credentialFingerprint(credentials.Session), err
}

func loginWeb(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, sameAccount bool) int {
	if !sameAccount {
		fmt.Fprintln(stderr, "tt: login: web login requires --same-account")
		loginNote(stderr, "This flag confirms that the Browser session and current Open API credential belong to the same TickTick account. TickTick does not expose an automatic identity check for both channels.")
		return exitUsage
	}
	_, openFingerprint, err := currentOpenCredential(ctx)
	if err != nil {
		loginNote(stderr, "tt: login: "+err.Error())
		return exitError
	}
	loginNote(stdout, "Browser authorization is an additional credential for Timer topics. Sign in to ticktick.com with the same account as the current Open API credential, then copy only the value of its cookie named t from browser site storage.")
	loginNote(stdout, "Treat this value like a password: paste it only at the hidden prompt; do not put it in command arguments, shell history, screenshots or messages.")
	session, err := readSecretNoEcho(ctx, stdin, bufio.NewReader(stdin), stdout, stderr, func() {
		fmt.Fprint(stdout, "Browser session t: ")
	})
	if err != nil {
		loginNote(stderr, "tt: login: "+err.Error())
		return exitError
	}
	client, err := webapi.NewClient(session, version, webapi.WithErrorAdvice(browserAuthAdvice))
	if err != nil {
		loginNote(stderr, "tt: login: "+err.Error())
		return exitError
	}
	topics, err := client.ListTopics(ctx)
	if err != nil {
		loginNote(stderr, "tt: login: Browser session was not saved: "+err.Error())
		return exitError
	}
	credentials := webCredentials{Version: 1, Session: strings.TrimSpace(session), OpenFingerprint: openFingerprint, BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
	st, err := store.Open(ctx, "")
	if err != nil {
		loginNote(stderr, "tt: login: Browser session was not saved: "+err.Error())
		return exitError
	}
	defer st.Close()
	if err := saveBoundWebCredentials(ctx, st, credentials); err != nil {
		loginNote(stderr, "tt: login: Browser session was not saved: "+err.Error())
		return exitError
	}
	if err := replaceWebTopics(ctx, st, topics, credentialFingerprint(credentials.Session), time.Now()); err != nil {
		loginNote(stderr, "tt: login: Browser session saved, but topic cache was not updated: "+err.Error())
		return exitError
	}
	path, _ := webCredentialsPath()
	fmt.Fprintf(stdout, "saved %s\n", path)
	fmt.Fprintf(stdout, "cached %d Timer topics\n", len(topics))
	return exitOK
}

func saveBoundWebCredentials(ctx context.Context, st *store.Store, credentials webCredentials) error {
	tokenPath, err := api.TokenPath()
	if err != nil {
		return err
	}
	lock, err := acquireAuthLock(ctx, tokenPath)
	if err != nil {
		return err
	}
	defer lock.Release()
	_, currentOpen, err := currentOpenCredential(ctx)
	if err != nil || currentOpen != credentials.OpenFingerprint {
		return errors.New("Open API credential changed during Browser login; repeat tt login web --same-account")
	}
	old, exists, err := loadWebCredentials()
	if err != nil {
		return err
	}
	previous := ""
	if exists {
		previous = credentialFingerprint(old.Session)
	}
	if err := st.RecoverBrowserCredential(ctx, previous, credentialFingerprint(credentials.Session)); err != nil {
		return err
	}
	return saveWebCredentials(credentials)
}

func replaceWebTopics(ctx context.Context, st *store.Store, topics []webapi.Topic, fingerprint string, now time.Time) error {
	rows := make([]store.FocusTopic, 0, len(topics))
	for _, topic := range topics {
		rows = append(rows, store.FocusTopic{ID: topic.ID, Name: topic.Name, Type: topic.Type, Status: topic.Status,
			SortOrder: topic.SortOrder, PomodoroTime: topic.PomodoroTime, Raw: topic.Raw})
	}
	return st.ReplaceFocusTopics(ctx, rows, fingerprint, now)
}

func cmdWebAuth(inv *invocation) int {
	if len(inv.data) != 2 || inv.data[0] != "web" || inv.data[1] != "status" && inv.data[1] != "logout" {
		return inv.misuse("web authorization takes status or logout")
	}
	action := inv.data[1]
	check := len(inv.refinements) == 1 && inv.refinements[0] == "--check"
	if len(inv.refinements) != 0 && (!check || action != "status") {
		return inv.misuse("only auth web status accepts --check")
	}
	path, err := webCredentialsPath()
	if err != nil {
		return inv.fail(err)
	}
	if action == "logout" {
		if err := logoutWebCredential(inv.ctx, path); err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			return inv.output(map[string]any{"state": "signed_out", "open_api_preserved": true, "local_work_preserved": true}, app.ResultMeta{Source: "local"})
		}
		return authText(inv, []string{"Browser session removed locally.", "Open API authorization, cached topics and local focus sessions were preserved."})
	}
	credentials, ok, err := loadWebCredentials()
	if err != nil {
		return inv.fail(err)
	}
	state, binding, count := "missing", "unbound", 0
	if ok {
		state = "available"
		_, current, currentErr := currentOpenCredential(inv.ctx)
		if currentErr != nil || current != credentials.OpenFingerprint {
			state, binding = "blocked", "mismatch"
		} else {
			binding = "user_confirmed_same_account"
		}
	}
	if check {
		client, fingerprint, err := webClient(inv.ctx)
		if err != nil {
			return inv.fail(err)
		}
		topics, err := client.ListTopics(inv.ctx)
		if err != nil {
			return inv.fail(err)
		}
		st, err := inv.openStore()
		if err != nil {
			return inv.fail(err)
		}
		defer st.Close()
		if err := replaceWebTopics(inv.ctx, st, topics, fingerprint, time.Now()); err != nil {
			return inv.fail(err)
		}
		count = len(topics)
	}
	if inv.jsonOutput {
		return inv.output(map[string]any{"state": state, "binding": binding, "checked": check, "topic_count": count, "identity_verified_by_provider": false}, app.ResultMeta{Source: map[bool]string{true: "server", false: "local"}[check]})
	}
	lines := []string{"Browser authorization: " + state, "Open credential binding: " + binding, "Provider identity comparison: unavailable"}
	if check {
		lines = append(lines, fmt.Sprintf("Server read accepted; cached Timer topics: %d", count))
	}
	return authText(inv, lines)
}

func logoutWebCredential(ctx context.Context, path string) error {
	tokenPath, err := api.TokenPath()
	if err != nil {
		return err
	}
	lock, err := acquireAuthLock(ctx, tokenPath)
	if err != nil {
		return err
	}
	defer lock.Release()
	credentials, exists, err := loadWebCredentials()
	if err != nil {
		return err
	}
	if exists {
		st, err := store.Open(ctx, "")
		if err != nil {
			return err
		}
		defer st.Close()
		fingerprint := credentialFingerprint(credentials.Session)
		if err := st.RecoverBrowserCredential(ctx, fingerprint, fingerprint); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.New("remove Browser credential: " + fullReportAtom(err.Error()))
	}
	return nil
}
