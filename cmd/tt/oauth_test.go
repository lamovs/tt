package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func withOAuthURLs(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldAuthorize, oldToken := oauthAuthorizeURL, oauthTokenURL
	oauthAuthorizeURL = srv.URL + "/oauth/authorize"
	oauthTokenURL = srv.URL + "/oauth/token"
	t.Cleanup(func() {
		oauthAuthorizeURL, oauthTokenURL = oldAuthorize, oldToken
		srv.Close()
	})
}

func oauthCredentialFixture(letter byte) string {
	return strings.Repeat(string(letter), 18) + `"\`
}

func oauthCredentialForms(value string) []string {
	jsonValue, err := json.Marshal(value)
	if err != nil {
		panic("marshal fixed credential fixture")
	}
	return []string{
		value,
		string(jsonValue[1 : len(jsonValue)-1]),
		url.QueryEscape(value),
		base64.StdEncoding.EncodeToString([]byte(value)),
	}
}

func assertCredentialMaterialAbsent(t *testing.T, got string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(got, value) {
			t.Fatal("OAuth diagnostic exposed credential material")
		}
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	got := buildAuthorizeURL("cid", "http://127.0.0.1:9977/callback", "st4te")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("buildAuthorizeURL produced an unparseable URL: %v", err)
	}
	q := parsed.Query()
	cases := map[string]string{
		"client_id":     "cid",
		"redirect_uri":  "http://127.0.0.1:9977/callback",
		"response_type": "code",
		"scope":         oauthScope,
		"state":         "st4te",
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("query[%q] = %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(got, oauthAuthorizeURL+"?") {
		t.Errorf("URL = %q, want it to start with %q", got, oauthAuthorizeURL+"?")
	}
}

func TestRandomStateIsNotRepeatedOrEmpty(t *testing.T) {
	a, err := randomState()
	if err != nil {
		t.Fatalf("randomState: %v", err)
	}
	b, err := randomState()
	if err != nil {
		t.Fatalf("randomState: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("randomState returned an empty value")
	}
	if a == b {
		t.Fatalf("two calls to randomState returned the same value: %q", a)
	}
}

func TestExchangeCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		assertOAuthClientAuth(t, r, "cid", "csecret")
		want := map[string]string{
			"grant_type":   "authorization_code",
			"code":         "the-code",
			"redirect_uri": "http://127.0.0.1:9977/callback",
		}
		for k, v := range want {
			if got := r.PostForm.Get(k); got != v {
				t.Errorf("form[%q] = %q, want %q", k, got, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok123","token_type":"bearer","refresh_token":"ref456"}`))
	}))
	withOAuthURLs(t, srv)

	tr, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1:9977/callback", "the-code")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if tr.AccessToken != "tok123" || tr.RefreshToken != "ref456" {
		t.Fatalf("tr = %+v, want access_token=tok123 refresh_token=ref456", tr)
	}
}

func TestExchangeCodeNoRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok123","token_type":"bearer"}`))
	}))
	withOAuthURLs(t, srv)

	tr, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1:9977/callback", "the-code")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if tr.AccessToken != "tok123" || tr.RefreshToken != "" {
		t.Fatalf("tr = %+v, want access_token=tok123 and no refresh token", tr)
	}
}

func TestExchangeCodeServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1:9977/callback", "bad-code")
	if err == nil {
		t.Fatal("exchangeCode succeeded against a server error, want an error")
	}
}

func TestExchangeCodeCopiesOnlyStandardTokenErrorCodes(t *testing.T) {
	codes := []string{
		"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope",
	}
	credential := oauthCredentialFixture('A')
	forms := oauthCredentialForms(credential)
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error": code, "error_description": strings.Join(forms, " "),
					"access_token": credential,
				})
			}))
			withOAuthURLs(t, srv)

			_, err := exchangeCode(context.Background(), "id", credential, "http://localhost/cb", credential)
			if err == nil {
				t.Fatal("expected a token endpoint refusal")
			}
			if want := "token request failed: http 400: oauth " + code; err.Error() != want {
				t.Fatal("token endpoint did not report the expected fixed OAuth category")
			}
			assertCredentialMaterialAbsent(t, err.Error(), forms...)
		})
	}
}

func TestExchangeCodeOmitsUnknownAndEncodedTokenErrorText(t *testing.T) {
	first := oauthCredentialFixture('B')
	second := oauthCredentialFixture('C')
	request := func(t *testing.T, credential string, known bool) string {
		t.Helper()
		forms := oauthCredentialForms(credential)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			code := strings.Join(forms, "-")
			if known {
				code = "invalid_grant"
			}
			json.NewEncoder(w).Encode(map[string]string{
				"error": code, "error_description": strings.Join(forms, " "),
				"refresh_token": credential,
			})
		}))
		withOAuthURLs(t, srv)
		_, err := exchangeCode(context.Background(), credential, credential, "http://localhost/cb", credential)
		if err == nil {
			t.Fatal("expected a token endpoint refusal")
		}
		assertCredentialMaterialAbsent(t, err.Error(), forms...)
		return err.Error()
	}

	if a, b := request(t, first, false), request(t, second, false); a != b {
		t.Fatal("unknown token response text changed the safe diagnostic")
	}
	if a, b := request(t, first, true), request(t, second, true); a != b {
		t.Fatal("credential fields changed a recognized OAuth diagnostic")
	}
}

func TestRunCallbackCopiesOnlyStandardAuthorizationErrors(t *testing.T) {
	codes := []string{
		"invalid_request", "unauthorized_client", "access_denied", "unsupported_response_type",
		"invalid_scope", "server_error", "temporarily_unavailable",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			credential := oauthCredentialFixture('D')
			forms := oauthCredentialForms(credential)
			browser, returned, status := runCallbackErrorFixture(t, code, strings.Join(forms, " "), "wrong", credential)
			if status != http.StatusOK {
				t.Fatal("callback error branch changed its HTTP status")
			}
			want := "authorization was not granted (" + code + ")"
			wantBrowser := "tt: " + want + ". You can close this tab."
			if returned != want || browser != wantBrowser {
				t.Fatal("callback did not report the expected fixed authorization category in both sinks")
			}
			assertCredentialMaterialAbsent(t, browser, forms...)
			assertCredentialMaterialAbsent(t, returned, forms...)
		})
	}

	first := oauthCredentialFixture('E')
	second := oauthCredentialFixture('F')
	recognized := func(value string) (string, string) {
		forms := oauthCredentialForms(value)
		returnValues := func() (string, string) {
			browser, returned, status := runCallbackErrorFixture(t, "access_denied", strings.Join(forms, " "), "wrong", value)
			if status != http.StatusOK {
				t.Fatal("callback error branch changed its HTTP status")
			}
			assertCredentialMaterialAbsent(t, browser, forms...)
			assertCredentialMaterialAbsent(t, returned, forms...)
			return browser, returned
		}
		return returnValues()
	}
	firstKnownBrowser, firstKnownReturned := recognized(first)
	secondKnownBrowser, secondKnownReturned := recognized(second)
	if firstKnownBrowser != secondKnownBrowser || firstKnownReturned != secondKnownReturned {
		t.Fatal("freeform callback fields changed a recognized authorization diagnostic")
	}

	run := func(value string) (string, string) {
		forms := oauthCredentialForms(value)
		unknown := strings.Join(forms, "-")
		browser, returned, status := runCallbackErrorFixture(t, unknown, strings.Join(forms, " "), "wrong", value)
		if status != http.StatusOK {
			t.Fatal("callback error branch changed its HTTP status")
		}
		assertCredentialMaterialAbsent(t, browser, forms...)
		assertCredentialMaterialAbsent(t, returned, forms...)
		return browser, returned
	}
	firstBrowser, firstReturned := run(first)
	secondBrowser, secondReturned := run(second)
	if firstBrowser != secondBrowser || firstReturned != secondReturned {
		t.Fatal("unknown callback text changed a safe output")
	}
}

func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func runCallbackErrorFixture(t *testing.T, code, description, state, authCode string) (string, string, int) {
	t.Helper()
	listener := newTestListener(t)
	resultCh := make(chan error, 1)
	go func() {
		_, err := runCallback(context.Background(), listener, "expected-state")
		resultCh <- err
	}()

	query := url.Values{}
	query.Set("error", code)
	query.Set("error_description", description)
	query.Set("state", state)
	query.Set("code", authCode)
	resp, err := http.Get("http://" + listener.Addr().String() + "/callback?" + query.Encode())
	if err != nil {
		t.Fatal("callback fixture request failed")
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal("callback fixture response could not be read")
	}
	returned := <-resultCh
	if returned == nil {
		t.Fatal("callback error branch returned success")
	}
	return string(body), returned.Error(), resp.StatusCode
}

func TestRunCallbackSuccess(t *testing.T) {
	listener := newTestListener(t)
	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	go func() {
		code, err := runCallback(context.Background(), listener, "expected-state")
		resultCh <- struct {
			code string
			err  error
		}{code, err}
	}()

	url := "http://" + listener.Addr().String() + "/callback?code=abc123&state=expected-state"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal("callback success fixture request failed")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	res := <-resultCh
	if res.err != nil {
		t.Fatal("callback success fixture returned an error")
	}
	if res.code != "abc123" {
		t.Fatal("callback did not return the authorization code")
	}
}

func TestRunCallbackStateMismatchIsRejected(t *testing.T) {
	listener := newTestListener(t)
	resultCh := make(chan error, 1)
	go func() {
		_, err := runCallback(context.Background(), listener, "expected-state")
		resultCh <- err
	}()

	url := "http://" + listener.Addr().String() + "/callback?code=abc123&state=wrong-state"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal("callback state fixture request failed")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if err := <-resultCh; err == nil {
		t.Fatal("runCallback accepted a mismatched state, want an error")
	}
}

func TestRunCallbackRejectsAMissingCodeAfterStateMatches(t *testing.T) {
	listener := newTestListener(t)
	resultCh := make(chan error, 1)
	go func() {
		_, err := runCallback(context.Background(), listener, "expected-state")
		resultCh <- err
	}()

	resp, err := http.Get("http://" + listener.Addr().String() + "/callback?state=expected-state")
	if err != nil {
		t.Fatal("callback missing-code fixture request failed")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("missing authorization code did not retain the bad-request status")
	}
	if err := <-resultCh; err == nil || err.Error() != "oauth callback: no code" {
		t.Fatal("missing authorization code did not retain its fixed refusal")
	}
}

func TestRunCallbackUserDenied(t *testing.T) {
	listener := newTestListener(t)
	resultCh := make(chan error, 1)
	go func() {
		_, err := runCallback(context.Background(), listener, "expected-state")
		resultCh <- err
	}()

	url := "http://" + listener.Addr().String() + "/callback?error=access_denied&state=expected-state"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal("callback denial fixture request failed")
	}
	resp.Body.Close()

	err = <-resultCh
	if err == nil {
		t.Fatal("runCallback treated a denial as success, want an error")
	}
	if !strings.Contains(err.Error(), "access_denied") && !strings.Contains(err.Error(), "not granted") {
		t.Fatal("callback denial did not retain its fixed explanation")
	}
}

func TestRunCallbackTimesOut(t *testing.T) {
	listener := newTestListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := runCallback(ctx, listener, "expected-state")
	if err == nil {
		t.Fatal("runCallback did not time out")
	}
}

func TestPostTokenFormRefusesARedirectThatWouldCarryTheBody(t *testing.T) {
	var mu sync.Mutex
	var targetCalls int
	var targetBody string

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		targetCalls++
		targetBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"handed-to-the-wrong-server"}`))
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1/callback", "AUTHCODE-abc123")

	mu.Lock()
	calls, body := targetCalls, targetBody
	mu.Unlock()
	if body != "" {
		t.Error("the redirect target was sent the request body")
	}
	if calls != 0 {
		t.Errorf("the redirect target was called %d times, want 0", calls)
	}

	if err == nil {
		t.Fatal("refreshAccessToken followed the redirect and reported no error")
	}
	if !errors.Is(err, errTokenRedirect) {
		t.Fatalf("err = %v, want it to be errTokenRedirect", err)
	}
}

func TestPostTokenFormRefusesASameHostRedirect(t *testing.T) {
	var mu sync.Mutex
	var elsewhereCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/oauth/token-elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/oauth/token-elsewhere", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhereCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"followed-the-redirect"}`))
	})
	withOAuthURLs(t, httptest.NewServer(mux))

	tr, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1:9977/callback", "the-code")

	mu.Lock()
	calls := elsewhereCalls
	mu.Unlock()
	if calls != 0 {
		t.Errorf("the redirect target was called %d times, want 0", calls)
	}

	if err == nil {
		t.Fatalf("exchangeCode followed a 302 and returned %+v, want an error", tr)
	}
	if !errors.Is(err, errTokenRedirect) {
		t.Fatalf("err = %v, want it to be errTokenRedirect", err)
	}
}

func TestPostTokenFormRefusalOmitsTheTargetAndCredentials(t *testing.T) {
	var mu sync.Mutex
	var targetCalls int
	credential := oauthCredentialFixture('G')
	forms := oauthCredentialForms(credential)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetCalls++
		mu.Unlock()
		w.Write([]byte(`{"access_token":"handed-to-the-wrong-server"}`))
	}))
	defer target.Close()

	targetHost := strings.TrimPrefix(target.URL, "http://")
	location := "http://" + targetHost + "/collect/" + url.PathEscape(forms[len(forms)-1])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), credential, credential, "http://127.0.0.1/callback", credential)

	mu.Lock()
	calls := targetCalls
	mu.Unlock()
	if calls != 0 {
		t.Errorf("the redirect target was called %d times, want 0", calls)
	}

	if err == nil {
		t.Fatal("refreshAccessToken followed the redirect and reported no error")
	}
	if !errors.Is(err, errTokenRedirect) {
		t.Fatal("redirect refusal lost errTokenRedirect identity")
	}
	if err.Error() != "token endpoint: redirect refused" {
		t.Fatal("redirect refusal did not use its fixed diagnostic")
	}
	assertCredentialMaterialAbsent(t, err.Error(), append(forms, location, targetHost)...)
}

func TestPostTokenFormReportsA3xxWithoutALocationAsAnOrdinaryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), "cid", "csecret", "http://127.0.0.1:9977/callback", "the-code")
	if err == nil {
		t.Fatal("exchangeCode read a 307 with no Location as a token, want an error")
	}
	if errors.Is(err, errTokenRedirect) {
		t.Fatalf("err = %v, want a plain failure: the policy is never consulted for a 3xx without a Location", err)
	}
	if !strings.Contains(err.Error(), "http 307") {
		t.Fatalf("err = %v, want it to name the status it got", err)
	}
}

func TestPostTokenFormReportsAnUnparseableLocationAsASafeOrdinaryFailure(t *testing.T) {

	credential := oauthCredentialFixture('H')
	forms := oauthCredentialForms(credential)
	location := "http://attacker.example/%zz?value=" + url.QueryEscape(credential)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), credential, credential, "http://127.0.0.1/callback", credential)
	if err == nil {
		t.Fatal("refreshAccessToken read a 307 with an unparseable Location as a token, want an error")
	}
	if errors.Is(err, errTokenRedirect) {
		t.Fatal("unparseable Location was incorrectly classified as a policy refusal")
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatal("unparseable Location lost net/http error identity")
	}
	assertCredentialMaterialAbsent(t, err.Error(), append(forms, location, oauthTokenURL)...)
}

func TestPostTokenFormKeepsTransportIdentityWithoutPrintingTheEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	withOAuthURLs(t, srv)
	credential := oauthCredentialFixture('I')
	forms := oauthCredentialForms(credential)
	oauthTokenURL += "/" + url.PathEscape(forms[len(forms)-1])
	srv.Close()

	_, err := exchangeCode(context.Background(), credential, credential, "http://localhost/cb", credential)
	if err == nil {
		t.Fatal("exchangeCode reported no error though nothing was listening on the endpoint")
	}
	if errors.Is(err, errTokenRedirect) {
		t.Fatal("ordinary transport failure was classified as a redirect refusal")
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatal("ordinary transport failure lost net/http error identity")
	}
	if urlErr.URL != oauthTokenURL {
		t.Fatal("ordinary transport failure lost its typed endpoint identity")
	}
	assertCredentialMaterialAbsent(t, err.Error(), append(forms, oauthTokenURL)...)
}

func TestPostTokenFormOmitsMalformedSuccessBodyAndKeepsDecodeIdentity(t *testing.T) {
	credential := oauthCredentialFixture('L')
	forms := oauthCredentialForms(credential)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{" + strings.Join(forms, ",")))
	}))
	withOAuthURLs(t, srv)

	_, err := exchangeCode(context.Background(), credential, credential, "http://localhost/cb", credential)
	if err == nil {
		t.Fatal("malformed token success response was accepted")
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatal("safe token decode failure lost JSON error identity")
	}
	if err.Error() != "token endpoint: response decoding failed" {
		t.Fatal("token decode failure did not use its fixed category")
	}
	assertCredentialMaterialAbsent(t, err.Error(), forms...)
}

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type oauthFailingBody struct {
	err error
}

func (b oauthFailingBody) Read([]byte) (int, error) { return 0, b.err }
func (b oauthFailingBody) Close() error             { return nil }

func TestPostTokenFormOmitsResponseReadFailureTextAndKeepsIdentity(t *testing.T) {
	credential := oauthCredentialFixture('M')
	forms := oauthCredentialForms(credential)
	readErr := errors.New(strings.Join(forms, " "))
	previousTransport := http.DefaultTransport
	http.DefaultTransport = oauthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       oauthFailingBody{err: readErr},
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	previousURL := oauthTokenURL
	oauthTokenURL = "https://example.invalid/token"
	t.Cleanup(func() { oauthTokenURL = previousURL })

	_, err := exchangeCode(context.Background(), credential, credential, "http://localhost/cb", credential)
	if err == nil {
		t.Fatal("token response read failure was accepted")
	}
	if !errors.Is(err, readErr) {
		t.Fatal("safe token response read failure lost cause identity")
	}
	if err.Error() != "token endpoint: response read failed" {
		t.Fatal("token response read failure did not use its fixed category")
	}
	assertCredentialMaterialAbsent(t, err.Error(), forms...)
}

type recordingRoundTripper struct {
	inner http.RoundTripper
	mu    sync.Mutex
	calls int
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.calls++
	rt.mu.Unlock()
	return rt.inner.RoundTrip(req)
}

func TestPostTokenFormGoesOutThroughTheDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok123","token_type":"bearer","refresh_token":"ref456"}`))
	}))
	withOAuthURLs(t, srv)

	previous := http.DefaultTransport
	recorder := &recordingRoundTripper{inner: previous}
	http.DefaultTransport = recorder
	t.Cleanup(func() { http.DefaultTransport = previous })

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	tr, err := postTokenForm(context.Background(), "cid", "csecret", form)
	if err != nil {
		t.Fatalf("postTokenForm: %v", err)
	}
	if tr.AccessToken != "tok123" || tr.RefreshToken != "ref456" {
		t.Fatalf("tr = %+v, want access_token=tok123 refresh_token=ref456", tr)
	}

	recorder.mu.Lock()
	calls := recorder.calls
	recorder.mu.Unlock()
	if calls != 1 {
		t.Fatalf("http.DefaultTransport carried %d requests, want 1: the token request went out through a transport of its own", calls)
	}
}
