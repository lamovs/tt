package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	oauthAuthorizeURL = "https://ticktick.com/oauth/authorize"
	oauthTokenURL     = "https://ticktick.com/oauth/token"
)

const (
	oauthPortalURL = "https://developer.ticktick.com/manage"
	oauthScope     = "tasks:read tasks:write"

	defaultLoginPort = 9977
	loginWaitTimeout = 5 * time.Minute
	oauthHTTPTimeout = 30 * time.Second
)

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func buildAuthorizeURL(clientID, redirectURI, state string) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("scope", oauthScope)
	v.Set("state", state)
	return oauthAuthorizeURL + "?" + v.Encode()
}

func pkceVerifier() (string, string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", "", errors.New("generate PKCE verifier failed")
	}
	verifier := base64.RawURLEncoding.EncodeToString(value)
	return verifier, pkceChallenge(verifier), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func buildPKCEAuthorizeURL(clientID, redirectURI, state, challenge string) string {
	return buildAuthorizeURL(clientID, redirectURI, state) + "&code_challenge_method=S256&code_challenge=" + url.QueryEscape(challenge)
}

type tokenResponse struct {
	AccessToken   string `json:"access_token"`
	TokenType     string `json:"token_type"`
	RefreshToken  string `json:"refresh_token"`
	Scope         string `json:"scope"`
	ScopeReturned bool   `json:"-"`
}

func (response *tokenResponse) UnmarshalJSON(data []byte) error {
	type wire tokenResponse
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decoded.ScopeReturned = fields["scope"] != nil && string(fields["scope"]) != "null"
	*response = tokenResponse(decoded)
	return nil
}

func exchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	return postTokenForm(ctx, clientID, clientSecret, form)
}

func exchangePKCECode(ctx context.Context, clientID, redirectURI, code, verifier string) (tokenResponse, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "code": {code}, "code_verifier": {verifier}}
	return postTokenFormMode(ctx, clientID, "", form, false)
}

var errTokenRedirect = errors.New("refused to follow a redirect from the token endpoint")

func refuseTokenRedirect(req *http.Request, _ []*http.Request) error {
	return errTokenRedirect
}

type tokenEndpointError struct {
	category string
	err      error
}

func (e *tokenEndpointError) Error() string { return "token endpoint: " + e.category }
func (e *tokenEndpointError) Unwrap() error { return e.err }

func tokenResponseErrorCode(body []byte) string {
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return ""
	}
	switch response.Error {
	case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope":
		return response.Error
	default:
		return ""
	}
}

func authorizationResponseErrorCode(value string) string {
	switch value {
	case "invalid_request", "unauthorized_client", "access_denied", "unsupported_response_type",
		"invalid_scope", "server_error", "temporarily_unavailable":
		return value
	default:
		return ""
	}
}

func authorizationFailure(value string) string {
	failure := "authorization was not granted"
	if code := authorizationResponseErrorCode(value); code != "" {
		failure += " (" + code + ")"
	}
	return failure
}

func postTokenForm(ctx context.Context, clientID, clientSecret string, form url.Values) (tokenResponse, error) {
	return postTokenFormMode(ctx, clientID, clientSecret, form, true)
}

func postTokenFormMode(ctx context.Context, clientID, clientSecret string, form url.Values, basic bool) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, oauthHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, &tokenEndpointError{category: "request construction failed", err: err}
	}
	if basic {
		req.SetBasicAuth(clientID, clientSecret)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{CheckRedirect: refuseTokenRedirect}

	resp, err := client.Do(req)
	if err != nil {
		category := "request transport failed"
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) && errors.Is(urlErr.Err, errTokenRedirect) {
			category = "redirect refused"
			cause = urlErr.Err
		} else if errors.Is(err, context.Canceled) {
			category = "request canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			category = "request timed out"
		}
		return tokenResponse{}, &tokenEndpointError{category: category, err: cause}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, &tokenEndpointError{category: "response read failed", err: err}
	}
	if resp.StatusCode != http.StatusOK {
		if code := tokenResponseErrorCode(body); code != "" {
			return tokenResponse{}, fmt.Errorf("token request failed: http %d: oauth %s", resp.StatusCode, code)
		}
		return tokenResponse{}, fmt.Errorf("token request failed: http %d", resp.StatusCode)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, &tokenEndpointError{category: "response decoding failed", err: err}
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, errors.New("token response had no access_token")
	}
	if flaw, _ := inspectTokenValue(tr.AccessToken); flaw == tokenUnsendable {
		return tokenResponse{}, errors.New("token response carried an unusable access_token")
	}
	return tr, nil
}

func runCallback(ctx context.Context, listener net.Listener, expectedState string) (string, error) {
	return runCallbackMode(ctx, listener, expectedState, false)
}

func runPKCECallback(ctx context.Context, listener net.Listener, expectedState string) (string, error) {
	return runCallbackMode(ctx, listener, expectedState, true)
}

func runCallbackMode(ctx context.Context, listener net.Listener, expectedState string, strict bool) (string, error) {
	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)
	send := func(r result) {
		select {
		case resultCh <- r:
		default:
		}
	}

	respond := func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
		fmt.Fprint(w, msg)

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if strict {
			if r.Method != http.MethodGet || r.Host != listener.Addr().String() || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(expectedState)) != 1 || len(q["code"]) > 1 || len(q["error"]) > 1 {
				respond(w, http.StatusBadRequest, "tt: invalid authorization callback.")
				send(result{err: errors.New("oauth callback rejected")})
				return
			}
		}
		switch {
		case q.Get("error") != "":
			failure := authorizationFailure(q.Get("error"))
			respond(w, http.StatusOK, "tt: "+failure+". You can close this tab.")
			send(result{err: errors.New(failure)})
		case q.Get("state") != expectedState:
			respond(w, http.StatusBadRequest, "tt: state does not match, rejecting this response. You can close this tab.")
			send(result{err: errors.New("oauth callback: state mismatch")})
		case q.Get("code") == "":
			respond(w, http.StatusBadRequest, "tt: no authorization code in the response. You can close this tab.")
			send(result{err: errors.New("oauth callback: no code")})
		default:
			respond(w, http.StatusOK, "tt: authorized. You can close this tab and return to the terminal.")
			send(result{code: q.Get("code")})
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(listener)
	defer func() {

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-resultCh:
		return res.code, res.err
	case <-ctx.Done():
		return "", fmt.Errorf("timed out waiting for the browser to complete authorization: %w", ctx.Err())
	}
}
