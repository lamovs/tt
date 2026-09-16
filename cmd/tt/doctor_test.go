package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/sync"
)

func TestCheckTokenEnvVarTakesPriorityOverFile(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "env-supplied-token")

	result, token := checkToken()
	if token.value != "env-supplied-token" {
		t.Fatalf("token = %q, want the value from $%s", token.value, tokenEnvVar)
	}
	if !token.fromEnv {
		t.Fatalf("token.fromEnv = false, want the source of a $%s token recorded", tokenEnvVar)
	}
	if result.status != statusOK {
		t.Fatalf("status = %v, want ok", result.status)
	}
	if !strings.Contains(result.summary, tokenEnvVar) {
		t.Fatalf("summary = %q, want it to mention $%s", result.summary, tokenEnvVar)
	}
}

func TestCheckTokenMissingSuggestsLogin(t *testing.T) {
	isolate(t)
	result, token := checkToken()
	if token.value != "" {
		t.Fatalf("token = %q, want empty when nothing is configured", token.value)
	}
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail", result.status)
	}
	if !strings.Contains(result.summary, "not found") {
		t.Fatalf("summary = %q, want the verdict to say there is no token", result.summary)
	}

	if !slices.Contains(result.notes, doctorIndent+loginHint) {
		t.Fatalf("notes = %v, want %q on a line of its own", result.notes, loginHint)
	}
}

func TestCheckTokenRejectsAnEmptyTokenFile(t *testing.T) {
	for name, content := range map[string]string{
		"no bytes at all": "",
		"only whitespace": "   \n\t\n",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			path := writeTokenFile(t, content)

			result, token := checkToken()
			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			if token.value != "" {
				t.Errorf("token = %q, want nothing handed on to the API check", token.value)
			}
			if !strings.Contains(result.summary, path) {
				t.Errorf("summary = %q, want it to name the file", result.summary)
			}
			if !strings.Contains(result.summary, "empty") {
				t.Errorf("summary = %q, want it to say the file is empty", result.summary)
			}
			if !slices.Contains(result.notes, doctorIndent+loginHint) {
				t.Errorf("notes = %v, want %q under the verdict, on a line of its own", result.notes, loginHint)
			}

			if _, err := syncToken(); err == nil {
				t.Error("syncToken took the file doctor has just failed")
			}
		})
	}
}

func TestCheckTokenReportsUnreadableCredentials(t *testing.T) {
	isolate(t)
	writeTokenFile(t, "a-token\n")
	credPath, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, _ := checkToken()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, credPath) {
		t.Errorf("notes = %v, want the file that cannot be read named", result.notes)
	}
	if strings.Contains(joined, "no refresh token saved") {
		t.Errorf("notes = %v, want no claim about a file doctor could not read", result.notes)
	}

	if _, _, lerr := loadCredentials(); lerr == nil {
		t.Fatal("loadCredentials read the broken file, so there is nothing for the check to report")
	}
}

func TestCheckTokenNotesMissingRefreshToken(t *testing.T) {
	isolate(t)
	if got := run(context.Background(), []string{"login", "token"}, strings.NewReader("a-token\n"), discardWriter{}, discardWriter{}); got != exitOK {
		t.Fatalf("login token setup failed: exit %d", got)
	}

	result, _ := checkToken()
	if result.status != statusOK {
		t.Fatalf("status = %v, want ok for a usable access token: %v", result.status, checkReport(result))
	}
	found := false
	for _, l := range result.notes {
		if strings.Contains(l, "no refresh token saved") {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %v, want a note about no refresh token being saved", result.notes)
	}
	joined := collapsed(noteText(result))
	if !strings.Contains(joined, "once this token stops working") {
		t.Errorf("notes = %v, want the future condition that makes another login necessary", result.notes)
	}
	if !strings.Contains(joined, "another login will be needed") {
		t.Errorf("notes = %v, want the conditional future remedy", result.notes)
	}
	if slices.Contains(result.notes, doctorIndent+loginHint) {
		t.Errorf("notes = %v, want no current login command while the access token is usable", result.notes)
	}
}

func TestOnEndSuggestionIsPasteable(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}

	cases := map[string]string{
		"no timer table":          "",
		"timer table exists":      "[timer]\nfocus = \"25m\"\n",
		"header with spaces":      "[ timer ]\nfocus = \"25m\"\n",
		"quoted header":           "[\"timer\"]\nfocus = \"25m\"\n",
		"literal quoted header":   "['timer']\nfocus = \"25m\"\n",
		"header with a comment":   "[timer] # session lengths\nfocus = \"25m\"\n",
		"a header inside a value": "default_project = \"\"\"\n[timer]\n\"\"\"\n",
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			path := writeConfig(t, existing)

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn for an unset on_end: %v", result.status, checkReport(result))
			}

			pasted := existing + strings.Join(suggestedSetting(t, result), "\n") + "\n"
			if err := os.WriteFile(path, []byte(pasted), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("the suggestion does not load back:\n%s\n%v", pasted, err)
			}
			if cfg.Timer.OnEnd != hint {
				t.Errorf("Timer.OnEnd = %q, want the suggested %q", cfg.Timer.OnEnd, hint)
			}
			if got := cmdConfig(context.Background(), discardWriter{}, discardWriter{}, nil); got != exitOK {
				t.Errorf("tt config = exit %d, want %d, for:\n%s", got, exitOK, pasted)
			}
		})
	}
}

func TestCheckNotifyWarnsAboutAQuotedPlaceholder(t *testing.T) {
	cases := map[string]struct {
		config string

		summary string
		notes   []string
	}{
		"in on_end": {
			config: "[timer]\non_end = 'notify-send \"tt done\" \"{task}\"'\n",
			notes:  []string{"timer.on_end: {task} in quotes", "close and reopen and leave the value bare"},
		},
		"in on_break_end, on_end being fine": {
			config: "[timer]\non_end = 'notify-send -- x {task}'\non_break_end = 'notify-send \"{task}\"'\n",
			notes:  []string{"timer.on_break_end: {task} in quotes"},
		},

		"in single quotes": {
			config: "[timer]\non_end = \"notify-send '{task}'\"\n",
			notes: []string{"timer.on_end: {task} in single quotes",
				"is not a reference at all"},
		},

		"in on_break_end, on_end not set at all": {
			config:  "[timer]\non_break_end = 'notify-send \"{task}\"'\n",
			summary: "timer.on_end is not set",
			notes:   []string{"timer.on_break_end: {task} in quotes"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, "notify-send")
			writeConfig(t, c.config)

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			if c.summary != "" && !strings.Contains(result.summary, c.summary) {
				t.Errorf("summary = %q, want it to carry %q", result.summary, c.summary)
			}
			joined := noteText(result)
			for _, want := range c.notes {
				if !strings.Contains(joined, want) {
					t.Errorf("notes = %v, want one of them to carry %q", result.notes, want)
				}
			}
		})
	}

}

func TestCheckNotifyAcceptsPlaceholdersOutsideQuotes(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "notify-send")
	writeConfig(t, "[timer]\non_end = 'notify-send -- \"tt done\" {task}'\non_break_end = 'notify-send -- x {project}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn for commands tt did not write: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	for _, unwanted := range []string{"in quotes", "option list still open"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("notes = %v, want nothing to remark on %q", result.notes, unwanted)
		}
	}
}

func TestQuotedPlaceholders(t *testing.T) {
	cases := []struct {
		command string
		bare    []string
		single  []string
	}{

		{command: `notify-send "tt done" {task}`},

		{command: `notify-send -- x {task}`},
		{command: `notify-send -- x tt:{task}`},

		{command: `notify-send "{task}"`, bare: []string{"{task}"}},
		{command: `notify-send "{task}" "{task}"`, bare: []string{"{task}"}},
		{command: `notify-send "{task} ({project})"`, bare: []string{"{task}", "{project}"}},
		{command: `notify-send "tt: {kind}" "{project}"`, bare: []string{"{kind}", "{project}"}},

		{command: `dump.sh "tt: {task} done"`, bare: []string{"{task}"}},

		{command: `dump.sh -message "{task}"`, bare: []string{"{task}"}},

		{command: `"{task}" --version`},

		{command: `notify-send '{task}'`, single: []string{"{task}"}},

		{command: `sh -c 'exec dump.sh {task}'`},

		{command: `notify-send "${task}"`},

		{command: `awk "{nonsense}"`},

		{command: `notify-send "done at $(date +%H:%M)" "{task}"`},

		{command: `X\=y "{task}"`, bare: []string{"{task}"}},

		{command: `dump.sh x > "{task}.log"`},
		{command: `TT_LOG="{task}" dump.sh x`},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			bare, single := quotedPlaceholders(c.command)
			if !slices.Equal(bare, c.bare) {
				t.Errorf("quotedPlaceholders(%q) bare = %v, want %v", c.command, bare, c.bare)
			}
			if !slices.Equal(single, c.single) {
				t.Errorf("quotedPlaceholders(%q) single = %v, want %v", c.command, single, c.single)
			}
		})
	}
}

func TestCheckNotifyWithoutAReadyCommand(t *testing.T) {
	isolate(t)
	withoutHints(t)

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, "set timer.on_end manually") {
		t.Errorf("notes = %v, want them to say the setting has to be written by hand", result.notes)
	}
	if !strings.Contains(joined, "$TT_TASK") {
		t.Errorf("notes = %v, want the variables to write it with", result.notes)
	}
}

func notifyPrograms(t *testing.T, names ...string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell to run the probe with: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(sh, filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func slowShell(t *testing.T, names ...string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell to slow down: %v", err)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("nothing to wait with: %v", err)
	}
	dir := notifyPrograms(t, names...)
	wrapper := filepath.Join(dir, "sh")
	if err := os.Remove(wrapper); err != nil {
		t.Fatal(err)
	}
	previous := notifyProbeTimeout
	notifyProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { notifyProbeTimeout = previous })
	seconds := strconv.Itoa(int((notifyProbeTimeout + time.Second).Seconds()))
	script := "#!" + sh + "\n" + sleep + " " + seconds + "\nexec " + sh + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func killedShell(t *testing.T, names ...string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell to kill: %v", err)
	}
	dir := notifyPrograms(t, names...)
	wrapper := filepath.Join(dir, "sh")
	if err := os.Remove(wrapper); err != nil {
		t.Fatal(err)
	}
	script := "#!" + sh + "\nkill -TERM $$\nexec " + sh + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func withoutAShell(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func withoutASessionBus(t *testing.T) {
	t.Helper()
	t.Setenv(busAddressVar, "")
	t.Setenv(runtimeDirVar, "")
}

func withASessionBus(t *testing.T) {
	t.Helper()
	t.Setenv(busAddressVar, "unix:path=/dev/null")
	t.Setenv(runtimeDirVar, "")
}

func withoutHints(t *testing.T) {
	t.Helper()
	previous := notifyHints
	notifyHints = map[string]string{}
	t.Cleanup(func() { notifyHints = previous })
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path, err := api.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func suggestedSetting(t *testing.T, result checkResult) []string {
	t.Helper()
	var block []string
	for _, l := range result.notes {
		if strings.HasPrefix(l, doctorNestedIndent) {
			block = append(block, strings.TrimPrefix(l, doctorNestedIndent))
		}
	}
	if len(block) == 0 {
		t.Fatalf("no setting to copy in %v", result.notes)
	}
	return block
}

func dottedTimerWayOutText() string {
	return collapsed(strings.Join([]string{dottedTimerByHand, dottedTimerSetting, dottedTimerByFix, runFixHint}, " "))
}

func noteText(r checkResult) string {
	return collapsed(strings.Join(r.notes, " "))
}

func checkReport(r checkResult) []string {
	return append(doctorSummary(r.summary), r.notes...)
}

func collapsed(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func isolateInALongPath(t *testing.T) {
	t.Helper()
	isolate(t)
	long := func(part string) string {
		dir := filepath.Join(t.TempDir(), strings.Repeat(part+"-", 3)+"end")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Setenv("XDG_CONFIG_HOME", long("a-rather-deeply-nested-configuration-directory"))
	t.Setenv("XDG_DATA_HOME", long("a-rather-deeply-nested-data-directory"))
}

func doctorLineFits(line string) bool {
	if utf8.RuneCountInString(line) <= cli.Width {
		return true
	}
	if strings.HasPrefix(line, doctorNestedIndent+"on_end = ") {
		return true
	}
	var breakable []string
	for _, word := range strings.Fields(line) {
		if utf8.RuneCountInString(word) > cli.Width {
			continue
		}
		breakable = append(breakable, word)
	}
	indent := utf8.RuneCountInString(line) - utf8.RuneCountInString(strings.TrimLeft(line, " "))
	return indent+utf8.RuneCountInString(strings.Join(breakable, " ")) <= cli.Width
}

func assertDoctorFits(t *testing.T, what, out string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !doctorLineFits(line) {
			t.Errorf("%s line %d is %d columns, want at most %d: %q",
				what, i+1, utf8.RuneCountInString(line), cli.Width, line)
		}
	}
}

func TestDoctorReportStaysInsideTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		seed   func(*testing.T)
	}{
		{name: "no config file at all"},
		{
			name:   "timer settings dotted at the root",
			config: "timer.on_break_end = \"echo break\"\n",
		},
		{
			name: "a command tt did not write",
			config: "[timer]\non_end = \"my-own-notifier '{task}'\"\n" +
				"on_break_end = \"my-own-notifier '{task}'\"\n",
		},
		{
			name:   "a config the parser refuses",
			config: "a_key_with_a_name_nobody_would_ever_choose_for_a_key_in_a_config = \"unterminated\n",
		},
		{
			name:   "the timer table opened twice",
			config: "timer.on_break_end = \"echo break\"\n[timer]\n",
		},
		{
			name: "a row nothing will ever send",
			seed: func(t *testing.T) {
				ctx := context.Background()
				st, err := store.Open(ctx, "")
				if err != nil {
					t.Fatal(err)
				}

				title := strings.Repeat("a task somebody wrote a long name for ", 6)
				if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title}); err != nil {
					t.Fatal(err)
				}

				if _, err := st.DB().ExecContext(ctx, `DELETE FROM outbox`); err != nil {
					t.Fatal(err)
				}
				st.Close()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateInALongPath(t)
			if tc.config != "" {
				writeConfig(t, tc.config)
			}
			if tc.seed != nil {
				tc.seed(t)
			}

			var out strings.Builder
			runDoctorChecks(context.Background(), &out)
			assertDoctorFits(t, "tt doctor", out.String())
		})
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestCheckNotifyAcceptsTheReadyCommands(t *testing.T) {
	for name, c := range map[string]struct {
		command string
		program string
		bus     bool
	}{
		"macOS": {config.NotifyMacOS, notifyFirstWord(config.NotifyMacOS), false},
		"Linux": {config.NotifyLinux, "notify-send", true},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, c.program)
			if c.bus {
				withASessionBus(t)
			} else {
				withoutASessionBus(t)
			}
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(c.command)+"\n"+
				"on_break_end = "+config.QuoteTOMLString(c.command)+"\n")

			result := checkNotify()
			if result.status != statusOK {
				t.Fatalf("status = %v, want ok for a command tt wrote itself whose program is here: %v",
					result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, c.program) {
				t.Errorf("summary = %q, want it to name the program that was looked up", result.summary)
			}

			if want := onEndKey + " and " + onBreakEndKey + " run " + c.program; !strings.Contains(result.summary, want) {
				t.Errorf("summary = %q, want both settings in one sentence with the verb they take: %q",
					result.summary, want)
			}

			joined := noteText(result)
			for _, unwanted := range []string{"in quotes", "option list still open", "cannot check",
				"tt does not read shell commands"} {
				if strings.Contains(joined, unwanted) {
					t.Errorf("notes = %v, want nothing said about the text of tt's own command (%q)",
						result.notes, unwanted)
				}
			}

			if !strings.Contains(joined, "whether one appears on screen") {
				t.Errorf("notes = %v, want the ok to say what it does not answer", result.notes)
			}
			if !slices.Contains(result.notes, doctorIndent+notifyTestHint) {
				t.Errorf("notes = %v, want %q whole on a line of its own", result.notes, notifyTestHint)
			}
		})
	}
}

func TestCheckNotifyRemarksOnAnOpenOptionList(t *testing.T) {
	const remark = "a value goes out with the option list still open"
	cases := map[string]struct {
		config string
		want   []string

		absent []string
	}{
		"in on_end": {
			config: "[timer]\non_end = 'notify-send \"tt done\" $TT_TASK'\n",
			want:   []string{"timer.on_end: " + remark, "takes for an option of its own"},
		},
		"in on_break_end, on_end being the ready command": {
			config: "[timer]\non_end = " + config.QuoteTOMLString(config.NotifyLinux) + "\n" +
				"on_break_end = 'notify-send tt {task}'\n",
			want: []string{"timer.on_break_end: " + remark},
		},
		"a dash inside a word is not the token": {
			config: "[timer]\non_end = 'notify-send --urgency=low $TT_TASK'\n",
			want:   []string{"timer.on_end: " + remark},
		},

		"the \"--\" comes after the value, around a value inside a script": {
			config: "[timer]\non_end = " + config.QuoteTOMLString(`osascript -e "display notification \"$TT_TASK\"" --`) + "\n",
			absent: []string{remark},
		},

		"the notifier takes no \"--\", so the value goes to an option": {
			config: "[timer]\non_end = 'terminal-notifier -title tt -message $TT_TASK'\n",
			absent: []string{remark},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, "notify-send", "osascript", "terminal-notifier")
			withASessionBus(t)
			writeConfig(t, c.config)

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			joined := noteText(result)
			for _, want := range c.want {
				if !strings.Contains(joined, want) {
					t.Errorf("notes = %v, want one of them to carry %q", result.notes, want)
				}
			}
			for _, unwanted := range c.absent {
				if strings.Contains(joined, unwanted) {
					t.Errorf("notes = %v, want nothing to remark %q", result.notes, unwanted)
				}
			}
		})
	}
}

func TestTheNotifyRemarksAreReadOffTheSubstitutedCommand(t *testing.T) {
	const openList = "a value goes out with the option list still open"
	const quoted = "{task} in quotes"
	cases := map[string]struct {
		command string
		want    []string
		absent  []string
	}{

		"a value handed to an option": {
			command: `terminal-notifier -message $TT_TASK`,
			absent:  []string{openList},
		},

		"a separator inside a quoted word": {
			command: `notify-send -- "done: a & b" "$TT_TASK"`,
			absent:  []string{openList},
		},

		"a script handed to another shell": {
			command: `sh -c 'exec dump.sh {task}'`,
			absent:  []string{openList, quoted},
		},

		"a placeholder in the middle of a quoted word": {
			command: `dump.sh "tt: {task} done"`,
			want:    []string{"timer.on_end: " + quoted},
		},

		"a \"--\" inside a word": {
			command: `notify-send "tt -- done" "$TT_TASK"`,
			want:    []string{"timer.on_end: " + openList},
		},

		"a second command in the chain": {
			command: `notify-send -- x "$TT_TASK" ; logger "$TT_PROJECT"`,
			want:    []string{"timer.on_end: " + openList},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, "notify-send", "terminal-notifier", "dump.sh", "logger", "osascript")
			withASessionBus(t)
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(c.command)+"\n")

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			joined := noteText(result)
			for _, want := range c.want {
				if !strings.Contains(joined, want) {
					t.Errorf("notes = %v, want one of them to carry %q", result.notes, want)
				}
			}
			for _, unwanted := range c.absent {
				if strings.Contains(joined, unwanted) {
					t.Errorf("notes = %v, want nothing to remark %q", result.notes, unwanted)
				}
			}
		})
	}
}

func TestCheckNotifySaysNothingAboutACommandItCannotSplit(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "notify-send", "osascript", "date")
	withASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(`notify-send "done at $(date +%H:%M)" "{task}"`)+"\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "cannot check") {
		t.Errorf("summary = %q, want it to say the command was not checked", result.summary)
	}
	joined := noteText(result)
	for _, unwanted := range []string{"in quotes", "option list still open"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("notes = %v, want nothing said about the text of a string tt cannot split: %q",
				result.notes, unwanted)
		}
	}
	if !strings.Contains(joined, "tt does not read shell commands") {
		t.Errorf("notes = %v, want the rule that says what was not read", result.notes)
	}
	if !strings.Contains(joined, "outside --fix's narrow repair") {
		t.Errorf("notes = %v, want the manual way out", result.notes)
	}
}

func TestCheckNotifySummaryNamesTheSettingsItIsAbout(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "notify-send")
	withASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(config.NotifyLinux)+"\n"+
		"on_break_end = 'notify-send tt {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "timer.on_break_end") {
		t.Errorf("summary = %q, want it to name the setting the notes below are about", result.summary)
	}
	if strings.Contains(result.summary, "timer.on_end") {
		t.Errorf("summary = %q, want it to leave out the setting there is nothing to say about", result.summary)
	}
}

func TestCheckNotifyKeepsTtsOwnCommandApartFromOneOfTheUsersOwn(t *testing.T) {
	isolate(t)

	notifyPrograms(t)
	withoutASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(config.NotifyLinux)+"\n"+
		"on_break_end = 'notify-send -- {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, onEndKey+" holds the ready command for "+systemLabel("notify-send")) {
		t.Errorf("summary = %q, want it to speak for the setting that holds tt's own command", result.summary)
	}
	if strings.Contains(result.summary, onBreakEndKey) {
		t.Errorf("summary = %q, want %s left out of a sentence saying tt wrote the command: it did not",
			result.summary, onBreakEndKey)
	}
	joined := noteText(result)
	if !strings.Contains(joined, onBreakEndKey+" runs \"notify-send\"") {
		t.Errorf("notes = %v, want the other setting answered as what it is - somebody's word, quoted back",
			result.notes)
	}
}

func TestCheckNotifyKeepsTwoMissingProgramsApart(t *testing.T) {
	isolate(t)

	notifyPrograms(t)
	writeConfig(t, "[timer]\non_end = 'alpha-notifier -- x'\non_break_end = 'beta-notifier -- x'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, onEndKey+" runs \"alpha-notifier\"") {
		t.Errorf("summary = %q, want the setting read first named with the program it actually runs",
			result.summary)
	}
	if strings.Contains(result.summary, onBreakEndKey) {
		t.Errorf("summary = %q, want %s out of a sentence about another setting's program",
			result.summary, onBreakEndKey)
	}
	joined := noteText(result)
	if !strings.Contains(joined, onBreakEndKey+" runs \"beta-notifier\"") {
		t.Errorf("notes = %v, want the second setting answered under its own program", result.notes)
	}
	if strings.Contains(result.summary, "beta-notifier") {
		t.Errorf("summary = %q, want the program the other setting runs left out of it", result.summary)
	}

	if n := strings.Count(joined, "tt does not read shell commands"); n != 1 {
		t.Errorf("notes = %v, want the rule said once and not %d times", result.notes, n)
	}
}

func TestCheckNotifyDoesNotVouchForACommandItDidNotWrite(t *testing.T) {
	for name, command := range map[string]string{
		"a \"--\" tacked on at the end":   `osascript -e "display notification \"$TT_TASK\" with title \"tt\"" --`,
		"a \"--\" inside a quoted word":   `osascript -e "say -- hi $TT_TASK"`,
		"a \"--\" from another command":   `osascript -e "..." -- x; logger $TT_TASK`,
		"a notifier that takes no \"--\"": `terminal-notifier -title "tt" -message "$TT_TASK"`,
		"a command passing nothing":       `afplay /System/Library/Sounds/Glass.aiff`,
		"a ready command, one word out":   strings.Replace(config.NotifyLinux, "-- ", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, "osascript", "terminal-notifier", "afplay", "notify-send")
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(command)+"\n")

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn for a command tt did not write: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, "cannot check") {
				t.Errorf("summary = %q, want it to say the command was not checked", result.summary)
			}

			if !strings.Contains(noteText(result), "tt does not read shell commands") {
				t.Errorf("notes = %v, want the rule that says what was not read", result.notes)
			}
		})
	}
}

func TestCheckNotifyAcceptsACommandWithNothingToPass(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "afplay", "notify-send")
	writeConfig(t, "[timer]\non_end = 'afplay /System/Library/Sounds/Glass.aiff'\non_break_end = 'notify-send tt \"break over\"'\n")

	result := checkNotify()
	if joined := noteText(result); strings.Contains(joined, "option list still open") {
		t.Errorf("notes = %v, want no such remark on commands that pass no value at all", result.notes)
	}
}

func TestPassesValueWithOptionsOpen(t *testing.T) {
	cases := map[string]bool{

		config.NotifyMacOS: false,
		config.NotifyLinux: false,

		`notify-send "tt done" "$TT_TASK"`:     true,
		`notify-send "tt done" {task}`:         true,
		`notify-send "${TT_TASK}"`:             true,
		`notify-send --urgency=low "$TT_TASK"`: true,

		`notify-send --{task}`:           true,
		`notify-send --title="$TT_TASK"`: false,
		`notify-send -d"$TT_TASK"`:       false,

		`notify-send "${TT_TASK:-no title}"`: false,

		`osascript -e "display notification \"$TT_TASK\"" --`: false,

		`osascript -e "..." -- x; logger $TT_TASK`: true,
		`notify-send -- x | logger $TT_TASK`:       true,
		`logger $TT_TASK; notify-send -- x`:        true,

		`notify-send -- "tt done" "$TT_TASK"`: false,
		"notify-send\t--\t{task}":             false,
		`notify-send -- {task}`:               false,
		`echo done | notify-send -- {task}`:   false,

		`notify-send "tt done"`:   false,
		`echo -- done`:            false,
		`notify-send "$TT_TASKS"`: false,

		`notify-send "\$TT_TASK"`:    false,
		`notify-send "${task}"`:      false,
		`awk "{nonsense}" /dev/null`: false,

		`notify-send -e "say -- hi" "$TT_TASK"`: true,
		`notify-send -- "tt & co" "$TT_TASK"`:   false,

		`dump.sh "--" "$TT_TASK"`:             false,
		`notify-send "tt -- done" "$TT_TASK"`: true,

		`terminal-notifier -title tt -message $TT_TASK`: false,

		`sh -c 'exec dump.sh {task}'`: false,

		`notify-send x "$TT_TASK" --`: true,

		`X\=y "$TT_TASK"`:                          true,
		`dump.sh -message \2>/dev/null "$TT_TASK"`: true,

		"terminal-notifier -message \\\n \"$TT_TASK\"": false,

		"dump.sh > \\\n -m \"$TT_TASK\"": true,

		`notify-send $'--' "$TT_TASK"`: false,
	}
	for command, want := range cases {
		t.Run(command, func(t *testing.T) {
			if got := passesValueWithOptionsOpen(command); got != want {
				t.Errorf("passesValueWithOptionsOpen(%q) = %v, want %v", command, got, want)
			}
		})
	}
}

func TestTheReadyCommandsPassTheCheckOnTheirOwn(t *testing.T) {
	for name, command := range map[string]string{
		"macOS": config.NotifyMacOS,
		"Linux": config.NotifyLinux,
	} {
		t.Run(name, func(t *testing.T) {

			if valueGoesOutWithOptionsOpen(command) {
				t.Errorf("the option list is not ended before the value: %s", command)
			}
		})
	}
}

func TestCheckNotifyWithoutAShell(t *testing.T) {
	const verdict = "there is none on this PATH"
	cases := map[string]struct {
		config string
		named  []string
	}{
		"nothing set at all": {},
		"a command in both settings": {
			config: "[timer]\non_end = 'notify-send -- x'\non_break_end = 'notify-send -- y'\n",
			named:  []string{"timer.on_end", "timer.on_break_end"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			withoutAShell(t)
			if c.config != "" {
				writeConfig(t, c.config)
			}

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, verdict) {
				t.Errorf("summary = %q, want it to carry %q", result.summary, verdict)
			}
			for _, key := range []string{onEndKey, onBreakEndKey} {
				holds := slices.Contains(c.named, key)
				if named := strings.Contains(result.summary, key); named != holds {
					if holds {
						t.Errorf("summary = %q, want it to name %s, which holds a command", result.summary, key)
					} else {
						t.Errorf("summary = %q, want %s left out of it: that key holds nothing",
							result.summary, key)
					}
				}
			}
			if len(c.named) == 0 && strings.Contains(result.summary, "is not set") {
				t.Errorf("summary = %q, want the machine reported rather than the setting", result.summary)
			}
			if len(result.notes) != 0 {
				t.Errorf("notes = %v, want nothing under a verdict with nothing to offer", result.notes)
			}
		})
	}
}

func TestCheckNotifyReportsAReadyCommandThisMachineCannotRun(t *testing.T) {

	readyFor := map[string]string{"macos": "darwin", "notify-send": "linux"}
	for name, c := range map[string]struct {
		command string
		program string
		sys     string
	}{
		"the macOS command": {config.NotifyMacOS, notifyFirstWord(config.NotifyMacOS), "macos"},
		"the Linux command": {config.NotifyLinux, "notify-send", "notify-send"},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)

			notifyPrograms(t)
			withoutASessionBus(t)
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(c.command)+"\n")

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn for a command whose program is not here: %v",
					result.status, checkReport(result))
			}
			for _, want := range []string{c.program, systemLabel(c.sys)} {
				if !strings.Contains(result.summary, want) {
					t.Errorf("summary = %q, want it to carry %q", result.summary, want)
				}
			}
			const (
				contrast     = ", and this is "
				sameSystem   = "does not resolve on this system"
				unrecognised = "a system tt does not recognise"
			)
			if readyFor[c.sys] == runtime.GOOS {
				if !strings.Contains(result.summary, sameSystem) {
					t.Errorf("summary = %q, want %q: this machine is what the command was written for",
						result.summary, sameSystem)
				}
				if strings.Contains(result.summary, contrast) {
					t.Errorf("summary = %q, want nothing about this being another system: it is not",
						result.summary)
				}
			} else if !strings.Contains(result.summary, contrast) {
				t.Errorf("summary = %q, want it to say which system this is as well as which the command is for",
					result.summary)
			}

			spellable := map[string]bool{"darwin": true, "linux": true, "windows": true}
			if spellable[runtime.GOOS] && strings.Contains(result.summary, unrecognised) {
				t.Errorf("summary = %q, want this %s machine named rather than called %q",
					result.summary, runtime.GOOS, unrecognised)
			}
		})
	}
}

func TestReadyCommandNoticeNamesTheMachineHonestly(t *testing.T) {

	programs := map[string]string{
		"macos":       notifyFirstWord(config.NotifyMacOS),
		"notify-send": notifyFirstWord(config.NotifyLinux),
	}
	cases := map[string]struct {
		sys      string
		machine  notifyMachine
		want     []string
		unwanted []string
	}{
		"the Linux command on a Linux box with no notify-send": {
			sys:      "notify-send",
			machine:  notifyMachine{system: "unknown", goos: "linux"},
			want:     []string{"the ready command for Linux (notify-send)", "does not resolve on this system"},
			unwanted: []string{"a system tt does not recognise", ", and this is "},
		},
		"the macOS command on that same box": {
			sys:      "macos",
			machine:  notifyMachine{system: "unknown", goos: "linux"},
			want:     []string{"the ready command for macOS", ", and this is Linux: ", "does not resolve here"},
			unwanted: []string{"a system tt does not recognise"},
		},
		"the macOS command on a Linux box that has notify-send": {
			sys:      "macos",
			machine:  notifyMachine{system: "notify-send", goos: "linux"},
			want:     []string{"the ready command for macOS", ", and this is Linux: ", "does not resolve here"},
			unwanted: []string{"(notify-send)"},
		},
		"the Linux command on Windows": {
			sys:      "notify-send",
			machine:  notifyMachine{system: "unknown", goos: "windows"},
			want:     []string{", and this is Windows: "},
			unwanted: []string{"a system tt does not recognise"},
		},
		"the Linux command on a Mac": {
			sys:     "notify-send",
			machine: notifyMachine{system: "macos", goos: "darwin"},
			want:    []string{"the ready command for Linux (notify-send)", ", and this is macOS: "},
		},
		"the macOS command on a Mac that has lost its notifier": {
			sys:      "macos",
			machine:  notifyMachine{system: "macos", goos: "darwin"},
			want:     []string{"does not resolve on this system"},
			unwanted: []string{", and this is "},
		},
		"the Linux command under WSL": {
			sys:     "notify-send",
			machine: notifyMachine{system: "wsl", goos: "linux"},
			want:    []string{", and this is WSL: "},
		},
		"an operating system tt cannot spell": {
			sys:     "notify-send",
			machine: notifyMachine{system: "unknown", goos: "plan9"},
			want:    []string{", and this is a system tt does not recognise: "},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := readyCommandNotice(onEndKey, false, c.sys, programs[c.sys], c.machine)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice = %q, want it to carry %q", got, want)
				}
			}
			for _, unwanted := range c.unwanted {
				if strings.Contains(got, unwanted) {
					t.Errorf("notice = %q, want nothing of %q in it", got, unwanted)
				}
			}
		})
	}
}

func TestNoReadyCommandNoticeSaysWhyThereIsNothingToOffer(t *testing.T) {
	const plain = "no ready command for this system"
	cases := map[string]struct {
		machine  notifyMachine
		want     string
		unwanted string
	}{
		"a Linux box with no notify-send": {
			machine:  notifyMachine{system: "unknown", goos: "linux"},
			want:     "needs notify-send",
			unwanted: plain,
		},
		"WSL, where no command has been written": {
			machine: notifyMachine{system: "wsl", goos: "linux"},
			want:    plain,
		},
		"Windows, which has no command either": {
			machine: notifyMachine{system: "unknown", goos: "windows"},
			want:    plain,
		},
		"an operating system tt knows nothing about": {
			machine: notifyMachine{system: "unknown", goos: "plan9"},
			want:    plain,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := noReadyCommandNotice(c.machine)
			if !strings.Contains(got, c.want) {
				t.Errorf("notice = %q, want it to carry %q", got, c.want)
			}
			if c.unwanted != "" && strings.Contains(got, c.unwanted) {
				t.Errorf("notice = %q, want nothing of %q in it: there is a command for this system",
					got, c.unwanted)
			}
		})
	}
}

func TestCheckNotifySeesTheSessionBusNotifySendNeeds(t *testing.T) {
	cases := map[string]struct {
		bus  func(*testing.T)
		want checkStatus
	}{
		"no bus named at all": {withoutASessionBus, statusWarn},
		"an address in the environment": {func(t *testing.T) {
			withASessionBus(t)
		}, statusOK},
		"a socket under the runtime directory": {func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "bus"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(busAddressVar, "")
			t.Setenv(runtimeDirVar, dir)
		}, statusOK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t, "notify-send")
			c.bus(t)
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(config.NotifyLinux)+"\n")

			result := checkNotify()
			if result.status != c.want {
				t.Fatalf("status = %v, want %v: %v", result.status, c.want, checkReport(result))
			}
			if c.want != statusWarn {
				return
			}
			for _, want := range []string{busAddressVar, runtimeDirVar} {
				if !strings.Contains(result.summary, want) {
					t.Errorf("summary = %q, want it to say what was looked for: %s", result.summary, want)
				}
			}

			var offered bool
			for _, l := range result.notes {
				if strings.HasPrefix(l, doctorNestedIndent) {
					offered = true
				}
			}
			if suits := readyCommandSuitsMachine("notify-send", thisMachine()); suits && offered {
				t.Errorf("notes = %v, want no block to copy where the config already holds this machine's command",
					result.notes)
			} else if !suits && !offered {
				t.Errorf("notes = %v, want the command for this machine under a config written for another",
					result.notes)
			}
		})
	}
}

func TestCheckNotifyNamesAProgramOfTheirOwnThatIsNotThere(t *testing.T) {
	isolate(t)
	notifyPrograms(t)
	writeConfig(t, "[timer]\non_end = 'my-own-notifier -- \"$TT_TASK\"'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	for _, want := range []string{"my-own-notifier", "does not resolve"} {
		if !strings.Contains(result.summary, want) {
			t.Errorf("summary = %q, want it to carry %q", result.summary, want)
		}
	}

	if !strings.Contains(noteText(result), "tt does not read shell commands") {
		t.Errorf("notes = %v, want the rule that says what was not read", result.notes)
	}

	if _, ok := notifyHints[detectSystem()]; !ok {
		return
	}
	if !strings.Contains(noteText(result), "outside --fix's narrow repair") {
		t.Errorf("notes = %v, want the manual way out underneath", result.notes)
	}
}

func TestCheckNotifyTakesTheShellsWordForABuiltin(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "my-own-notifier")
	writeConfig(t, "[timer]\non_end = 'exec my-own-notifier -- {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn for a command tt did not write: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "cannot check") {
		t.Errorf("summary = %q, want the unchecked verdict and no accusation", result.summary)
	}
	if strings.Contains(result.summary+noteText(result), "does not resolve") {
		t.Errorf("report = %v / %q, want nothing said to be missing: exec is a builtin",
			result.notes, result.summary)
	}
}

func TestNotifyProbeTellsSilenceFromAnAnswer(t *testing.T) {
	t.Run("a word the shell finds", func(t *testing.T) {
		notifyPrograms(t, "my-own-notifier")
		if got := probeWord("my-own-notifier"); got != wordResolves {
			t.Errorf("probeWord = %v, want it to resolve: the program is on PATH", got)
		}
	})
	t.Run("a word the shell does not find", func(t *testing.T) {
		notifyPrograms(t)
		if got := probeWord("my-own-notifier"); got != wordDoesNotResolve {
			t.Errorf("probeWord = %v, want the one answer that is about the config", got)
		}
	})
	t.Run("a shell that does not come back", func(t *testing.T) {
		slowShell(t, "my-own-notifier")
		if got := probeWord("my-own-notifier"); got != probeUnanswered {
			t.Errorf("probeWord = %v, want no answer at all: the program is on PATH and the shell never said so",
				got)
		}
	})
	t.Run("a shell something kills", func(t *testing.T) {
		killedShell(t, "my-own-notifier")
		if got := probeWord("my-own-notifier"); got != probeUnanswered {
			t.Errorf("probeWord = %v, want no answer at all: the shell was killed before it looked anything up",
				got)
		}
	})
}

func TestCheckNotifySaysNothingItCouldNotLookUp(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)
	slowShell(t, notifyFirstWord(hint))
	withASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(hint)+"\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn where nothing was established: %v", result.status, checkReport(result))
	}
	if strings.Contains(result.summary, "does not resolve") {
		t.Errorf("summary = %q, want no finding about a program that is on PATH", result.summary)
	}
	if !strings.Contains(result.summary, "unanswered") {
		t.Errorf("summary = %q, want it to say the question went unanswered", result.summary)
	}

	joined := noteText(result)
	if !strings.Contains(joined, "nothing was established about the command here") {
		t.Errorf("notes = %v, want the note to say what the summary above it says", result.notes)
	}
	if strings.Contains(joined, "what resolves is the program") {
		t.Errorf("notes = %v, want no claim of resolution under a verdict that established none", result.notes)
	}
	if !slices.Contains(result.notes, doctorIndent+notifyTestHint) {
		t.Errorf("notes = %v, want %q whole on a line of its own", result.notes, notifyTestHint)
	}
}

func TestNotifyOutcomeForSaysNothingAboutAUsersWordItCouldNotLookUp(t *testing.T) {
	for name, shell := range map[string]func(*testing.T, ...string){
		"a shell that does not come back": slowShell,
		"a shell something kills":         killedShell,
	} {
		t.Run(name, func(t *testing.T) {
			shell(t, "my-own-notifier")

			out := notifyOutcomeFor(notifyCommand{onEndKey, "my-own-notifier -- {task}"})
			if out.kind != notifyUnchecked {
				t.Errorf("kind = %v, want the command left unchecked: nothing was looked up", out.kind)
			}
			if out.program != "" {
				t.Errorf("program = %q, want no program named by an outcome that established none", out.program)
			}
		})
	}
}

func TestCheckNotifySaysNothingAboutAUsersProgramItCouldNotLookUp(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)
	slowShell(t, notifyFirstWord(hint), "my-own-notifier")
	withASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(hint)+"\n"+
		"on_break_end = 'my-own-notifier -- {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn where nothing was established: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "unanswered") {
		t.Errorf("summary = %q, want the unanswered lookup to take it", result.summary)
	}
	joined := noteText(result)
	if strings.Contains(result.summary+joined, "does not resolve") {
		t.Errorf("report = %q / %v, want no program accused: both are on this PATH", result.summary, result.notes)
	}
	if !strings.Contains(joined, onBreakEndKey+" runs a command tt did not write") {
		t.Errorf("notes = %v, want the user's command left where an unlooked-up word sits", result.notes)
	}
}

func TestProbeWordPutsTheWordToTheShellAsAValue(t *testing.T) {
	notifyPrograms(t)
	marker := filepath.Join(t.TempDir(), "ran")

	if got := probeWord("true; : > " + marker); got != wordDoesNotResolve {
		t.Errorf("probeWord = %v, want the whole of it looked up as one word", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the probe ran what came after the %q (stat: %v), want the word passed as a value", ";", err)
	}
}

func TestNotifyFirstWordIgnoresLeadingBlanks(t *testing.T) {
	const want = "notify-send"
	for name, command := range map[string]string{
		"a space in front":               " notify-send -- {task}",
		"a tab in front":                 "\tnotify-send -- {task}",
		"several of both":                "  \t notify-send -- {task}",
		"nothing but the word":           "  notify-send",
		"nothing to trim at all":         "notify-send -- {task}",
		"a tab between it and the rest":  "notify-send\t-- {task}",
		"a tab in front and another one": "\tnotify-send\t-- {task}",
	} {
		t.Run(name, func(t *testing.T) {
			if got := notifyFirstWord(command); got != want {
				t.Errorf("notifyFirstWord(%q) = %q, want %q", command, got, want)
			}
		})
	}
}

func TestCheckNotifySaysNothingAboutAFirstWordThatIsNotAName(t *testing.T) {
	for name, command := range map[string]string{
		"a variable holding the program": `$NOTIFIER -- "$TT_TASK"`,
		"a quoted program":               `"my notifier" -- "$TT_TASK"`,
		"an assignment in front":         `FOO=bar my-own-notifier -- "$TT_TASK"`,
		"a path of the user's own":       `./notify.sh -- "$TT_TASK"`,
		"a placeholder":                  `{task}`,
		"nothing but blanks":             "   ",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			notifyPrograms(t)
			writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(command)+"\n")

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, "cannot check") {
				t.Errorf("summary = %q, want the unchecked verdict", result.summary)
			}
			if strings.Contains(result.summary+noteText(result), "does not resolve") {
				t.Errorf("report = %v / %q, want no finding about a word that was never looked up",
					result.notes, result.summary)
			}
		})
	}
}

func TestCheckNotifyReportsOnBreakEndUnderAnUnsetOnEnd(t *testing.T) {
	isolate(t)
	notifyPrograms(t)
	writeConfig(t, "[timer]\non_break_end = 'my-own-notifier -- {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	for _, want := range []string{"timer.on_break_end", "my-own-notifier"} {
		if !strings.Contains(result.summary, want) {
			t.Errorf("summary = %q, want it to carry %q", result.summary, want)
		}
	}
	joined := noteText(result)
	if !strings.Contains(joined, "timer.on_end is not set") {
		t.Errorf("notes = %v, want the setting that is missing reported under the verdict", result.notes)
	}
	if _, ok := notifyHints[detectSystem()]; !ok {
		return
	}
	if !strings.Contains(joined, "suggested for "+systemLabel(detectSystem())) {
		t.Errorf("notes = %v, want the unset setting to keep the suggestion it always had", result.notes)
	}

	if !strings.Contains(joined, onBreakEndKey+" has to be set by hand") {
		t.Errorf("notes = %v, want the setting --fix does not write named as one for the reader", result.notes)
	}
	for _, unwanted := range []string{"already gives that key a value", "defines it twice"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("notes = %v, want nothing said about a value on_end has not got (%q)",
				result.notes, unwanted)
		}
	}
}

func TestCheckNotifyOffersNothingToPasteUnderASettingFixDoesNotWrite(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)

	notifyPrograms(t, notifyFirstWord(hint), "my-own-notifier")
	withASessionBus(t)
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(hint)+"\n"+
		"on_break_end = 'my-own-notifier -- {task}'\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "timer.on_break_end") {
		t.Errorf("summary = %q, want it to name the setting complained about", result.summary)
	}
	joined := noteText(result)
	if !strings.Contains(joined, "--fix writes on_end and no other key") {
		t.Errorf("notes = %v, want them to say what --fix will and will not do here", result.notes)
	}

	if !strings.Contains(joined, "already gives that key a value") {
		t.Errorf("notes = %v, want the reason nothing is offered to paste", result.notes)
	}
	for _, l := range result.notes {
		if strings.HasPrefix(l, doctorNestedIndent) {
			t.Fatalf("notes = %v, want nothing to paste under a setting --fix does not write", result.notes)
		}
	}
}

func TestNotifyWayOutSpeaksForTheSettingItStandsUnder(t *testing.T) {
	report := notifyReport{machine: notifyMachine{system: "macos", goos: "darwin"}, onEndSet: true}
	for name, c := range map[string]struct {
		kind notifyOutcomeKind
		keys []string
		hint bool
	}{
		"a lookup that never came back, under on_end":       {notifyUnanswered, []string{onEndKey}, true},
		"a lookup that never came back, under on_break_end": {notifyUnanswered, []string{onBreakEndKey}, false},
		"a program that resolves, under on_end":             {notifyResolves, []string{onEndKey}, true},
		"a program that resolves, under on_break_end":       {notifyResolves, []string{onBreakEndKey}, false},
		"both settings in one verdict":                      {notifyUnanswered, []string{onEndKey, onBreakEndKey}, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := notifyGroup{
				notifySettingOutcome: notifySettingOutcome{
					key: c.keys[0], kind: c.kind, sys: "macos", ready: true,
					program: notifyFirstWord(config.NotifyMacOS),
				},
				keys: c.keys,
			}
			lines := notifyWayOutLines(g, report)
			joined := collapsed(strings.Join(lines, " "))
			if got := slices.Contains(lines, doctorIndent+notifyTestHint); got != c.hint {
				if c.hint {
					t.Fatalf("lines = %v, want %q whole on a line of its own", lines, notifyTestHint)
				}
				t.Fatalf("lines = %v, want no command that would answer for another setting", lines)
			}
			if c.hint {
				return
			}

			for _, want := range []string{"runs what " + onEndKey + " holds and nothing else",
				"putting this command there for a run"} {
				if !strings.Contains(joined, want) {
					t.Errorf("lines = %v, want them to carry %q", lines, want)
				}
			}
		})
	}
}

func TestTheWayOutOfAMissingBusFollowsWhoseCommandItIs(t *testing.T) {
	g := notifyGroup{
		notifySettingOutcome: notifySettingOutcome{
			key: onEndKey, kind: notifyNoBus, sys: "notify-send", ready: true,
			program: notifyFirstWord(config.NotifyLinux),
		},
		keys: []string{onEndKey},
	}
	for name, c := range map[string]struct {
		machine notifyMachine
		want    string
	}{
		"on the Linux box the command was written for": {notifyMachine{system: "notify-send", goos: "linux"}, ""},
		"on a Mac, which has a command of its own":     {notifyMachine{system: "macos", goos: "darwin"}, "the ready command for macOS"},
	} {
		t.Run(name, func(t *testing.T) {
			lines := notifyWayOutLines(g, notifyReport{machine: c.machine, onEndSet: true})
			if c.want == "" {
				if len(lines) != 0 {
					t.Fatalf("lines = %v, want nothing under a verdict whose only answer is already in the file", lines)
				}
				return
			}
			if !strings.Contains(collapsed(strings.Join(lines, " ")), c.want) {
				t.Fatalf("lines = %v, want them to carry %q", lines, c.want)
			}
		})
	}
}

func TestReadyCommandLinesAnswerForTheMachineTheyAreGiven(t *testing.T) {
	const (
		fixWrites  = "--fix writes on_end and no other key"
		fixPuts    = "--fix puts it in on_end"
		repairPuts = "--fix preserves the current timer.on_end command"
	)
	macOS := notifyMachine{system: "macos", goos: "darwin"}
	for name, c := range map[string]struct {
		keys     []string
		report   notifyReport
		want     []string
		unwanted []string
		block    bool
		silent   bool
	}{
		"a machine tt has no command for": {
			keys:     []string{onEndKey},
			report:   notifyReport{machine: notifyMachine{system: "unknown", goos: "windows"}},
			want:     []string{"no ready command for this system", "$TT_TASK"},
			unwanted: []string{fixPuts, fixWrites},
		},
		"a Linux box with no notify-send": {
			keys:     []string{onEndKey},
			report:   notifyReport{machine: notifyMachine{system: "unknown", goos: "linux"}},
			want:     []string{"needs notify-send", "there is none on this PATH"},
			unwanted: []string{"no ready command for this system", fixPuts},
		},
		"a Mac whose notifier is not on PATH": {
			keys:   []string{onEndKey},
			report: notifyReport{machine: macOS, onEndSet: true, readyProgramMissing: true},
			want: []string{"the ready command for macOS needs " + notifyFirstWord(config.NotifyMacOS), "does not resolve here",
				"no setting supplies it"},
			unwanted: []string{fixPuts, fixWrites},
		},

		"the same Mac, with on_end not set": {
			keys:   []string{onBreakEndKey},
			report: notifyReport{machine: macOS, readyProgramMissing: true},
			silent: true,
		},
		"a config whose timer keys stand at the root": {
			keys:     []string{onEndKey},
			report:   notifyReport{machine: macOS, onEndSet: true, src: []byte("timer.on_end = \"echo done\"\n")},
			want:     []string{"at the root", dottedTimerWayOutText()},
			unwanted: []string{fixPuts},
		},
		"unsupported custom on unknown system keeps the generated-command gate": {
			keys: []string{onEndKey},
			report: notifyReport{
				machine:  notifyMachine{system: "unknown", goos: "windows"},
				onEndSet: true, onEndCommand: `custom-recorder -- "prefix {task}"`,
				src: []byte("timer.on_end = 'custom-recorder'\n"),
			},
			want:     []string{"no ready command for this system"},
			unwanted: []string{"there is a ready command", "supported whole-word quote repair"},
		},
		"a setting --fix does not write": {
			keys:   []string{onBreakEndKey},
			report: notifyReport{machine: macOS, onEndSet: true},
			want: []string{fixWrites, onBreakEndKey + " has to be set by hand",
				"already gives that key a value"},
			unwanted: []string{fixPuts},
		},
		"on_end, which is the key --fix writes": {
			keys:     []string{onEndKey},
			report:   notifyReport{machine: macOS, onEndSet: true, onEndCommand: `custom-recorder -- "{task}"`},
			want:     []string{repairPuts},
			unwanted: []string{fixPuts, fixWrites, "has to be set by hand", "on_end = ", "custom-recorder"},
		},
		"supported custom repair needs no generated command": {
			keys: []string{onEndKey},
			report: notifyReport{
				machine:  notifyMachine{system: "unknown", goos: "windows"},
				onEndSet: true, onEndCommand: `custom-recorder -- "{task}"`,
			},
			want:     []string{repairPuts, "tt doctor --fix", "complete escaped old and repaired values"},
			unwanted: []string{"no ready command", "on_end = ", "custom-recorder"},
		},
		"supported custom repair still honors a refused write probe": {
			keys: []string{onEndKey},
			report: notifyReport{
				machine:  notifyMachine{system: "unknown", goos: "windows"},
				onEndSet: true, onEndCommand: `custom-recorder -- "{task}"`, writeRefused: true,
			},
			want:     []string{"supported whole-word quote repair", "write probe was refused"},
			unwanted: []string{"no ready command", "tt doctor --fix", "on_end = "},
		},
		"supported custom repair still honors dotted timer keys": {
			keys: []string{onEndKey},
			report: notifyReport{
				machine:  notifyMachine{system: "unknown", goos: "windows"},
				onEndSet: true, onEndCommand: `custom-recorder -- "{task}"`,
				src: []byte("timer.on_end = 'custom-recorder -- \\\"{task}\\\"'\n"),
			},
			want:     []string{"supported whole-word quote repair", "at the root", "--fix refuses"},
			unwanted: []string{"no ready command"},
		},
		"one verdict over both settings": {
			keys:   []string{onEndKey, onBreakEndKey},
			report: notifyReport{machine: macOS, onEndSet: true, onEndCommand: `custom-recorder -- "{task}"`},
			want:   []string{repairPuts, fixWrites, onBreakEndKey + " has to be set by hand"},

			unwanted: []string{fixPuts, onEndKey + " has to be set by hand", "on_end = ", "custom-recorder"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			lines := readyCommandLines(c.keys, c.report)
			if c.silent {
				if len(lines) != 0 {
					t.Fatalf("lines = %v, want nothing said twice in one report", lines)
				}
				return
			}
			joined := collapsed(strings.Join(lines, " "))
			for _, want := range c.want {
				if !strings.Contains(joined, want) {
					t.Errorf("lines = %v, want them to carry %q", lines, want)
				}
			}
			for _, unwanted := range c.unwanted {
				if strings.Contains(joined, unwanted) {
					t.Errorf("lines = %v, want nothing of %q in them", lines, unwanted)
				}
			}
			var block bool
			for _, l := range lines {
				if strings.HasPrefix(l, doctorNestedIndent) {
					block = true
				}
			}
			if block != c.block {
				if c.block {
					t.Errorf("lines = %v, want the setting spelled out to be copied", lines)
				} else {
					t.Errorf("lines = %v, want nothing offered for copying here", lines)
				}
			}
		})
	}
}

func TestSupportedCustomRepairAdvicePrintsNoCommandBytes(t *testing.T) {
	hostile := "custom-recorder fixed-\u0085\u202e\x1b-tail -- \"{task}\""
	if _, changed, err := repairOnEndCommand(hostile); err != nil || !changed {
		t.Fatalf("fixture is not a supported custom repair: changed=%v err=%v", changed, err)
	}
	lines := readyCommandLines([]string{onEndKey}, notifyReport{
		machine:      notifyMachine{system: "unknown", goos: "windows"},
		onEndSet:     true,
		onEndCommand: hostile,
	})
	var out strings.Builder
	printCheck(&out, checkResult{status: statusWarn, summary: "notify: timer.on_end needs repair", notes: lines})
	printed := out.String()
	if strings.ContainsAny(printed, "\u0085\u202e\x1b") {
		t.Fatalf("final doctor output contains raw custom-command controls: %q", printed)
	}
	for _, want := range []string{"--fix preserves", "removes only", "tt doctor --fix", "complete escaped old and repaired values"} {
		if !strings.Contains(printed, want) {
			t.Errorf("final doctor output = %q, want accurate repair advice %q", printed, want)
		}
	}
	for _, unwanted := range []string{"custom-recorder", "on_end = ", "no ready command"} {
		if strings.Contains(printed, unwanted) {
			t.Errorf("final doctor output = %q, want no %q", printed, unwanted)
		}
	}
}

func TestTheSuggestionUnderAnUnsetOnEndAnswersForTheMachine(t *testing.T) {
	unset := notifyGroup{
		notifySettingOutcome: notifySettingOutcome{key: onEndKey, kind: notifyUnset},
		keys:                 []string{onEndKey},
	}
	macOS := notifyMachine{system: "macos", goos: "darwin"}
	for name, c := range map[string]struct {
		report   notifyReport
		want     []string
		unwanted []string
		block    bool
	}{
		"a machine tt has no command for": {
			report:   notifyReport{machine: notifyMachine{system: "unknown", goos: "windows"}},
			want:     []string{"no ready command for this system", "set timer.on_end manually", "$TT_TASK"},
			unwanted: []string{"suggested for", runFixHint},
		},
		"a Mac whose notifier is not on PATH": {
			report: notifyReport{machine: macOS, readyProgramMissing: true},
			want: []string{"the ready command for macOS needs " + notifyFirstWord(config.NotifyMacOS), "does not resolve here",
				"set timer.on_end manually", "$TT_TASK"},
			unwanted: []string{"suggested for", runFixHint},
		},
		"a Mac with everything it needs": {
			report: notifyReport{machine: macOS},
			want:   []string{"suggested for macOS", runFixHint},
			block:  true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			lines := notifyOutcomeLines(unset, c.report)
			joined := collapsed(strings.Join(lines, " "))
			for _, want := range c.want {
				if !strings.Contains(joined, want) {
					t.Errorf("lines = %v, want them to carry %q", lines, want)
				}
			}
			for _, unwanted := range c.unwanted {
				if strings.Contains(joined, unwanted) {
					t.Errorf("lines = %v, want nothing of %q in them", lines, unwanted)
				}
			}
			var block bool
			for _, l := range lines {
				if strings.HasPrefix(l, doctorNestedIndent) {
					block = true
				}
			}
			if block != c.block {
				if c.block {
					t.Errorf("lines = %v, want the setting spelled out to be copied", lines)
				} else {
					t.Errorf("lines = %v, want nothing offered for copying where it cannot run", lines)
				}
			}
		})
	}
}

func TestCheckNotifyDoesNotOfferACommandItHasJustCalledBroken(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	for name, content := range map[string]string{
		"the setting holds it": "[timer]\non_end = " + config.QuoteTOMLString(hint) + "\n",
		"another setting holds it and on_end is empty": "[timer]\non_break_end = " +
			config.QuoteTOMLString(hint) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)

			notifyPrograms(t)
			withoutASessionBus(t)
			writeConfig(t, content)

			result := checkNotify()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			joined := noteText(result)

			if n := strings.Count(joined, "does not resolve here, so there is none to offer"); n != 1 {
				t.Errorf("notes = %v, want the reason nothing is offered said once and not %d times",
					result.notes, n)
			}
			if !strings.Contains(joined, notifyFirstWord(hint)) {
				t.Errorf("notes = %v, want the program that is missing named", result.notes)
			}
			for _, unwanted := range []string{runFixHint, "--fix puts it in on_end", "suggested for "} {
				if strings.Contains(joined, unwanted) {
					t.Errorf("notes = %v, want nothing of %q under a command tt has just said cannot run",
						result.notes, unwanted)
				}
			}
			for _, l := range result.notes {
				if strings.HasPrefix(l, doctorNestedIndent) {
					t.Errorf("notes = %v, want nothing to paste that the program is missing for", result.notes)
				}
			}
		})
	}
}

func TestCheckNotifyKeepsAnUnsetOnEndAboveAMissingBus(t *testing.T) {
	isolate(t)
	notifyPrograms(t, "notify-send")
	withoutASessionBus(t)
	writeConfig(t, "[timer]\non_break_end = "+config.QuoteTOMLString(config.NotifyLinux)+"\n")

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, onEndKey+" is not set") {
		t.Errorf("summary = %q, want the setting that is missing to take it", result.summary)
	}
	if strings.Contains(result.summary, busAddressVar) {
		t.Errorf("summary = %q, want the bus reported under it rather than as it", result.summary)
	}
	joined := noteText(result)
	for _, want := range []string{onBreakEndKey, busAddressVar} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want the other setting answered under the verdict: %q", result.notes, want)
		}
	}
}

func TestCheckNotifyAgreesWithConfigAboutWhyItCannotLoad(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*testing.T)
		class   string
		other   string
	}{
		{
			name: "invalid contents",
			arrange: func(t *testing.T) {
				writeConfig(t, "[timer]\non_end = \"notify-send '{task}'\n")
			},
			class: "invalid",
			other: "cannot be read",
		},
		{
			name: "path cannot be read",
			arrange: func(t *testing.T) {
				path, err := config.Path()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			class: "cannot be read",
			other: "invalid",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			c.arrange(t)

			configResult := checkConfig(doctorCache{})
			notifyResult := checkNotify()
			if configResult.status != statusFail {
				t.Fatalf("config status = %v, want fail: %v", configResult.status, checkReport(configResult))
			}
			if notifyResult.status != statusWarn {
				t.Fatalf("notify status = %v, want warn: %v", notifyResult.status, checkReport(notifyResult))
			}
			for what, summary := range map[string]string{
				"config": configResult.summary,
				"notify": notifyResult.summary,
			} {
				if !strings.Contains(summary, c.class) {
					t.Errorf("%s summary = %q, want failure class %q", what, summary, c.class)
				}
				if strings.Contains(summary, c.other) {
					t.Errorf("%s summary = %q, want no conflicting failure class %q", what, summary, c.other)
				}
			}
			if len(notifyResult.notes) != 0 {
				t.Errorf("notify notes = %v, want nothing read off a config that did not load", notifyResult.notes)
			}

			var out strings.Builder
			runDoctorChecksWithWriteProbe(context.Background(), &out, func() configWriteObservation {
				return configWriteObservation{state: configWriteUnknown}
			})
			combined := collapsed(out.String())
			for _, want := range []string{
				collapsed("fail " + configResult.summary),
				collapsed("warn " + notifyResult.summary),
			} {
				if !strings.Contains(combined, want) {
					t.Errorf("combined report omitted %q", want)
				}
			}
		})
	}
}

func TestCheckNotifyBoundsAProgramNameOutOfTheConfig(t *testing.T) {
	isolateInALongPath(t)
	notifyPrograms(t)
	program := strings.Repeat("a-program-somebody-gave-a-long-name.", 8) + "end"
	writeConfig(t, "[timer]\non_end = "+config.QuoteTOMLString(program+" -- {task}")+"\n")

	var out strings.Builder
	runDoctorChecks(context.Background(), &out)
	assertDoctorFits(t, "tt doctor", out.String())
	if !strings.Contains(out.String(), "does not resolve") {
		t.Fatalf("tt doctor printed %q, want the program reported as one that is not here", out.String())
	}
	if strings.Contains(out.String(), program) {
		t.Errorf("tt doctor printed the whole %d-character program name, want it cut to what the line holds",
			len(program))
	}
}

func TestCheckCacheTellsNeverFromAnUnreadableStamp(t *testing.T) {
	now := time.Now()
	saved := doctorNow
	doctorNow = func() time.Time { return now }
	t.Cleanup(func() { doctorNow = saved })

	stamp := func(s string) *string { return &s }
	cases := map[string]struct {
		stamp  *string
		want   string
		status checkStatus

		say string
	}{
		"a cache that has never synced": {nil, "synced never", statusOK, ""},
		"a stamp tt wrote": {stamp(model.NewTime(now.Add(-2 * time.Hour)).StoreString()),
			" ago", statusOK, ""},
		"a sync a moment ago": {stamp(model.NewTime(now).StoreString()),
			"synced just now", statusOK, ""},
		"a shade ahead of the clock": {stamp(model.NewTime(now.Add(500 * time.Millisecond)).StoreString()),
			"synced just now", statusOK, ""},
		"an hour ahead of the clock": {stamp(model.NewTime(now.Add(time.Hour)).StoreString()),
			"synced in the future", statusWarn, "ahead of this clock, so a scheduled sync is not due"},
		"two hundred years ahead": {stamp(model.NewTime(now.AddDate(200, 0, 0)).StoreString()),
			"synced in the future", statusWarn, "ahead of this clock, so a scheduled sync is not due"},
		"further ahead than a duration reaches": {stamp("9999-12-31T23:59:59.000Z"),
			"synced in the future", statusWarn, "further ahead of this clock than tt measures an age over"},
		"further behind than a duration reaches": {stamp("1600-01-01T00:00:00.000Z"),
			"synced further back than this clock measures", statusWarn,
			"further behind this clock than tt measures an age over"},
		"not a timestamp":    {stamp("yesterday"), "synced unknown", statusWarn, "which is not a timestamp"},
		"an empty stamp":     {stamp(""), "synced unknown", statusWarn, "which is not a timestamp"},
		"a stamp from a log": {stamp("Sep  4 12:00:00"), "synced unknown", statusWarn, "which is not a timestamp"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			buildCache(t, func(ctx context.Context, st *store.Store) {
				if c.stamp == nil {
					return
				}
				if err := st.SetMeta(ctx, sync.LastSyncKey, *c.stamp); err != nil {
					t.Fatal(err)
				}
			})

			result, _ := checkCache(ctx)
			if result.status != c.status {
				t.Fatalf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if !strings.Contains(result.summary, c.want) {
				t.Errorf("summary = %q, want it to carry %q", result.summary, c.want)
			}
			if c.status == statusWarn && !strings.Contains(noteText(result), sync.LastSyncKey) {
				t.Errorf("notes = %v, want them to name the stamp behind the warning", result.notes)
			}
			if c.say != "" && !strings.Contains(noteText(result), c.say) {
				t.Errorf("notes = %v, want them to say %q", result.notes, c.say)
			}

			if strings.Contains(collapsed(result.summary)+" "+noteText(result), "2562047h47m16s") {
				t.Errorf("the report carries the saturated duration rather than a measurement:\n%s",
					strings.Join(checkReport(result), "\n"))
			}
		})
	}
}

func TestCheckCacheBoundsEveryAnswerAboutTheLastSync(t *testing.T) {

	long := func(s string) string { return "\n" + s + strings.Repeat("9", 400) + "Z\n" }
	cases := map[string]struct {
		stamp string
		say   string
	}{
		"a stamp nothing can read":               {"not-a-time\n" + strings.Repeat("z", 400), "is not a timestamp"},
		"further ahead than a duration reaches":  {long("9999-12-31T23:59:59."), "further ahead of this clock"},
		"further behind than a duration reaches": {long("1600-01-01T00:00:00."), "further behind this clock"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			buildCache(t, func(ctx context.Context, st *store.Store) {
				if err := st.SetMeta(ctx, sync.LastSyncKey, c.stamp); err != nil {
					t.Fatal(err)
				}
			})

			result, _ := checkCache(ctx)
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			if !strings.Contains(noteText(result), `\n`) {
				t.Errorf("notes omitted the escaped line break from the stored stamp")
			}
			for i, line := range result.notes {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
				}
			}

			if joined := noteText(result); !strings.Contains(joined, c.say) {
				t.Errorf("notes = %v, want them to say %q about the stamp", result.notes, c.say)
			}
		})
	}

	t.Run("nothing could be got out of the column at all", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()

		gone := "a\nb" + strings.Repeat("z", 400)
		path := buildCache(t, func(ctx context.Context, st *store.Store) {
			seedCacheContents(t, ctx, st)
			execCache(t, ctx, st, `DROP TABLE meta`)
			execCache(t, ctx, st, `CREATE TABLE "`+gone+`" (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
			execCache(t, ctx, st, `CREATE VIEW meta AS SELECT key, value FROM "`+gone+`"`)
			execCache(t, ctx, st, `DROP TABLE "`+gone+`"`)
		})
		insp, err := store.Inspect(ctx, path)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		defer insp.Close()

		age, note, finding := lastSyncAge(ctx, insp.Store)
		if age != "unknown" {
			t.Errorf("age = %q, want unknown: nothing was got out of the column", age)
		}

		if !finding {
			t.Errorf("lastSyncAge called this no finding, and it is the file refusing a read")
		}
		if !strings.HasPrefix(note, cacheStoreLead) {
			t.Errorf("note = %q, want the answer behind %q rather than standing as a sentence of tt's",
				note, cacheStoreLead)
		}
		if strings.Contains(note, strings.Repeat("z", 100)) {
			t.Errorf("note = %q, want the answer cut rather than printed whole", note)
		}
		lines := doctorNote(note)
		if len(lines) != 1 {
			t.Errorf("the note came out as %d lines: %v - the budget behind the lead is worked out "+
				"so that the two of them come to the prose width together", len(lines), lines)
		}
		for i, line := range lines {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
			}
		}
	})

	t.Run("a raw driver answer at the escaping boundary", func(t *testing.T) {
		note := lastSyncReadFailure(errors.New("read meta\n" + strings.Repeat("z", 300)))
		if !strings.HasPrefix(note, cacheStoreLead) {
			t.Errorf("note does not begin with %q", cacheStoreLead)
		}
		if !strings.Contains(note, `\n`) {
			t.Errorf("note omitted the escaped line break from the raw driver answer")
		}
		if lines := doctorNote(note); len(lines) != 1 {
			t.Errorf("raw driver answer rendered on %d lines, want one", len(lines))
		}
	})
}

func TestLastSyncAgeSaysNothingAboutTheStampWhenTheRunEnded(t *testing.T) {
	isolate(t)
	path := buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		stamp := model.NewTime(time.Now().Add(-2 * time.Hour)).StoreString()
		if err := st.SetMeta(ctx, sync.LastSyncKey, stamp); err != nil {
			t.Fatal(err)
		}
	})
	insp, err := store.Inspect(context.Background(), path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	defer insp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	age, note, finding := lastSyncAge(ctx, insp.Store)

	if finding {
		t.Errorf("lastSyncAge called this a finding about the cache; the run ended and the file " +
			"answered every read above it, so the verdict must not move over it")
	}
	if strings.Contains(note, "context canceled") {
		t.Errorf("note = %q, want the run named as what ended this, not tt's word for it quoted back "+
			"as news about a cache that answered everything it was asked", note)
	}
	if strings.HasPrefix(note, cacheStoreLead) {
		t.Errorf("note = %q, want it in tt's own words: %q hands the sentence to the store, and the "+
			"store was not the one that stopped", note, cacheStoreLead)
	}
	if !strings.Contains(note, "not asked for") {
		t.Errorf("note = %q, want it to say the stamp was not asked for", note)
	}
	if age == "unknown" {
		t.Errorf("age = %q, which is the word for a stamp that was read and could not be understood; "+
			"nothing here read one", age)
	}
	if !strings.Contains(age, "not asked") {
		t.Errorf("age = %q, want the summary to say the age was not asked for", age)
	}
}

func TestCheckCacheNamesTheWayBackFromAParkedEntry(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := st.Enqueue(ctx, store.OutboxEntry{
		Target: store.TargetOpenAPI, Op: store.OpTaskCreate, TaskID: "t1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := st.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %d entr(y/ies), %v; want the one that was queued", len(items), err)
	}
	if err := st.MarkFailed(ctx, seq, items[0].LeaseToken, "400 bad request"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	result, _ := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, "1 outbox entry(s) failed") {
		t.Errorf("notes = %v, want the parked entry counted", result.notes)
	}
	if !strings.Contains(joined, sync.RetryHint) {
		t.Errorf("notes = %v, want them to carry %q", result.notes, sync.RetryHint)
	}
}

func TestCheckCacheNamesTheWayOutThatEndsAParkedEntry(t *testing.T) {
	isolate(t)
	seedParkedCreate(t)

	result, _ := checkCache(context.Background())
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	for _, want := range []string{sync.RetryHint, dropParkedHint, "1 parked create(s) among them", "second copy"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want them to carry %q", result.notes, want)
		}
	}
}

func TestDoctorSaysAWayOutWhereAUserCanTypeIt(t *testing.T) {
	isolate(t)

	const long = `run "tt sync --retry-failed --including-the-parked-creates" to put every parked entry back in line`
	if n := utf8.RuneCountInString(long); n <= doctorTextWidth {
		t.Fatalf("the fixture is %d columns and the fold is %d wide, so it folds nowhere and proves nothing", n, doctorTextWidth)
	}
	lines := doctorHint(long)
	if len(lines) != 1 {
		t.Fatalf("doctorHint broke a command over %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if got := strings.TrimPrefix(lines[0], doctorIndent); got != long {
		t.Errorf("doctorHint said %q, want the command word for word", got)
	}

	seedParkedCreate(t)
	result, _ := checkCache(context.Background())
	for _, hint := range []string{sync.RetryHint, dropParkedHint} {
		if !slices.Contains(result.notes, doctorIndent+hint) {
			t.Errorf("the way out %q was not said on a line of its own:\n%s", hint, strings.Join(result.notes, "\n"))
		}
	}
}

func TestCheckCacheSaysNothingAboutParkedCreatesWhenThereAreNone(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := st.Enqueue(ctx, store.OutboxEntry{
		Target: store.TargetOpenAPI, Op: store.OpTaskUpdate, TaskID: "srv-1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := st.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %d entr(y/ies), %v; want the one that was queued", len(items), err)
	}
	if err := st.MarkFailed(ctx, seq, items[0].LeaseToken, "400 bad request"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	result, _ := checkCache(ctx)
	joined := noteText(result)
	if !strings.Contains(joined, "1 outbox entry(s) failed") {
		t.Fatalf("notes = %v, want the parked entry counted", result.notes)
	}
	if strings.Contains(joined, "parked create(s) among them") {
		t.Errorf("notes = %v, want no caution about creates: the parked entry is an update", result.notes)
	}
}

func TestCheckCacheShowsOrphanedLocalRows(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Dentist"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.DB().ExecContext(ctx, `DELETE FROM outbox`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	result, _ := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	for _, want := range []string{
		"1 task(s) made offline will never be sent", "no create is left in the queue",
		`"Dentist"`, strconv.Quote(created.Id), "shown rather than removed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want them to carry %q", result.notes, want)
		}
	}

	st, err = store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Task(ctx, created.Id); err != nil {
		t.Errorf("the row is gone after a check that only reports (%v)", err)
	}
}

func TestCheckCacheSaysNothingAboutOrphansInAHealthyCache(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Call the dentist"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	result, _ := checkCache(ctx)
	if strings.Contains(noteText(result), "no create is left in the queue") {
		t.Errorf("notes = %v, want nothing about orphans: the create is queued", result.notes)
	}
}

func TestCheckCacheHoldsBothHalvesOfAnOrphanedRow(t *testing.T) {
	t.Run("an id nothing may print as it stands", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()

		id := "local-a\nSECOND-LINE\x1b[31m"
		orphanedRow(t, ctx, id, "Call the dentist")

		result, _ := checkCache(ctx)
		if !strings.Contains(noteText(result), strconv.Quote(id)) {
			t.Errorf("notes = %v, want the id written out escaped and whole", result.notes)
		}
		for i, line := range result.notes {
			if strings.Contains(line, "\n") {
				t.Errorf("note %d carries a line break, so one row prints as two and the count "+
					"over the listing stops describing it: %q", i+1, line)
			}
			if strings.Contains(line, "\x1b") {
				t.Errorf("note %d carries an escape sequence out of the cache: %q", i+1, line)
			}
		}
	})

	t.Run("a title longer than the line", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()

		orphanedRow(t, ctx, "local-1234567890abcdef", strings.Repeat("Я", 300))

		result, _ := checkCache(ctx)
		for i, line := range result.notes {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
			}
		}
	})
}

func orphanedRow(t *testing.T, ctx context.Context, id, title string) {
	t.Helper()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	execCache(t, ctx, st, `INSERT INTO tasks (id, project_id, title, local) VALUES (?, 'p1', ?, 1)`, id, title)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func buildCache(t *testing.T, fill func(ctx context.Context, st *store.Store)) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if fill != nil {
		fill(ctx, st)
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func execCache(t *testing.T, ctx context.Context, st *store.Store, stmt string, args ...any) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func seedCacheContents(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()

	execCache(t, ctx, st, `INSERT INTO projects (id, name) VALUES ('p1', 'Дом')`)
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Call the dentist"}); err != nil {
		t.Fatal(err)
	}
}

func schemaLatest(t *testing.T, path string) int {
	t.Helper()
	insp, err := store.Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer insp.Close()
	return insp.SchemaLatest
}

func shapeToVersion(t *testing.T, ctx context.Context, st *store.Store, shape, claim int) {
	t.Helper()
	d, err := st.CompareSchema(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missing) > 0 {
		t.Fatalf("a cache this build just made lacks %v of what schema v%d describes", d.Missing, shape)
	}
	for _, kind := range []string{"index", "column", "table"} {
		for _, o := range d.Extra {
			if o.Kind != kind {
				continue
			}
			switch kind {
			case "index":
				execCache(t, ctx, st, `DROP INDEX `+quoteSQLName(o.Name))
			case "table":
				execCache(t, ctx, st, `DROP TABLE `+quoteSQLName(o.Name))
			case "column":
				table, column, ok := strings.Cut(o.Name, ".")
				if !ok {
					t.Fatalf("a column difference is named %q, want table.column", o.Name)
				}
				execCache(t, ctx, st, `ALTER TABLE `+quoteSQLName(table)+` DROP COLUMN `+quoteSQLName(column))
			}
		}
	}
	execCache(t, ctx, st, `UPDATE meta SET value = ? WHERE key = 'schema_version'`, strconv.Itoa(claim))
	after, err := st.CompareSchema(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Missing) > 0 || len(after.Extra) > 0 {
		t.Fatalf("the fixture is not schema v%d after all: missing %v, extra %v", shape, after.Missing, after.Extra)
	}
}

func quoteSQLName(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func damagedCache(t *testing.T) string {
	t.Helper()
	var pageSize int64
	path := buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		var taskID string
		if err := st.DB().QueryRowContext(ctx, `SELECT id FROM tasks LIMIT 1`).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3000; i++ {
			execCache(t, ctx, st, `INSERT INTO items (task_id, id, title) VALUES (?, ?, ?)`,
				taskID, "i"+strconv.Itoa(i), "step "+strconv.Itoa(i))
		}
		if err := st.DB().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
			t.Fatal(err)
		}

		drainPragma(t, ctx, st, `PRAGMA wal_checkpoint(TRUNCATE)`)
	})
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() <= pageSize {
		t.Fatalf("the cache is %d bytes at a page size of %d: there is no last page to damage", fi.Size(), pageSize)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 16), fi.Size()-pageSize); err != nil {
		t.Fatal(err)
	}
	return path
}

func metaDamagedCache(t *testing.T) string {
	t.Helper()
	var root, pageSize int64
	path := buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		if err := st.DB().QueryRowContext(ctx,
			`SELECT rootpage FROM sqlite_schema WHERE name = 'meta'`).Scan(&root); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
			t.Fatal(err)
		}

		drainPragma(t, ctx, st, `PRAGMA wal_checkpoint(TRUNCATE)`)
	})
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 16), (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	return path
}

func cacheWithManyPages(t *testing.T) string {
	t.Helper()
	return buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		var taskID string
		if err := st.DB().QueryRowContext(ctx, `SELECT id FROM tasks LIMIT 1`).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		tx, err := st.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 30000; i++ {
			if _, err := tx.ExecContext(ctx, `INSERT INTO items (task_id, id, title) VALUES (?, ?, ?)`,
				taskID, "i"+strconv.Itoa(i), "step "+strconv.Itoa(i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		drainPragma(t, ctx, st, `PRAGMA wal_checkpoint(TRUNCATE)`)
	})
}

func drainPragma(t *testing.T, ctx context.Context, st *store.Store, stmt string) {
	t.Helper()
	rows, err := st.DB().QueryContext(ctx, stmt)
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func cacheDirListing(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type cacheSnapshot struct {
	digest string
	mode   os.FileMode
	size   int64
	mtime  string
}

func snapCache(t *testing.T, path string) cacheSnapshot {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return cacheSnapshot{
		digest: fileDigest(t, path),
		mode:   fi.Mode(),
		size:   fi.Size(),
		mtime:  fi.ModTime().String(),
	}
}

func assertCacheUntouched(t *testing.T, what, path string, before cacheSnapshot) {
	t.Helper()
	after := snapCache(t, path)
	if after.digest != before.digest {
		t.Errorf("%s: the cache bytes changed (%s -> %s)", what, before.digest, after.digest)
	}
	if after.mode != before.mode {
		t.Errorf("%s: the cache mode changed (%v -> %v)", what, before.mode, after.mode)
	}
	if after.size != before.size {
		t.Errorf("%s: the cache size changed (%d -> %d)", what, before.size, after.size)
	}
	if after.mtime != before.mtime {
		t.Errorf("%s: the cache mtime changed (%s -> %s)", what, before.mtime, after.mtime)
	}
}

func chmodBites(t *testing.T, dir string, mode os.FileMode) bool {
	t.Helper()
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	probe := filepath.Join(dir, "a-file-nothing-else-makes")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return true
	}
	f.Close()
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	return false
}

func TestCheckCacheDoesNotCreateTheCacheItInspects(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}

	result, cache := checkCache(ctx)
	if result.status != statusSkipped {
		t.Fatalf("status = %v, want skip: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, path) {
		t.Errorf("summary = %q, want the path it did not find named", result.summary)
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that opened nothing")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) = %v, want the file still not to be there", path, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the check made the directory %q as well", filepath.Dir(path))
	}
}

func TestCheckCacheLeavesTheCacheItInspectsAlone(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		drainPragma(t, ctx, st, `PRAGMA wal_checkpoint(TRUNCATE)`)
	})
	before := fileDigest(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	result, _ := checkCache(ctx)
	if result.status != statusOK {
		t.Fatalf("status = %v, want ok on a sound cache: %v", result.status, checkReport(result))
	}
	if after := fileDigest(t, path); after != before {
		t.Errorf("the cache was written to by a check that only reads (%s, was %s)", after, before)
	}
	if again, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if again.Mode() != info.Mode() {
		t.Errorf("the cache is now mode %v, was %v", again.Mode(), info.Mode())
	}
	base := filepath.Base(path)
	want := []string{base, base + "-shm", base + "-wal"}
	if got := cacheDirListing(t, path); !slices.Equal(got, want) {
		t.Errorf("the directory holds %v, want exactly %v - the two sidecars are the accepted cost of "+
			"a read-only open, and anything else is a write", got, want)
	}
}

func TestCheckCacheWritesNothingWhateverTheCacheHolds(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) string
	}{
		{"a healthy cache", func(t *testing.T) string {
			return buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				drainPragma(t, ctx, st, `PRAGMA wal_checkpoint(TRUNCATE)`)
			})
		}},
		{"a damaged cache", func(t *testing.T) string {
			return damagedCache(t)
		}},
		{"a cache from the future", func(t *testing.T) string {
			path := buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
			})
			latest := schemaLatest(t, path)

			st, err := store.Open(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			execCache(t, context.Background(), st, `UPDATE meta SET value = ? WHERE key = 'schema_version'`,
				strconv.Itoa(latest+1))
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"a cache with no meta table", func(t *testing.T) string {
			return buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				execCache(t, ctx, st, `DROP TABLE meta`)
			})
		}},
		{"a cache with no schema_version row", func(t *testing.T) string {
			return buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				execCache(t, ctx, st, `DELETE FROM meta WHERE key = 'schema_version'`)
			})
		}},
		{"a cache lacking part of its schema", func(t *testing.T) string {
			return buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				execCache(t, ctx, st, `ALTER TABLE tasks DROP COLUMN content`)
			})
		}},
		{"a cache carrying more than its schema", func(t *testing.T) string {
			return buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				execCache(t, ctx, st, `CREATE TABLE scratch (a)`)
			})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path := c.build(t)
			before := snapCache(t, path)
			listBefore := cacheDirListing(t, path)

			result, _ := checkCache(context.Background())
			assertCacheUntouched(t, c.name, path, before)

			if result.status == statusSkipped {
				t.Fatalf("status = skip over a cache that is there: %v", checkReport(result))
			}
			base := filepath.Base(path)
			want := []string{base, base + "-shm", base + "-wal"}
			got := cacheDirListing(t, path)
			if !slices.Equal(got, want) && !slices.Equal(got, listBefore) {
				t.Errorf("the directory holds %v, want %v or the set it started with %v",
					got, want, listBefore)
			}
		})
	}
}

func TestCheckCacheRendersEveryOpenOutcomeOnce(t *testing.T) {
	cases := []struct {
		name  string
		stage func(t *testing.T, path string) context.Context

		status checkStatus

		sqliteSpoke bool
	}{
		{name: "no cache at all", status: statusSkipped, stage: func(t *testing.T, path string) context.Context {
			return context.Background()
		}},
		{name: "a zero byte cache", status: statusSkipped, stage: func(t *testing.T, path string) context.Context {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return context.Background()
		}},
		{name: "a directory at the cache path", status: statusFail, stage: func(t *testing.T, path string) context.Context {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return context.Background()
		}},
		{name: "a cache that cannot be stat-ed", status: statusFail, stage: func(t *testing.T, path string) context.Context {
			buildCache(t, nil)
			if !chmodBites(t, filepath.Dir(path), 0o000) {
				t.Skip("the chmod does not bite here, so there is nothing this case can stage")
			}
			return context.Background()
		}},
		{name: "a run that ended", status: statusFail, stage: func(t *testing.T, path string) context.Context {
			buildCache(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
		{name: "a file that is not a database", status: statusFail, sqliteSpoke: true, stage: func(t *testing.T, path string) context.Context {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
				t.Fatal(err)
			}
			return context.Background()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			ctx := c.stage(t, path)

			result, _ := checkCache(ctx)
			block := strings.Join(checkReport(result), "\n")
			if result.status != c.status {
				t.Errorf("status = %v, want %v: the two properties below hold over branches this "+
					"fixture is not about, so the outcome is named as well:\n%s",
					result.status, c.status, block)
			}
			if n := strings.Count(block, path); n > 1 {
				t.Errorf("the path appears %d times in the block:\n%s", n, block)
			}

			if quoted := strings.Contains(block, cacheDriverLead+`"`); quoted != c.sqliteSpoke {
				t.Errorf("the block quotes SQLite = %v, want %v:\n%s", quoted, c.sqliteSpoke, block)
			}
		})
	}
}

func TestCacheOpenVerdictKeepsThisBuildsOwnFaultOutOfSQLitesMouth(t *testing.T) {
	const path = "/home/u/.local/share/ticktick/cache.db"
	const reason = "migration versions must run 1..5 without gaps"
	result := cacheOpenVerdict(path, fmt.Errorf("%w: %w", store.ErrBadMigrations, errors.New(reason)))

	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	block := strings.Join(checkReport(result), "\n")
	if strings.Contains(block, cacheDriverLead) {
		t.Errorf("the block puts this build's own complaint behind the driver's lead, and SQLite has "+
			"not been asked anything at this point:\n%s", block)
	}
	if !strings.Contains(collapsed(block), reason) {
		t.Errorf("the block does not carry what would not load, which is the only thing here anybody "+
			"can act on:\n%s", block)
	}
	if n := strings.Count(block, path); n != 1 {
		t.Errorf("the path appears %d times, want once: it is the longest thing in these lines, and "+
			"the sentence that names it is the one saying the file was not opened:\n%s", n, block)
	}
	if !strings.Contains(collapsed(block), "what is wrong is the tt doing the complaining") {
		t.Errorf("the block does not say the fault is this build's, which is the whole of the "+
			"branch:\n%s", block)
	}
}

func TestCacheCompareFailureTellsThisBuildsOwnSchemaFromTheUsersFile(t *testing.T) {
	const path = "/home/u/.local/share/ticktick/cache.db"

	reason := "migration 0002_rev_and_lease:\nnear \"CREATE" +
		strings.Repeat("z", 400) + "\": syntax error"

	t.Run("the reference this build makes for itself", func(t *testing.T) {
		result := cacheCompareFailure(path,
			fmt.Errorf("%w: %w", store.ErrBadReferenceSchema, errors.New(reason)))
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
		}
		block := collapsed(strings.Join(checkReport(result), "\n"))
		if strings.Contains(block, "the schema the file holds could not be read") {
			t.Errorf("the block reports the user's file over a schema built in memory out of this "+
				"build's own migrations:\n%s", block)
		}
		if !strings.Contains(block, "nothing the file said is in question") {
			t.Errorf("the block does not say the file is not what this is about:\n%s", block)
		}
		if !strings.Contains(block, "migration 0002") {
			t.Errorf("the block does not carry which body would not run: the sentinel's own sentence "+
				"is the summary's, and what the note owes is the cause:\n%s", block)
		}
		if strings.Contains(block, strings.Repeat("z", 100)) {
			t.Errorf("the block carries the answer whole rather than cut:\n%s", block)
		}
		if !strings.Contains(noteText(result), `\n`) {
			t.Errorf("the note omitted the escaped line break from the comparison failure")
		}
		for i, line := range result.notes {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
			}
		}
	})

	t.Run("a run that ended inside the comparison", func(t *testing.T) {
		result := cacheCompareFailure(path, fmt.Errorf("%w: create meta table: %w",
			store.ErrBadReferenceSchema, context.DeadlineExceeded))
		if !strings.Contains(result.summary, "the run ended") {
			t.Errorf("summary = %q, want the run named: a deadline that lands inside the reference "+
				"is a run that stopped and not a tt that cannot build its own schema", result.summary)
		}
	})

	t.Run("the cache itself", func(t *testing.T) {
		result := cacheCompareFailure(path, errors.New("read the schema: malformed database schema (x)"))
		if !strings.Contains(result.summary, "the schema the file holds could not be read") {
			t.Errorf("summary = %q, want the verdict written for a read of the file that did not "+
				"finish", result.summary)
		}
	})
}

func TestTheReadsBelowTheOpenBlameTheRunAndNotTheFile(t *testing.T) {
	const path = "/home/u/.local/share/ticktick/cache.db"
	verdicts := map[string]func(error) checkResult{
		"the version that could not be read": func(err error) checkResult {
			return cacheVersionUnreadable(path, err)
		},
		"one of the reads below it": func(err error) checkResult {
			return cacheReadFailure(path, "the outbox counts", cacheStoreLead, err)
		},
	}
	answers := map[string]error{
		"a run the user stopped":             context.Canceled,
		"a deadline that expired":            context.DeadlineExceeded,
		"the store's words round a deadline": fmt.Errorf("outbox counts: %w", context.DeadlineExceeded),
	}
	for verdict, render := range verdicts {
		for answer, err := range answers {
			t.Run(verdict+", "+answer, func(t *testing.T) {
				result := render(err)
				if result.status != statusFail {
					t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
				}
				block := collapsed(strings.Join(checkReport(result), "\n"))
				if !strings.Contains(block, "the run ended") {
					t.Errorf("the block blames the file for a run that was over:\n%s", block)
				}
				if strings.Contains(block, cacheStoreLead+`"`) || strings.Contains(block, cacheDriverLead+`"`) {
					t.Errorf("the block quotes somebody as having said \"context ...\", which is tt's "+
						"own word for a run that stopped:\n%s", block)
				}
				if strings.Contains(block, "could not be read") {
					t.Errorf("the block says the file would not answer, and the file was never "+
						"asked:\n%s", block)
				}
			})
		}
	}

	ordinary := cacheReadFailure(path, "the outbox counts", cacheStoreLead,
		errors.New("outbox counts: database disk image is malformed (11)"))
	if !strings.Contains(ordinary.summary, "the outbox counts could not be read") {
		t.Errorf("summary = %q, want the read named: the diversion is for a run that ended and "+
			"nothing else", ordinary.summary)
	}
}

func TestCheckCacheSaysThereIsNoCacheOnlyWhenThereIsNone(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := buildCache(t, nil)
	if !chmodBites(t, filepath.Dir(path), 0o000) {
		t.Skip("the chmod does not bite here, so there is nothing this test can stage")
	}

	result, _ := checkCache(ctx)
	if result.status == statusSkipped {
		t.Fatalf("status = skip over a cache that is there and could not be looked at: %v", checkReport(result))
	}
	if strings.Contains(result.summary, "no cache") {
		t.Errorf("summary = %q, want it not to say the cache is missing", result.summary)
	}
	if !strings.Contains(result.summary, "cannot look at") {
		t.Errorf("summary = %q, want it to say the stat was what failed", result.summary)
	}
}

func TestCheckCacheRefusesToOpenWhatIsNotAFile(t *testing.T) {
	isolate(t)
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}

	result, cache := checkCache(context.Background())
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "is a directory, not a regular file") {
		t.Errorf("summary = %q, want it to name what it found there", result.summary)
	}
	if !strings.Contains(noteText(result), "waits for whoever writes to it") {
		t.Errorf("notes = %v, want the reason it is refused rather than opened", result.notes)
	}
	if cache.read {
		t.Errorf("cache.read = true after a path that was never opened")
	}
}

func TestCheckCacheSaysWhatItCouldNotDo(t *testing.T) {
	t.Run("a file that is not a database", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		path, err := store.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("this is not a database\n"+strings.Repeat("z", 400)), 0o600); err != nil {
			t.Fatal(err)
		}

		result, cache := checkCache(ctx)
		assertUnreadable(t, result, path)
		if cache.read {
			t.Errorf("cache.read = true after a cache that could not be opened")
		}
		if strings.Contains(noteText(result), "read-only") {
			t.Errorf("notes = %v, want nothing about the read-only open: SQLite did not refuse a write here",
				result.notes)
		}
	})

	t.Run("a cache in a directory this user cannot write", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		path := buildCache(t, nil)
		if !chmodBites(t, filepath.Dir(path), 0o500) {
			t.Skip("the chmod does not bite here, so there is nothing this test can stage")
		}

		result, _ := checkCache(ctx)
		assertUnreadable(t, result, path)
		if !strings.Contains(noteText(result), "writes nothing to it") {
			t.Errorf("notes = %v, want the sentence that says whose fault the driver's wording is",
				result.notes)
		}
	})

	t.Run("a cache whose schema SQLite will not parse", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		name := "a\nb" + strings.Repeat("z", 300)
		path := buildCache(t, func(ctx context.Context, st *store.Store) {
			seedCacheContents(t, ctx, st)
			execCache(t, ctx, st, `CREATE TABLE `+quoteSQLName(name)+` (x)`)

			execCache(t, ctx, st, `PRAGMA writable_schema = ON`)
			execCache(t, ctx, st, `UPDATE sqlite_schema SET sql = 'this is not sql' WHERE name = ?`, name)
			execCache(t, ctx, st, `PRAGMA writable_schema = OFF`)
		})

		result, _ := checkCache(ctx)
		assertUnreadable(t, result, path)
		if strings.Contains(noteText(result), strings.Repeat("z", 100)) {
			t.Errorf("notes = %v, want the name out of the cache cut rather than printed whole",
				result.notes)
		}
	})
}

func TestCacheUnreadableKeepsDriverAnswerOnOneBoundedLine(t *testing.T) {
	reason := "driver\\path\"\n\x1b" + strings.Repeat("z", 300)
	result := cacheUnreadable("/cache.db", errors.New(reason))
	if len(result.notes) != 1 {
		t.Fatalf("cacheUnreadable made %d note lines, want one physical line", len(result.notes))
	}
	line := result.notes[0]
	if lead := doctorIndent + cacheDriverLead; !strings.HasPrefix(line, lead) {
		t.Errorf("first note = %q, want exact lead %q", line, lead)
	}
	if n := utf8.RuneCountInString(line); n != cli.Width {
		t.Errorf("first note is %d runes wide, want the hard cap %d", n, cli.Width)
	}
	for _, escaped := range []string{`\\`, `\"`, `\n`, `\x1b`} {
		if !strings.Contains(line, escaped) {
			t.Errorf("first note omitted escaped driver text %q", escaped)
		}
	}
	if strings.ContainsAny(line, "\n\x1b") {
		t.Errorf("first note passed a raw driver control character through")
	}
}

func TestCheckCacheSaysNothingAboutSQLiteWhenTheRunEnded(t *testing.T) {
	t.Run("a run that was over before the open", func(t *testing.T) {
		isolate(t)
		buildCache(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result, cache := checkCache(ctx)
		assertRunEnded(t, result, cache)
	})

	t.Run("a run that ended over the pages", func(t *testing.T) {
		isolate(t)
		path := cacheWithManyPages(t)
		openStart := time.Now()
		insp, err := store.Inspect(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		openSpan := time.Since(openStart)
		pagesStart := time.Now()
		_, sound, err := cacheIntegrity(context.Background(), insp.Store)
		if err != nil || !sound {
			t.Fatalf("the fixture is not a sound cache: sound = %v, err = %v", sound, err)
		}
		pagesSpan := time.Since(pagesStart)
		insp.Close()
		deadline := pagesSpan / 4
		if deadline < 8*openSpan {
			t.Skipf("the open takes %v and a quarter of the pages %v, so the deadline cannot be put "+
				"between them on this machine", openSpan, deadline)
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		result, cache := checkCache(ctx)
		assertRunEnded(t, result, cache)
	})
}

func assertRunEnded(t *testing.T, result checkResult, cache doctorCache) {
	t.Helper()
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "the run ended") {
		t.Errorf("summary = %q, want it to say the run ended rather than blame the file", result.summary)
	}
	if strings.Contains(noteText(result), "SQLite reports") {
		t.Errorf("notes = %v, want nothing quoted as SQLite's: the run ended, the file did not answer badly",
			result.notes)
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that read nothing out of the cache")
	}
}

func assertUnreadable(t *testing.T, result checkResult, path string) {
	t.Helper()
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "cannot read ") || !strings.Contains(result.summary, path) {
		t.Errorf("summary = %q, want it to say it could not read %s", result.summary, path)
	}
	if strings.Contains(result.summary, "cannot open") {
		t.Errorf("summary = %q, still says open, which is the word the driver's answer contradicts",
			result.summary)
	}
	if !strings.Contains(noteText(result), "SQLite reports") {
		t.Errorf("notes = %v, want the driver quoted rather than paraphrased", result.notes)
	}
	for i, line := range result.notes {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
}

func TestCacheSchemaVerdict(t *testing.T) {
	const path = "/home/u/.local/share/ticktick/cache.db"
	const latest = 5
	cases := []struct {
		name    string
		found   int
		hasMeta bool
		hasRow  bool
		status  checkStatus
		done    bool
		say     string
	}{
		{"a version behind this build", 4, true, true, statusWarn, false, "doctor does not apply the migration"},
		{"the version this build carries", 5, true, true, statusOK, false, ""},
		{"a version this build does not read", 6, true, true, statusFail, true, "update tt to the version that wrote it"},
		{"a row that reads zero", 0, true, true, statusFail, true, "gives the schema version as 0"},
		{"a row that reads below zero", -3, true, true, statusFail, true, "gives the schema version as -3"},
		{"a meta table with no version in it", 0, true, false, statusFail, true, "no schema_version row"},
		{"no meta table at all", 0, false, false, statusFail, true, "not a cache tt wrote"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, done := cacheSchemaVerdict(path, c.found, c.hasMeta, c.hasRow, latest)
			if result.status != c.status {
				t.Errorf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if done != c.done {
				t.Errorf("done = %v, want %v", done, c.done)
			}
			if c.say == "" {
				return
			}
			if said := collapsed(result.summary) + " " + noteText(result); !strings.Contains(said, c.say) {
				t.Errorf("the verdict is %q, want it to say %q", said, c.say)
			}
		})
	}

	zeros := map[string]string{}
	for _, c := range []struct {
		name    string
		hasMeta bool
		hasRow  bool
	}{
		{"no meta table", false, false},
		{"a meta table with no row", true, false},
		{"a row reading 0", true, true},
	} {
		result, _ := cacheSchemaVerdict(path, 0, c.hasMeta, c.hasRow, latest)
		if other, same := zeros[result.summary]; same {
			t.Errorf("%s and %s both say %q, and they are different faults with different "+
				"things to do about them", other, c.name, result.summary)
		}
		zeros[result.summary] = c.name
	}

	for _, found := range []int{0, -3} {
		below, _ := cacheSchemaVerdict(path, found, true, true, latest)
		said := collapsed(below.summary) + " " + noteText(below)
		if !strings.Contains(said, "does not put it right") {
			t.Errorf("the verdict for version %d is %q, want it to say the next command does not "+
				"repair this", found, said)
		}
	}

	future, _ := cacheSchemaVerdict(path, latest+1, true, true, latest)
	if !strings.Contains(noteText(future), "move the file aside") {
		t.Errorf("notes = %v, want the store's own caution about not deleting the cache", future.notes)
	}
}

func TestCheckCacheStopsWhereTheVersionVerdictStops(t *testing.T) {
	cases := map[string]struct {
		fill func(t *testing.T, ctx context.Context, st *store.Store)
		say  string
	}{
		"no meta table at all": {
			func(t *testing.T, ctx context.Context, st *store.Store) {
				execCache(t, ctx, st, `DROP TABLE meta`)
			},
			"has no meta table, so it is not a cache tt wrote",
		},
		"a meta table with no version in it": {
			func(t *testing.T, ctx context.Context, st *store.Store) {
				execCache(t, ctx, st, `DELETE FROM meta WHERE key = 'schema_version'`)
			},
			"carries no schema version",
		},

		"a version row holding a number this build never wrote": {
			func(t *testing.T, ctx context.Context, st *store.Store) {
				execCache(t, ctx, st, `UPDATE meta SET value = '0' WHERE key = 'schema_version'`)
			},
			"gives the schema version as 0",
		},
		"a version this build does not read": {
			func(t *testing.T, ctx context.Context, st *store.Store) {

				latest, err := st.SchemaVersion(ctx)
				if err != nil {
					t.Fatal(err)
				}
				execCache(t, ctx, st, `UPDATE meta SET value = ? WHERE key = 'schema_version'`,
					strconv.Itoa(latest+1))
			},
			"newer than this build reads",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				c.fill(t, ctx, st)
			})

			result, cache := checkCache(ctx)
			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, c.say) {
				t.Errorf("summary = %q, want it to say %q", result.summary, c.say)
			}
			if strings.Contains(result.summary, "the schema the file holds could not be read") {
				t.Errorf("summary = %q, want the verdict written for this state rather than the "+
					"complaint of a comparison that should never have been made", result.summary)
			}
			if strings.Contains(result.summary, "task(s)") {
				t.Errorf("summary = %q, want no counts: the check stopped before it read them",
					result.summary)
			}
			if cache.read {
				t.Errorf("cache.read = true after a check that stopped at the version")
			}
		})
	}
}

func TestCheckCacheBoundsAVersionNothingCanRead(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	value := "v\n" + strings.Repeat("z", 400)
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		execCache(t, ctx, st, `UPDATE meta SET value = ? WHERE key = 'schema_version'`, value)
	})

	result, cache := checkCache(ctx)
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "the schema version in its meta table could not be read") {
		t.Errorf("summary = %q, want tt's own sentence about a version it could not read", result.summary)
	}
	if strings.Contains(result.summary, strings.Repeat("z", 20)) {
		t.Errorf("summary = %q, want the value out of the cache in a note and not in tt's own sentence",
			result.summary)
	}
	if !strings.Contains(noteText(result), cacheStoreLead) {
		t.Errorf("notes = %v, want the answer quoted rather than paraphrased", result.notes)
	}
	if strings.Contains(noteText(result), strings.Repeat("z", 100)) {
		t.Errorf("notes = %v, want the value cut rather than printed whole", result.notes)
	}
	if !strings.Contains(noteText(result), `\\n`) {
		t.Errorf("notes omitted the escaped line break from the store's quoted value")
	}
	for i, line := range result.notes {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that stopped at the version")
	}
	direct := cacheVersionUnreadable("/cache.db", errors.New("schema version\n"+strings.Repeat("z", 300)))
	if !strings.Contains(noteText(direct), `\n`) {
		t.Errorf("direct version error omitted the escaped line break at the raw error boundary")
	}
}

func TestCheckCacheBoundsAReadThatWouldNotFinish(t *testing.T) {
	cases := []struct {
		name string
		what string
		lead string
	}{
		{"an answer the driver gave as it is", "the task count", cacheDriverLead},
		{"an answer the store has worded first", "the outbox counts", cacheStoreLead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			name := "a\nb" + strings.Repeat("z", 400)
			result := cacheReadFailure(path, c.what, c.lead,
				fmt.Errorf("malformed database schema (%s)", name))

			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, c.what+" could not be read") {
				t.Errorf("summary = %q, want tt's own sentence about the read that did not finish",
					result.summary)
			}
			if !strings.Contains(result.summary, path) {
				t.Errorf("summary = %q, want the file it is about named", result.summary)
			}
			if strings.Contains(result.summary, strings.Repeat("z", 20)) {
				t.Errorf("summary = %q, want the answer in a note and not in tt's own sentence",
					result.summary)
			}
			if !strings.Contains(noteText(result), c.lead) {
				t.Errorf("notes = %v, want the answer behind %q and whole", result.notes, c.lead)
			}
			if strings.Contains(noteText(result), strings.Repeat("z", 100)) {
				t.Errorf("notes = %v, want the answer cut rather than printed whole", result.notes)
			}
			if len(result.notes) != 1 {
				t.Errorf("the note came out as %d lines: %v - the budget behind %q is worked out "+
					"so that the lead and the answer come to the prose width together, and a note "+
					"that wrapped is a budget that has stopped belonging to its lead",
					len(result.notes), result.notes, c.lead)
			}
			if !strings.Contains(noteText(result), `\n`) {
				t.Errorf("notes omitted the escaped line break from the unreadable schema name")
			}
			for i, line := range result.notes {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
				}
			}
		})
	}
}

func TestEachReadStandsBehindTheVoiceThatAnsweredIt(t *testing.T) {
	want := map[string]struct{ lead, why string }{
		"the schema the file holds": {"cacheStoreLead",
			"CompareSchema is the store's own method and words every answer before it leaves: " +
				"the range it will not compare against, the read of sqlite_schema, the columns of " +
				"one table. The driver is inside some of them and not others"},
		"the task count": {"cacheDriverLead",
			"a SELECT count(*) put to the connection here, with nothing of the store's around what " +
				"comes back"},
		"the list names": {"cacheDriverLead",
			"a SELECT over projects put to the connection here, the same way, through cacheListNames"},
		"the outbox counts": {"cacheStoreLead",
			"OutboxCounts wraps what went wrong in \"outbox counts: ...\" before anyone here sees it"},
		"the parked creates": {"cacheStoreLead",
			"FailedCreates wraps its own answer the same way"},
		"the rows made offline": {"cacheStoreLead",
			"OrphanedLocalTasks wraps the query's failures, and its stamp column goes through " +
				"ParseStamp - a refusal by Go over a value SQLite delivered, and the measured case " +
				"for why this lead may not name the driver"},
		"the declared relationships": {"cacheStoreLead",
			"ForeignKeyViolations wraps both query and row-stream failures in the store's own " +
				"foreign-key-check category"},
		"the task-to-project references": {"cacheStoreLead",
			"UncachedTaskProjects wraps both query and row-stream failures in the store's own " +
				"uncached-project category"},
		"the item-to-task references": {"cacheStoreLead",
			"DanglingItemTasks wraps both query and row-stream failures in the store's own " +
				"dangling-item category"},
	}

	if strings.Contains(cacheStoreLead, "SQLite") {
		t.Errorf("cacheStoreLead = %q, and four of these six sites can fail without the driver "+
			"having spoken at all", cacheStoreLead)
	}
	if !strings.Contains(cacheDriverLead, "SQLite") {
		t.Errorf("cacheDriverLead = %q, want the driver named: it is the lead for the answers that "+
			"are the driver's own", cacheDriverLead)
	}

	fset := token.NewFileSet()
	files := parsePackage(t, fset, false)
	f, ok := files["doctor.go"]
	if !ok {
		t.Fatal("doctor.go is not in the package any more; this test reads the call sites out of it")
	}
	found := map[string]bool{}
	forEachCall(f, func(fn string, call *ast.CallExpr) {
		if calleeName(call.Fun) != "cacheReadFailure" || fn == "cacheReadFailure" {
			return
		}
		if len(call.Args) != 4 {
			t.Fatalf("%s: cacheReadFailure is called with %d arguments; this test reads the read's "+
				"name and its lead off the call", fset.Position(call.Pos()), len(call.Args))
		}
		what := literalText(call.Args[1])
		lead := calleeName(call.Args[2])
		site, listed := want[what]
		switch {
		case !listed:
			t.Errorf("%s reports %q and no row here says which voice answers it; add one, saying "+
				"what the method behind it does with what it gets", fset.Position(call.Pos()), what)
		case found[what]:
			t.Errorf("%s reports %q a second time, and the row cannot say which of the two it is "+
				"about", fset.Position(call.Pos()), what)
		case lead != site.lead:
			t.Errorf("%s puts %q behind %s, want %s: %s",
				fset.Position(call.Pos()), what, lead, site.lead, site.why)
		}
		found[what] = true
	})
	for what := range want {
		if !found[what] {
			t.Errorf("a row here pairs a lead with %q, and nothing in doctor.go reports that read "+
				"any more", what)
		}
	}
}

func TestCheckCacheReportsADamagedFile(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := damagedCache(t)

	insp, err := store.Inspect(ctx, path)
	if err != nil {
		t.Fatalf("the damaged cache cannot be opened at all, so this fixture is about something else: %v", err)
	}
	st := insp.Store
	var tasks int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Errorf("the task count no longer answers on this fixture: %v", err)
	}
	if _, _, _, err := cacheListNames(ctx, st); err != nil {
		t.Errorf("the list read no longer answers on this fixture: %v", err)
	}
	if _, err := st.OutboxCounts(ctx); err != nil {
		t.Errorf("OutboxCounts no longer answers on this fixture: %v", err)
	}
	if _, err := st.FailedCreates(ctx); err != nil {
		t.Errorf("FailedCreates no longer answers on this fixture: %v", err)
	}
	if _, err := st.OrphanedLocalTasks(ctx); err != nil {
		t.Errorf("OrphanedLocalTasks no longer answers on this fixture: %v", err)
	}
	if _, _, err := st.Meta(ctx, sync.LastSyncKey); err != nil {
		t.Errorf("the stamp read no longer answers on this fixture: %v", err)
	}
	insp.Close()
	if t.Failed() {
		t.Fatal("the damage has moved onto a b-tree this check reads, so the fixture no longer stages finding 25")
	}

	result, cache := checkCache(ctx)
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "the database file is damaged") {
		t.Errorf("summary = %q, want it to say the file is damaged", result.summary)
	}
	joined := noteText(result)
	if !strings.Contains(joined, "SQLite reports") {
		t.Errorf("notes = %v, want what SQLite said about the file quoted under the verdict", result.notes)
	}
	if !strings.Contains(joined, collapsed(cacheAsideNote)) {
		t.Errorf("notes = %v, want the caution about moving the file aside rather than deleting it",
			result.notes)
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that stopped at the pages")
	}
	for i, line := range result.notes {
		if strings.Contains(line, "\n") {
			t.Errorf("note %d carries a line break, so the listing stops counting: %q", i+1, line)
		}
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
}

func TestCheckCacheReportsDamageInTheMetaTableAsDamage(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := metaDamagedCache(t)

	insp, err := store.Inspect(ctx, path)
	if err != nil {
		t.Fatalf("the fixture cannot be opened at all, so it is about something else: %v", err)
	}
	if _, _, _, err := insp.Version(ctx); err == nil {
		t.Fatalf("the version reads on this fixture, so it no longer stages the question of the order")
	}
	insp.Close()

	result, cache := checkCache(ctx)
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "the database file is damaged") {
		t.Errorf("summary = %q, want the file reported as damaged rather than as a version nothing could read",
			result.summary)
	}
	if !strings.Contains(noteText(result), collapsed(cacheAsideNote)) {
		t.Errorf("notes = %v, want the caution about moving the file aside rather than deleting it",
			result.notes)
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that stopped at the pages")
	}
}

func TestCheckCacheFailsOnWhatTheCacheLacks(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		execCache(t, ctx, st, `ALTER TABLE tasks DROP COLUMN content`)
	})

	result, cache := checkCache(ctx)
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "does not have everything") {
		t.Errorf("summary = %q, want it to say the tables are not what the version describes", result.summary)
	}
	if strings.Contains(result.summary, "task(s)") {
		t.Errorf("summary = %q, want no counts: the check stopped before it read them", result.summary)
	}
	joined := noteText(result)
	if !strings.Contains(joined, `"tasks.content"`) {
		t.Errorf("notes = %v, want the missing column named", result.notes)
	}
	if !strings.Contains(joined, collapsed(cacheAsideNote)) {
		t.Errorf("notes = %v, want the caution about moving the file aside", result.notes)
	}
	if cache.read {
		t.Errorf("cache.read = true after a check that stopped above the list read")
	}
}

func TestCheckCacheWarnsOnWhatTheCacheHasBesides(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		execCache(t, ctx, st, `ALTER TABLE tasks ADD COLUMN zz TEXT`)
		execCache(t, ctx, st, `CREATE TABLE scratch (a)`)
	})

	result, cache := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "task(s)") {
		t.Errorf("summary = %q, want the counts: they were read and they are good", result.summary)
	}
	joined := noteText(result)
	for _, want := range []string{`"tasks.zz"`, `"scratch"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want them to name %s", result.notes, want)
		}
	}
	if strings.Contains(joined, collapsed(cacheAsideNote)) {
		t.Errorf("notes = %v, want nothing about moving the file aside: nothing here needs setting aside",
			result.notes)
	}
	if !cache.read {
		t.Errorf("cache.read = false after a check that read the lists")
	}
}

func TestCheckCacheWarnsOnAnIndexTheCacheLacks(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		execCache(t, ctx, st, `DROP INDEX idx_tasks_due`)
	})

	result, cache := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "1 task(s), 1 list(s)") {
		t.Errorf("summary = %q, want the counts: every read answered", result.summary)
	}
	joined := noteText(result)
	if !strings.Contains(joined, `"idx_tasks_due"`) {
		t.Errorf("notes = %v, want the missing index named", result.notes)
	}
	if !strings.Contains(joined, "none of them a table or a column") {
		t.Errorf("notes = %v, want the listing headed by what makes this the quiet half", result.notes)
	}
	if strings.Contains(joined, collapsed(cacheAsideNote)) {
		t.Errorf("notes = %v, want nothing about moving the file aside: the cache is usable", result.notes)
	}
	if !cache.read {
		t.Errorf("cache.read = false on a cache every read answered")
	}
}

func TestCheckCacheBoundsAnObjectNameOutOfTheCache(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		execCache(t, ctx, st, `CREATE TABLE `+quoteSQLName("a\nb")+` (x)`)
		execCache(t, ctx, st, `CREATE TABLE `+quoteSQLName(strings.Repeat("z", 300))+` (x)`)
	})

	result, _ := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	for i, line := range result.notes {
		if strings.Contains(line, "\n") {
			t.Errorf("note %d carries a line break, so the listing stops counting: %q", i+1, line)
		}
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("note %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
	if !strings.Contains(noteText(result), "do not produce") {
		t.Errorf("notes = %v, want the listing the names were cut for", result.notes)
	}
}

func TestDoctorListsToTheCapAndCountsTheRest(t *testing.T) {
	const extra = 25
	const listed = 20
	const counted = extra - listed
	if maxDoctorListed != listed {
		t.Fatalf("maxDoctorListed is %d and this fixture is written round %d: the report now cuts its "+
			"listings somewhere else, and the sentence over the constant has to be read again",
			maxDoctorListed, listed)
	}
	isolate(t)
	ctx := context.Background()
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		for i := 0; i < extra; i++ {
			execCache(t, ctx, st, `CREATE TABLE `+quoteSQLName("zz"+strconv.Itoa(i))+` (a)`)
		}
	})

	result, _ := checkCache(ctx)
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	rows := 0
	for _, line := range result.notes {
		if strings.HasPrefix(line, doctorNestedIndent+`table "zz`) {
			rows++
		}
	}
	if rows != listed {
		t.Errorf("the report names %d of the %d tables one by one, want %d", rows, extra, listed)
	}
	if want := "and " + strconv.Itoa(counted) + " more, not listed here"; !strings.Contains(noteText(result), want) {
		t.Errorf("notes = %v, want %q under the listing: the count is what the cap costs a reader",
			result.notes, want)
	}
	if !strings.Contains(noteText(result), strconv.Itoa(extra)+" thing(s)") {
		t.Errorf("notes = %v, want the whole number said above the listing whatever the cap prints",
			result.notes)
	}
}

func TestCheckCacheCarriesTheVersionNoteIntoAFail(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := buildCache(t, nil)
	latest := schemaLatest(t, path)
	if latest < 2 {
		t.Skip("this build carries one migration, so there is no version to be behind")
	}
	behind := latest - 1
	isolate(t)
	buildCache(t, func(ctx context.Context, st *store.Store) {
		seedCacheContents(t, ctx, st)
		shapeToVersion(t, ctx, st, behind, behind)
		execCache(t, ctx, st, `ALTER TABLE tasks DROP COLUMN content`)
	})

	result, _ := checkCache(ctx)
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, `"tasks.content"`) {
		t.Errorf("notes = %v, want the missing column named", result.notes)
	}
	if !strings.Contains(joined, "doctor does not apply the migration") {
		t.Errorf("notes = %v, want the version note carried down into the fail", result.notes)
	}
}

func TestCheckCacheReadsEveryVersion(t *testing.T) {
	isolate(t)
	latest := schemaLatest(t, buildCache(t, nil))
	for k := 1; k <= latest; k++ {
		t.Run("v"+strconv.Itoa(k), func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			buildCache(t, func(ctx context.Context, st *store.Store) {
				seedCacheContents(t, ctx, st)
				shapeToVersion(t, ctx, st, k, k)
			})

			result, cache := checkCache(ctx)
			want := statusWarn
			if k == latest {
				want = statusOK
			}
			if result.status != want {
				t.Fatalf("status = %v, want %v: %v", result.status, want, checkReport(result))
			}
			if !strings.Contains(result.summary, "(schema v"+strconv.Itoa(k)+", 1 task(s), 1 list(s)") {
				t.Errorf("summary = %q, want the version found and the counts read at it", result.summary)
			}
			if !cache.read {
				t.Errorf("cache.read = false on a cache every read answered")
			}
		})
	}
}

func TestCheckCacheHandsTheListNamesOn(t *testing.T) {
	t.Run("a cache with lists in it", func(t *testing.T) {
		isolate(t)
		ctx := context.Background()
		buildCache(t, func(ctx context.Context, st *store.Store) {
			execCache(t, ctx, st, `INSERT INTO projects (id, name) VALUES ('p2', 'Работа')`)
			execCache(t, ctx, st, `INSERT INTO projects (id, name) VALUES ('p1', 'Дом')`)
			execCache(t, ctx, st, `INSERT INTO projects (id, name) VALUES ('p3', '')`)
		})

		result, cache := checkCache(ctx)
		if result.status != statusOK {
			t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
		}
		if !cache.read {
			t.Fatalf("cache.read = false after the list read answered")
		}
		if cache.lists != 3 {
			t.Errorf("cache.lists = %d, want 3: a row with no name is a list too", cache.lists)
		}
		if want := []string{"Дом", "Работа"}; !slices.Equal(cache.names, want) {
			t.Errorf("cache.names = %v, want %v - the names, in the order the query sorted them, "+
				"and only the ones a name can be matched against", cache.names, want)
		}
	})

	t.Run("no cache at all", func(t *testing.T) {
		isolate(t)

		_, cache := checkCache(context.Background())
		if cache.read {
			t.Fatalf("cache.read = true where there is no cache to have read")
		}
		if len(cache.names) != 0 {
			t.Errorf("cache.names = %v, want nothing: no question was put to any file", cache.names)
		}
	})
}

func TestCheckConfigWarnsWhenATableIsOpenedTwice(t *testing.T) {
	cases := map[string]struct {
		content string
		status  checkStatus
		want    []string
		absent  []string
		clause  string
	}{
		"a dotted timer key at the root, and no header": {
			content: "timer.focus = '25m'\n",
			status:  statusOK,
		},
		"a dotted sync key at the root, and no header": {
			content: "sync.interval = '60s'\n",
			status:  statusOK,
		},
		"settings under a [timer] header": {
			content: "[timer]\nfocus = '25m'\n",
			status:  statusOK,
		},
		"settings under a [sync] header": {
			content: "[sync]\ninterval = '60s'\n",
			status:  statusOK,
		},
		"both a dotted timer key and a later header": {
			content: "timer.focus = '25m'\n\n[timer]\nshort_break = '5m'\n",
			status:  statusWarn,
			want:    []string{"timer.focus", "tomllib", "Cannot declare ('timer',) twice", "[timer]"},
			clause:  "the timer settings are not strict TOML 1.0",
		},
		"both a dotted sync key and a later header": {
			content: "sync.move_by_recreate = 'never'\n\n[sync]\ninterval = '60s'\n",
			status:  statusWarn,
			want:    []string{"sync.move_by_recreate", "Cannot declare ('sync',) twice", "[sync]"},
			absent:  []string{"[timer]"},
			clause:  "the sync settings are not strict TOML 1.0",
		},

		"both a dotted focus_upload key and a bare header": {
			content: "focus_upload.enabled = true\n\n[focus_upload]\n",
			status:  statusWarn,
			want:    []string{"focus_upload.enabled", "Cannot declare ('focus_upload',) twice"},
			clause:  "the focus_upload settings are not strict TOML 1.0",
		},
		"two tables at once, and the file's order decides": {
			content: "sync.move_by_recreate = 'never'\ntimer.focus = '25m'\n\n[timer]\n" +
				"auto_break = true\n\n[sync]\ninterval = '60s'\n",
			status: statusWarn,
			want: []string{
				"timer.focus", "sync.move_by_recreate",

				"Cannot declare ('timer',) twice",
			},
			absent: []string{"Cannot declare ('sync',) twice"},
			clause: "the timer and sync settings are not strict TOML 1.0",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			writeConfig(t, c.content)

			result := checkConfig(doctorCache{})
			if result.status != c.status {
				t.Fatalf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if c.clause == "" {
				if strings.Contains(result.summary, "strict TOML") {
					t.Errorf("summary = %q, want no word about strict TOML for a file that keeps the rule",
						result.summary)
				}
			} else if !strings.Contains(result.summary, c.clause) {
				t.Errorf("summary = %q, want it to carry the clause %q", result.summary, c.clause)
			}
			joined := noteText(result)
			for _, s := range c.want {
				if !strings.Contains(joined, s) {
					t.Errorf("notes = %v, want them to mention %q", result.notes, s)
				}
			}
			for _, s := range c.absent {
				if strings.Contains(joined, s) {
					t.Errorf("notes = %v, want nothing in them about %q", result.notes, s)
				}
			}

			if strings.Contains(joined, "only the timer settings") {
				t.Errorf("notes = %v, want no disclaimer about a check that now asks every table",
					result.notes)
			}
		})
	}
}

func TestOnEndSuggestionAgreesWithFix(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)
	path := writeConfig(t, "timer.on_break_end = \"echo break\"\n")

	if _, _, err := applyOnEndHint(path, hint); err == nil {
		t.Fatal("applyOnEndHint took a file whose timer keys are dotted at the root, so there is nothing to agree with")
	}

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn for an unset on_end: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, dottedTimerWayOutText()) {
		t.Errorf("notes = %v, want the two steps --fix itself names", result.notes)
	}
	if !strings.Contains(joined, "timer.on_break_end") {
		t.Errorf("notes = %v, want the key that stands in the way named", result.notes)
	}

	for _, l := range result.notes {
		if strings.HasPrefix(l, doctorNestedIndent) {
			t.Fatalf("notes = %v, want no block to paste into a file no paste can fix", result.notes)
		}
	}

	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != "timer.on_break_end = \"echo break\"\n" {
		t.Errorf("config = %q (%v), want it untouched", got, rerr)
	}
}

func TestCheckAPISkipsWhenThereIsNoToken(t *testing.T) {
	isolate(t)
	previous := newAPIClient
	newAPIClient = func(token string) *api.Client {
		t.Errorf("the API check built a client with no token in hand")

		return api.NewClient(token, version, api.WithBaseURL("http://127.0.0.1:1"))
	}
	t.Cleanup(func() { newAPIClient = previous })

	result := checkAPI(context.Background(), doctorToken{}, doctorCache{})
	if result.status != statusSkipped {
		t.Fatalf("status = %v, want skip: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "not tried") {
		t.Errorf("summary = %q, want it to say the check was not made", result.summary)
	}
	if len(result.notes) != 0 {
		t.Errorf("notes = %v, want nothing under it: the token check above has said it all", result.notes)
	}
}

func TestDoctorReportCountsASkipApartFromAFailure(t *testing.T) {
	cases := map[string]struct {
		results []checkResult
		want    string
		exit    int
	}{
		"a skip beside an ok": {
			results: []checkResult{{status: statusOK, summary: "one: fine"}, {status: statusSkipped, summary: "two: not tried"}},
			want:    "\n1 ok, 0 warn, 0 fail, 1 skip\n",
			exit:    exitOK,
		},
		"no skip at all": {
			results: []checkResult{
				{status: statusOK, summary: "one: fine"},
				{status: statusWarn, summary: "two: degraded"},
				{status: statusFail, summary: "three: broken"},
			},
			want: "\n1 ok, 1 warn, 1 fail\n",
			exit: exitError,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := doctorReport(&out, c.results); code != c.exit {
				t.Errorf("exit code = %d, want %d", code, c.exit)
			}
			if !strings.HasSuffix(out.String(), c.want) {
				t.Errorf("printed %q, want it to end on the tally %q", out.String(), c.want)
			}
		})
	}
}

func TestCheckAPINamesWhereTheTokenCameFrom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	previous := newAPIClient
	newAPIClient = func(token string) *api.Client {
		return api.NewClient(token, version, api.WithBaseURL(srv.URL))
	}
	t.Cleanup(func() { newAPIClient = previous })

	cases := map[string]struct {
		token        doctorToken
		want, unwant string
	}{
		"read out of the environment": {doctorToken{value: "stale", fromEnv: true}, "$" + tokenEnvVar, "token file"},
		"read out of the file":        {doctorToken{value: "stale"}, "the token file", tokenEnvVar},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			result := checkAPI(context.Background(), c.token, doctorCache{})
			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			if !strings.Contains(result.summary, "401") {
				t.Errorf("summary = %q, want the answer the server gave", result.summary)
			}
			if !strings.Contains(result.summary, c.want) {
				t.Errorf("summary = %q, want it to point at %s", result.summary, c.want)
			}
			if strings.Contains(result.summary, c.unwant) {
				t.Errorf("summary = %q, want nothing about %s", result.summary, c.unwant)
			}
		})
	}
}

func withAPIServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	previous := newAPIClient
	newAPIClient = func(token string) *api.Client {
		return api.NewClient(token, version, api.WithBaseURL(srv.URL), api.WithMaxRetries(1))
	}
	t.Cleanup(func() { newAPIClient = previous })
}

func apiAnswerLines(r checkResult) []string {
	var out []string
	for _, line := range r.notes {
		if strings.HasPrefix(line, doctorNestedIndent) {
			out = append(out, line)
		}
	}
	return out
}

func TestCheckAPIBoundsTheAnswerItCouldNotGetPast(t *testing.T) {

	const marker = "Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	previous := newAPIClient
	newAPIClient = func(token string) *api.Client {
		return api.NewClient(token, version, api.WithBaseURL(srv.URL+"/"+strings.Repeat(marker, 300)))
	}
	t.Cleanup(func() { newAPIClient = previous })
	srv.Close()

	result := checkAPI(context.Background(), doctorToken{value: "stale"}, doctorCache{})

	if result.summary != "api: unreachable, tt works offline" {
		t.Errorf("summary = %q, want the constant verdict of the branch where nothing answered", result.summary)
	}
	answer := apiAnswerLines(result)
	if len(answer) == 0 || len(answer) > apiAnswerMaxLines {
		t.Fatalf("the answer stands on %d lines, want between 1 and %d: %v", len(answer), apiAnswerMaxLines, result.notes)
	}
	budget := -(apiAnswerMaxLines - 1) * (quotedRuneMax - 1)
	for i := 0; i < apiAnswerMaxLines; i++ {
		budget += doctorNestedTextWidth
	}
	var total int
	for _, line := range answer {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("an answer line is %d runes, want at most %d: %q", n, cli.Width, line)
		}
		total += utf8.RuneCountInString(strings.TrimPrefix(line, doctorNestedIndent))
	}
	if total > budget {
		t.Errorf("the answer is %d runes over %d lines, want at most the %d the budget pays for",
			total, len(answer), budget)
	}

	for _, line := range result.notes {
		if strings.HasPrefix(line, doctorNestedIndent) {
			continue
		}
		if strings.Contains(line, marker) {
			t.Errorf("a note outside the answer carries it: %q", line)
		}
	}
}

func TestCheckAPIWritesAHostileAnswerAsTextAndNothingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("\x1b[2Jgone\r\nwarn cache: nothing wrong here \"ok\"\xff"))
	}))
	defer srv.Close()
	withAPIServer(t, srv)

	result := checkAPI(context.Background(), doctorToken{value: "stale"}, doctorCache{})
	for _, r := range result.summary {

		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("the verdict carries %U as itself: %q", r, result.summary)
		}
	}
	if !strings.Contains(result.summary, `\x1b`) || strings.Contains(result.summary, `\\x1b`) {
		t.Errorf("the verdict reads %q, want the raw body quoted exactly once", result.summary)
	}
	if len(result.notes) != 0 {
		t.Errorf("notes = %v, want nothing under a verdict that carries the answer itself", result.notes)
	}
}

func TestCheckAPIKeepsTheAnswerOutOfItsOwnSentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	withAPIServer(t, srv)
	srv.Close()

	result := checkAPI(context.Background(), doctorToken{value: "stale"}, doctorCache{})
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: tt works offline: %v", result.status, checkReport(result))
	}
	notes := noteText(result)
	for _, want := range []string{"Not all of it is tt's", "none of it is advice from tt"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes = %q, want %q over the block", notes, want)
		}
	}
	words := map[int]string{1: "one", 2: "two", 3: "three", 4: "four"}
	if want := "cut to at most " + words[apiAnswerMaxLines] + " lines"; !strings.Contains(notes, want) {
		t.Errorf("notes = %q, want %q - the note spells apiAnswerMaxLines out in words", notes, want)
	}
}

func TestCheckAPIKeepsTheReasonOnScreen(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	withAPIServer(t, srv)
	srv.Close()

	result := checkAPI(context.Background(), doctorToken{value: "stale"}, doctorCache{})
	answer := apiAnswerLines(result)
	if len(answer) == 0 {
		t.Fatalf("nothing came back under the verdict: %v", checkReport(result))
	}
	var joined string
	for _, line := range answer {
		joined += strings.TrimPrefix(line, doctorNestedIndent)
	}
	if !strings.Contains(joined, "transport failure") {
		t.Fatal("the answer lost the fixed transport category")
	}
	if strings.Contains(joined, srv.URL) {
		t.Fatal("the ordinary transport diagnostic copied its request URL")
	}
}

func TestCheckAPINamesTheRequestItMade(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		w.Write([]byte(`[{"id":"1","name":"one"}]`))
	}))
	defer srv.Close()
	withAPIServer(t, srv)

	result := checkAPI(context.Background(), doctorToken{value: "fine"}, doctorCache{})
	if result.status != statusOK {
		t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, apiCheckCall) {
		t.Errorf("summary = %q, want the one request it made named in it", result.summary)
	}
	if asked != apiCheckCall {
		t.Errorf("the check asked for %q and calls itself %q", asked, apiCheckCall)
	}
}

func TestCheckAPIVerdictByAnswer(t *testing.T) {
	answer := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if code != http.StatusOK {
				w.WriteHeader(code)
			}
			w.Write([]byte(body))
		}
	}
	lead := "api: " + apiCheckCall
	cases := []struct {
		name    string
		handler http.HandlerFunc
		status  checkStatus
		opening string
	}{
		{"a refusal with nothing to say", answer(401, ""), statusFail,
			lead + ": the token was rejected (401), check "},
		{"a refusal that said why", answer(401, `{"error":"token expired"}`), statusFail,
			lead + `: the token was rejected (401: "{\"error\":\"token expired\"}"), check `},
		{"forbidden", answer(403, `{"error":"forbidden"}`), statusFail,
			lead + `: the request was refused (403: "{\"error\":\"forbidden\"}")`},
		{"no such endpoint", answer(404, `{"error":"no such endpoint"}`), statusFail,
			lead + `: there is no such endpoint (404: "{\"error\":\"no such endpoint\"}")`},
		{"come back later", answer(429, `{"error":"rate limited"}`), statusWarn,
			lead + `: whatever answered asked tt to come back later (429: `},
		{"a failure at the other end", answer(500, `{"error":"internal"}`), statusWarn,
			lead + `: whatever answered failed (500: `},
		{"a code nobody anticipated", answer(418, `{"error":"teapot"}`), statusWarn,
			lead + `: unexpected answer (418: `},
		{"a page where the JSON should be", answer(200, "<html><body>sign in</body></html>"), statusWarn,
			lead + `: the answer could not be read (http 200: the answer could not be decoded: `},
		{"a 200 carrying nothing", answer(200, ""), statusWarn,
			lead + `: the answer could not be read (http 200: the answer carried nothing this call can use)`},

		{"a 200 carrying an empty object", answer(200, "{}"), statusWarn,
			lead + `: the answer could not be read (http 200: the answer carried nothing this call can use: "{}")`},
		{"more than the client reads", answer(200, strings.Repeat("x", 10<<20+1)), statusWarn,
			lead + `: the answer is larger than tt reads (http 200: over the `},
		{"two lists", answer(200, `[{"id":"1","name":"one"},{"id":"2","name":"two"}]`), statusOK,
			lead + ` answered, 2 list(s)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			withAPIServer(t, srv)

			result := checkAPI(context.Background(), doctorToken{value: "stale"}, doctorCache{})
			if result.status != c.status {
				t.Errorf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if !strings.HasPrefix(result.summary, c.opening) {
				t.Errorf("summary = %q, want it to open %q", result.summary, c.opening)
			}

			if strings.Contains(result.summary, "unreachable") {
				t.Errorf("summary = %q, want no word about the network from a server that answered", result.summary)
			}
			if !strings.Contains(result.summary, apiCheckCall) {
				t.Errorf("summary = %q, want the request it made named in it", result.summary)
			}
		})
	}
}

func TestCheckAPIKeepsAServerBodyInsideTheWidth(t *testing.T) {
	const advice = "the token FAKE-TOKEN-0123456789 has expired. To restore access run: " +
		"rm -rf ~/.local/share/ticktick"
	unbreakable := strings.Repeat("R", 300)

	for _, c := range []struct {
		name    string
		code    int
		body    string
		fromEnv bool
	}{
		{"two kilobytes with a word nothing can break", 500,
			strings.Repeat("word ", 250) + unbreakable + " " + strings.Repeat("word ", 100), false},
		{"an answer written as advice", 500, advice, false},
		{"a refusal, the token out of the file", 401, unbreakable, false},
		{"a refusal, the token out of the environment", 401, unbreakable, true},
		{"forbidden", 403, unbreakable, false},
		{"no such endpoint", 404, unbreakable, false},
		{"a code nobody anticipated", 418, unbreakable, false},
		{"come back later", 429, unbreakable, false},
		{"a failure at the other end", 503, unbreakable, false},
		{"a page where the JSON should be", 200, "<html>" + unbreakable + "</html>", false},
		{"an answer carrying nothing this call can use", 200, "{}", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.code != http.StatusOK {
					w.WriteHeader(c.code)
				}
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			withAPIServer(t, srv)

			result := checkAPI(context.Background(), doctorToken{value: "stale", fromEnv: c.fromEnv}, doctorCache{})
			report := checkReport(result)
			assertDoctorFits(t, "the api check", strings.Join(report, "\n"))
			for i, line := range report {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("line %d of the verdict is %d runes, want at most %d: %q", i+1, n, cli.Width, line)
				}
			}
			if c.body != advice {
				return
			}
			if strings.Contains(result.summary, c.body) {
				t.Errorf("summary = %q, want the answer only as the bound rendered it", result.summary)
			}

			want := cli.Foreign(c.body, apiDetailBudget-len("500: "))
			if !strings.Contains(result.summary, want) {
				t.Errorf("summary = %q, want the answer in it as %q", result.summary, want)
			}
		})
	}
}

func TestCheckAPIKeepsTheTokenOutOfAnAnswerThatEchoesIt(t *testing.T) {
	const token = "SECRETTOKENVALUE0123456789"

	t.Run("a body carrying the header it was sent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad ` + r.Header.Get("Authorization") + `"}`))
		}))
		defer srv.Close()
		withAPIServer(t, srv)

		result := checkAPI(context.Background(), doctorToken{value: token}, doctorCache{})
		if report := strings.Join(checkReport(result), "\n"); strings.Contains(report, token) {
			t.Fatal("the API report exposed credential material")
		}
		if !strings.Contains(result.summary, "[REDACTED]") {
			t.Fatal("the API report did not preserve the source redaction marker")
		}
	})

	t.Run("a redirect carrying it to a port nothing answers on", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/"+
				strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusFound)
		}))
		defer srv.Close()
		withAPIServer(t, srv)

		result := checkAPI(context.Background(), doctorToken{value: token}, doctorCache{})

		if result.summary != "api: unreachable, tt works offline" {
			t.Fatalf("summary = %q, want the verdict of the branch where nothing answered", result.summary)
		}
		answer := apiAnswerLines(result)
		if len(answer) == 0 {
			t.Fatalf("the verdict carries no answer under it: %v", result.notes)
		}
		var rejoined string
		for _, line := range answer {
			rejoined += strings.TrimPrefix(line, doctorNestedIndent)
		}
		if strings.Contains(rejoined, token) {
			t.Fatal("the unreachable API detail exposed credential material")
		}
		if !strings.Contains(rejoined, "transport failure") {
			t.Fatal("the unreachable API detail lost its fixed transport category")
		}
	})
}

func TestAPIRedactsTokenBeforeEscaping(t *testing.T) {
	const token = `TOKEN-"quoted"-\-0123456789ABCDEF`
	quoted := strconv.Quote(token)
	escapedToken := quoted[1 : len(quoted)-1]
	type rendering struct {
		name string
		make func(sent string) string
	}
	cases := []rendering{
		{
			name: "typed API detail",
			make: func(sent string) string {
				return apiDetail(&api.StatusError{StatusCode: 500, Body: "echo " + token + " end"}, sent)
			},
		},
		{
			name: "generic unreachable answer",
			make: func(sent string) string {
				lines := unreachableAnswerLines(errors.New("echo "+token+" end"), sent)
				var joined string
				for _, line := range lines {
					joined += strings.TrimPrefix(line, doctorNestedIndent)
				}
				return joined
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if withoutRedaction := c.make(""); !strings.Contains(withoutRedaction, escapedToken) {
				t.Fatal("fixture does not keep the encoded token inside the rendering budget")
			}
			got := c.make(token)
			if strings.Contains(got, token) {
				t.Error("rendering retained the raw token")
			}
			if strings.Contains(got, escapedToken) {
				t.Error("rendering retained the encoded token")
			}
			if !strings.Contains(got, "<the token tt sent>") {
				t.Error("rendering omitted the marker for the removed token")
			}
		})
	}
}

func TestCheckAPIWeighsAnEmptyAnswerAgainstTheCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	withAPIServer(t, srv)

	for _, c := range []struct {
		name   string
		cache  doctorCache
		status checkStatus
	}{
		{"a cache holding two lists", doctorCache{read: true, lists: 2}, statusWarn},
		{"a cache holding one, which may be the inbox", doctorCache{read: true, lists: 1}, statusOK},
		{"a cache that did not answer", doctorCache{}, statusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			result := checkAPI(context.Background(), doctorToken{value: "fine"}, c.cache)
			if result.status != c.status {
				t.Fatalf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if !strings.Contains(result.summary, "no lists at all") {
				t.Errorf("summary = %q, want the answer described in the words sync describes it in", result.summary)
			}
			if c.status != statusWarn {
				if len(result.notes) != 0 {
					t.Errorf("notes = %v, want nothing said about a cache this answer does not contradict", result.notes)
				}
				return
			}
			if !strings.Contains(result.summary, "the cache holds 2 list(s)") {
				t.Errorf("summary = %q, want doctor's own count of what the cache holds", result.summary)
			}

			if !slices.Contains(result.notes, doctorIndent+allowProjectDropHint) {
				t.Errorf("notes = %v, want %q on a line of its own", result.notes, allowProjectDropHint)
			}
			if !strings.Contains(allowProjectDropHint, sync.AllowProjectDropFlag) {
				t.Errorf("allowProjectDropHint = %q, want it to name %s", allowProjectDropHint, sync.AllowProjectDropFlag)
			}
		})
	}
}

func TestCheckTokenRefusesAValueThatCannotBeSent(t *testing.T) {
	for _, c := range []struct {
		name    string
		value   string
		kind    string
		sources []string
	}{
		{"a token pasted across two lines", "abc\ndef", "a line break", []string{"$" + tokenEnvVar, "the file"}},
		{"a file that was never text", "abc\x00def", "a NUL", []string{"the file"}},
		{"a value carrying a control byte", "abc\x01def", "a control character", []string{"$" + tokenEnvVar, "the file"}},
	} {
		for _, source := range c.sources {
			t.Run(c.name+" out of "+source, func(t *testing.T) {
				isolate(t)
				if source == "the file" {
					writeTokenFile(t, c.value+"\n")
				} else {
					t.Setenv(tokenEnvVar, c.value)
				}

				result, token := checkToken()
				if result.status != statusFail {
					t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
				}
				if !strings.Contains(result.summary, c.kind) {
					t.Errorf("summary = %q, want the byte named as %q", result.summary, c.kind)
				}
				if !strings.Contains(result.summary, "cannot be sent") {
					t.Errorf("summary = %q, want it to say nothing was sent", result.summary)
				}
				report := strings.Join(checkReport(result), "\n")
				if strings.Contains(report, c.value) {
					t.Errorf("the report carries the value: %q", report)
				}
				if token.value != "" {
					t.Errorf("token = %q, want nothing handed on to the API check", token.value)
				}

				paste := strings.Contains(noteText(result), "across two lines")
				if want := c.kind == "a line break"; paste != want {
					t.Errorf("notes = %q, want the paste sentence under a line break and under nothing else",
						noteText(result))
				}

				if source != "the file" {
					onOneLine := strings.Contains(noteText(result), "on one line")
					if want := c.kind == "a line break"; onOneLine != want {
						t.Errorf("notes = %q, want the remedy to follow the byte that was found", noteText(result))
					}
				}

				if api := checkAPI(context.Background(), token, doctorCache{}); api.status != statusSkipped {
					t.Errorf("the api check = %v, want skip after a token that cannot be sent: %v",
						api.status, checkReport(api))
				}
			})
		}
	}
}

func TestCheckTokenLooksAtTheValueItVouchesFor(t *testing.T) {
	const page = "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>"

	t.Run("with nothing else wrong", func(t *testing.T) {
		isolate(t)
		path := writeTokenFile(t, page+"\n")

		result, token := checkToken()
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		if !strings.Contains(result.summary, path) || !strings.Contains(result.summary, "mode 0600") {
			t.Errorf("summary = %q, want the path and the mode it always carried", result.summary)
		}
		if !strings.Contains(result.summary, "not shaped like a token") {
			t.Errorf("summary = %q, want it to say the value is not shaped like a token", result.summary)
		}

		if token.value != page {
			t.Errorf("token = %q, want the value handed on as it stands", token.value)
		}
		if n := strings.Count(strings.Join(result.notes, "\n"), loginHint); n != 1 {
			t.Errorf("notes = %v, want %q exactly once", result.notes, loginHint)
		}
		if !strings.Contains(noteText(result), "no refresh token saved") {
			t.Errorf("notes = %v, want the independent missing-refresh observation", result.notes)
		}
	})

	t.Run("beside an oauth.json that will not parse", func(t *testing.T) {
		isolate(t)
		writeTokenFile(t, page+"\n")
		credPath, err := credentialsPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(credPath, []byte("{not json at all"), 0o600); err != nil {
			t.Fatal(err)
		}

		result, _ := checkToken()
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		if !strings.Contains(notes, credPath) {
			t.Errorf("notes = %v, want the file that cannot be read named", result.notes)
		}
		if strings.Contains(notes, loginHint) {
			t.Errorf("notes = %v, want no %q under the sentence saying that command stops on this file",
				result.notes, loginHint)
		}
	})
}

func TestCheckConfigMissingFileIsStillOK(t *testing.T) {
	isolate(t)

	result := checkConfig(doctorCache{})
	if result.status != statusOK {
		t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "not found") {
		t.Errorf("summary = %q, want it to say the file is missing", result.summary)
	}
	if !strings.Contains(result.summary, "default_project not checked") {
		t.Errorf("summary = %q, want the settings question reported as not put: there is no cache to "+
			"put it to", result.summary)
	}
	if strings.Contains(result.summary, "cannot be created") {
		t.Errorf("summary = %q, want nothing about creating a config in a directory that takes one",
			result.summary)
	}
	notes := noteText(result)
	for _, command := range []string{"--fix", "config --init"} {
		if strings.Contains(notes, command) {
			t.Errorf("notes = %v, want no %q offered to a machine with nothing wrong with it",
				result.notes, command)
		}
	}

	if strings.Contains(notes, "could not be read") {
		t.Errorf("notes = %v, want no reading failure claimed about a machine that has never synced",
			result.notes)
	}
}

func TestCheckConfigInvalidFileIsStillFail(t *testing.T) {
	isolate(t)
	writeConfig(t, "default_project = \"unterminated\n")

	result := checkConfig(doctorCache{read: true, lists: 1, names: []string{"Inbox"}})
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "invalid") {
		t.Errorf("summary = %q, want it to say the file is invalid", result.summary)
	}
	if strings.Contains(result.summary, "default_project") {
		t.Errorf("summary = %q, want no clause about a setting read off a file the parser refused",
			result.summary)
	}
}

func TestCheckConfigCannotReadIsNotInvalid(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file at mode 000 anyway")
	}
	isolate(t)
	path := writeConfig(t, "default_project = 'Work'\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })

	result := checkConfig(doctorCache{})
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, "cannot be read") {
		t.Errorf("summary = %q, want it to say the file could not be read", result.summary)
	}
	if strings.Contains(result.summary, "invalid") {
		t.Errorf("summary = %q, want no verdict about the contents of a file nothing opened",
			result.summary)
	}
	if notes := noteText(result); !strings.Contains(notes, `permission\x20denied`) {
		t.Errorf("notes = %v, want the refusal itself carried under the verdict", result.notes)
	}
}

func TestCheckConfigChecksDefaultProjectAgainstTheCache(t *testing.T) {
	shipped := config.Default().DefaultProject
	cases := map[string]struct {
		content string
		cache   doctorCache
		status  checkStatus
		clause  string
		want    []string
		absent  []string

		named map[string]int
	}{
		"the name reaches one list": {
			content: "default_project = 'Work'\n",
			cache:   doctorCache{read: true, lists: 2, names: []string{"Inbox", "Work"}},
			status:  statusOK,
			clause:  "default_project matches one list",

			absent: []string{"default_project is"},
		},
		"the name reaches nothing and the file sets it": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{read: true, lists: 2, names: []string{"Inbox", "Work"}},
			status:  statusWarn,
			clause:  "default_project matches no list",
			want: []string{
				"default_project is \"Nonexistent List\" and no list in the cache has that name",
				"the list names in the cache: \"Inbox\", \"Work\"",
				"that name is what \"tt add\" uses when no list is given",
				"a list renamed on the server reaches the cache with a sync",
				"that key set to one of those names, in the file above, settles it",
			},

			absent: []string{"the name tt ships with", "carry no name"},
		},
		"the name reaches nothing beside named and unnamed rows": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{read: true, lists: 4, names: []string{"Inbox", "Work"}},
			status:  statusWarn,
			clause:  "default_project matches no list",
			want: []string{
				"the list names in the cache: \"Inbox\", \"Work\"",
				"2 cache row(s) counted above carry no name and cannot be listed",
			},
			named: map[string]int{"Inbox": 1, "Work": 1},
		},
		"the name reaches nothing and the file that was read does not set it": {
			content: config.Template(),
			cache:   doctorCache{read: true, lists: 2, names: []string{"Inbox", "Work"}},
			status:  statusWarn,
			clause:  "default_project matches no list",
			want: []string{
				"default_project is \"" + shipped + "\" and no list in the cache has that name",
				"the config file does not set it, so this is the name tt ships with",
				"that key set to one of those names, in the file above, settles it",
			},
			absent: []string{"nothing is at that path", "tt config --init"},
		},
		"the name reaches nothing and there is no file": {
			cache:  doctorCache{read: true, lists: 2, names: []string{"Inbox", "Work"}},
			status: statusWarn,
			clause: "default_project matches no list",
			want: []string{
				"nothing is at that path to set it, so this is the name tt ships with",
				"\"tt config --init\" writes a starter config at that path",
			},

			absent: []string{"in the file above"},
		},
		"the name reaches several lists": {
			content: "default_project = 'Work'\n",
			cache:   doctorCache{read: true, lists: 2, names: []string{"Work A", "Work B"}},
			status:  statusWarn,
			clause:  "default_project matches 2 lists",
			want: []string{
				"default_project is \"Work\" and 2 lists match it: \"Work A\", \"Work B\"",
				"\"tt add\" refuses a name that matches more than one list; the name must resolve to one cached list, and an exact name shared by several lists is still ambiguous",
			},
		},
		"the cache check did not get as far as the names": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{},
			status:  statusOK,
			clause:  "default_project not checked",
			want:    []string{"the cache check above did not get as far as the list names"},

			absent: []string{"could not be read"},
		},
		"the cache answered and holds no lists yet": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{read: true},
			status:  statusOK,
			clause:  "default_project not checked",
			want:    []string{"the cache holds no lists yet, and a sync is what fills it"},
			absent:  []string{"matches no list"},
		},

		"the cache holds rows and not one of them carries a name": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{read: true, lists: 3},
			status:  statusOK,
			clause:  "default_project not checked",
			want:    []string{"the cache holds 3 list(s) and not one of them carries a name"},
			absent:  []string{"the cache holds no lists yet", "the list names in the cache:"},
		},
		"two rows carry one name and the listing counts them both": {
			content: "default_project = 'Nonexistent List'\n",
			cache:   doctorCache{read: true, lists: 2, names: []string{"Work", "Work"}},
			status:  statusWarn,
			clause:  "default_project matches no list",
			named:   map[string]int{"Work": 2},
		},

		"two rows carry one name and both are what makes it ambiguous": {
			content: "default_project = 'work'\n",
			cache:   doctorCache{read: true, lists: 2, names: []string{"Work", "Work"}},
			status:  statusWarn,
			clause:  "default_project matches 2 lists",
			want:    []string{"and 2 lists match it: "},
			named:   map[string]int{"Work": 2},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if c.content != "" {
				writeConfig(t, c.content)
			}

			result := checkConfig(c.cache)
			if result.status != c.status {
				t.Fatalf("status = %v, want %v: %v", result.status, c.status, checkReport(result))
			}
			if !strings.Contains(result.summary, c.clause) {
				t.Errorf("summary = %q, want it to carry the clause %q", result.summary, c.clause)
			}
			notes := noteText(result)
			for _, s := range c.want {
				if !strings.Contains(notes, s) {
					t.Errorf("notes = %v, want them to say %q", result.notes, s)
				}
			}
			for _, s := range c.absent {
				if strings.Contains(notes, s) {
					t.Errorf("notes = %v, want nothing in them about %q", result.notes, s)
				}
			}
			for name, times := range c.named {
				if got := strings.Count(notes, strconv.Quote(name)); got != times {
					t.Errorf("notes = %v, want %q named %d time(s) and it stands there %d, which is "+
						"a listing disagreeing with the count printed beside it",
						result.notes, name, times, got)
				}
			}
			assertDoctorFits(t, "the config check", strings.Join(checkReport(result), "\n"))
		})
	}

	t.Run("the rendered cache count includes the unnamed rows", func(t *testing.T) {
		isolate(t)
		writeConfig(t, "default_project = 'Nonexistent List'\n")
		ctx := context.Background()
		buildCache(t, func(ctx context.Context, st *store.Store) {
			for _, project := range []struct{ id, name string }{
				{"p1", "Inbox"}, {"p2", "Work"}, {"p3", ""}, {"p4", ""},
			} {
				execCache(t, ctx, st, `INSERT INTO projects (id, name) VALUES (?, ?)`, project.id, project.name)
			}
		})
		cacheResult, cache := checkCache(ctx)
		configResult := checkConfig(cache)
		var out strings.Builder
		doctorReport(&out, []checkResult{cacheResult, configResult})
		report := collapsed(out.String())
		for _, want := range []string{
			"4 list(s)",
			"the list names in the cache: \"Inbox\", \"Work\"",
			"2 cache row(s) counted above carry no name and cannot be listed",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("combined cache and config report omitted %q", want)
			}
		}
	})
}

func TestCheckConfigSummaryKeepsItsClausesInOneOrder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory at mode 500 anyway")
	}

	t.Run("nothing at the path, and nothing that can be put there", func(t *testing.T) {
		isolate(t)
		path, err := config.Path()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })

		result := checkConfig(withObservedConfigWrite(doctorCache{
			read: true, lists: 2, names: []string{"Inbox", "Work"},
		}))
		want := "config: " + strconv.Quote(path) + " (not found, using defaults, its target directory refused a config-write probe, " +
			"default_project matches no list)"
		if result.summary != want {
			t.Errorf("summary  = %q\nwant       %q", result.summary, want)
		}
	})

	t.Run("a file tt read that a strict reader would refuse", func(t *testing.T) {
		isolate(t)
		path := writeConfig(t, "timer.focus = '25m'\n\n[timer]\nshort_break = '5m'\n")

		matching := doctorCache{read: true, lists: 1, names: []string{config.Default().DefaultProject}}
		result := checkConfig(matching)
		want := "config: " + strconv.Quote(path) + " (found, tt reads it, the timer settings are not strict TOML 1.0, " +
			"default_project matches one list)"
		if result.summary != want {
			t.Errorf("summary  = %q\nwant       %q", result.summary, want)
		}
	})
}

func TestCheckConfigNamesAtMostTheCapOfLists(t *testing.T) {
	isolate(t)
	writeConfig(t, "default_project = 'Nonexistent List'\n")
	extra := 7
	var names []string
	for i := 0; i < maxDoctorListed+extra; i++ {
		names = append(names, fmt.Sprintf("list-%03d", i))
	}

	result := checkConfig(doctorCache{read: true, lists: len(names), names: names})
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
	}
	notes := noteText(result)
	if want := fmt.Sprintf("and %d more", extra); !strings.Contains(notes, want) {
		t.Errorf("notes = %v, want the trailer %q", result.notes, want)
	}
	if !strings.Contains(notes, names[maxDoctorListed-1]) {
		t.Errorf("notes = %v, want the last name inside the cap named", result.notes)
	}
	if strings.Contains(notes, names[maxDoctorListed]) {
		t.Errorf("notes = %v, want nothing past the cap named", result.notes)
	}
	assertDoctorFits(t, "the config check", strings.Join(checkReport(result), "\n"))
}

func TestConfigListNamesKeepsTheOrderItWasGiven(t *testing.T) {

	names := []string{"work", "Inbox", "aaa"}
	want := `"work", "Inbox", "aaa"`
	if got := configListNames(names); got != want {
		t.Errorf("configListNames(%q) = %q, want %q - the names are printed in the order the cache "+
			"handed them over", names, got, want)
	}
}

func TestCheckConfigLeavesNothingBehind(t *testing.T) {
	cases := []struct {
		name          string
		makeConfigDir bool
	}{
		{"the config directory exists and is probed", true},
		{"the config directory is absent and its parent is probed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(path)
			if c.makeConfigDir {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				dir = filepath.Dir(dir)
			}

			result := checkConfig(withObservedConfigWrite(doctorCache{}))
			if result.status != statusOK {
				t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, e := range entries {
				left = append(left, e.Name())
			}
			if len(left) != 0 {
				t.Errorf("%s holds %v after a check that only reports", dir, left)
			}
		})
	}
}

func TestCheckConfigWarnsAboutASymlinkWithNoTarget(t *testing.T) {

	matching := doctorCache{read: true, lists: 1, names: []string{config.Default().DefaultProject}}
	elsewhere := doctorCache{read: true, lists: 1, names: []string{"Inbox"}}

	t.Run("a link to a name that was never created", func(t *testing.T) {
		isolate(t)
		linkConfig(t, "dotfiles/config.toml")

		result := checkConfig(matching)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn even where the settings question answers cleanly: %v",
				result.status, checkReport(result))
		}
		if !strings.Contains(result.summary, "a symbolic link with no target, using defaults") {
			t.Errorf("summary = %q, want it to say what is at the path", result.summary)
		}
		if strings.Contains(result.summary, "not found") {
			t.Errorf("summary = %q, want it not to call a path with a link on it empty", result.summary)
		}
		notes := noteText(result)
		if !strings.Contains(notes, "it points at \"dotfiles/config.toml\"") {
			t.Errorf("notes = %v, want the link's own text named", result.notes)
		}
		if !strings.Contains(notes, "nothing is read from it and every setting is the default") {
			t.Errorf("notes = %v, want the consequence said: this is the line a reader looks for "+
				"after their focus quietly went back to 25m", result.notes)
		}
	})

	t.Run("a link to a name that was never created, and a name that reaches nothing", func(t *testing.T) {
		isolate(t)
		linkConfig(t, "dotfiles/config.toml")

		result := checkConfig(elsewhere)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		if !strings.Contains(notes, "nothing tt can read sets it, so this is the name tt ships with") {
			t.Errorf("notes = %v, want the origin worded to what tt established", result.notes)
		}

		if strings.Contains(notes, "nothing sets it in a file") {
			t.Errorf("notes = %v, want no claim about a file nobody read", result.notes)
		}

		if strings.Contains(notes, "writes a starter config at that path") {
			t.Errorf("notes = %v, want no way out that this very path refuses", result.notes)
		}
	})

	t.Run("a chain whose first hop exists", func(t *testing.T) {
		isolate(t)
		dir := configDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("b.toml", filepath.Join(dir, "a.toml")); err != nil {
			t.Fatal(err)
		}
		linkConfig(t, "a.toml")

		result := checkConfig(matching)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)

		if !strings.Contains(notes, "it points at \"a.toml\"") {
			t.Errorf("notes = %v, want the one hop named", result.notes)
		}
		if strings.Contains(notes, "b.toml") {
			t.Errorf("notes = %v, want no claim about the far end of a chain this check did not walk",
				result.notes)
		}
	})

	t.Run("a link that resolves is an ordinary file", func(t *testing.T) {
		isolate(t)
		dir := configDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "real.toml"), []byte("default_project = 'Work'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		linkConfig(t, "real.toml")

		result := checkConfig(doctorCache{read: true, lists: 1, names: []string{"Work"}})
		if result.status != statusOK {
			t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
		}
		if !strings.Contains(result.summary, "found, tt reads it") {
			t.Errorf("summary = %q, want the ordinary verdict", result.summary)
		}

		if strings.Contains(noteText(result), "symbolic link") {
			t.Errorf("notes = %v, want no remark about a link that resolves", result.notes)
		}
	})

	t.Run("a link tt could not follow", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory at mode 000 anyway")
		}
		isolate(t)
		dir := configDir(t)
		locked := filepath.Join(dir, "secret")
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "config.toml"), []byte("default_project = 'Work'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o700) })
		linkConfig(t, "secret/config.toml")

		result := checkConfig(matching)
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
		}
		if !strings.Contains(result.summary, `permission\x20denied`) {
			t.Errorf("summary = %q, want the refusal as it stands", result.summary)
		}

		for _, wrong := range []string{"invalid", "no target"} {
			if strings.Contains(result.summary, wrong) {
				t.Errorf("summary = %q, want no %q about a target nothing established anything about",
					result.summary, wrong)
			}
		}
	})
}

func TestConfigWriteNotesKeepCompleteHostileTargetAndAnchorTails(t *testing.T) {
	isolate(t)
	target := strings.Repeat("target-", 24) + "line\n\x1b-target-tail"
	path := linkConfig(t, target)
	linkNotes := strings.Join(configLinkNotes(path, false), "\n")
	if !strings.Contains(linkNotes, fullReportAtom(target)) || strings.Contains(linkNotes, "\x1b") {
		t.Fatalf("link notes lost or exposed the target tail: %q", linkNotes)
	}

	anchor := filepath.Join(t.TempDir(), strings.Repeat("anchor-", 24)+"line\n\x1b-anchor-tail")
	refused := &os.PathError{Op: "open", Path: filepath.Join(anchor, ".tt-doctor-test"), Err: os.ErrPermission}
	refusalNotes := strings.Join(configWriteRefusal(anchor, refused, false), "\n")
	if !strings.Contains(refusalNotes, fullReportAtom(anchor)) || strings.Contains(refusalNotes, "\x1b") {
		t.Fatalf("write-refusal notes lost or exposed the anchor tail: %q", refusalNotes)
	}
}

func linkConfig(t *testing.T, target string) string {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckConfigWarnsWhenNothingCanBeCreatedThere(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory at mode 500 anyway")
	}
	t.Run("the config directory refuses a write", func(t *testing.T) {
		isolate(t)
		dir := configDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })

		cache := withObservedConfigWrite(doctorCache{})
		result := checkConfig(cache)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		if !strings.Contains(result.summary, "target directory refused a config-write probe") {
			t.Errorf("summary = %q, want the clause that stops \"not found\" reading as \"one can be made\"",
				result.summary)
		}
		notes := noteText(result)

		if !strings.Contains(notes, fullReportAtom(cache.configWrite.anchor)+": permission denied") {
			t.Errorf("notes = %v, want the directory that refused named with the reason", result.notes)
		}

		if strings.Contains(notes, ".tt-doctor-") {
			t.Errorf("notes = %v, want the directory named rather than the probe's own file",
				result.notes)
		}
		if !strings.Contains(notes, "is what settles it") {
			t.Errorf("notes = %v, want a way out under a warn, as every other note here has",
				result.notes)
		}
	})

	t.Run("the config directory is absent and its parent refuses the mkdir", func(t *testing.T) {
		isolate(t)
		parent := filepath.Dir(configDir(t))
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(parent, 0o700) })

		cache := withObservedConfigWrite(doctorCache{})
		result := checkConfig(cache)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		if notes := noteText(result); !strings.Contains(notes,
			fullReportAtom(cache.configWrite.anchor)+": permission denied") {
			t.Errorf("notes = %v, want the nearest existing ancestor named: that is where MkdirAll "+
				"would have to write", result.notes)
		}
	})

	t.Run("the config directory is a link that leads nowhere", func(t *testing.T) {
		isolate(t)
		dir := configDir(t)
		parent := filepath.Dir(dir)
		if err := os.Symlink(filepath.Join(parent, "not-there"), dir); err != nil {
			t.Fatal(err)
		}

		cache := withObservedConfigWrite(doctorCache{})
		result := checkConfig(cache)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		if !strings.Contains(notes, fullReportAtom(cache.configWrite.anchor)+
			" is a symbolic link that leads nowhere") {
			t.Errorf("notes = %v, want the component that cannot become a directory named", result.notes)
		}
		if !strings.Contains(notes, "removing the link, is what settles it") {
			t.Errorf("notes = %v, want the way out this shape has", result.notes)
		}

		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tt-doctor-") {
				t.Errorf("%s holds %s: the walk wrote above a component that stops it", parent, e.Name())
			}
		}
	})

	t.Run("the settings verdict offers no way out through a path that takes no config", func(t *testing.T) {
		isolate(t)
		dir := configDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })

		result := checkConfig(withObservedConfigWrite(doctorCache{
			read: true, lists: 2, names: []string{"Inbox", "Work"},
		}))
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		if !strings.Contains(notes, "target directory refused a probe") {
			t.Fatalf("notes = %v, want the refusal this case is built on", result.notes)
		}
		if !strings.Contains(notes, "no list in the cache has that name") {
			t.Fatalf("notes = %v, want the settings verdict reached in full", result.notes)
		}
		if strings.Contains(notes, "tt config --init") {
			t.Errorf("notes = %v, want no command offered for writing a config at a path this same "+
				"block has just said takes none", result.notes)
		}

		if !strings.Contains(notes, "nothing is at that path to set it, so this is the name tt ships with") {
			t.Errorf("notes = %v, want the origin sentence, which is true whatever the probe found",
				result.notes)
		}
		if !strings.Contains(notes, "the list names in the cache: ") {
			t.Errorf("notes = %v, want the names the configured one could have been", result.notes)
		}
	})
}

func configDir(t *testing.T) string {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path)
}

func withObservedConfigWrite(cache doctorCache) doctorCache {
	cache.configWrite = observeConfigWrite()
	return cache
}

func refusedConfigWrite(anchor string) configWriteObservation {
	return configWriteObservation{
		state:  configWriteRefused,
		anchor: anchor,
		err:    &os.PathError{Op: "open", Path: filepath.Join(anchor, ".tt-doctor-test"), Err: os.ErrPermission},
	}
}

func TestCheckConfigCombinesAWriteRefusalWithEverySourceState(t *testing.T) {
	const anchor = "/refused/config-target"
	matching := doctorCache{
		read:        true,
		lists:       1,
		names:       []string{"Work"},
		configWrite: refusedConfigWrite(anchor),
	}

	t.Run("missing", func(t *testing.T) {
		isolate(t)
		cache := matching
		cache.names = []string{"Inbox"}
		result := checkConfig(cache)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		for _, want := range []string{"no config can be read", anchor, "no list in the cache has that name", "with a sync"} {
			if !strings.Contains(notes, want) {
				t.Errorf("notes = %v, want %q retained", result.notes, want)
			}
		}
		if strings.Contains(notes, "config --init") {
			t.Errorf("notes = %v, want no config-writing command after its target refused the probe", result.notes)
		}
	})

	t.Run("readable and valid", func(t *testing.T) {
		isolate(t)
		writeConfig(t, "default_project = 'Work'\n")
		result := checkConfig(matching)
		if result.status != statusWarn {
			t.Fatalf("status = %v, want refusal to raise ok to warn: %v", result.status, checkReport(result))
		}
		notes := noteText(result)
		if !strings.Contains(notes, "the config path exists") || !strings.Contains(notes, anchor) {
			t.Errorf("notes = %v, want the existing-file refusal and measured directory", result.notes)
		}
		if strings.Contains(notes, "every setting is the default") {
			t.Errorf("notes = %v, want no missing-file claim for a config that was read", result.notes)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		isolate(t)
		writeConfig(t, "[timer\n")
		result := checkConfig(matching)
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail preserved: %v", result.status, checkReport(result))
		}
		if notes := noteText(result); !strings.Contains(notes, anchor) {
			t.Errorf("notes = %v, want the independent write refusal retained", result.notes)
		} else {
			for _, falseClaim := range []string{"in use", "still attempts the real write", "every setting is the default"} {
				if strings.Contains(notes, falseClaim) {
					t.Errorf("notes = %v, want no %q claim about an invalid config", result.notes, falseClaim)
				}
			}
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		isolate(t)
		path, err := config.Path()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		result := checkConfig(matching)
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail preserved: %v", result.status, checkReport(result))
		}
		if notes := noteText(result); !strings.Contains(notes, anchor) {
			t.Errorf("notes = %v, want the independent write refusal retained", result.notes)
		} else {
			for _, falseClaim := range []string{"in use", "still attempts the real write", "every setting is the default"} {
				if strings.Contains(notes, falseClaim) {
					t.Errorf("notes = %v, want no %q claim about an unreadable config", result.notes, falseClaim)
				}
			}
		}
	})
}

func TestCheckConfigTreatsSuccessfulAndUnknownWriteObservationsAsNoRefusal(t *testing.T) {
	for name, state := range map[string]configWriteState{
		"successful probe": configWriteSucceeded,
		"unknown":          configWriteUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			cache := doctorCache{
				read:        true,
				lists:       1,
				names:       []string{"Inbox"},
				configWrite: configWriteObservation{state: state},
			}
			result := checkConfig(cache)
			if result.status != statusWarn {
				t.Fatalf("status = %v, want only the default_project warn: %v", result.status, checkReport(result))
			}
			notes := noteText(result)
			if !strings.Contains(notes, "config --init") {
				t.Errorf("notes = %v, want ordinary write advice when no refusal was observed", result.notes)
			}
			if strings.Contains(notes, "write probe was refused") || strings.Contains(notes, "refused a probe") {
				t.Errorf("notes = %v, want no refusal inferred from %s", result.notes, name)
			}
		})
	}
}

func TestCheckConfigWithAWriteRefusalKeepsDiagnosisAndDropsEditAdvice(t *testing.T) {
	refused := refusedConfigWrite("/refused/config-target")
	t.Run("dotted tables and default_project", func(t *testing.T) {
		isolate(t)
		writeConfig(t, "default_project = 'Elsewhere'\ntimer.focus = '25m'\n\n[timer]\nshort_break = '5m'\n")
		result := checkConfig(doctorCache{
			read: true, lists: 1, names: []string{"Inbox"}, configWrite: refused,
		})
		notes := noteText(result)
		for _, want := range []string{"opened twice", "TOML 1.0 allows", "no list in the cache", "with a sync"} {
			if !strings.Contains(notes, want) {
				t.Errorf("notes = %v, want diagnosis or non-writing remedy %q", result.notes, want)
			}
		}
		for _, unwanted := range []string{"moving those under", "that key set to one of those names"} {
			if strings.Contains(notes, unwanted) {
				t.Errorf("notes = %v, want no config edit advice %q", result.notes, unwanted)
			}
		}
	})

	t.Run("dangling config link", func(t *testing.T) {
		isolate(t)
		linkConfig(t, "missing/config.toml")
		cache := doctorCache{
			read: true, lists: 1, names: []string{config.Default().DefaultProject}, configWrite: refused,
		}
		result := checkConfig(cache)
		notes := noteText(result)
		for _, want := range []string{"symbolic link", "it points at", "config --init\" refuses"} {
			if !strings.Contains(notes, want) {
				t.Errorf("notes = %v, want link diagnosis %q", result.notes, want)
			}
		}
		for _, unwanted := range []string{"creating the file at the end", "--fix writes to the file"} {
			if strings.Contains(notes, unwanted) {
				t.Errorf("notes = %v, want no target-write advice %q", result.notes, unwanted)
			}
		}
	})
}

func TestObserveConfigWriteUsesTheResolvedTargetDirectory(t *testing.T) {
	isolate(t)
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "actual.toml")
	if err := os.WriteFile(target, []byte("default_project = 'Work'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	got := observeConfigWrite()
	if got.state != configWriteSucceeded {
		t.Fatalf("observation = %+v, want a successful probe", got)
	}
	want, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.anchor != want {
		t.Errorf("probe directory = %q, want resolved target directory %q", got.anchor, want)
	}
}

func TestObserveConfigWriteDoesNotSkipAnExistingConfig(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "default_project = 'Work'\n")
	wantTarget, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	got := observeConfigWriteWithProbe(func(target string) (string, error) {
		calls++
		if target != wantTarget {
			t.Errorf("probed target = %q, want %q", target, wantTarget)
		}
		return filepath.Dir(target), os.ErrPermission
	})
	if calls != 1 {
		t.Fatalf("probe called %d times, want once for an existing config", calls)
	}
	if got.state != configWriteRefused {
		t.Fatalf("observation = %+v, want injected refusal retained", got)
	}
}

func TestObserveConfigWriteRefusesARealNonWritableExistingTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory at mode 500 anyway")
	}
	isolate(t)
	path := writeConfig(t, "default_project = 'Work'\n")
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	got := observeConfigWrite()
	if got.state != configWriteRefused {
		t.Fatalf("observation = %+v, want the real temporary-file write refused", got)
	}
	result := checkConfig(doctorCache{
		read: true, lists: 1, names: []string{"Work"}, configWrite: got,
	})
	if result.status != statusWarn {
		t.Fatalf("status = %v, want existing-file refusal to raise ok to warn: %v",
			result.status, checkReport(result))
	}
}

func TestWriteRefusalSuppressesOnlyNotifyConfigEdits(t *testing.T) {
	macOS := notifyMachine{system: "macos", goos: "darwin"}
	unset := notifyGroup{
		notifySettingOutcome: notifySettingOutcome{key: onEndKey, kind: notifyUnset},
		keys:                 []string{onEndKey},
	}
	assertAbsent := func(t *testing.T, lines []string, unwanted ...string) {
		t.Helper()
		joined := collapsed(strings.Join(lines, " "))
		for _, s := range unwanted {
			if strings.Contains(joined, s) {
				t.Errorf("lines = %v, want no config-writing advice %q", lines, s)
			}
		}
	}

	t.Run("unset outcomes keep machine facts and variables", func(t *testing.T) {
		cases := []struct {
			name   string
			report notifyReport
			want   []string
		}{
			{
				name: "no ready command",
				report: notifyReport{
					machine: notifyMachine{system: "unknown", goos: "windows"}, writeRefused: true,
				},
				want: []string{"no ready command for this system", "$TT_TASK"},
			},
			{
				name: "ready program missing",
				report: notifyReport{
					machine: macOS, readyProgramMissing: true, writeRefused: true,
				},
				want: []string{"needs " + notifyFirstWord(config.NotifyMacOS), "does not resolve here", "$TT_TASK"},
			},
			{
				name:   "ready command available",
				report: notifyReport{machine: macOS, writeRefused: true},
				want:   []string{"$TT_TASK"},
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				lines := notifyOutcomeLines(unset, c.report)
				joined := collapsed(strings.Join(lines, " "))
				for _, want := range c.want {
					if !strings.Contains(joined, want) {
						t.Errorf("lines = %v, want %q retained", lines, want)
					}
				}
				assertAbsent(t, lines, "set timer.on_end manually", runFixHint, "on_end = ")
			})
		}
	})

	t.Run("quoted placeholder keeps its diagnosis", func(t *testing.T) {
		lines := quotedPlaceholderLines([]notifyCommand{{
			key: onEndKey, command: `notify-send "{task}"`,
		}}, true)
		joined := collapsed(strings.Join(lines, " "))
		for _, want := range []string{"{task} in quotes", "split on spaces and globbed"} {
			if !strings.Contains(joined, want) {
				t.Errorf("lines = %v, want diagnosis %q", lines, want)
			}
		}
		assertAbsent(t, lines, "write the environment form", "placeholder as it stands")
	})

	t.Run("open option list remains an explanation", func(t *testing.T) {
		lines := openOptionListLines([]notifyCommand{{
			key: onEndKey, command: `notify-send "tt done" "$TT_TASK"`,
		}})
		joined := collapsed(strings.Join(lines, " "))
		for _, want := range []string{"option list still open", "a task title comes down from the server"} {
			if !strings.Contains(joined, want) {
				t.Errorf("lines = %v, want rule explanation %q retained", lines, want)
			}
		}
	})

	t.Run("on_break_end keeps the notify-test scope without temporary-copy advice", func(t *testing.T) {
		for _, kind := range []notifyOutcomeKind{notifyResolves, notifyUnanswered} {
			group := notifyGroup{
				notifySettingOutcome: notifySettingOutcome{key: onBreakEndKey, kind: kind},
				keys:                 []string{onBreakEndKey},
			}
			lines := notifyWayOutLines(group, notifyReport{machine: macOS, writeRefused: true})
			if joined := collapsed(strings.Join(lines, " ")); !strings.Contains(joined, notifyTestScope) {
				t.Errorf("kind %v lines = %v, want notify-test scope retained", kind, lines)
			}
			assertAbsent(t, lines, "putting this command there for a run")
		}
	})

	t.Run("on_end keeps the real notify test", func(t *testing.T) {
		group := notifyGroup{
			notifySettingOutcome: notifySettingOutcome{key: onEndKey, kind: notifyResolves},
			keys:                 []string{onEndKey},
		}
		lines := notifyWayOutLines(group, notifyReport{machine: macOS, writeRefused: true})
		if !slices.Contains(lines, doctorIndent+notifyTestHint) {
			t.Errorf("lines = %v, want the non-writing notify test retained", lines)
		}
	})

	t.Run("ready command alternatives retain machine remedies", func(t *testing.T) {
		cases := []struct {
			name     string
			keys     []string
			report   notifyReport
			want     []string
			unwanted []string
		}{
			{
				name: "no command for the machine", keys: []string{onEndKey},
				report: notifyReport{
					machine: notifyMachine{system: "unknown", goos: "windows"}, writeRefused: true,
				},
				want: []string{"no ready command for this system", "$TT_TASK"},
			},
			{
				name: "program missing", keys: []string{onEndKey},
				report: notifyReport{
					machine: macOS, onEndSet: true, readyProgramMissing: true, writeRefused: true,
				},
				want: []string{"needs " + notifyFirstWord(config.NotifyMacOS), "PATH or an install"},
			},
			{
				name: "dotted timer keys", keys: []string{onEndKey},
				report: notifyReport{
					machine: macOS, src: []byte("timer.focus = '25m'\n"), writeRefused: true,
				},
				want:     []string{"at the root", "nothing here can be copied"},
				unwanted: []string{dottedTimerSetting, runFixHint},
			},
			{
				name: "on_break_end", keys: []string{onBreakEndKey},
				report:   notifyReport{machine: macOS, onEndSet: true, writeRefused: true},
				want:     []string{"ready command for macOS", "ends its option list"},
				unwanted: []string{"has to be set by hand", runFixHint},
			},
			{
				name: "on_end", keys: []string{onEndKey},
				report:   notifyReport{machine: macOS, writeRefused: true},
				want:     []string{"ready command for macOS", "ends its option list"},
				unwanted: []string{"--fix puts it in on_end", "on_end = ", runFixHint},
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				lines := readyCommandLines(c.keys, c.report)
				joined := collapsed(strings.Join(lines, " "))
				for _, want := range c.want {
					if !strings.Contains(joined, want) {
						t.Errorf("lines = %v, want %q retained", lines, want)
					}
				}
				assertAbsent(t, lines, c.unwanted...)
				for _, line := range lines {
					if strings.HasPrefix(line, doctorNestedIndent) {
						t.Errorf("lines = %v, want no block offered for copying", lines)
					}
				}
			})
		}
	})

	t.Run("dotted unset suggestion keeps diagnosis", func(t *testing.T) {
		lines := onEndSuggestion("macos", config.NotifyMacOS, []byte("timer.focus = '25m'\n"), true)
		joined := collapsed(strings.Join(lines, " "))
		for _, want := range []string{"config writes timer.focus", "at the root", "invalid TOML 1.0"} {
			if !strings.Contains(joined, want) {
				t.Errorf("lines = %v, want diagnosis %q retained", lines, want)
			}
		}
		assertAbsent(t, lines, "suggested for", dottedTimerSetting, runFixHint)
	})
}

func TestRunDoctorChecksSharesOneWriteRefusalAcrossConfigAndNotify(t *testing.T) {
	for name, c := range map[string]struct {
		arrange    func(*testing.T)
		configLead string
	}{
		"missing": {
			configLead: "warn config:",
		},
		"existing and valid": {
			arrange:    func(t *testing.T) { writeConfig(t, "default_project = 'Work'\n") },
			configLead: "warn config:",
		},
		"invalid": {
			arrange:    func(t *testing.T) { writeConfig(t, "[timer\n") },
			configLead: "fail config:",
		},
		"unreadable": {
			arrange: func(t *testing.T) {
				path, err := config.Path()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			configLead: "fail config:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if c.arrange != nil {
				c.arrange(t)
			}
			calls := 0
			var out bytes.Buffer
			runDoctorChecksWithWriteProbe(context.Background(), &out, func() configWriteObservation {
				calls++
				return refusedConfigWrite("/refused/shared-target")
			})
			if calls != 1 {
				t.Fatalf("write probe called %d times, want once for the complete report", calls)
			}
			report := out.String()
			if !strings.Contains(report, c.configLead) {
				t.Errorf("report = %q, want config result led by %q", report, c.configLead)
			}
			if !strings.Contains(report, "/refused/shared-target") {
				t.Errorf("report = %q, want the shared refusal in the config check", report)
			}
			for _, advice := range []string{runFixHint, "tt config --init", "set timer.on_end manually", "on_end = "} {
				if strings.Contains(report, advice) {
					t.Errorf("report offers %q after the shared refusal: %q", advice, report)
				}
			}
		})
	}
}

func TestCheckConfigProbesNoHigherThanTheConfigsOwnPlace(t *testing.T) {
	assertSaidNothing := func(t *testing.T, result checkResult) {
		t.Helper()
		if result.status != statusOK {
			t.Fatalf("status = %v, want ok: %v", result.status, checkReport(result))
		}
		if strings.Contains(result.summary, "cannot be created") {
			t.Errorf("summary = %q, want no verdict about a write this check did not make",
				result.summary)
		}
		if notes := noteText(result); strings.Contains(notes, "nothing can be written there") {
			t.Errorf("notes = %v, want nothing said about a directory nothing was asked of",
				result.notes)
		}
	}

	t.Run("the tree above the config directory is not there", func(t *testing.T) {
		isolate(t)
		root := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "a", "b", "c"))

		assertSaidNothing(t, checkConfig(withObservedConfigWrite(doctorCache{})))
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var left []string
		for _, e := range entries {
			left = append(left, e.Name())
		}
		if len(left) != 0 {
			t.Errorf("%s holds %v after a check that only reports", root, left)
		}
	})

	t.Run("and what stands above it refuses writes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a directory at mode 500 anyway")
		}
		isolate(t)
		root := t.TempDir()
		if err := os.Chmod(root, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(root, 0o700) })
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "a", "b", "c"))

		result := checkConfig(withObservedConfigWrite(doctorCache{}))
		assertSaidNothing(t, result)
		if strings.Contains(noteText(result), root) {
			t.Errorf("notes = %v, want no word about %s: the walk asked it a question that is not "+
				"this check's to ask, three directories above the one the config goes in",
				result.notes, root)
		}
	})
}

func TestCheckConfigFailsOnANonDirectoryComponent(t *testing.T) {
	isolate(t)
	dir := configDir(t)
	if err := os.WriteFile(dir, []byte("a file where the config directory has to be\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := checkConfig(doctorCache{read: true, lists: 1, names: []string{"Inbox"}})
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if !strings.Contains(result.summary, `not\x20a\x20directory`) {
		t.Errorf("summary = %q, want the operating system's own word for what is in the way",
			result.summary)
	}
	if !strings.Contains(result.summary, dir) {
		t.Errorf("summary = %q, want the path whose component cannot hold a config named",
			result.summary)
	}
	for _, verdict := range []string{"invalid", "not found", "cannot be created there", "default_project"} {
		if strings.Contains(result.summary, verdict) {
			t.Errorf("summary = %q, want no %q about a path tt could not even look at",
				result.summary, verdict)
		}
	}
}

func TestCheckConfigEscapesWhatItDidNotWrite(t *testing.T) {

	hostileValue := "\x1b[31m\u202e\n" + strings.Repeat("z", 200)
	hostileName := "\x1b[32m\u202e\n" + strings.Repeat("q", 200)

	assertEscaped := func(t *testing.T, notes []string) {
		t.Helper()
		for i, line := range notes {
			if strings.ContainsAny(line, "\x1b\n\u202e") {
				t.Errorf("note %d carries somebody else's bytes as they stand: %q", i+1, line)
			}
		}
		if !strings.Contains(strings.Join(notes, " "), `\x1b`) {
			t.Errorf("notes = %v, want the escape sequence written out rather than dropped", notes)
		}
	}
	assertNothingRaw := func(t *testing.T, notes []string) {
		t.Helper()
		assertEscaped(t, notes)
		for i, line := range notes {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("note %d is %d runes, want at most %d: %q", i+1, n, cli.Width, line)
			}
		}
		assertDoctorFits(t, "the config check", strings.Join(notes, "\n"))
	}

	t.Run("the default_project value and the list names", func(t *testing.T) {
		isolate(t)
		writeConfig(t, "default_project = \"\\u001B[31m\\u202E\\n"+strings.Repeat("z", 200)+"\"\n")

		result := checkConfig(doctorCache{
			read:  true,
			lists: 2,
			names: []string{hostileName, "an ordinary list"},
		})
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		assertNothingRaw(t, result.notes)

		assertDoctorFits(t, "the config summary", strings.Join(doctorSummary(result.summary), "\n"))
	})

	t.Run("the target of a link with no target", func(t *testing.T) {
		isolate(t)
		linkConfig(t, "/nowhere/"+hostileValue)

		result := checkConfig(doctorCache{})
		if result.status != statusWarn {
			t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
		}
		assertEscaped(t, result.notes)
		if !strings.Contains(noteText(result), fullReportAtom("/nowhere/"+hostileValue)) {
			t.Fatalf("notes lost the full dangling target: %v", result.notes)
		}
	})

	t.Run("the dotted keys of a table opened twice", func(t *testing.T) {
		notes := doubleOpenNotes([]config.DoubleOpen{{
			Table:  config.TimerTable,
			Dotted: []string{"timer." + hostileName, "timer.focus"},
		}}, false)
		assertNothingRaw(t, notes)
	})

	t.Run("the parser's sentence about a line of the file", func(t *testing.T) {
		isolate(t)
		key := "\"\\u202E\\u001B[31m" + strings.Repeat("z", 200) + "\""
		writeConfig(t, key+" = 1\n"+key+" = 2\n")

		result := checkConfig(doctorCache{})
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
		}
		assertNothingRaw(t, result.notes)
	})

	t.Run("the error of a read that reached no line", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file at mode 000 anyway")
		}
		isolate(t)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), hostileName))
		path := writeConfig(t, "default_project = 'Work'\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o600) })

		result := checkConfig(doctorCache{})
		if result.status != statusFail {
			t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
		}
		assertDoctorFits(t, "the unreadable config note", strings.Join(result.notes, "\n"))
		if strings.ContainsAny(result.summary, "\x1b\n\u202e") || strings.Contains(noteText(result), path) {
			t.Fatal("the unreadable config retained raw path data or repeated it in the note")
		}
		if strings.ContainsAny(noteText(result), "\x1b\n\u202e") {
			t.Fatal("the unreadable config note retained path bytes after removing its duplicate context")
		}
		if !strings.Contains(noteText(result), `permission\x20denied`) {
			t.Errorf("notes = %v, want the reason, which is what a cut takes first and is the whole "+
				"of what this note adds to the summary above it", result.notes)
		}
	})

	t.Run("the directory a write was refused in", func(t *testing.T) {
		refused := &os.PathError{Op: "open", Path: "/nowhere/.tt-doctor-42", Err: os.ErrPermission}
		anchor := "/nowhere/" + hostileName
		refusedNotes := configWriteRefusal(anchor, refused, false)
		assertEscaped(t, refusedNotes)
		if !strings.Contains(strings.Join(refusedNotes, "\n"), fullReportAtom(anchor)) {
			t.Fatalf("notes lost the full refused anchor: %v", refusedNotes)
		}

		danglingNotes := configWriteRefusal(anchor, errDanglingComponent, false)
		assertEscaped(t, danglingNotes)
		if !strings.Contains(strings.Join(danglingNotes, "\n"), fullReportAtom(anchor)) {
			t.Fatalf("notes lost the full dangling anchor: %v", danglingNotes)
		}
	})
}

func TestDoctorChecksSayNothingWhenInterrupted(t *testing.T) {
	t.Run("interrupted", func(t *testing.T) {
		isolate(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var out bytes.Buffer
		if code := runDoctorChecks(ctx, &out); code != exitInterrupted {
			t.Errorf("exit code = %d, want %d", code, exitInterrupted)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q, want nothing at all", out.String())
		}
	})

	t.Run("not interrupted", func(t *testing.T) {
		isolate(t)

		var out bytes.Buffer
		if code := runDoctorChecks(context.Background(), &out); code != exitError {
			t.Errorf("exit code = %d, want %d: a sandbox with no token fails the token check", code, exitError)
		}

		if !regexp.MustCompile(`\n\d+ ok, \d+ warn, \d+ fail(, \d+ skip)?\n`).MatchString(out.String()) {
			t.Errorf("printed %q, want the tally of a report that was made", out.String())
		}
	})
}

func TestReadyCommandLinesAgreeWithFix(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)
	const content = "timer.on_end = \"notify-send Done\"\n"
	path := writeConfig(t, content)

	if _, _, err := applyOnEndHint(path, hint); err == nil {
		t.Fatal("applyOnEndHint took a file whose timer keys are dotted at the root, so there is nothing to agree with")
	}

	result := checkNotify()
	if result.status != statusWarn {
		t.Fatalf("status = %v, want warn for a command tt did not write: %v", result.status, checkReport(result))
	}
	joined := noteText(result)
	if !strings.Contains(joined, dottedTimerWayOutText()) {
		t.Errorf("notes = %v, want the two steps --fix itself names", result.notes)
	}
	if !strings.Contains(joined, "at the root") {
		t.Errorf("notes = %v, want the reason --fix will not run on this file", result.notes)
	}

	for _, l := range result.notes {
		if strings.HasPrefix(l, doctorNestedIndent) {
			t.Fatalf("notes = %v, want no block to paste into a file no paste can fix", result.notes)
		}
	}

	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != content {
		t.Errorf("config = %q (%v), want it untouched", got, rerr)
	}
}

func TestCheckTokenNamesTheFileThatStopsTheLoginItRecommends(t *testing.T) {
	for name, tokenFile := range map[string]string{
		"no token file at all": "",
		"an empty token file":  "   \n",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if tokenFile != "" {
				writeTokenFile(t, tokenFile)
			}
			credPath, err := credentialsPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credPath, []byte("{not json at all"), 0o600); err != nil {
				t.Fatal(err)
			}

			result, token := checkToken()
			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			if token.value != "" {
				t.Errorf("token = %q, want nothing handed on to the API check", token.value)
			}
			if !slices.Contains(result.notes, doctorIndent+loginHint) {
				t.Fatalf("notes = %v, want %q under the verdict, on a line of its own", result.notes, loginHint)
			}
			joined := noteText(result)
			if !strings.Contains(joined, credPath) {
				t.Errorf("notes = %v, want the file that stops that login named", result.notes)
			}
			if !strings.Contains(joined, "stops on it") {
				t.Errorf("notes = %v, want it said that the command above will not get past that file", result.notes)
			}

			if _, _, lerr := loadCredentials(); lerr == nil {
				t.Fatal("loadCredentials read the broken file, so there is nothing for the check to report")
			}
		})
	}
}

func TestDoctorFixRefusesADottedConfigBeforeItPromisesAnything(t *testing.T) {
	sys := detectSystem()
	if _, ok := notifyHints[sys]; !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)
	const content = "timer.on_end = 'notify-send -- \"{task}\"'\n"
	path := writeConfig(t, content)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if out := stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing promised about a write that cannot happen", out)
	}
	got := stderr.String()
	if len(got) < len("tt: \"\"\n") || !strings.HasPrefix(got, "tt: ") || got[len(got)-1] != '\n' {
		t.Fatalf("stderr = %q, want one quoted preparation error", got)
	}
	decoded, err := strconv.Unquote(got[len("tt: ") : len(got)-1])
	if err != nil || !strings.Contains(collapsed(decoded), dottedTimerWayOutText()) {
		t.Errorf("stderr decoded as %q (%v), want the way out --fix names", decoded, err)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Errorf("a backup at %s (stat: %v), from a run that wrote nothing", path+backupSuffix, err)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != content {
		t.Errorf("config = %q (%v), want it untouched", got, rerr)
	}
}

func TestDefaultProjectDuplicateExactNamesDoNotPromiseThatExactnessWillResolveThem(t *testing.T) {
	isolate(t)
	writeConfig(t, "default_project = 'Work'\n")
	ctx := context.Background()
	var resolverErr error
	buildCache(t, func(ctx context.Context, st *store.Store) {
		execCache(t, ctx, st, `INSERT INTO projects (id, name, search) VALUES ('p1', 'Work', 'work')`)
		execCache(t, ctx, st, `INSERT INTO projects (id, name, search) VALUES ('p2', 'Work', 'work')`)
		_, resolverErr = (cli.Resolver{Store: st}).Project(ctx, "Work")
	})
	if resolverErr == nil {
		t.Fatal("the real resolver accepted an exact name shared by two cached lists")
	}
	var ambiguous *cli.AmbiguousError
	if !errors.As(resolverErr, &ambiguous) {
		t.Fatalf("resolver error = %T %v, want *cli.AmbiguousError", resolverErr, resolverErr)
	}
	if ambiguous.What != "list" || ambiguous.Query != "Work" || len(ambiguous.Choices) != 2 {
		t.Fatalf("ambiguous result = %#v, want list Work with both exact-name choices", ambiguous)
	}
	_, cache := checkCache(ctx)
	result := checkConfig(cache)
	notes := collapsed(noteText(result))
	const want = "\"tt add\" refuses a name that matches more than one list; the name must resolve to one cached list, and an exact name shared by several lists is still ambiguous"
	if !strings.Contains(notes, want) {
		t.Fatalf("notes = %q, want the exact duplicate-name advice", notes)
	}
	if strings.Contains(notes, "until the name is exact") {
		t.Fatalf("notes retained the remedy the real resolver just disproved: %q", notes)
	}
}

func TestExtraSchemaObjectsCarryNoUnaffectedOperationsAssurance(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	path := buildCache(t, func(ctx context.Context, st *store.Store) {
		if err := st.SetMeta(ctx, "w2_probe", "before"); err != nil {
			t.Fatalf("baseline SetMeta: %v", err)
		}
		execCache(t, ctx, st, `CREATE TRIGGER audit_extra BEFORE UPDATE ON meta BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	})
	result, _ := checkCache(ctx)
	notes := collapsed(noteText(result))
	if result.status != statusWarn || !strings.Contains(notes, `trigger "audit_extra"`) {
		t.Fatalf("extra trigger was not listed as a warning: %v", checkReport(result))
	}
	const want = "these extra objects were found and listed; this check has not established their effect on ordinary operations"
	if !strings.Contains(notes, want) {
		t.Fatalf("notes = %q, want the inspection-limited assurance", notes)
	}
	if strings.Contains(notes, "nothing tt does is changed") {
		t.Fatalf("notes retained the disproved unaffected-operations assurance: %q", notes)
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetMeta(ctx, "w2_probe", "after"); err == nil {
		t.Fatal("extra trigger did not affect the ordinary SetMeta operation")
	} else if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("SetMeta failed for %q, want the extra trigger's blocked refusal", err)
	}
	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER audit_extra`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(ctx, "w2_probe", "after"); err != nil {
		t.Fatalf("SetMeta still fails after the extra trigger is removed: %v", err)
	}
}

func TestDoctorDoesNotReuseAnUncertainConfigSettingLine(t *testing.T) {
	cases := []struct {
		name          string
		src           string
		unknown       string
		invalid       string
		forbiddenLine int
	}{
		{
			"quoted header", "[\"timer]\"]\nfocus = '25m'\n[timer]\nfocus = 'invalid'\n",
			`line 1: "unknown table \"timer]\"`, `line 4: "timer.focus: invalid duration \"invalid\"`, 2,
		},
		{
			"literal dot in quoted root key", "\"timer.focus\" = '25m'\n[timer]\nfocus = 'invalid'\n",
			`line 1: "unknown key \"timer.focus\"`, `line 3: "timer.focus: invalid duration \"invalid\"`, 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			writeConfig(t, tc.src)
			result := checkConfig(doctorCache{})
			if result.status != statusFail {
				t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
			}
			var out strings.Builder
			printCheck(&out, result)
			printed := out.String()
			for _, want := range []string{tc.unknown, tc.invalid} {
				if !strings.Contains(printed, want) {
					t.Fatalf("doctor omitted the associated problem %q: %q", want, printed)
				}
			}
			if tc.forbiddenLine > 0 {
				if bad := fmt.Sprintf("line %d:", tc.forbiddenLine); strings.Contains(printed, bad) {
					t.Fatalf("doctor assigned a problem to unrelated %s %q", bad, printed)
				}
			}
		})
	}
}

func TestDoctorNamesTheOptionItDidNotUnderstand(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"beside the option it takes", []string{"doctor", "--fix", "--bogus"}, `tt: doctor: unknown option "--bogus"`},
		{"before the option it takes", []string{"doctor", "--bogus", "--fix"}, `tt: doctor: unknown option "--bogus"`},
		{"on its own", []string{"doctor", "--bogus"}, `tt: doctor: unknown option "--bogus"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, nil, &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitUsage, stderr.String())
			}

			first := strings.SplitN(stderr.String(), "\n", 2)[0]
			if first != c.want {
				t.Errorf("stderr began %q, want %q", first, c.want)
			}

			if out := stdout.String(); out != "" {
				t.Errorf("stdout = %q, want nothing printed for a line tt refused", out)
			}
		})
	}
}

func TestDoctorTakesFixTwice(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	previous := canAnswer
	canAnswer = func(any) bool { return false }
	t.Cleanup(func() { canAnswer = previous })

	var stdout, stderr bytes.Buffer
	args := []string{"doctor", "--fix", "--fix"}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitError {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitError, stderr.String())
	}
	got := stderr.String()
	if strings.Contains(got, "unknown option") {
		t.Errorf("stderr = %q, want the second --fix taken rather than refused", got)
	}
	if !strings.Contains(got, "refusing to prompt") {
		t.Errorf("stderr = %q, want the run to have reached the question --fix puts", got)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("--fix touched %s without an answer (stat: %v)", path, err)
	}
}

func TestTokenCheckSaysTheLoginWhereAUserCanTypeIt(t *testing.T) {
	isolateInALongPath(t)
	writeTokenFile(t, "   \n")

	result, _ := checkToken()
	if result.status != statusFail {
		t.Fatalf("status = %v, want fail: %v", result.status, checkReport(result))
	}
	if n := utf8.RuneCountInString(result.summary + " " + loginHint); n <= doctorTextWidth {
		t.Fatalf("the verdict and the command are %d columns together and the fold is %d wide, so nothing here would have folded and the fixture proves nothing", n, doctorTextWidth)
	}
	if !slices.Contains(result.notes, doctorIndent+loginHint) {
		t.Errorf("notes = %v, want %q on a line of its own", result.notes, loginHint)
	}

	if strings.Contains(result.summary, "tt login") {
		t.Errorf("summary = %q, want the command under the verdict rather than inside it", result.summary)
	}
}

func TestBlockedLoginSaysTheOtherLoginWhereAUserCanTypeIt(t *testing.T) {
	isolate(t)
	credPath, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	lines := blockedLoginLines(errorsFromCredentials(t))
	if !slices.Contains(lines, doctorIndent+loginTokenHint) {
		t.Errorf("lines = %v, want %q on a line of its own", lines, loginTokenHint)
	}

	if !strings.Contains(collapsed(strings.Join(lines, " ")), "stops on it") {
		t.Errorf("lines = %v, want the file that stops the login named above the way round it", lines)
	}
}

func errorsFromCredentials(t *testing.T) error {
	t.Helper()
	_, _, err := loadCredentials()
	if err == nil {
		t.Fatal("loadCredentials read the broken file, so there is nothing to report")
	}
	return err
}

func TestCredentialsNoteKeepsTheLoginWhole(t *testing.T) {
	const quoted = `"tt login"`
	for _, c := range []struct {
		name  string
		lines func(*testing.T) []string
	}{
		{"no token to go with it", func(t *testing.T) []string {
			return blockedLoginLines(errorsFromCredentials(t))
		}},
		{"a token already in hand", func(t *testing.T) []string {
			writeTokenFile(t, "a-token\n")
			result, _ := checkToken()
			if result.status != statusWarn {
				t.Fatalf("status = %v, want warn: %v", result.status, checkReport(result))
			}
			return result.notes
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			credPath, err := credentialsPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credPath, []byte("{not json at all"), 0o600); err != nil {
				t.Fatal(err)
			}

			lines := c.lines(t)
			whole := collapsed(strings.Join(lines, " "))
			at := strings.Index(whole, quoted)
			if at < 0 {
				t.Fatalf("the note reads %q, want it to name the login that stops on that file", whole)
			}

			if at == 0 {
				t.Fatalf("the note opens with the command, so nothing stands ahead of it and the fixture proves nothing:\n%s", strings.Join(lines, "\n"))
			}
			if n := utf8.RuneCountInString(whole[at:]); n <= doctorTextWidth {
				t.Fatalf("the sentence carrying the command is %d columns against a fold of %d, so it folds nowhere and this test proves nothing", n, doctorTextWidth)
			}

			var head bool
			for _, line := range lines {
				head = head || strings.HasPrefix(strings.TrimPrefix(line, doctorIndent), quoted)
			}
			if !head {
				t.Errorf("the command does not begin a line, so nothing but luck keeps the fold out of it:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

func TestDottedTimerWayOutStaysTypeable(t *testing.T) {
	whole := dottedTimerWayOutText()
	if n := utf8.RuneCountInString(whole); n <= doctorTextWidth || n <= fixMessageWidth {
		t.Fatalf("the way out is %d columns against folds of %d and %d, so it folds nowhere and this test proves nothing", n, doctorTextWidth, fixMessageWidth)
	}

	for _, want := range []string{dottedTimerSetting, runFixHint} {
		if !slices.Contains(dottedTimerWayOutLines(), doctorIndent+want) {
			t.Errorf("doctor said the way out as %v, want %q on a line of its own", dottedTimerWayOutLines(), want)
		}
	}

	refusal := strings.Split(fixParagraph(dottedTimerRefusal("/tmp/config.toml", []string{"timer.on_end"}).Error()), "\n")
	for _, want := range []string{dottedTimerSetting, runFixHint} {
		if !slices.Contains(refusal, "  "+want) {
			t.Errorf("--fix said the way out as %v, want %q on a line of its own", refusal, want)
		}
	}
}

func TestOnEndSuggestionSaysTheFixWhereAUserCanTypeIt(t *testing.T) {
	sys := detectSystem()
	hint, ok := notifyHints[sys]
	if !ok {
		t.Skipf("no ready notification command for %s", systemLabel(sys))
	}
	isolate(t)

	lead := "suggested for " + systemLabel(sys) + ": " + runFixHint + ", or add this to the end of the config file:"
	if n := utf8.RuneCountInString(lead); n <= doctorTextWidth {
		t.Fatalf("the lead carrying the command is %d columns against a fold of %d, so it folds nowhere and this test proves nothing", n, doctorTextWidth)
	}
	lines := onEndSuggestion(sys, hint, nil, false)
	if !slices.Contains(lines, doctorIndent+runFixHint) {
		t.Errorf("the suggestion reads %v, want %q on a line of its own", lines, runFixHint)
	}

	if len(suggestedSetting(t, checkResultFromLines(lines))) == 0 {
		t.Errorf("the suggestion reads %v, want the setting still offered for copying", lines)
	}
}

func checkResultFromLines(lines []string) checkResult {
	return checkResult{notes: lines}
}
