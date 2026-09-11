package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/cli"
)

func TestSetupHelpAndInvalidArgumentsDoNotInstall(t *testing.T) {
	for _, args := range [][]string{{"setup"}, {"help", "setup"}, {"setup", "--help"}, {"setup", "unknown"}, {"setup", "install", "--remove"}, {"setup", "notifications", "--yes"}, {"setup", "background", "--json"}} {
		var out, stderr bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &stderr)
		if len(args) == 1 || args[0] == "help" || args[1] == "--help" {
			if code != exitOK || !strings.Contains(out.String(), "Optional") && !strings.Contains(out.String(), "Background sync") {
				t.Fatalf("help %v: %d %s %s", args, code, &out, &stderr)
			}
		} else if code != exitUsage {
			t.Fatalf("invalid arguments %v: %d %s %s", args, code, &out, &stderr)
		}
	}
}

func TestSetupFailureEscapesControlsAndWrapsText(t *testing.T) {
	var stderr bytes.Buffer
	inv := &invocation{stderr: &stderr}
	if setupFailure(inv, errors.New("unsafe\x1b[31m\n"+strings.Repeat("failure ", 30))) != exitError {
		t.Fatal("unexpected exit")
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.ContainsRune(line, '\x1b') || cli.DisplayWidth(line) > cli.Width {
			t.Fatalf("unsafe output: %q", line)
		}
	}
}

func TestSetupKeepsStablePathAcrossHomebrewVersions(t *testing.T) {
	root := t.TempDir()
	versioned := filepath.Join(root, "Cellar", "tt", "1.2.3", "bin", "tt")
	if err := os.MkdirAll(filepath.Dir(versioned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versioned, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(bin, "tt")
	if err := os.Symlink(versioned, stable); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got := setupExecutable(versioned); got != stable {
		t.Fatalf("got %q, want stable path %q", got, stable)
	}
	other := filepath.Join(root, "other")
	if err := os.WriteFile(other, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := setupExecutable(other); got != other {
		t.Fatalf("selected another installation: %q", got)
	}
}
