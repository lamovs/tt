package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/webapi"
)

type authorizationAdvice struct {
	err     error
	message string
}

func (e *authorizationAdvice) Error() string {
	return api.OneLine(e.message + " (" + e.err.Error() + ")")
}
func (e *authorizationAdvice) Unwrap() error { return e.err }

func openRecoveryCommand() string {
	if strings.TrimSpace(os.Getenv(tokenEnvVar)) != "" {
		return "TT_TOKEN is overriding the saved credential. Unset TT_TOKEN, then run tt login token --same-account and enter a new personal API token at the hidden prompt."
	}
	credentials, exists, err := loadCredentials()
	if err == nil && exists {
		port := ""
		if redirect, err := url.Parse(credentials.RedirectURI); err == nil && redirect.Port() != "" {
			if n, err := strconv.Atoi(redirect.Port()); err == nil && n > 0 && n <= 65535 {
				port = " --port " + strconv.Itoa(n)
			}
		}
		switch credentials.Method {
		case "oauth_legacy":
			return "Run tt login --same-account" + port + " and authorize the same TickTick account."
		case "oauth_pkce":
			id := "'" + strings.ReplaceAll(credentials.ClientID, "'", "'\"'\"'") + "'"
			return "Run tt login --pkce --client-id " + id + " --same-account" + port + " with the same TickTick account."
		}
	}
	return "Create a new personal API token in TickTick Settings > Account > API Token, then run tt login token --same-account and paste it at the hidden prompt."
}

func openAuthAdvice(err error) error {
	var already *authorizationAdvice
	if err == nil || errors.As(err, &already) {
		return err
	}
	message := ""
	var status *api.StatusError
	switch {
	case errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized:
		message = "Open API authorization was rejected; the token may have expired or been revoked. " + openRecoveryCommand() + " Queued work is preserved."
	case errors.As(err, &status) && status.StatusCode == http.StatusForbidden:
		message = "Open API denied this operation (HTTP 403). Check account access and token permissions with tt auth status --check; this does not prove token expiry."
	case errors.Is(err, store.ErrCredentialChanged):
		message = "Open API credential differs from the cache binding. For a renewed token of the same account: " + openRecoveryCommand() + " For another account, use a separate XDG_DATA_HOME."
	case errors.Is(err, store.ErrSignedOut):
		message = "Open API is signed out. " + openRecoveryCommand()
	}
	if message == "" {
		return err
	}
	return &authorizationAdvice{err: err, message: message}
}

func browserAuthAdvice(err error) error {
	var status *webapi.StatusError
	if !errors.As(err, &status) {
		return err
	}
	switch status.StatusCode {
	case http.StatusUnauthorized:
		return &authorizationAdvice{err: err, message: "Browser session cookie t was rejected; it may have expired or been revoked. Sign in to ticktick.com with the same account, copy the current t cookie, then run tt login web --same-account. Timer topics and topic uploads need this session; queued work is preserved."}
	case http.StatusForbidden:
		return &authorizationAdvice{err: err, message: "Browser API denied access (HTTP 403); this does not prove cookie expiry. Check the same account in ticktick.com and run tt auth web status --check."}
	default:
		return err
	}
}

func recoveryError(err error) error {
	return &authorizationAdvice{err: openAuthAdvice(err), message: "Same-account authorization recovery failed; queued work was preserved"}
}

func writeAuthorizationAdvice(w io.Writer, prefix string, err error) bool {
	var advice *authorizationAdvice
	if !errors.As(err, &advice) {
		return false
	}
	text := api.OneLine(uiPrivateText(err.Error()))
	lines := cli.Wrap(text, cli.Width-cli.DisplayWidth(prefix))
	for i, line := range lines {
		lead := prefix
		if i > 0 {
			lead = strings.Repeat(" ", cli.DisplayWidth(prefix))
		}
		fmt.Fprintln(w, lead+line)
	}
	return true
}
