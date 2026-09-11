package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/config"
)

func TestApplyOnEndHintNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	_, backup, err := applyOnEndHint(path, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	if backup != "" {
		t.Errorf("backup = %q, want nothing reported when there was nothing to copy", backup)
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Fatalf("Timer.OnEnd = %q, want %q", cfg.Timer.OnEnd, "notify-send hello")
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Fatalf("a backup file must not be created when there was nothing to back up")
	}
}

func TestApplyOnEndHintSectionExistsNoKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "default_project = \"Work\"\n\n[timer]\nfocus = \"50m\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := applyOnEndHint(path, "notify-send hello"); err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Fatalf("Timer.OnEnd = %q, want %q", cfg.Timer.OnEnd, "notify-send hello")
	}
	if cfg.Timer.Focus.String() != "50m" {
		t.Fatalf("Timer.Focus = %q, want the original 50m preserved", cfg.Timer.Focus)
	}
	if cfg.DefaultProject != "Work" {
		t.Fatalf("DefaultProject = %q, want the original value preserved", cfg.DefaultProject)
	}

	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q, %v; want the original content", backup, err)
	}
}

func TestApplyOnEndHintKeyExistsIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[timer]\non_end = \"old command\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := applyOnEndHint(path, "new command"); err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if cfg.Timer.OnEnd != "new command" {
		t.Fatalf("Timer.OnEnd = %q, want %q", cfg.Timer.OnEnd, "new command")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "on_end"); n != 1 {
		t.Fatalf("file has %d on_end lines, want exactly 1:\n%s", n, got)
	}
	if strings.Contains(string(got), "old command") {
		t.Fatalf("old value survived the replace:\n%s", got)
	}
}

func TestApplyOnEndHintRefusesAnInvalidSourceBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	original := "[timer]\nlong_every = 99\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint succeeded on a file that was already invalid, want an error")
	}
	if strings.Contains(err.Error(), "restored") {
		t.Fatalf("error = %v, want a prewrite refusal rather than a restore", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("file = %q, want the invalid source untouched:\n%q", got, original)
	}
	if _, berr := os.Lstat(path + backupSuffix); !os.IsNotExist(berr) {
		t.Fatalf("a backup exists for an edit refused before writing: %v", berr)
	}
}

func TestApplyOnEndHintRefusesMalformedOnEndEvenWhenReplacementWouldRepairIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[timer]\non_end =\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint repaired a source it could not parse")
	}
	if written != "" || backup != "" {
		t.Errorf("written, backup = %q, %q; want a source refusal before writing", written, backup)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Errorf("config = %q, %v; want malformed source untouched", got, err)
	}
	assertPathMissing(t, path+backupSuffix)
}

func TestApplyOnEndHintFindsTheHeaderInEverySpelling(t *testing.T) {
	for name, header := range map[string]string{
		"plain":            "[timer]",
		"spaces inside":    "[ timer ]",
		"tabs inside":      "[\ttimer\t]",
		"basic quoted":     `["timer"]`,
		"literal quoted":   `['timer']`,
		"trailing comment": "[timer] # session lengths",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			original := "default_project = 'Work'\n\n" + header + "\nfocus = '50m'\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, _, err := applyOnEndHint(path, "notify-send hello"); err != nil {
				t.Fatalf("applyOnEndHint: %v", err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("the written config does not parse: %v", err)
			}
			if cfg.Timer.OnEnd != "notify-send hello" {
				t.Errorf("Timer.OnEnd = %q, want the command that was written", cfg.Timer.OnEnd)
			}
			if cfg.Timer.Focus.String() != "50m" || cfg.DefaultProject != "Work" {
				t.Errorf("the rest of the config moved: %+v", cfg)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), header) {
				t.Errorf("the header was rewritten instead of used as it stands:\n%s", got)
			}
		})
	}
}

func TestApplyOnEndHintRefusesToWriteIntoAValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "default_project = \"\"\"\n[timer]\n\"\"\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint reported success on a file it could not edit")
	}
	if strings.Contains(err.Error(), "restored") {
		t.Errorf("error = %v, want the invalid candidate refused before writing", err)
	}
	if !strings.Contains(err.Error(), "proposed edit was refused") {
		t.Errorf("error = %v, want it to identify the candidate refusal", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("file = %q, want the original content untouched:\n%q", got, original)
	}
	if _, berr := os.Lstat(path + backupSuffix); !os.IsNotExist(berr) {
		t.Fatalf("a backup exists for an invalid candidate that was never written: %v", berr)
	}
}

func TestDoctorFixEscapesAnInvalidSourceBeforePrompt(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	withAnAnswerableStdin(t)
	key := "key\x1b[31m\u202e" + strings.Repeat("k", 120)
	original := config.QuoteTOMLString(key) + " = 1\n" + config.QuoteTOMLString(key) + " = 2\n"
	path := writeConfig(t, original)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	out := stderr.String()
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\u202e") {
		t.Errorf("invalid-source refusal contains a raw terminal control rune: %q", out)
	}
	if !strings.Contains(out, `\x1b`) || !strings.Contains(out, `\u202e`) {
		t.Errorf("invalid-source refusal does not show both escaped controls: %q", out)
	}
	if strings.Contains(out, "write it?") {
		t.Errorf("invalid source reached the prompt: %q", out)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Errorf("config = %q, %v; want exact invalid source untouched", got, err)
	}
	assertPathMissing(t, path+backupSuffix)
}

func TestDoctorFixLabelsAnInvalidCandidateBeforeWriting(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	withAnAnswerableStdin(t)
	original := "[timer]\non_break_end = \"\"\"\non_end = \"\"\"\n"
	path := writeConfig(t, original)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	out := stderr.String()
	if !strings.Contains(out, "proposed replacement rejected; original config unchanged") {
		t.Errorf("candidate refusal lost its stage: %q", out)
	}
	if strings.Contains(out, "write it?") {
		t.Errorf("invalid candidate reached the prompt: %q", out)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Errorf("config = %q, %v; want exact source untouched", got, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime = %v, want unchanged %v", after.ModTime(), before.ModTime())
	}
	assertPathMissing(t, path+backupSuffix)
}

func TestCheckOnEndEditCatchesWhatTheTextCannot(t *testing.T) {
	const hint = "notify-send hello"
	before := config.Default()
	after := before
	after.Timer.OnEnd = hint

	if err := checkOnEndEdit(after, &before, hint); err != nil {
		t.Fatalf("the edit that did exactly what was asked was rejected: %v", err)
	}
	if err := checkOnEndEdit(before, &before, hint); err == nil {
		t.Error("an edit that never set on_end was accepted")
	}
	collateral := after
	collateral.DefaultProject = "not what the user wrote"
	if err := checkOnEndEdit(collateral, &before, hint); err == nil {
		t.Error("an edit that changed another setting was accepted")
	}
	if err := checkOnEndEdit(collateral, nil, hint); err != nil {
		t.Errorf("with nothing to compare against only on_end matters: %v", err)
	}
}

func TestCheckOnEndEditBoundsWhatTheFileHolds(t *testing.T) {
	var after config.Config
	after.Timer.OnEnd = "notify-send\n" + strings.Repeat("y", 400)

	err := checkOnEndEdit(after, nil, "notify-send tt")
	if err == nil {
		t.Fatalf("checkOnEndEdit took %q for the command it was given", after.Timer.OnEnd)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the message carries a line break, so it prints as two lines: %q", err.Error())
	}
	for i, line := range strings.Split(fixParagraph(err.Error()), "\n") {
		if n := utf8.RuneCountInString(line); n > fixMessageWidth {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, fixMessageWidth, line)
		}
	}
}

func TestDoctorFixWithoutAReadyCommand(t *testing.T) {
	isolate(t)
	withoutHints(t)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if !strings.Contains(collapsed(stderr.String()), noReadyCommandNotice(thisMachine())) {
		t.Errorf("stderr = %q, want the sentence the report gives this machine", stderr.String())
	}
	if !strings.Contains(stderr.String(), "$TT_TASK") {
		t.Errorf("stderr = %q, want the variables to write the command with", stderr.String())
	}

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--fix touched %s with nothing to write there (stat: %v)", path, err)
	}
}

func TestDoctorFixStopsWhereTheReportStopsWithoutAShell(t *testing.T) {
	isolate(t)
	withoutAShell(t)
	withAHintForThisMachine(t)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if !strings.Contains(collapsed(stderr.String()), noShellSentence) {
		t.Errorf("stderr = %q, want the sentence the report gives this machine", stderr.String())
	}
	if strings.Contains(stderr.String(), "will replace timer.on_end") {
		t.Errorf("stderr = %q, want no question about a write that cannot help", stderr.String())
	}
	if out := stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing: nothing was written", out)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--fix wrote %s on a machine with no shell to run it (stat: %v)", path, err)
	}
}

func TestDoctorFixRefusesACommandThisMachineCannotRun(t *testing.T) {
	for name, c := range map[string]struct {
		shell  func(*testing.T, ...string)
		names  []string
		refuse bool
	}{
		"the program the command needs is not on this PATH": {
			shell: func(t *testing.T, names ...string) { notifyPrograms(t, names...) },

			refuse: true,
		},
		"the program is here": {
			shell: func(t *testing.T, names ...string) { notifyPrograms(t, names...) },
			names: []string{"notify-send"},
		},
		"a shell that does not come back": {
			shell: func(t *testing.T, names ...string) { slowShell(t, names...) },
		},
		"a shell something kills": {
			shell: func(t *testing.T, names ...string) { killedShell(t, names...) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			c.shell(t, c.names...)
			withAHintForThisMachine(t)

			var stdout, stderr strings.Builder
			code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr)
			if code != exitError {
				t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
			}
			said := collapsed(stderr.String())
			refused := readyProgramMissingWayOut(detectSystem(), notifyHints[detectSystem()])
			if strings.Contains(said, refused) != c.refuse {
				if c.refuse {
					t.Fatalf("stderr = %q, want the sentence the report gives this machine", stderr.String())
				}
				t.Fatalf("stderr = %q, want no refusal on an answer nobody got", stderr.String())
			}

			if asked := strings.Contains(said, "will replace timer.on_end"); asked == c.refuse {
				if c.refuse {
					t.Errorf("stderr = %q, want no question about a write that cannot help", stderr.String())
				} else {
					t.Errorf("stderr = %q, want the run to reach the question it came for", stderr.String())
				}
			}
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("--fix touched %s without an answer to write on (stat: %v)", path, err)
			}
		})
	}
}

func TestDoctorFixPutsItsQuestionWhereTheAnswerGoes(t *testing.T) {
	isolateInALongPath(t)
	withReadyHint(t)
	path := writeConfig(t, "[timer]\n")
	if err := os.WriteFile(path+backupSuffix, []byte("manual backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if out := stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing: nothing was written, so there is no record", out)
	}
	for _, want := range []string{
		"will replace timer.on_end", "this writes", "will be kept at",
		"stdin is not a terminal",
	} {
		if !strings.Contains(collapsed(stderr.String()), want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr.String(), want)
		}
	}
	if want := path + backupSuffix + ".1"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want the exact prospective backup %q", stderr.String(), want)
	}
	assertPathMissing(t, path+backupSuffix+".1")
}

func TestDoctorFixQuestionStaysInsideTheTerminal(t *testing.T) {
	isolateInALongPath(t)
	withReadyHint(t)
	linkConfigTo(t, "[timer]\n")

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if !strings.Contains(collapsed(stderr.String()), "one of the names on the way there is a symbolic link") {
		t.Fatalf("stderr = %q, want the sentence this test is about", stderr.String())
	}
	assertDoctorFits(t, "tt doctor --fix", stderr.String())
}

func TestDoctorFixRecordStaysInsideTheTerminalAndOnStdout(t *testing.T) {
	isolateInALongPath(t)
	linkConfigTo(t, "[timer]\n")
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	prepared, unchanged, err := prepareOnEndHint(path, "notify-send hello")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}

	var stdout, stderr strings.Builder
	if code := finishFix(prepared, &stdout, &stderr); code != exitOK {
		t.Fatalf("finishFix = exit %d, want %d (stderr: %q)", code, exitOK, stderr.String())
	}
	if out := stderr.String(); out != "" {
		t.Errorf("stderr = %q, want the record of a write that happened on stdout", out)
	}
	if !strings.Contains(collapsed(stdout.String()), "one of the names on the way there is a symbolic link") {
		t.Fatalf("stdout = %q, want the sentence this test is about", stdout.String())
	}
	assertDoctorFits(t, "tt doctor --fix", stdout.String())
}

func linkConfigTo(t *testing.T, content string) {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "config-kept-in-a-dotfiles-repository.toml")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorFixRefusalStaysInsideTheTerminal(t *testing.T) {
	isolateInALongPath(t)
	withReadyHint(t)
	writeConfig(t, "timer.on_end = 'notify-send -- \"{task}\"'\n")

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	assertDoctorFits(t, "tt doctor --fix", stderr.String())
}

func withAnAnswerableStdin(t *testing.T) {
	t.Helper()
	previous := canAnswer
	canAnswer = func(any) bool { return true }
	t.Cleanup(func() { canAnswer = previous })
}

func TestDoctorFixSaysSoWhenTheAnswerIsNo(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "no thanks\n", ""} {
		t.Run(strconv.Quote(answer), func(t *testing.T) {
			isolate(t)
			withReadyHint(t)
			withAnAnswerableStdin(t)
			const content = "[timer]\nfocus = '25m'\n"
			path := writeConfig(t, content)

			var stdout, stderr strings.Builder
			if code := doctorFix(context.Background(), strings.NewReader(answer), &stdout, &stderr); code != exitOK {
				t.Fatalf("doctor --fix = exit %d, want %d for an answer of no", code, exitOK)
			}
			if out := stdout.String(); out != "" {
				t.Errorf("stdout = %q, want nothing: nothing was written, so there is no record", out)
			}
			if got := stderr.String(); !strings.Contains(got, "aborted, nothing written") {
				t.Errorf("stderr = %q, want the answer to the question printed where the question was", got)
			}

			if got, rerr := os.ReadFile(path); rerr != nil || string(got) != content {
				t.Errorf("config = %q (%v), want it untouched", got, rerr)
			}
			if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
				t.Errorf("a backup at %s (stat: %v), from a run that wrote nothing", path+backupSuffix, err)
			}
		})
	}
}

func TestDoctorFixSaysSoWhenInterruptedAtThePrompt(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	withAnAnswerableStdin(t)
	const content = "[timer]\nfocus = '25m'\n"
	path := writeConfig(t, content)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr strings.Builder
	if code := doctorFix(ctx, strings.NewReader("y\n"), &stdout, &stderr); code != exitInterrupted {
		t.Fatalf("doctor --fix = exit %d, want %d (stderr: %q)", code, exitInterrupted, stderr.String())
	}
	if out := stdout.String(); out != "" {
		t.Errorf("stdout = %q, want nothing: nothing was written, so there is no record", out)
	}
	if got := stderr.String(); !strings.Contains(got, "interrupted, nothing written") {
		t.Errorf("stderr = %q, want the interrupt said where the question was put", got)
	}

	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != content {
		t.Errorf("config = %q (%v), want it untouched by a run that was interrupted", got, rerr)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Errorf("a backup at %s (stat: %v), from a run that wrote nothing", path+backupSuffix, err)
	}
}

func TestDoctorFixEscapesBothCompleteReplacementValues(t *testing.T) {
	isolate(t)
	withAnAnswerableStdin(t)
	notifyPrograms(t, "notify-send")
	old := "notify-send -- \"{task}\"\t" + strings.Repeat("o", 180) + "-old-tail"
	want := "notify-send -- {task}\t" + strings.Repeat("o", 180) + "-old-tail"
	previous := notifyHints
	notifyHints = map[string]string{detectSystem(): "generated-notifier -- {task}"}
	t.Cleanup(func() { notifyHints = previous })
	original := "[timer]\non_end = " + config.QuoteTOMLString(old) + "\n"
	path := writeConfig(t, original)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("n\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitOK)
	}
	question := stderr.String()
	for name, value := range map[string]string{"old": old, "new": want} {
		shown := fixValue(value)
		if !strings.Contains(question, shown) {
			t.Errorf("question does not contain the complete escaped %s value %q:\n%s", name, shown, question)
		}
	}
	if strings.Contains(question, "generated-notifier") {
		t.Errorf("question offered the generated replacement instead of the selected custom repair: %q", question)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("config changed after no: %q, %v", got, err)
	}
}

func TestApplyOnEndHintDoesNothingWhenTheEffectiveValueMatches(t *testing.T) {
	const hint = "notify-send hello"
	for name, original := range map[string]string{
		"timer table":      "[timer]\non_end = 'notify-send hello'\n",
		"dotted timer key": "timer.on_end = 'notify-send hello'\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				written, backup, err := applyOnEndHint(path, hint)
				if err != nil {
					t.Fatalf("call %d: applyOnEndHint: %v", i+1, err)
				}
				if written != "" || backup != "" {
					t.Errorf("call %d: written, backup = %q, %q; want an unchanged outcome", i+1, written, backup)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original {
				t.Fatalf("config = %q, %v; want exact original bytes", got, err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
				t.Errorf("mode, mtime = %v, %v; want %v, %v", after.Mode(), after.ModTime(), before.Mode(), before.ModTime())
			}
			if matches, err := filepath.Glob(path + backupSuffix + "*"); err != nil || len(matches) != 0 {
				t.Errorf("backups = %v, %v; want none", matches, err)
			}
		})
	}
}

func TestDoctorFixReportsAnUnchangedValueWithoutPrompting(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	hint := notifyHints[detectSystem()]
	path := writeConfig(t, "timer.on_end = "+config.QuoteTOMLString(hint)+"\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "is already") || !strings.Contains(stdout.String(), "nothing written") {
		t.Errorf("stdout = %q, want the explicit unchanged outcome", stdout.String())
	}
	if strings.Contains(stderr.String(), "write it?") {
		t.Errorf("stderr = %q, want no prompt for an unchanged value", stderr.String())
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime = %v, want unchanged %v", after.ModTime(), before.ModTime())
	}
}

func TestDoctorFixChecksThePreparedTargetWriteBeforePrompting(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	path := writeConfig(t, "[timer]\nfocus = '25m'\n")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := canAnswer
	canAnswer = func(any) bool {
		t.Fatal("doctor asked whether stdin could answer after the target refused its write probe")
		return false
	}
	t.Cleanup(func() { canAnswer = previous })

	calls := 0
	var stdout, stderr strings.Builder
	code := doctorFixWithWriteProbe(context.Background(), strings.NewReader("y\n"), &stdout, &stderr,
		func(target string) configWriteObservation {
			calls++
			if target != resolved {
				t.Errorf("probed target = %q, want prepared target %q", target, resolved)
			}
			return refusedConfigWrite(filepath.Dir(target))
		})
	if code != exitError {
		t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
	}
	if calls != 1 {
		t.Fatalf("write probe called %d times, want once", calls)
	}
	if out := stderr.String(); !strings.Contains(out, "write probe was refused") || strings.Contains(out, "write it?") {
		t.Errorf("stderr = %q, want refusal before any question", out)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(original) {
		t.Errorf("config = %q, %v; want exact source untouched", got, err)
	}
	assertPathMissing(t, path+backupSuffix)
}

func TestDoctorFixNoOpAndInvalidSourceStopBeforeTheWriteProbe(t *testing.T) {
	t.Run("equal effective value", func(t *testing.T) {
		isolate(t)
		withReadyHint(t)
		hint := notifyHints[detectSystem()]
		writeConfig(t, "timer.on_end = "+config.QuoteTOMLString(hint)+"\n")
		var stdout, stderr strings.Builder
		code := doctorFixWithWriteProbe(context.Background(), strings.NewReader("y\n"), &stdout, &stderr,
			func(string) configWriteObservation {
				t.Fatal("equal value reached the write probe")
				return configWriteObservation{}
			})
		if code != exitOK || !strings.Contains(stdout.String(), "nothing written") {
			t.Fatalf("doctor --fix = exit %d, stdout %q, stderr %q; want unchanged", code, stdout.String(), stderr.String())
		}
	})

	t.Run("invalid source", func(t *testing.T) {
		isolate(t)
		withReadyHint(t)
		writeConfig(t, "[timer\n")
		var stdout, stderr strings.Builder
		code := doctorFixWithWriteProbe(context.Background(), strings.NewReader("y\n"), &stdout, &stderr,
			func(string) configWriteObservation {
				t.Fatal("invalid source reached the write probe")
				return configWriteObservation{}
			})
		if code != exitError || strings.Contains(stderr.String(), "write it?") {
			t.Fatalf("doctor --fix = exit %d, stderr %q; want pre-probe parse refusal", code, stderr.String())
		}
	})
}

func TestDoctorFixDoesNotTreatUnknownAsAWriteRefusal(t *testing.T) {
	for name, state := range map[string]configWriteState{
		"successful probe": configWriteSucceeded,
		"unknown":          configWriteUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			withReadyHint(t)
			withAnAnswerableStdin(t)
			writeConfig(t, "[timer]\nfocus = '25m'\n")
			calls := 0
			var stdout, stderr strings.Builder
			code := doctorFixWithWriteProbe(context.Background(), strings.NewReader("n\n"), &stdout, &stderr,
				func(string) configWriteObservation {
					calls++
					return configWriteObservation{state: state}
				})
			if code != exitOK || calls != 1 {
				t.Fatalf("doctor --fix = exit %d, probe calls %d; want one observation and declined prompt", code, calls)
			}
			if out := stderr.String(); !strings.Contains(out, "write it?") || !strings.Contains(out, "aborted") {
				t.Errorf("stderr = %q, want ordinary prompt for %s", out, name)
			}
		})
	}
}

func TestApplyOnEndHintKeepsAnExistingBackupAndUsesTheFirstFreeName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[timer]\non_end = 'old command'\n"
	manual := []byte("manual backup, not tt's\n")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+backupSuffix, manual, 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyOnEndHint(path, "new command")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	if written != target || backup != target+backupSuffix+".1" {
		t.Errorf("written, backup = %q, %q; want %q, %q", written, backup, target, target+backupSuffix+".1")
	}
	if got, err := os.ReadFile(path + backupSuffix); err != nil || !slices.Equal(got, manual) {
		t.Errorf("manual backup = %q, %v; want it preserved", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != original {
		t.Errorf("selected backup = %q, %v; want exact original", got, err)
	}
}

func TestApplyOnEndHintPreservesEveryOccupiedBackupKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[timer]\non_end = 'old command'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	regular := path + backupSuffix
	link := regular + ".1"
	directory := regular + ".2"
	if err := os.WriteFile(regular, []byte("keep regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}

	_, backup, err := applyOnEndHint(path, "new command")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	if backup != target+backupSuffix+".3" {
		t.Fatalf("backup = %q, want first free name %q", backup, target+backupSuffix+".3")
	}
	if got, err := os.ReadFile(regular); err != nil || string(got) != "keep regular" {
		t.Errorf("regular collision changed: %q, %v", got, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink collision changed: %v, %v", info, err)
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() {
		t.Errorf("directory collision changed: %v, %v", info, err)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != original {
		t.Errorf("backup = %q, %v; want exact original", got, err)
	}
}

func TestApplyPreparedOnEndHintRefusesARaceForThePromisedBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[timer]\non_end = 'old command'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	p, unchanged, err := prepareOnEndHint(path, "new command")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}
	interloper := []byte("created after the prompt\n")
	if err := os.WriteFile(p.backup, interloper, 0o600); err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyPreparedOnEndHint(p)
	if err == nil {
		t.Fatal("applyPreparedOnEndHint replaced an occupied promised backup")
	}
	if written != "" || backup != "" || !strings.Contains(err.Error(), p.backup) {
		t.Errorf("written, backup, error = %q, %q, %v; want a refusal naming the promised path", written, backup, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Errorf("config = %q, %v; want it untouched", got, err)
	}
	if got, err := os.ReadFile(p.backup); err != nil || !slices.Equal(got, interloper) {
		t.Errorf("racing entry = %q, %v; want it preserved", got, err)
	}
	if _, err := os.Lstat(path + backupSuffix + ".1"); !os.IsNotExist(err) {
		t.Errorf("--fix silently switched to another backup name: %v", err)
	}
}

func TestApplyPreparedOnEndHintRefusesEveryStaleSourceState(t *testing.T) {
	const original = "[timer]\non_end = 'old command'\n"
	const replacement = "[timer]\non_end = 'somebody else wrote this'\n"

	t.Run("a missing source appeared", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		p, unchanged, err := prepareOnEndHint(path, "new command")
		if err != nil || unchanged {
			t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
		}
		if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
		assertPreparedFixRefused(t, p)
		if got, err := os.ReadFile(path); err != nil || string(got) != replacement {
			t.Errorf("appeared config = %q, %v; want it untouched", got, err)
		}
	})

	t.Run("an existing source disappeared", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		p, unchanged, err := prepareOnEndHint(path, "new command")
		if err != nil || unchanged {
			t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		assertPreparedFixRefused(t, p)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("deleted config was recreated: %v", err)
		}
		assertPathMissing(t, p.backup)
	})

	t.Run("source bytes changed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		p, unchanged, err := prepareOnEndHint(path, "new command")
		if err != nil || unchanged {
			t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
		}
		if err := os.WriteFile(path, []byte(replacement), p.mode.Perm()); err != nil {
			t.Fatal(err)
		}
		assertPreparedFixRefused(t, p)
		if got, err := os.ReadFile(path); err != nil || string(got) != replacement {
			t.Errorf("changed config = %q, %v; want the later bytes untouched", got, err)
		}
		assertPathMissing(t, p.backup)
	})

	t.Run("source mode changed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		p, unchanged, err := prepareOnEndHint(path, "new command")
		if err != nil || unchanged {
			t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		assertPreparedFixRefused(t, p)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("mode = %v, %v; want the later 0640 untouched", info, err)
		}
		assertPathMissing(t, p.backup)
	})

	t.Run("config symlink was repointed", func(t *testing.T) {
		dir := t.TempDir()
		shown := filepath.Join(dir, "shown.toml")
		other := filepath.Join(dir, "other.toml")
		if err := os.WriteFile(shown, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(other, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Symlink(shown, path); err != nil {
			t.Fatal(err)
		}
		p, unchanged, err := prepareOnEndHint(path, "new command")
		if err != nil || unchanged {
			t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, path); err != nil {
			t.Fatal(err)
		}
		assertPreparedFixRefused(t, p)
		if got, err := os.ReadFile(shown); err != nil || string(got) != original {
			t.Errorf("shown target = %q, %v; want it untouched", got, err)
		}
		if got, err := os.ReadFile(other); err != nil || string(got) != replacement {
			t.Errorf("new target = %q, %v; want it untouched", got, err)
		}
		assertPathMissing(t, p.backup)
	})
}

func TestApplyPreparedOnEndHintRefusesARepointedAncestorForAMissingLeaf(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "config-dir")
	if err := os.Symlink(a, linkedDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkedDir, "config.toml")
	p, unchanged, err := prepareOnEndHint(path, "new command")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	if p.target != filepath.Join(resolvedA, "config.toml") {
		t.Errorf("prepared target = %q, want missing leaf under resolved parent %q", p.target, resolvedA)
	}
	if err := os.Remove(linkedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, linkedDir); err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyPreparedOnEndHint(p)
	if err == nil {
		t.Errorf("applyPreparedOnEndHint wrote %q with backup %q after the parent link was repointed", written, backup)
	}
	assertPathMissing(t, filepath.Join(a, "config.toml"))
	assertPathMissing(t, filepath.Join(b, "config.toml"))
}

func TestApplyPreparedOnEndHintChecksAgainImmediatelyBeforePublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[timer]\non_end = 'old command'\n"
	later := "[timer]\non_end = 'written while --fix was preparing'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	p, unchanged, err := prepareOnEndHint(path, "new command")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}

	written, backup, err := applyPreparedOnEndHintBeforePublish(p, func() {
		if err := os.WriteFile(path, []byte(later), p.mode.Perm()); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || written != "" || backup != p.backup {
		t.Fatalf("written, backup, error = %q, %q, %v; want refusal after the selected backup", written, backup, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != later {
		t.Errorf("config = %q, %v; want the later writer preserved", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != original {
		t.Errorf("backup = %q, %v; want exact captured source", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".tmp") {
			t.Errorf("candidate temp survived refusal: %s", entry.Name())
		}
	}
}

func assertPreparedFixRefused(t *testing.T, p onEndFix) {
	t.Helper()
	written, backup, err := applyPreparedOnEndHint(p)
	if err == nil {
		t.Fatal("applyPreparedOnEndHint accepted a source state different from the prepared snapshot")
	}
	if written != "" || backup != "" {
		t.Errorf("written, backup = %q, %q; want nothing created before stale-source refusal", written, backup)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists, want it absent (lstat: %v)", path, err)
	}
}

func TestDoctorFixWritesWhenTheAnswerIsYes(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	withAnAnswerableStdin(t)
	path := writeConfig(t, "[timer]\nfocus = '25m'\n")

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("doctor --fix = exit %d, want %d (stderr: %q)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "wrote ") {
		t.Errorf("stdout = %q, want the record of the write", stdout.String())
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("the config does not load after --fix: %v", err)
	}
	if cfg.Timer.OnEnd != notifyHints[detectSystem()] {
		t.Errorf("timer.on_end = %q, want the command --fix said it would write", cfg.Timer.OnEnd)
	}
	if cfg.Timer.Focus.String() != "25m" {
		t.Errorf("timer.focus = %s, want the setting that was already there left alone", cfg.Timer.Focus)
	}
}

func TestDoctorFixPromptKeepsTheCompleteHostileTargetBeforeRefusingInput(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	hostileHome := filepath.Join(t.TempDir(), "config\u00a0 home\n\x1b\u202e")
	t.Setenv("XDG_CONFIG_HOME", hostileHome)
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader(""), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = %d, want refusal for non-terminal input", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("doctor --fix wrote a record before consent: %q", stdout.String())
	}
	if strings.ContainsAny(stderr.String(), "\x1b\u202e") {
		t.Fatalf("prompt retained a raw terminal control from its target: %q", stderr.String())
	}
	lines := strings.Split(stderr.String(), "\n")
	var targetToken, lookupToken string
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "this writes "):
			targetToken = line[len("this writes "):]
		case line == "this writes" && i+1 < len(lines):
			targetToken = lines[i+1]
		case strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `",`):
			lookupToken = line[:len(line)-1]
		}
	}
	got, err := strconv.Unquote(targetToken)
	if err != nil || got != target {
		t.Fatalf("prompt target recovered %q, %v; want exact target %q in %q", got, err, target, stderr.String())
	}
	if lookupToken != "" {
		gotLookup, err := strconv.Unquote(lookupToken)
		if err != nil || gotLookup != path {
			t.Fatalf("lookup path recovered %q, %v; want exact path %q", gotLookup, err, path)
		}
	}
	if !strings.Contains(stderr.String(), "stdin is not a terminal") {
		t.Fatalf("prompt did not preserve its refusal: %q", stderr.String())
	}
}

func TestPreparationErrorKeepsACompleteOpaqueReason(t *testing.T) {
	raw := "  prepare\u00a0\u2003 failed\n\x1b\u202e " + `literal\n\x20` + " " +
		strings.Repeat("z", 81) + string([]byte{0xff}) + "  "
	var out strings.Builder
	printOnEndPreparationError(&out, errors.New(raw))
	printed := out.String()
	if len(printed) == 0 || printed[len(printed)-1] != '\n' || !strings.HasPrefix(printed, "tt: ") {
		t.Fatalf("preparation error is not one finished command line: %q", printed)
	}
	got, err := strconv.Unquote(printed[len("tt: ") : len(printed)-1])
	if err != nil || got != raw {
		t.Fatalf("preparation error recovered %q, %v; want exact reason %q", got, err, raw)
	}
}

func TestDoctorFixPromptPreservesTargetLookupAndBackupFinalAtoms(t *testing.T) {
	isolate(t)
	withReadyHint(t)
	withAnAnswerableStdin(t)
	fixture := newHostileFixDisplayFixture(t)
	targetMembers := directoryNames(t, filepath.Dir(fixture.prepared.target))
	lookupMembers := directoryNames(t, filepath.Dir(fixture.prepared.path))

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("n\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("doctor --fix = exit %d, want declined prompt; stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no record without consent", stdout.String())
	}
	assertFinalFixAtoms(t, stderr.String(), fixture.prepared.target, fixture.prepared.path, fixture.prepared.backup)
	if got, err := os.ReadFile(fixture.prepared.target); err != nil || string(got) != fixture.original {
		t.Fatalf("target after no = %q, %v; want exact source", got, err)
	}
	if got, err := os.ReadFile(fixture.prepared.target + backupSuffix); err != nil || string(got) != fixture.manualBackup {
		t.Fatalf("occupied backup after no = %q, %v; want exact existing bytes", got, err)
	}
	assertPathMissing(t, fixture.prepared.backup)
	if got := directoryNames(t, filepath.Dir(fixture.prepared.target)); !slices.Equal(got, targetMembers) {
		t.Errorf("target directory members after no = %q, want %q", got, targetMembers)
	}
	if got := directoryNames(t, filepath.Dir(fixture.prepared.path)); !slices.Equal(got, lookupMembers) {
		t.Errorf("lookup directory members after no = %q, want %q", got, lookupMembers)
	}
}

func TestFinishFixSuccessPreservesWrittenLookupAndBackupFinalAtoms(t *testing.T) {
	isolate(t)
	fixture := newHostileFixDisplayFixture(t)

	var stdout, stderr strings.Builder
	if code := finishFix(fixture.prepared, &stdout, &stderr); code != exitOK {
		t.Fatalf("finishFix = exit %d, want success; stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want a successful record only on stdout", stderr.String())
	}
	assertFinalFixAtoms(t, stdout.String(), fixture.prepared.target, fixture.prepared.path, fixture.prepared.backup)
	if got, err := os.ReadFile(fixture.prepared.backup); err != nil || string(got) != fixture.original {
		t.Fatalf("new backup = %q, %v; want exact source", got, err)
	}
	if got, err := os.ReadFile(fixture.prepared.target + backupSuffix); err != nil || string(got) != fixture.manualBackup {
		t.Fatalf("occupied backup = %q, %v; want exact existing bytes", got, err)
	}
	cfg, err := config.LoadFile(fixture.prepared.target)
	if err != nil {
		t.Fatalf("LoadFile after finishFix: %v", err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("timer.on_end = %q, want written replacement", cfg.Timer.OnEnd)
	}
}

func TestFinishFixErrorPreservesCompleteFilesystemReason(t *testing.T) {
	isolate(t)
	fixture := newHostileFixDisplayFixture(t)
	if err := os.Mkdir(fixture.prepared.backup, 0o700); err != nil {
		t.Fatal(err)
	}
	f, rawErr := os.OpenFile(fixture.prepared.backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if rawErr == nil {
		f.Close()
		t.Fatal("exclusive open unexpectedly replaced the occupied promised backup")
	}
	want := "create promised backup " + fixture.prepared.backup + ": " + rawErr.Error()

	var stdout, stderr strings.Builder
	if code := finishFix(fixture.prepared, &stdout, &stderr); code != exitError {
		t.Fatalf("finishFix = exit %d, want failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no write record after failure", stdout.String())
	}
	if strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("finish error is not one physical line: %q", stderr.String())
	}
	assertFinalFixAtoms(t, stderr.String(), want)
	if got, err := os.ReadFile(fixture.prepared.target); err != nil || string(got) != fixture.original {
		t.Fatalf("target after failed finish = %q, %v; want exact source", got, err)
	}
	if info, err := os.Stat(fixture.prepared.backup); err != nil || !info.IsDir() {
		t.Fatalf("occupied promised backup = %v, %v; want preserved directory", info, err)
	}
}

type hostileFixDisplayFixture struct {
	prepared     onEndFix
	original     string
	manualBackup string
}

func newHostileFixDisplayFixture(t *testing.T) hostileFixDisplayFixture {
	t.Helper()
	root := t.TempDir()
	lookupHome := filepath.Join(root, hostileFixPathComponent("lookup"))
	t.Setenv("XDG_CONFIG_HOME", lookupHome)
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(root, "targets")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, hostileFixPathComponent("target"))
	const original = "[timer]\nfocus = '25m'\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	const manualBackup = "manual backup that must remain\n"
	if err := os.WriteFile(target+backupSuffix, []byte(manualBackup), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, unchanged, err := prepareOnEndHint(path, "notify-send hello")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}
	if prepared.backup != prepared.target+backupSuffix+".1" {
		t.Fatalf("prepared backup = %q, want first unoccupied name %q", prepared.backup, prepared.target+backupSuffix+".1")
	}
	return hostileFixDisplayFixture{prepared: prepared, original: original, manualBackup: manualBackup}
}

func hostileFixPathComponent(role string) string {
	return "  " + role + "  repeated space \u00a0\u2003\n\x1b\u202e quote\" literal\\n\\x20\\u2003 " + strings.Repeat("wrap", 24) + "  "
}

func assertFinalFixAtoms(t *testing.T, printed string, want ...string) {
	t.Helper()
	for _, raw := range []string{"\x1b", "\u202e", "\u00a0", "\u2003"} {
		if strings.Contains(printed, raw) {
			t.Fatalf("final output contains raw hostile rune %U: %q", []rune(raw)[0], printed)
		}
	}
	var decoded []string
	for _, line := range strings.Split(printed, "\n") {
		for offset := 0; offset < len(line); {
			start := strings.IndexByte(line[offset:], '"')
			if start < 0 {
				break
			}
			start += offset
			found := false
			for end := start + 1; end < len(line); end++ {
				if line[end] != '"' {
					continue
				}
				value, err := strconv.Unquote(line[start : end+1])
				if err != nil {
					continue
				}
				decoded = append(decoded, value)
				offset = end + 1
				found = true
				break
			}
			if !found {
				break
			}
		}
	}
	var relevant []string
	for _, value := range decoded {
		if slices.Contains(want, value) {
			relevant = append(relevant, value)
		}
	}
	if !slices.Equal(relevant, want) {
		t.Fatalf("decoded final path/reason tokens = %q, want exact sequence %q; output=%q", relevant, want, printed)
	}
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func withReadyHint(t *testing.T) {
	t.Helper()
	notifyPrograms(t, "notify-send")
	withAHintForThisMachine(t)
}

func withAHintForThisMachine(t *testing.T) {
	t.Helper()
	previous := notifyHints
	notifyHints = map[string]string{detectSystem(): "notify-send tt"}
	t.Cleanup(func() { notifyHints = previous })
}

func TestDoctorFixRefusesAStdinThatCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin func(*testing.T) io.Reader
	}{
		{"a redirect from /dev/null", func(t *testing.T) io.Reader { return charDevice(t) }},
		{"a pipe", func(t *testing.T) io.Reader {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close(); w.Close() })
			return r
		}},
		{"a reader that is no file", func(t *testing.T) io.Reader { return strings.NewReader("y\n") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			withReadyHint(t)

			var stdout, stderr strings.Builder
			if code := doctorFix(context.Background(), tc.stdin(t), &stdout, &stderr); code != exitError {
				t.Fatalf("doctor --fix = exit %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr.String(), "stdin is not a terminal") {
				t.Errorf("stderr = %q, want the refusal to prompt", stderr.String())
			}
			if strings.Contains(stdout.String(), "write it?") {
				t.Errorf("the question was put to a stream that cannot answer it: %q", stdout.String())
			}
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("--fix touched %s without an answer (stat: %v)", path, err)
			}
		})
	}
}

func TestSetTimerOnEndUncommentsTemplateHeader(t *testing.T) {
	out := setTimerOnEnd([]byte(config.Template()), "notify-send hi")
	if strings.Contains(string(out), "# [timer]") {
		t.Fatalf("the [timer] header must be uncommented:\n%s", out)
	}
	if !strings.Contains(string(out), "[timer]") {
		t.Fatalf("the [timer] header is missing entirely:\n%s", out)
	}
	if !strings.Contains(string(out), `on_end = 'notify-send hi'`) {
		t.Fatalf("on_end was not set:\n%s", out)
	}
}

func TestNotifyHintsSurviveTheConfigFile(t *testing.T) {
	for sys, hint := range notifyHints {
		path := filepath.Join(t.TempDir(), "config.toml")
		if _, _, err := applyOnEndHint(path, hint); err != nil {
			t.Errorf("%s: applyOnEndHint: %v", sys, err)
			continue
		}
		cfg, err := config.LoadFile(path)
		if err != nil {
			t.Errorf("%s: the written config does not load: %v", sys, err)
			continue
		}
		if cfg.Timer.OnEnd != hint {
			t.Errorf("%s: Timer.OnEnd = %q, want %q", sys, cfg.Timer.OnEnd, hint)
		}
	}
}

func TestApplyOnEndHintRefusesADottedTimerKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "default_project = 'Work'\ntimer.focus = '25m'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	written, _, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint wrote a file only tt can read, want a refusal")
	}

	if written != "" {
		t.Errorf("written = %q after a refusal, want nothing named", written)
	}
	if !strings.Contains(err.Error(), "timer.focus") {
		t.Errorf("error = %v, want it to name the key that is in the way", err)
	}
	if !strings.Contains(err.Error(), timerHeader) {
		t.Errorf("error = %v, want it to say where the settings have to go", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("file = %q, want it untouched:\n%q", got, original)
	}

	if _, serr := os.Stat(path + backupSuffix); !os.IsNotExist(serr) {
		t.Errorf("a backup was made for a fix that never wrote anything (stat: %v)", serr)
	}
}

func TestApplyOnEndHintKeepsTheFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[timer]\nfocus = '25m'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	_, backup, err := applyOnEndHint(path, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	for _, p := range []string{path, backup} {
		info, serr := os.Stat(p)
		if serr != nil {
			t.Fatal(serr)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Errorf("%s is mode %04o, want the 0640 the config had", p, got)
		}
	}
}

func TestApplyOnEndHintLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[timer]\nfocus = '25m'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp1234", []byte("half a config"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := applyOnEndHint(path, "notify-send hello"); err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"config.toml", "config.toml" + backupSuffix}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("the directory holds %v, want exactly %v", names, want)
	}
}

func TestApplyOnEndHintLeavesAloneWhatMerelyLooksLikeATemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[timer]\nfocus = '25m'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := []string{
		"config.toml.tmpl",
		"config.toml.tmp.orig",
		"config.toml.tmp-backup",
		"config.toml.tmp",
	}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("none of tt's business"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const leftover = "config.toml.tmp1234"
	if err := os.WriteFile(filepath.Join(dir, leftover), []byte("half a config"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := applyOnEndHint(path, "notify-send hello"); err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is gone, and it is not a name tt ever writes: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
		t.Errorf("the leftover of an interrupted write survived (stat: %v)", err)
	}
}

func TestApplyOnEndHintNamesTheBackupWhenTheWriteFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chflags is a BSD flag; there is no portable way to fail one rename and not the other")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[timer]\nfocus = '25m'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	manualBackup := []byte("manual backup\n")
	if err := os.WriteFile(path+backupSuffix, manualBackup, 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := resolveConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chflags", "uchg", path).CombinedOutput(); err != nil {
		t.Skipf("chflags uchg %s: %v (%s)", path, err, out)
	}

	t.Cleanup(func() { exec.Command("chflags", "nouchg", path).Run() })

	_, backup, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint reported success on a config it cannot replace")
	}
	if backup == "" {
		t.Fatal("no backup reported for a file that was copied before the write")
	}
	if backup != target+backupSuffix+".1" {
		t.Errorf("backup = %q, want selected free name %q", backup, target+backupSuffix+".1")
	}
	if !strings.Contains(err.Error(), backup) {
		t.Errorf("error = %v, want it to account for the copy at %s", err, backup)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("file = %q, want it untouched:\n%q", got, original)
	}
	if b, berr := os.ReadFile(backup); berr != nil || string(b) != original {
		t.Fatalf("backup = %q, %v; want the config as it was", b, berr)
	}
	if b, berr := os.ReadFile(path + backupSuffix); berr != nil || !slices.Equal(b, manualBackup) {
		t.Fatalf("pre-existing backup = %q, %v; want it preserved", b, berr)
	}
}

func TestApplyOnEndHintKeepsTheConfigWhenItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory anyway")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "[timer]\nfocus = '25m'\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	written, _, err := applyOnEndHint(path, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint reported success in a directory it cannot write to")
	}

	if written != "" {
		t.Errorf("written = %q after a failure, want nothing named", written)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Fatalf("file = %q, want it untouched:\n%q", got, original)
	}
}

func TestApplyOnEndHintFollowsALiveSymlink(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-config.toml")
	original := "[timer]\nfocus = '25m'\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	written, _, err := applyOnEndHint(link, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	if !namesTheSameFile(t, written, target) {
		t.Errorf("written = %q, want the file the link points at, %s", written, target)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is a plain file after --fix; the link into the dotfiles repo was destroyed", link)
	}
	if dest, derr := os.Readlink(link); derr != nil || dest != target {
		t.Fatalf("readlink(%s) = %q, %v; want it still naming %s", link, dest, derr, target)
	}

	cfg, err := config.LoadFile(target)
	if err != nil {
		t.Fatalf("the file the link points to does not parse: %v", err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Fatalf("Timer.OnEnd = %q, want the new setting written into the link's target", cfg.Timer.OnEnd)
	}

	entries, err := os.ReadDir(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the link's directory holds %v, want only the link itself", names)
	}
}

func TestApplyOnEndHintCreatesTheTargetOfADanglingSymlink(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-config.toml")
	link := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyOnEndHint(link, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	if backup != "" {
		t.Errorf("backup = %q, want nothing reported: a dangling link has no previous file to copy", backup)
	}
	if !namesTheSameFile(t, written, target) {
		t.Errorf("written = %q, want the target the link already named, %s", written, target)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is a plain file after --fix; the link was destroyed rather than followed", link)
	}
	if dest, derr := os.Readlink(link); derr != nil || dest != target {
		t.Fatalf("readlink(%s) = %q, %v; want it unchanged, still naming %s", link, dest, derr, target)
	}

	cfg, err := config.LoadFile(target)
	if err != nil {
		t.Fatalf("--fix did not create a readable file at the link's target: %v", err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Fatalf("Timer.OnEnd = %q, want the new setting", cfg.Timer.OnEnd)
	}
}

func TestApplyOnEndHintFollowsAChainOfDanglingLinks(t *testing.T) {
	targetDir := t.TempDir()
	final := filepath.Join(targetDir, "real-config.toml")
	mid := filepath.Join(targetDir, "linked-config.toml")
	if err := os.Symlink(final, mid); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink(mid, link); err != nil {
		t.Fatal(err)
	}

	written, _, err := applyOnEndHint(link, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	for _, l := range []string{link, mid} {
		info, lerr := os.Lstat(l)
		if lerr != nil {
			t.Fatalf("lstat %s: %v", l, lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is a plain file after --fix; a link in the chain was written over rather than followed", l)
		}
	}

	cfg, err := config.LoadFile(final)
	if err != nil {
		t.Fatalf("--fix created no readable file at the end of the chain (%s): %v", final, err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("Timer.OnEnd = %q, want the new setting", cfg.Timer.OnEnd)
	}
	if !namesTheSameFile(t, written, final) {
		t.Errorf("written = %q, want the end of the chain, %s", written, final)
	}
}

func TestApplyOnEndHintCreatesTheTargetDirectoryOfADanglingSymlink(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := filepath.Join(t.TempDir(), "dotfiles", "tt")
	target := filepath.Join(targetDir, "real-config.toml")
	link := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	written, backup, err := applyOnEndHint(link, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}
	if backup != "" {
		t.Errorf("backup = %q, want nothing reported: there was no previous file to copy", backup)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is a plain file after --fix; the link was written over rather than followed", link)
	}
	cfg, err := config.LoadFile(target)
	if err != nil {
		t.Fatalf("--fix created no readable file at %s: %v", target, err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("Timer.OnEnd = %q, want the new setting", cfg.Timer.OnEnd)
	}
	if !namesTheSameFile(t, written, target) {
		t.Errorf("written = %q, want the target the link named, %s", written, target)
	}

	dir, err := os.Stat(targetDir)
	if err != nil {
		t.Fatalf("stat %s: %v", targetDir, err)
	}
	if got := dir.Mode().Perm(); got != 0o700 {
		t.Errorf("%s is mode %04o, want 0700", targetDir, got)
	}
}

func TestApplyOnEndHintRefusesAnInvalidSourceThroughALiveSymlink(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-config.toml")
	original := "[timer]\nlong_every = 99\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, backup, err := applyOnEndHint(link, "notify-send hello")
	if err == nil {
		t.Fatal("applyOnEndHint reported success on a config that does not load after the edit")
	}
	if strings.Contains(err.Error(), "restored") {
		t.Errorf("error = %v, want the source refused before any write", err)
	}

	info, lerr := os.Lstat(link)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is a plain file after the prewrite refusal; the link was replaced", link)
	}
	if dest, derr := os.Readlink(link); derr != nil || dest != target {
		t.Fatalf("readlink(%s) = %q, %v; want it still naming %s", link, dest, derr, target)
	}
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != original {
		t.Fatalf("the file behind the link = %q, %v; want the original content untouched:\n%q", got, rerr, original)
	}
	if backup != "" {
		t.Errorf("backup = %q, want none for a refusal before writing", backup)
	}
	if _, rerr := os.Lstat(target + backupSuffix); !os.IsNotExist(rerr) {
		t.Errorf("a backup exists for a source refused before writing: %v", rerr)
	}

	entries, err := os.ReadDir(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the link's directory holds %v, want only the link itself", names)
	}
}

func TestApplyOnEndHintFollowsARelativeLinkOutOfALinkedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deep", "real"), 0o700); err != nil {
		t.Fatal(err)
	}

	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(filepath.Join("deep", "real"), linkDir); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "config.toml")
	target := filepath.Join("..", "b", "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	written, _, err := applyOnEndHint(link, "notify-send hello")
	if err != nil {
		t.Fatalf("applyOnEndHint: %v", err)
	}

	want := filepath.Join(root, "deep", "b", "config.toml")
	astray := filepath.Join(root, "b", "config.toml")
	if _, serr := os.Lstat(astray); serr == nil {
		t.Errorf("--fix wrote %s, which is not where the link points", astray)
	}
	cfg, err := config.LoadFile(want)
	if err != nil {
		t.Fatalf("--fix created no readable config at %s: %v", want, err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("Timer.OnEnd = %q, want the new setting", cfg.Timer.OnEnd)
	}
	if !namesTheSameFile(t, written, want) {
		t.Errorf("written = %q, want %s", written, want)
	}

	for _, l := range []string{linkDir, link} {
		info, lerr := os.Lstat(l)
		if lerr != nil {
			t.Fatalf("lstat %s: %v", l, lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer a link", l)
		}
	}
	if dest, derr := os.Readlink(link); derr != nil || dest != target {
		t.Errorf("readlink(%s) = %q, %v; want it unchanged, still naming %s", link, dest, derr, target)
	}
}

func TestApplyOnEndHintFollowsAnAbsoluteTargetWithADotDot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	const bystanderContent = "default_project = 'Somebody Else'\n\n[timer]\nfocus = '50m'\n"
	bystander := filepath.Join(root, "b", "config.toml")
	if err := os.WriteFile(bystander, []byte(bystanderContent), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(filepath.Join(root, "real", "sub"), linkDir); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "config.toml")
	if err := os.Symlink(linkDir+"/../b/config.toml", link); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, "real", "b", "config.toml")
	got, rerr := resolveConfigPath(link)
	if rerr != nil {
		t.Fatalf("resolveConfigPath: %v", rerr)
	}
	if got != want {
		t.Errorf("resolveConfigPath = %q, want %q - where the kernel follows the link", got, want)
	}

	deepLink := filepath.Join(root, "deep-config.toml")
	if err := os.Symlink(linkDir+"/../b/c/config.toml", deepLink); err != nil {
		t.Fatal(err)
	}
	wantDeep := filepath.Join(root, "real", "b", "c", "config.toml")
	if got, rerr := resolveConfigPath(deepLink); rerr != nil || got != wantDeep {
		t.Errorf("resolveConfigPath(%s) = %q, %v; want %q", deepLink, got, rerr, wantDeep)
	}

	written, _, aerr := applyOnEndHint(link, "notify-send hello")
	if aerr != nil {
		t.Fatalf("applyOnEndHint: %v", aerr)
	}
	if b, berr := os.ReadFile(bystander); berr != nil || string(b) != bystanderContent {
		t.Errorf("--fix rewrote %s, a file this link never named: %q, %v", bystander, b, berr)
	}
	if _, berr := os.Stat(bystander + backupSuffix); berr == nil {
		t.Errorf("--fix left a backup of a file it had no business copying, at %s", bystander+backupSuffix)
	}

	cfg, cerr := config.LoadFile(link)
	if cerr != nil {
		t.Fatalf("the config tt itself reads is still not there: %v", cerr)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("Timer.OnEnd = %q, want the new setting", cfg.Timer.OnEnd)
	}
	if written != want {
		t.Errorf("written = %q, want %q", written, want)
	}
}

func TestFinishFixWritesToTheConfirmedTarget(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "real-config.toml")
	if err := os.WriteFile(target, []byte("[timer]\nfocus = '25m'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	prepared, unchanged, err := prepareOnEndHint(path, "notify-send hello")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}

	var stdout, stderr strings.Builder
	if code := finishFix(prepared, &stdout, &stderr); code != exitOK {
		t.Fatalf("finishFix = exit %d, want %d; stderr=%q", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), target) {
		t.Errorf("stdout = %q, want the target named as what was written", stdout.String())
	}
	cfg, err := config.LoadFile(target)
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", target, err)
	}
	if cfg.Timer.OnEnd != "notify-send hello" {
		t.Errorf("Timer.OnEnd = %q, want the new setting written into the confirmed target", cfg.Timer.OnEnd)
	}
	if info, lerr := os.Lstat(path); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer the symlink into the confirmed target", path)
	}
}

func TestFinishFixRefusesWhenTheTargetChangedSincePrompt(t *testing.T) {
	linkDir := t.TempDir()
	targetDir := t.TempDir()
	shownContent := "[timer]\nfocus = '25m'\n"
	shown := filepath.Join(targetDir, "shown.toml")
	if err := os.WriteFile(shown, []byte(shownContent), 0o600); err != nil {
		t.Fatal(err)
	}
	otherContent := "[timer]\nfocus = '50m'\n"
	other := filepath.Join(targetDir, "other.toml")
	if err := os.WriteFile(other, []byte(otherContent), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkDir, "config.toml")
	if err := os.Symlink(shown, path); err != nil {
		t.Fatal(err)
	}

	target, err := resolveConfigPath(path)
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if !namesTheSameFile(t, target, shown) {
		t.Fatalf("resolveConfigPath(%s) = %q, want it to name %s", path, target, shown)
	}
	prepared, unchanged, err := prepareOnEndHint(path, "notify-send hello")
	if err != nil || unchanged {
		t.Fatalf("prepareOnEndHint = unchanged %v, %v", unchanged, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	if code := finishFix(prepared, &stdout, &stderr); code != exitError {
		t.Fatalf("finishFix = exit %d, want %d; stdout=%q stderr=%q", code, exitError, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "changed") {
		t.Errorf("stderr = %q, want it to say the target changed", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing written to be claimed", stdout.String())
	}
	if got, rerr := os.ReadFile(shown); rerr != nil || string(got) != shownContent {
		t.Errorf("%s = %q, %v; want the promised target untouched, the write was refused", shown, got, rerr)
	}
	if got, rerr := os.ReadFile(other); rerr != nil || string(got) != otherContent {
		t.Errorf("%s = %q, %v; want the new target untouched too - it was never shown or agreed to", other, got, rerr)
	}
	if _, berr := os.Stat(shown + backupSuffix); !os.IsNotExist(berr) {
		t.Errorf("a backup at %s from a refused write", shown+backupSuffix)
	}
	if _, berr := os.Stat(other + backupSuffix); !os.IsNotExist(berr) {
		t.Errorf("a backup at %s from a refused write", other+backupSuffix)
	}
}

func namesTheSameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ai, aerr := os.Lstat(a)
	if aerr != nil {
		t.Logf("lstat %s: %v", a, aerr)
		return false
	}
	bi, berr := os.Lstat(b)
	if berr != nil {
		t.Logf("lstat %s: %v", b, berr)
		return false
	}
	return os.SameFile(ai, bi)
}

func TestRepairOnEndCommandChangesOnlySupportedWholeWords(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
		changed bool
	}{
		{"double quoted", `custom-recorder -- "{task}"`, `custom-recorder -- {task}`, true},
		{"single quoted", `custom-recorder -- '{task}'`, `custom-recorder -- {task}`, true},
		{"same placeholder twice", `custom-recorder -- "{task}" keep '{task}'`, `custom-recorder -- {task} keep {task}`, true},
		{"same placeholder in proper and redundant contexts", `custom-recorder -- {task} "{task}"`, `custom-recorder -- {task} {task}`, true},
		{"multiple placeholders and exact spacing", "custom-recorder\t--  \"{task}\"   '{project}' tail", "custom-recorder\t--  {task}   {project} tail", true},
		{"dash data after proven marker", `custom-recorder -- data '-c' "{task}"`, `custom-recorder -- data '-c' {task}`, true},
		{"already correct custom", `custom-recorder -- {task}`, `custom-recorder -- {task}`, false},
		{"generated command", config.NotifyMacOS, config.NotifyMacOS, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := repairOnEndCommand(tc.command)
			if err != nil || got != tc.want || changed != tc.changed {
				t.Fatalf("repairOnEndCommand(%q) = %q, %v, %v; want %q, %v, nil",
					tc.command, got, changed, err, tc.want, tc.changed)
			}
		})
	}
}

func TestRepairOnEndCommandKeepsUnsupportedShellManual(t *testing.T) {
	cases := map[string]string{
		"assignment":             `MODE=quiet custom-recorder -- "{task}"`,
		"redirect":               `custom-recorder -- "{task}" >output`,
		"shell script":           `sh -c 'custom-recorder "{task}"'`,
		"shell combined flags":   `sh -ec -- "{task}"`,
		"bash combined flags":    `bash -xc -- "{task}"`,
		"env shell wrapper":      `env sh -c -- "{task}"`,
		"busybox shell wrapper":  `busybox sh -c -- "{task}"`,
		"concatenated":           `custom-recorder -- "prefix {task}"`,
		"raw variable":           `custom-recorder -- "$TT_TASK"`,
		"mixed raw and brace":    `custom-recorder -- "$TT_TASK" "{project}"`,
		"unknown option":         `custom-recorder -message "{task}"`,
		"ash script":             `ash -c -- "{task}"`,
		"unknown wrapper script": `unknown-wrapper -c -- "{task}"`,
		"unknown before marker":  `custom-recorder -unknown -- "{task}"`,
		"comment context":        `custom-recorder -- "{task}" # note`,
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			got, changed, err := repairOnEndCommand(command)
			if !errors.Is(err, errManualOnEndRepair) || got != "" || changed {
				t.Fatalf("repairOnEndCommand(%q) = %q, %v, %v; want manual refusal", command, got, changed, err)
			}
		})
	}
}

func TestDoctorFixRepairsTheSelectedCustomCommandWithoutRunningIt(t *testing.T) {
	isolate(t)
	withAnAnswerableStdin(t)
	dir := notifyPrograms(t, "custom-recorder")
	marker := filepath.Join(t.TempDir(), "notifier-ran")
	recorder := filepath.Join(dir, "custom-recorder")
	if err := os.WriteFile(recorder, []byte("#!/bin/sh\nprintf x > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := notifyHints
	notifyHints = map[string]string{detectSystem(): "generated-notifier -- {task}"}
	t.Cleanup(func() { notifyHints = previous })
	const oldCommand = `custom-recorder -- "{task}" keep-this-byte-for-byte`
	const newCommand = `custom-recorder -- {task} keep-this-byte-for-byte`
	original := "[timer]\nfocus = '25m'\non_end = " + config.QuoteTOMLString(oldCommand) + "\non_break_end = 'leave this alone'\n"
	path := writeConfig(t, original)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("doctor --fix = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), fixValue(newCommand)) || strings.Contains(stderr.String(), "generated-notifier") {
		t.Fatalf("question did not name only the selected custom repair: %q", stderr.String())
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timer.OnEnd != newCommand || cfg.Timer.OnBreakEnd != "leave this alone" || cfg.Timer.Focus.String() != "25m" {
		t.Fatalf("written config = %+v", cfg.Timer)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("doctor executed the notifier: %v", err)
	}
	backup, err := os.ReadFile(path + backupSuffix)
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q, %v; want exact original", backup, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doctorFix(context.Background(), strings.NewReader("must not be read\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("second doctor --fix = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "already") || stderr.String() != "" || strings.Contains(stdout.String(), "write it?") {
		t.Fatalf("second doctor --fix prompted or claimed a write: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, written) || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("repeat fix changed config bytes, time, or mode")
	}
	if _, err := os.Stat(path + backupSuffix + ".1"); !os.IsNotExist(err) {
		t.Fatalf("repeat fix created another backup: %v", err)
	}
}

func TestDoctorFixPreflightsTheSelectedCustomProgram(t *testing.T) {
	isolate(t)
	withAnAnswerableStdin(t)
	notifyPrograms(t, "generated-notifier")
	previous := notifyHints
	notifyHints = map[string]string{detectSystem(): "generated-notifier -- {task}"}
	t.Cleanup(func() { notifyHints = previous })
	original := "[timer]\non_end = 'missing-custom -- \"{task}\"'\n"
	path := writeConfig(t, original)

	var stdout, stderr strings.Builder
	if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("doctor --fix = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "" || strings.Contains(stderr.String(), "write it?") ||
		!strings.Contains(stderr.String(), "missing-custom") || strings.Contains(stderr.String(), "generated-notifier") {
		t.Fatalf("selected-program refusal stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != original {
		t.Fatalf("config = %q, %v; want exact original", got, err)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Fatalf("program refusal created a backup: %v", err)
	}
}

func TestDoctorFixLeavesUnsupportedCustomCommandsUnprompted(t *testing.T) {
	for name, command := range map[string]string{
		"assignment":             `MODE=quiet custom-recorder -- "{task}"`,
		"redirect":               `custom-recorder -- "{task}" >output`,
		"shell script":           `sh -c 'custom-recorder "{task}"'`,
		"shell combined flags":   `sh -ec -- "{task}"`,
		"bash combined flags":    `bash -xc -- "{task}"`,
		"env shell wrapper":      `env sh -c -- "{task}"`,
		"busybox shell wrapper":  `busybox sh -c -- "{task}"`,
		"concatenated":           `custom-recorder -- "prefix {task}"`,
		"raw variable":           `custom-recorder -- "$TT_TASK"`,
		"mixed raw and brace":    `custom-recorder -- "$TT_TASK" "{project}"`,
		"unknown options":        `custom-recorder -message "{task}"`,
		"ash script":             `ash -c -- "{task}"`,
		"unknown wrapper script": `unknown-wrapper -c -- "{task}"`,
		"unknown before marker":  `custom-recorder -unknown -- "{task}"`,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			withAnAnswerableStdin(t)
			notifyPrograms(t, "custom-recorder")
			original := "[timer]\non_end = " + config.QuoteTOMLString(command) + "\n"
			path := writeConfig(t, original)
			var stdout, stderr strings.Builder
			if code := doctorFix(context.Background(), strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
				t.Fatalf("doctor --fix = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stdout.String() != "" || strings.Contains(stderr.String(), "write it?") ||
				!strings.Contains(stderr.String(), "nothing written and no question asked") {
				t.Fatalf("unsupported result stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != original {
				t.Fatalf("config = %q, %v; want exact original", got, err)
			}
			if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
				t.Fatalf("unsupported repair created a backup: %v", err)
			}
		})
	}
}
