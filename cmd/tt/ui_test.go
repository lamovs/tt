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

func TestUIDispatchAndNonTerminalRefusalDoNotOpenCache(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"ui"}, exitError, "interactive terminal on stdin and stdout"},
		{[]string{"ui", "extra"}, exitUsage, "takes no arguments"},
		{[]string{"ui", "--sync"}, exitUsage, "unknown option"},
		{[]string{"ui", "--private"}, exitError, "interactive terminal"},
		{[]string{"ui", "--private", "--sync"}, exitUsage, "unknown option"},
		{[]string{"ui", "--help"}, exitOK, "tt ui - "},
		{[]string{"help", "ui"}, exitOK, "tt ui - "},
		{[]string{"ui", "--help", "--color"}, exitOK, "tt ui - "},
		{[]string{"tui"}, exitUsage, "unknown command"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			isolate(t)
			var out, errOut bytes.Buffer
			code := run(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
			if code != tc.code || !strings.Contains(out.String()+errOut.String(), tc.want) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
			if strings.Contains(out.String()+errOut.String(), "\x1b") {
				t.Fatal("redirected invocation emitted terminal escapes")
			}
			path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ticktick")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("dispatch touched the cache: %v", err)
			}
		})
	}
}

func TestUIPrivateModeIsOffUnlessTheFlagAsksForIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		private    bool
		unknown    string
		understood bool
	}{
		{"no options", nil, false, "", true},
		{"private", []string{"--private"}, true, "", true},
		{"private repeated", []string{"--private", "--private"}, true, "", true},
		{"unknown alone", []string{"--sync"}, false, "--sync", false},
		{"unknown after private", []string{"--private", "--quiet"}, false, "--quiet", false},
		{"stray word", []string{"--private", ""}, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			private, unknown, understood := uiRefinements(tc.args)
			if private != tc.private || unknown != tc.unknown || understood != tc.understood {
				t.Fatalf("uiRefinements(%q) = %v, %q, %v; want %v, %q, %v",
					tc.args, private, unknown, understood, tc.private, tc.unknown, tc.understood)
			}
		})
	}
}

func TestUIHelpDescribesPrivateMode(t *testing.T) {
	help := strings.Join(commands["ui"].help.Render(cli.PlainPalette()), "\n")
	for _, want := range []string{"--private", "Ctrl+K"} {
		if !strings.Contains(help, want) {
			t.Fatalf("tt ui help never mentions %q:\n%s", want, help)
		}
	}
}

func TestUIFailureBoundsFinalStderr(t *testing.T) {
	for _, text := range []string{"broken\n\x1b]52;c;data\a", strings.Repeat("long error \u754c", 100)} {
		var stderr bytes.Buffer
		inv := &invocation{verb: "ui", stderr: &stderr}
		if code := inv.uiFailure(errors.New(text)); code != exitError {
			t.Fatalf("failure exited %d", code)
		}
		line := strings.TrimSuffix(stderr.String(), "\n")
		if !strings.HasPrefix(line, "tt: ui: ") || cli.DisplayWidth(line) > cli.Width ||
			strings.ContainsAny(line, "\n\r\x1b\a") {
			t.Fatalf("unsafe final diagnostic: %q", line)
		}
	}
}
