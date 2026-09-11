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
