package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/config"
)

func TestClassifySystem(t *testing.T) {
	cases := []struct {
		name       string
		goos       string
		wsl        bool
		notifySend bool
		want       string
	}{
		{"macos", "darwin", false, false, "macos"},
		{"macos wins even with a stray wsl signal", "darwin", true, true, "macos"},
		{"wsl on linux", "linux", true, false, "wsl"},
		{"wsl takes priority over notify-send", "linux", true, true, "wsl"},
		{"linux with notify-send", "linux", false, true, "notify-send"},
		{"plain linux", "linux", false, false, "unknown"},
		{"other os", "windows", false, false, "unknown"},
		{"wsl signal ignored off linux", "windows", true, false, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifySystem(c.goos, c.wsl, c.notifySend); got != c.want {
				t.Errorf("classifySystem(%q, %v, %v) = %q, want %q", c.goos, c.wsl, c.notifySend, got, c.want)
			}
		})
	}
}

func TestNotifyHintsMatchTheSupportedSystems(t *testing.T) {
	deliberatelyMissing := map[string]string{
		"unknown": "there is no system to write a command for",
		"wsl":     "no powershell.exe form has been proven against a real WSL setup",
	}
	for _, sys := range []string{"macos", "notify-send", "wsl", "unknown"} {
		_, has := notifyHints[sys]
		why, missing := deliberatelyMissing[sys]
		switch {
		case has && missing:
			t.Errorf("notifyHints has a command for %q, which must have none: %s", sys, why)
		case !has && !missing:
			t.Errorf("notifyHints is missing an entry for %q", sys)
		}
	}
}

func TestNotifyHintsAreTheOnesTheTemplateShows(t *testing.T) {
	for sys, hint := range notifyHints {
		if !strings.Contains(config.Template(), config.QuoteTOMLString(hint)) {
			t.Errorf("the config template does not show the %s command:\n%s", sys, hint)
		}
	}
}

var hostileTitles = map[string]string{
	"shell substitution":      `$(touch '%s')`,
	"shell backticks":         "`touch '%s'`",
	"shell quote break":       `"; touch '%s'; #`,
	"applescript quote break": `" & (do shell script "touch '%s'") & "`,
	"powershell quote break":  `'; New-Item -ItemType File -Path '%s'; '`,

	"read as an option": `-eproperty p : (do shell script "touch '%s'")`,
}

func TestNotifyHintsDoNotRunTheTaskTitle(t *testing.T) {
	for sys, hint := range notifyHints {
		words := strings.Fields(hint)
		if len(words) == 0 {
			t.Errorf("the hint for %q is empty", sys)
			continue
		}
		prog := words[0]
		for name, title := range hostileTitles {
			t.Run(sys+" "+name, func(t *testing.T) {
				stub := filepath.Join(t.TempDir(), "notifier")
				if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				command := stub + strings.TrimPrefix(hint, prog)
				marker := filepath.Join(t.TempDir(), "executed")
				vars := NotifyVars{
					Kind:     "focus",
					Task:     fmt.Sprintf(title, marker),
					Project:  "Inbox",
					Duration: "25m",
					Cycle:    "1",
				}
				if _, err := RunNotify(context.Background(), command, vars); err != nil {
					t.Fatalf("the hint could not be run at all: %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("the task title ran as code: %s exists (stat: %v)", marker, err)
				}
			})
		}
	}
}

func TestAHostileTitleRunsWhereTheGuardIsMissing(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("osascript is not installed here")
	}
	unguarded := map[string]string{
		"applescript quote break": `osascript -e "return \"$TT_TASK\""`,
		"read as an option":       `osascript -e "on run argv" -e "return item 1 of argv" -e "end run" "$TT_TASK"`,
	}
	for probe, command := range unguarded {
		t.Run(probe, func(t *testing.T) {
			title, ok := hostileTitles[probe]
			if !ok {
				t.Fatalf("the %q probe is gone; there is nothing left to control", probe)
			}
			marker := filepath.Join(t.TempDir(), "executed")
			vars := NotifyVars{Kind: "focus", Task: fmt.Sprintf(title, marker), Project: "Inbox"}
			if _, err := RunNotify(context.Background(), command, vars); err != nil {
				t.Fatalf("the command could not be run at all: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("the title did not run even without the guard (%v): "+
					"the probe no longer demonstrates anything", err)
			}
		})
	}
}
