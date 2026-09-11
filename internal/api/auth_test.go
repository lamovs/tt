package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenPath_XDGDataHomeSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	got, err := TokenPath()
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	want := filepath.Join("/custom/data", "ticktick", "token")
	if got != want {
		t.Errorf("TokenPath() = %q, want %q", got, want)
	}
}

func TestTokenPath_XDGDataHomeUnset(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	got, err := TokenPath()
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "ticktick", "token")
	if got != want {
		t.Errorf("TokenPath() = %q, want %q", got, want)
	}
}

func TestLoadTokenFrom_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  abc123\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := LoadTokenFrom(path)
	if err != nil {
		t.Fatalf("LoadTokenFrom: %v", err)
	}
	if info.Value != "abc123" {
		t.Errorf("Value = %q, want %q", info.Value, "abc123")
	}
	if info.Insecure {
		t.Error("Insecure = true for a 0600 file, want false")
	}
}

func TestLoadTokenFrom_InsecurePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("abc123"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := LoadTokenFrom(path)
	if err != nil {
		t.Fatalf("LoadTokenFrom: %v", err)
	}
	if !info.Insecure {
		t.Error("Insecure = false for a 0644 file, want true")
	}
	if info.Value != "abc123" {
		t.Errorf("Value = %q, want %q even though permissions are wide", info.Value, "abc123")
	}
}

func TestLoadTokenFrom_Missing(t *testing.T) {
	_, err := LoadTokenFrom(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected an error for a missing token file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
}
