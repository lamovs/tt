package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

type authScopeStatus struct {
	Requested     []string `json:"requested"`
	Returned      []string `json:"returned"`
	ReturnedKnown bool     `json:"returned_known"`
}

type authStatus struct {
	State        string           `json:"state"`
	Source       string           `json:"credential_source"`
	Method       string           `json:"method"`
	Binding      string           `json:"binding"`
	Generation   uint64           `json:"generation"`
	Identity     string           `json:"account_identity"`
	Scopes       authScopeStatus  `json:"scopes"`
	Checked      bool             `json:"checked"`
	Check        string           `json:"check"`
	Projects     *int             `json:"project_count,omitempty"`
	Capabilities []app.Capability `json:"capabilities"`
}

func init() {
	register(command{help: cli.Help{
		Verb: "auth", Summary: "inspect authorization or remove local access credentials",
		Examples: []cli.Example{
			{Cmd: "tt auth status", What: "read local credential and cache binding state"},
			{Cmd: "tt auth status --check", What: "try one bounded read of server lists"},
			{Cmd: "tt auth logout", What: "remove bearer and refresh credentials; keep local work"},
			{Cmd: "tt auth web status --check", What: "check the separate Browser session and refresh Timer topics"},
			{Cmd: "tt auth web logout", What: "remove only the local Browser session"},
		},
		Sections: []cli.HelpSection{
			{Title: "Status", Items: []string{
				"Status never prints access or refresh tokens.",
				"Requested and returned scopes are reported separately. A successful list read does not prove write access or account identity.",
			}},
			{Title: "Local logout", Items: []string{
				"logout removes local bearer and refresh credentials; it does not revoke them on the server.",
				"Cache, queued work, configuration and local timers are preserved. TT_TOKEN is blocked and the cache keeps its credential binding.",
				"Resume with the same credential, or renew access with tt login token --same-account (personal token) or the original OAuth login plus --same-account. Another account needs a separate XDG_DATA_HOME.",
			}},
			{Title: "Own public client", Items: []string{
				"Use tt login --pkce --client-id ID with your own public app registration. Its callback must exactly match the displayed loopback URL.",
			}},
			{Title: "Timer topic authorization", Items: []string{
				"Browser authorization is separate from the Open API bearer and is used only for Timer topics and their focus records.",
				"Personal token and OAuth are alternative Open API login methods, not two credentials that must both be configured.",
				"TickTick exposes no cross-channel identity comparison. login web requires an explicit same-account confirmation and stores both credential fingerprints.",
				"After Open API renewal, repeat tt login web --same-account. Browser renewal preserves frozen topic sessions and their upload recovery state.",
			}},
		},
		SeeAlso: []string{"login", "doctor", "sync"},
	}, run: cmdAuth})
}

func cmdAuth(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("auth does not expose raw credential data")
	}
	if len(inv.data) > 0 && inv.data[0] == "web" {
		return cmdWebAuth(inv)
	}
	action := "status"
	if len(inv.data) == 1 {
		action = inv.data[0]
	} else if len(inv.data) > 1 {
		return inv.misuse("auth takes status or logout")
	}
	check := len(inv.refinements) == 1 && inv.refinements[0] == "--check"
	if len(inv.refinements) != 0 && (!check || action != "status") {
		return inv.misuse("only auth status accepts --check")
	}
	if action == "logout" {
		ctx, cancel := context.WithTimeout(inv.ctx, 30*time.Second)
		defer cancel()
		if err := logoutCredential(ctx); err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			return inv.output(map[string]any{"state": "signed_out", "server_revoked": false, "local_work_preserved": true, "environment_token_blocked": true}, app.ResultMeta{Source: "local"})
		}
		return authText(inv, []string{"Signed out locally. Bearer and refresh credentials were removed.", "Cache, queue, configuration and local timers were preserved.", "TT_TOKEN cannot resume this cache. Log in with the same credential."})
	}
	if action != "status" {
		return inv.misuse("auth takes status or logout")
	}
	status, token, err := readAuthStatus(inv.ctx)
	if err != nil {
		return inv.fail(err)
	}
	if check {
		if status.State != "available" {
			return inv.fail(errors.New("no usable bound credential; inspect tt auth status"))
		}
		ctx, cancel := context.WithTimeout(inv.ctx, 5*time.Second)
		defer cancel()
		projects, err := managedAPIClient(token, api.WithMaxRetries(1)).ListProjects(ctx)
		if err != nil {
			return inv.fail(err)
		}
		status, _, err = readAuthStatus(ctx)
		if err != nil {
			return inv.fail(err)
		}
		status.Checked = true
		status.Check = "read_accepted"
		count := len(projects)
		status.Projects = &count
	}
	if inv.jsonOutput {
		source := "local"
		if status.Checked {
			source = "server"
		}
		return inv.output(status, app.ResultMeta{Source: source})
	}
	lines := []string{"Authorization: " + status.State, "Credential source: " + status.Source, "Cache binding: " + status.Binding, "Account identity: unknown"}
	if len(status.Scopes.Requested) == 0 {
		lines = append(lines, "Requested scopes: unknown")
	} else {
		lines = append(lines, "Requested scopes: "+strings.Join(status.Scopes.Requested, " "))
	}
	if !status.Scopes.ReturnedKnown {
		lines = append(lines, "Returned scopes: unknown")
	} else {
		lines = append(lines, "Returned scopes: "+strings.Join(status.Scopes.Returned, " "))
	}
	if status.Checked {
		lines = append(lines, "Server read accepted; write permissions remain unverified.")
	}
	return authText(inv, lines)
}

func authText(inv *invocation, lines []string) int {
	for _, line := range lines {
		if err := cli.WriteLines(inv.stdout, cli.Wrap(cli.Foreign(line, cli.Width), cli.Width)); err != nil {
			return inv.fail(err)
		}
	}
	return exitOK
}

func readAuthStatus(ctx context.Context) (authStatus, string, error) {
	out := authStatus{State: "missing", Source: "none", Method: "unknown", Binding: "unbound", Identity: "unknown", Check: "not_checked", Scopes: authScopeStatus{Requested: []string{}, Returned: []string{}}, Capabilities: app.Capabilities()}
	var binding store.AuthBinding
	if inspection, err := store.Inspect(ctx, ""); err == nil {
		var exists bool
		binding, exists, err = inspection.Store.AuthBinding(ctx)
		inspection.Close()
		if err != nil {
			return out, "", err
		}
		if exists {
			out.Binding, out.Generation = "bound", binding.Generation
		}
	} else if !errors.Is(err, store.ErrNoCache) {
		return out, "", err
	}
	token := strings.TrimSpace(os.Getenv(tokenEnvVar))
	if token != "" {
		out.Source = "environment"
	} else {
		info, err := api.LoadToken()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, "", err
		}
		token = info.Value
		if token != "" {
			out.Source = "file"
		}
	}
	if token != "" {
		out.State = "available"
	}
	if binding.SignedOut {
		out.State = "signed_out"
	} else if token != "" && binding.Fingerprint != "" && binding.Fingerprint != credentialFingerprint(token) {
		out.State, out.Binding = "blocked", "mismatch"
	}
	if out.Source == "file" && out.State == "available" {
		credentials, ok, err := loadCredentials()
		if err != nil {
			return out, "", err
		}
		if ok {
			switch credentials.Method {
			case "token", "oauth_legacy", "oauth_pkce":
				out.Method = credentials.Method
			}
			out.Scopes.Requested, _ = safeScopeWords(strings.Join(credentials.RequestedScopes, " "), token, credentials.ClientSecret, credentials.RefreshToken)
			var safe bool
			out.Scopes.Returned, safe = safeScopeWords(strings.Join(credentials.ReturnedScopes, " "), token, credentials.ClientSecret, credentials.RefreshToken)
			out.Scopes.ReturnedKnown = credentials.ScopesKnown && safe
		}
	}
	return out, token, nil
}

func localAuthBlock() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inspection, err := store.Inspect(ctx, "")
	if err != nil {
		return nil
	}
	defer inspection.Close()
	binding, exists, err := inspection.Store.AuthBinding(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if binding.SignedOut {
		return store.ErrSignedOut
	}
	path, err := api.TokenPath()
	if err != nil {
		return err
	}
	token, err := selectedTokenAt(path)
	if err != nil {
		return nil
	}
	if token != "" && credentialFingerprint(token) != binding.Fingerprint {
		return store.ErrCredentialChanged
	}
	return nil
}

func safeScopeWords(value string, secrets ...string) ([]string, bool) {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return []string{}, false
		}
	}
	out := strings.Fields(value)
	for _, scope := range out {
		for _, c := range scope {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(":._-/", c)) {
				return []string{}, false
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}
