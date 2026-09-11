package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/api"
)

func putDiskCredentials(t *testing.T, token string, tokenMode, oauthMode os.FileMode, refresh string) (string, string) {
	t.Helper()
	tokenPath, err := api.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), tokenMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokenPath, tokenMode); err != nil {
		t.Fatal(err)
	}
	oauthPath, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	oauth := fmt.Sprintf(`{"client_id":"id","client_secret":"secret","refresh_token":%q}`, refresh)
	if err := os.WriteFile(oauthPath, []byte(oauth), oauthMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(oauthPath, oauthMode); err != nil {
		t.Fatal(err)
	}
	return tokenPath, oauthPath
}

func TestCheckTokenEnvPrecedenceStillInspectsDiskHygiene(t *testing.T) {
	t.Run("missing disk is normal", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "selected-env-token")
		result, token := checkToken()
		if result.status != statusOK || token.value != "selected-env-token" || !token.fromEnv {
			t.Fatalf("result = %v, token source/value = %v/%q", checkReport(result), token.fromEnv, token.value)
		}
	})

	t.Run("bad disk warns without replacing env", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "selected-env-token")
		tokenPath, oauthPath := putDiskCredentials(t, "disk-token", 0o644, 0o644, "refresh")
		if err := os.WriteFile(oauthPath, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		result, token := checkToken()
		if result.status != statusWarn || token.value != "selected-env-token" || !token.fromEnv {
			t.Fatalf("result = %v, token source/value = %v/%q", checkReport(result), token.fromEnv, token.value)
		}
		text := collapsed(noteText(result))
		if !strings.Contains(text, "unused saved app credentials") || !strings.Contains(text, tokenPath) {
			t.Fatalf("notes = %v, want corrupt oauth and saved-token hygiene", result.notes)
		}
	})
}

func TestCheckTokenPermissionBoundaryAndResolvedTargets(t *testing.T) {
	for _, mode := range []os.FileMode{0o500, 0o600, 0o700} {
		t.Run(fmt.Sprintf("safe-%04o", mode), func(t *testing.T) {
			isolate(t)
			putDiskCredentials(t, "saved-token", mode, mode, "refresh")
			result, _ := checkToken()
			if result.status != statusOK {
				t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
			}
		})
	}

	t.Run("group and other bits", func(t *testing.T) {
		isolate(t)
		tokenPath, _ := putDiskCredentials(t, "saved-token", 0o640, 0o604, "refresh")
		dir := filepath.Dir(tokenPath)
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		result, _ := checkToken()
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		text := collapsed(noteText(result))
		for _, want := range []string{"credential directory", "saved token", "saved app credentials"} {
			if !strings.Contains(text, want) {
				t.Errorf("notes = %v, want %q", result.notes, want)
			}
		}
	})

	t.Run("symlink to regular target", func(t *testing.T) {
		isolate(t)
		tokenPath, _ := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
		target := tokenPath + ".target"
		if err := os.Rename(tokenPath, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, tokenPath); err != nil {
			t.Fatal(err)
		}
		result, token := checkToken()
		if result.status != statusOK || token.value != "saved-token" {
			t.Fatalf("result = %v, token = %q", checkReport(result), token.value)
		}
		resolvedTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.summary, resolvedTarget) {
			t.Fatalf("summary = %q, want resolved target %q", result.summary, resolvedTarget)
		}
	})

	t.Run("symlink target directory", func(t *testing.T) {
		isolate(t)
		tokenPath, _ := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
		external := filepath.Join(t.TempDir(), "external")
		if err := os.Mkdir(external, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(external, 0o777); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(external, "token")
		if err := os.Rename(tokenPath, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, tokenPath); err != nil {
			t.Fatal(err)
		}
		result, _ := checkToken()
		if result.status != statusWarn || !strings.Contains(collapsed(noteText(result)), "saved token target directory") {
			t.Fatalf("result = %v, want immediate target-directory warning", checkReport(result))
		}
	})

	t.Run("oauth symlink target directory", func(t *testing.T) {
		isolate(t)
		_, oauthPath := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
		external := filepath.Join(t.TempDir(), "external")
		if err := os.Mkdir(external, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(external, 0o777); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(external, "oauth.json")
		if err := os.Rename(oauthPath, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, oauthPath); err != nil {
			t.Fatal(err)
		}
		result, _ := checkToken()
		if result.status != statusWarn || !strings.Contains(collapsed(noteText(result)), "saved app credentials target directory") {
			t.Fatalf("result = %v, want immediate target-directory warning", checkReport(result))
		}
	})
}

func TestCheckTokenTreatsWhitespaceRefreshAsAbsent(t *testing.T) {
	isolate(t)
	putDiskCredentials(t, "saved-token", 0o600, 0o600, "  \t ")
	result, _ := checkToken()
	if !strings.Contains(collapsed(noteText(result)), "no refresh token saved") {
		t.Fatalf("notes = %v, want whitespace-only refresh treated as absent", result.notes)
	}
}

func TestCheckTokenKeepsPermissionFindingsWhenValuesFail(t *testing.T) {
	t.Run("uninspectable credential directory", func(t *testing.T) {
		isolate(t)
		tokenPath, _ := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
		dir := filepath.Dir(tokenPath)
		if err := os.Chmod(dir, 0o111); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		result, token := checkToken()
		if result.status != statusWarn || token.value != "saved-token" ||
			!strings.Contains(collapsed(noteText(result)), "directory for saved credentials cannot be inspected") {
			t.Fatalf("result = %v, token = %q; want independent directory finding", checkReport(result), token.value)
		}
	})

	t.Run("empty token", func(t *testing.T) {
		isolate(t)
		putDiskCredentials(t, "  \t ", 0o644, 0o644, "refresh")
		result, _ := checkToken()
		text := collapsed(noteText(result))
		if result.status != statusFail || !strings.Contains(text, "saved token") ||
			!strings.Contains(text, "saved app credentials") || strings.Count(text, "mode 0644") != 2 {
			t.Fatalf("result = %v, want both permission findings beside the empty-value failure", checkReport(result))
		}
	})

	t.Run("corrupt oauth through external symlink", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "selected-env-token")
		_, oauthPath := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
		external := filepath.Join(t.TempDir(), "external")
		if err := os.Mkdir(external, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(external, 0o777); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(external, "oauth.json")
		if err := os.WriteFile(target, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(oauthPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, oauthPath); err != nil {
			t.Fatal(err)
		}
		result, token := checkToken()
		text := collapsed(noteText(result))
		for _, want := range []string{"unused saved app credentials", "mode 0644", "saved app credentials target directory"} {
			if !strings.Contains(text, want) {
				t.Errorf("notes = %v, want %q", result.notes, want)
			}
		}
		if result.status != statusWarn || token.value != "selected-env-token" || !token.fromEnv {
			t.Fatalf("result = %v, token source/value = %v/%q", checkReport(result), token.fromEnv, token.value)
		}
	})
}

func TestSavedRefreshAvailabilityTrimsWhitespace(t *testing.T) {
	for value, want := range map[string]bool{"": false, " \t\n ": false, " refresh ": true} {
		if got := hasRefreshToken(value); got != want {
			t.Errorf("hasRefreshToken(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestCheckTokenDoesNotWriteCredentialFiles(t *testing.T) {
	isolate(t)
	tokenPath, oauthPath := putDiskCredentials(t, "saved-token", 0o600, 0o600, "refresh")
	type state struct {
		hash [32]byte
		mode os.FileMode
	}
	read := func(path string) state {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return state{hash: sha256.Sum256(data), mode: fi.Mode().Perm()}
	}
	beforeToken, beforeOAuth := read(tokenPath), read(oauthPath)
	checkToken()
	if after := read(tokenPath); after != beforeToken {
		t.Fatalf("token changed: before=%v after=%v", beforeToken, after)
	}
	if after := read(oauthPath); after != beforeOAuth {
		t.Fatalf("oauth changed: before=%v after=%v", beforeOAuth, after)
	}
}

func TestCheckTokenDoesNotCallADanglingLinkMissing(t *testing.T) {
	isolate(t)
	tokenPath, err := api.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(tokenPath), "absent"), tokenPath); err != nil {
		t.Fatal(err)
	}
	result, _ := checkToken()
	if result.status != statusFail || strings.Contains(result.summary, "token: not found") {
		t.Fatalf("result = %v, want existing unusable link", checkReport(result))
	}
}

func TestCheckTokenEnvReportsUnusableOAuthLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(string) string
	}{
		{"dangling", func(path string) string { return path + ".absent" }},
		{"loop", func(path string) string { return path }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv(tokenEnvVar, "selected-env-token")
			path, err := credentialsPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target(path), path); err != nil {
				t.Fatal(err)
			}
			result, token := checkToken()
			if result.status != statusWarn || token.value != "selected-env-token" ||
				!strings.Contains(collapsed(noteText(result)), "unused saved app credentials") {
				t.Fatalf("result = %v, token = %q", checkReport(result), token.value)
			}
		})
	}
}
