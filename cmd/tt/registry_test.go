package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestEveryCommandCarriesItsHelp(t *testing.T) {
	for verb, c := range commands {
		if c.help.Verb != verb {
			t.Errorf("the command registered as %q calls itself %q", verb, c.help.Verb)
		}
		if c.help.Summary == "" {
			t.Errorf("%q has no summary, so the index has nothing to print next to it", verb)
		}
		if c.run == nil {
			t.Errorf("%q has nothing to run", verb)
		}
	}
}

func TestEveryCommandHelpIsStructuredAndFitsWidth(t *testing.T) {
	for verb, c := range commands {
		if verb != "help" && len(c.help.Sections) == 0 {
			t.Errorf("%q has no structured help sections", verb)
		}
		for _, line := range c.help.Render(cli.PlainPalette()) {
			if width := cli.DisplayWidth(line); width > cli.Width {
				t.Errorf("%q help line is %d columns wide: %q", verb, width, line)
			}
			if strings.Contains(line, "\x1b") {
				t.Errorf("%q plain help contains an escape sequence: %q", verb, line)
			}
		}
	}
}

func TestIndexShowsEveryCommand(t *testing.T) {
	shown := map[string]bool{}
	for _, g := range indexGroups() {
		for _, h := range g.Verbs {
			if shown[h.Verb] {
				t.Errorf("%q appears in the index twice", h.Verb)
			}
			shown[h.Verb] = true
		}
	}
	for verb := range commands {
		if !shown[verb] {
			t.Errorf("%q is registered but does not appear in the index", verb)
		}
	}
}

func TestIndexNamesTheGroupsAndTheCommands(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), nil, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(nil) = %d, want %d", got, exitOK)
	}
	out := stdout.String()
	for _, want := range []string{
		"tt: TickTick in the terminal",
		"Everyday tasks",
		"Focus and time",
		"Projects and resources",
		"Setup and maintenance",
		"sync",
		"send queued changes and refresh the cache",
		`Run "tt help <command>" for examples.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the index does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nOther\n") {
		t.Errorf("the known commands fell back to an unexplained Other group:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 80 {
			t.Errorf("an index line is wider than 80 columns:\n%q", line)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("the index carried escape sequences to a buffer that is not a terminal:\n%q", out)
	}
}

func TestHelpIsAnsweredForEveryCommand(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sync", "--help"}, "tt sync - "},
		{[]string{"sync", "-h"}, "tt sync - "},
		{[]string{"help", "sync"}, "tt sync - "},

		{[]string{"config", "--init", "--help"}, "tt config - "},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, nil, &stdout, &stderr); got != exitOK {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, exitOK, stderr.String())
			}
			if !strings.HasPrefix(stdout.String(), c.want) {
				t.Errorf("run(%v) printed %q, want it to start with %q", c.args, stdout.String(), c.want)
			}
		})
	}

	isolate(t)
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"config", "--init", "--help"}, nil, &stdout, &stderr)
	if strings.Contains(stdout.String(), "wrote ") {
		t.Errorf("--help ran the command: %q", stdout.String())
	}
}

func TestHelpRefusesWhatItCannotExplain(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"help"}, exitOK},
		{[]string{"help", "sync"}, exitOK},
		{[]string{"help", "frobnicate"}, exitUsage},
		{[]string{"help", "sync", "doctor"}, exitUsage},
		{[]string{"help", "--bogus"}, exitUsage},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, nil, &stdout, &stderr); got != c.want {
				t.Fatalf("run(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}

func TestTakeColor(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRest []string
		wantMode config.ColorMode
		given    bool

		before  bool
		wantErr bool
	}{
		{"nothing to take", []string{"1", "--fix"}, []string{"1", "--fix"}, "", false, false, false},
		{"joined value", []string{"--color=never"}, nil, config.ColorNever, true, false, false},
		{"separate value", []string{"--color", "always"}, nil, config.ColorAlways, true, false, false},
		{"among other options", []string{"1", "--color=auto", "--fix"}, []string{"1", "--fix"}, config.ColorAuto, true, false, false},
		{"no value at all", []string{"--color"}, nil, "", false, false, true},
		{"a value that is not one", []string{"--color=beige"}, nil, "", false, false, true},
		{"the last one wins", []string{"--color=never", "--color=always"}, nil, config.ColorAlways, true, false, false},

		{"a title with the word in it", []string{"buy --color paint", "--fix"}, []string{"buy --color paint", "--fix"}, "", false, false, false},

		{"the marker is passed on", []string{"--", "-5 min plank"}, []string{"--", "-5 min plank"}, "", false, false, false},
		{"the marker after an option", []string{"1", "--color=never", "--", "-x"}, []string{"1", "--", "-x"}, config.ColorNever, true, true, false},

		{"the option after the marker", []string{"--", "--color=never"}, []string{"--", "--color=never"}, "", false, false, false},

		{"the option behind the marker does not win",
			[]string{"--color=never", "--", "--color=always"},
			[]string{"--", "--color=always"}, config.ColorNever, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rest, mode, given, before, err := takeColor(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("takeColor(%v) error = %v, want error: %v", c.args, err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if strings.Join(rest, "|") != strings.Join(c.wantRest, "|") {
				t.Errorf("takeColor(%v) left %v, want %v", c.args, rest, c.wantRest)
			}
			if mode != c.wantMode || given != c.given {
				t.Errorf("takeColor(%v) = %q, given=%v; want %q, given=%v", c.args, mode, given, c.wantMode, c.given)
			}
			if before != c.before {
				t.Errorf("takeColor(%v) reported an option before the marker = %v, want %v", c.args, before, c.before)
			}
		})
	}
}

func TestColorOptionIsAcceptedByEveryCommand(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"version", "--color=never"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(version --color=never) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if got := run(context.Background(), []string{"version", "--color=beige"}, nil, &stdout, &stderr); got != exitUsage {
		t.Fatalf("run(version --color=beige) = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "--color") {
		t.Errorf("stderr = %q, want it to name the option it refused", stderr.String())
	}
}

func TestHelpWinsOverAColorOptionThatWillNotParse(t *testing.T) {
	for _, args := range [][]string{
		{"ls", "--help", "--color"},
		{"ls", "--color=beige", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitOK {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
			}
			if !strings.Contains(stdout.String(), "tt ls") {
				t.Errorf("stdout = %q, want the help for ls", stdout.String())
			}
			if stderr.String() != "" {
				t.Errorf("stderr = %q, want nothing: the line asked a question and got its answer", stderr.String())
			}
		})
	}
}

func TestInvocationMisuseSendsThemToTheExamples(t *testing.T) {
	var stderr bytes.Buffer
	inv := &invocation{verb: "due", stderr: &stderr}
	if got := inv.misuse("unknown option %q", "--bogus"); got != exitUsage {
		t.Errorf("misuse() = %d, want %d", got, exitUsage)
	}
	want := "tt: due: unknown option \"--bogus\"\nrun \"tt help due\" for examples\n"
	if stderr.String() != want {
		t.Errorf("got %q, want %q", stderr.String(), want)
	}
}

func TestInvocationFailReadsLikeEveryOtherFailure(t *testing.T) {
	var stderr bytes.Buffer
	inv := &invocation{verb: "done", stderr: &stderr}
	if got := inv.fail(context.Canceled); got != exitError {
		t.Errorf("fail() = %d, want %d", got, exitError)
	}
	if want := "tt: done: context canceled\n"; stderr.String() != want {
		t.Errorf("got %q, want %q", stderr.String(), want)
	}
}

func TestTaskOutcome(t *testing.T) {
	cases := []struct {
		name     string
		ids      []string
		err      error
		stopped  bool
		wantCode int
		wantOK   bool
		wantErr  string
	}{
		{"a task was named", []string{"abc"}, nil, false, exitOK, true, ""},

		{"the user declined", nil, cli.ErrCancelled, false, exitOK, false, ""},

		{"no task at all", nil, cli.ErrNoReference, false, exitUsage, false, noTaskGivenFor("done")},
		{"nothing matched", nil, &cli.NoMatchError{What: "task", Query: "remont"}, false, exitError, false, "no task matches"},
		{"too much matched", nil, &cli.AmbiguousError{What: "task", Query: "re", Choices: []string{"a", "b"}}, false, exitError, false, "matches 2 tasks"},
		{"the cache would not open", nil, errors.New("database is locked"), false, exitError, false, "database is locked"},

		{"the signal landed while it was resolving", nil, context.Canceled, true, exitInterrupted, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.stopped {
				cancel()
			}
			var stderr bytes.Buffer
			inv := &invocation{ctx: ctx, verb: "done", stderr: &stderr}
			ids, code, ok := inv.taskOutcome(c.ids, c.err)
			if code != c.wantCode || ok != c.wantOK {
				t.Fatalf("taskOutcome(%v) = %d, %v; want %d, %v", c.err, code, ok, c.wantCode, c.wantOK)
			}
			if ok && len(ids) != len(c.ids) {
				t.Errorf("the ids did not come back: %v", ids)
			}
			if c.wantErr == "" {
				if stderr.Len() != 0 {
					t.Errorf("something was said about it: %q", stderr.String())
				}
				return
			}
			if !strings.Contains(stderr.String(), c.wantErr) {
				t.Errorf("stderr = %q, want %q in it", stderr.String(), c.wantErr)
			}
			if !strings.HasPrefix(stderr.String(), "tt: done: ") {
				t.Errorf("stderr = %q, want it to read like every other failure of a command", stderr.String())
			}
		})
	}
}

func TestTaskOutcomeMeetsTheSignalInARealRun(t *testing.T) {
	isolate(t)
	seed, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := seed.CreateTask(context.Background(), model.Task{ProjectId: "p1", Title: "buy milk"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	seed.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saved := canAsk
	canAsk = func(*invocation) bool { cancel(); return false }
	t.Cleanup(func() { canAsk = saved })

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"done", "milk"}, strings.NewReader(""), &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("run(done milk) = %d, want %d (stderr: %s)", got, exitInterrupted, stderr.String())
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Errorf("stdout = %q, stderr = %q, want nothing: ttMain says the run was interrupted",
			stdout.String(), stderr.String())
	}
}

func TestResolveTasksAndValueSplitsBeforeItResolves(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Work"}}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	task, err := st.CreateTask(ctx, model.Task{Title: "Забрать посылку", ProjectId: "p1"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	knownDates := map[string]bool{"fri": true, "18:00": true, "fri 18:00": true}
	accepts := cli.ValueParser(func(v string) error {
		if knownDates[v] {
			return nil
		}
		return fmt.Errorf("%q is not a date tt reads", v)
	})

	var stdout, stderr bytes.Buffer
	inv := &invocation{ctx: ctx, verb: "due", stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}

	ids, value, code, ok := inv.resolveTasksAndValue(st, []string{"забрать", "посылку", "fri", "18:00"},
		store.StatusOpen, "date", accepts)
	if !ok {
		t.Fatalf("resolveTasksAndValue() = %d, not ok (stderr: %s)", code, stderr.String())
	}
	if value != "fri 18:00" {
		t.Errorf("value = %q, want the whole date", value)
	}
	if len(ids) != 1 || ids[0] != task.Id {
		t.Errorf("ids = %v, want the task the title named (%s)", ids, task.Id)
	}

	stderr.Reset()
	if _, _, code, ok = inv.resolveTasksAndValue(st, []string{"забрать", "посылку"},
		store.StatusOpen, "date", accepts); ok || code != exitUsage {
		t.Fatalf("a line with no date in it = %d, ok=%v; want %d", code, ok, exitUsage)
	}
	if !strings.Contains(stderr.String(), "date") {
		t.Errorf("stderr = %q, want it to name what it could not find", stderr.String())
	}

	stderr.Reset()
	if _, _, code, ok = inv.resolveTasksAndValue(st, []string{"1", "позавчера"},
		store.StatusOpen, "date", accepts); ok || code != exitUsage {
		t.Fatalf("a date that will not read = %d, ok=%v; want %d", code, ok, exitUsage)
	}
	said := stderr.String()
	if !strings.Contains(said, "could not read a date") || !strings.Contains(said, `"позавчера" is not a date tt reads`) {
		t.Errorf("stderr = %q, want both that the line would not divide and the parser's reason", said)
	}
}

func TestRegisterRefusesWhatCannotWork(t *testing.T) {
	cases := map[string]command{
		"no verb":     {help: cli.Help{Summary: "does something"}, run: func(*invocation) int { return exitOK }},
		"a duplicate": {help: cli.Help{Verb: "sync", Summary: "again"}, run: func(*invocation) int { return exitOK }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("register accepted it")
				}
			}()
			register(c)
		})
	}
}

func TestResolverAsksNobodyWhenNeitherEndIsATerminal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	inv := &invocation{
		ctx:    context.Background(),
		verb:   "done",
		stdin:  strings.NewReader(""),
		stdout: &stdout,
		stderr: &stderr,
	}
	r := inv.resolver(nil)
	if r.Ask {
		t.Error("a run with no terminal at either end would have stopped to ask a question")
	}
	if r.Stderr != io.Writer(&stderr) {
		t.Error("the question does not go to stderr")
	}
}

func TestAskable(t *testing.T) {
	cases := []struct {
		name   string
		stdin  bool
		stderr bool
		want   bool
	}{
		{"a pipe at both ends", false, false, false},
		{"a terminal to type at and a file to write into", true, false, false},
		{"a question nobody could answer", false, true, false},
		{"a terminal at both ends", true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := askable(c.stdin, c.stderr); got != c.want {
				t.Errorf("askable(%v, %v) = %v, want %v", c.stdin, c.stderr, got, c.want)
			}
		})
	}
}

func TestNoTaskGivenFitsTheWidth(t *testing.T) {
	for _, verb := range []string{"add", "done", "version"} {
		lines := strings.Split(noTaskGivenFor(verb), "\n")
		lines[0] = "tt: " + verb + ": " + lines[0]
		for i, line := range lines {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("tt %s: line %d is %d columns, want at most %d: %q", verb, i+1, n, cli.Width, line)
			}
		}
	}
}

func TestRefusalsLeaveRoomForTheLongestVerb(t *testing.T) {
	longest := ""
	for verb := range commands {
		if len(verb) > len(longest) {
			longest = verb
		}
	}
	if longest == "" {
		t.Fatal("no commands are registered, so there is no verb to measure")
	}

	prefix := "tt: " + longest + ": "

	long := strings.Repeat("Z", 300)
	for _, err := range []error{
		&cli.NoMatchError{What: "list", Query: long, Scope: "the open tasks"},
		&cli.AmbiguousError{What: "task", Query: long, Choices: []string{"a", "b"}},
	} {
		first := strings.SplitN(err.Error(), "\n", 2)[0]
		if n := utf8.RuneCountInString(prefix + first); n > cli.Width {
			t.Errorf("%q in front of a refusal makes a line of %d columns, want at most %d - "+
				"internal/cli holds back less room than the longest verb here needs:\n%q",
				prefix, n, cli.Width, first)
		}
	}
}

func TestQuoteWordMeasuresThePrefixInRunes(t *testing.T) {

	const before = "перед словом "
	if len(before) == utf8.RuneCountInString(before) {
		t.Fatalf("before = %q takes one byte per letter, so it pins nothing", before)
	}

	line := quoteWord(before, strings.Repeat("z", 300))
	if n := utf8.RuneCountInString(line); n != cli.Width {
		t.Errorf("quoteWord wrote a line of %d columns, want the whole %d:\n%q", n, cli.Width, line)
	}
	if !strings.HasPrefix(line, before) || !strings.Contains(line, `"zzz`) {
		t.Errorf("quoteWord = %q, want the prefix with the cut word quoted behind it", line)
	}
}
