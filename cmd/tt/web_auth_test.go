package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

func TestWebCredentialIsSeparateAndBoundToOpenCredential(t *testing.T) {
	isolate(t)
	const token = "open-token"
	path, err := api.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, token+"\n"); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindCredential(context.Background(), credentialFingerprint(token)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	credentials := webCredentials{Version: 1, Session: "browser-session", OpenFingerprint: credentialFingerprint(token), BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := saveWebCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	webPath, _ := webCredentialsPath()
	if webPath == path {
		t.Fatal("Browser and Open credentials share a file")
	}
	info, err := os.Stat(webPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info, err)
	}
	client, fingerprint, err := webClient(context.Background())
	if err != nil || client == nil || fingerprint != credentialFingerprint(credentials.Session) {
		t.Fatalf("client %v fingerprint %q error %v", client, fingerprint, err)
	}

	credentials.OpenFingerprint = strings.Repeat("a", 64)
	if err := saveWebCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	if _, _, err := webClient(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("binding error = %v", err)
	}
}

func TestWebCredentialRejectsInvalidBindingMetadata(t *testing.T) {
	isolate(t)
	for _, credentials := range []webCredentials{
		{Version: 1, Session: "browser-session", OpenFingerprint: "short", BoundAt: time.Now().UTC().Format(time.RFC3339Nano)},
		{Version: 1, Session: "browser-session", OpenFingerprint: strings.Repeat("a", 64), BoundAt: "not-a-time"},
		{Version: 1, Session: "browser;other=value", OpenFingerprint: strings.Repeat("a", 64), BoundAt: time.Now().UTC().Format(time.RFC3339Nano)},
	} {
		if err := saveWebCredentials(credentials); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := loadWebCredentials(); err == nil || ok {
			t.Fatalf("accepted %+v", credentials)
		}
	}
}
