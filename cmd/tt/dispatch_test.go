package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
)

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name            string
		args            []string
		wantData        []string
		wantRefinements []string
	}{
		{"empty", nil, nil, nil},
		{"data only", []string{"test"}, []string{"test"}, nil},
		{"refinement only", []string{"--init"}, nil, []string{"--init"}},
		{"data then refinement", []string{"test", "--init"}, []string{"test"}, []string{"--init"}},
		{"lone dash is data", []string{"-", "--init"}, []string{"-"}, []string{"--init"}},
		{"refinement stops further data collection", []string{"a", "--x", "b"}, []string{"a"}, []string{"--x", "b"}},

		{"the marker makes what follows data", []string{"--", "-5 min plank"}, []string{"-5 min plank"}, nil},
		{"data before the marker too", []string{"a", "--", "-b"}, []string{"a", "-b"}, nil},
		{"an option after the marker is data", []string{"--", "-A", "--color=never"}, []string{"-A", "--color=never"}, nil},
		{"the marker alone gives nothing", []string{"--"}, nil, nil},

		{"after an option it is a refinement", []string{"-A", "--", "x"}, nil, []string{"-A", "--", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data, refinements := splitArgs(c.args)
			if !slicesEqual(data, c.wantData) || !slicesEqual(refinements, c.wantRefinements) {
				t.Fatalf("splitArgs(%v) = %v, %v; want %v, %v", c.args, data, refinements, c.wantData, c.wantRefinements)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv(tokenEnvVar, "")
	previous := loginAfterSave
	loginAfterSave = func(context.Context, string, io.Writer, io.Writer) int { return exitOK }
	t.Cleanup(func() { loginAfterSave = previous })
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args prints help", nil, exitOK},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"unknown leading flag", []string{"--bogus"}, exitUsage},
		{"--version", []string{"--version"}, exitOK},
		{"version verb", []string{"version"}, exitOK},
		{"version verb rejects extra args", []string{"version", "extra"}, exitUsage},
		{"config rejects data", []string{"config", "extra"}, exitUsage},
		{"config rejects unknown option", []string{"config", "--bogus"}, exitUsage},
		{"config with no file", []string{"config"}, exitOK},
		{"config --init on empty dir", []string{"config", "--init"}, exitOK},
		{"doctor rejects data", []string{"doctor", "extra"}, exitUsage},
		{"doctor rejects unknown option", []string{"doctor", "--bogus"}, exitUsage},
		{"doctor in a clean sandbox has no token", []string{"doctor"}, exitError},
		{"doctor --fix refuses a non-terminal stdin or unknown system", []string{"doctor", "--fix"}, exitError},
		{"notify requires the test subcommand", []string{"notify"}, exitUsage},
		{"notify rejects an unknown subcommand", []string{"notify", "bogus"}, exitUsage},
		{"notify test with nothing configured", []string{"notify", "test"}, exitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr)
			if got != c.want {
				t.Fatalf("run(%v) = %d, want %d\nstdout: %s\nstderr: %s", c.args, got, c.want, stdout.String(), stderr.String())
			}
		})
	}
}

func TestLoginPortIsOffered(t *testing.T) {
	const flag = "--port"
	if got := helpText(t, "login"); !strings.Contains(got, flag) {
		t.Errorf("tt help login does not offer %s:\n%s", flag, got)
	}
	if !strings.Contains(loginUsage, flag) {
		t.Errorf("the usage line does not name %s: %q", flag, loginUsage)
	}
}

func TestLoginHelpExplainsExistingAccessToken(t *testing.T) {
	got := strings.Join(strings.Fields(helpText(t, "login")), " ")
	for _, want := range []string{"tt login obtains the access token", "~/.local/share/ticktick/token", "$XDG_DATA_HOME/ticktick/token", "tt login token", "hidden prompt", "do not put the token in the command", "without sending older queued changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("tt help login does not contain %q:\n%s", want, got)
		}
	}
}

func helpText(t *testing.T, verb string) string {
	t.Helper()
	c, ok := commands[verb]
	if !ok {
		t.Fatalf("no command registered as %q", verb)
	}
	return strings.Join(c.help.Render(cli.PlainPalette()), "\n")
}

func TestRunConfigInitRefusesExistingFile(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"config", "--init"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("first --init = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	got := run(context.Background(), []string{"config", "--init"}, nil, &stdout, &stderr)
	if got != exitError {
		t.Fatalf("second --init = %d, want %d", got, exitError)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("stderr = %q, want a mention of the file already existing", stderr.String())
	}
}

func TestRunConfigInitWritesTemplate(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"config", "--init"}, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}

	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "tt", "config.toml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != config.Template() {
		t.Fatalf("written file does not match config.Template()")
	}
}

func TestInterruptedTellsAUsersStopFromADeadline(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()

	cases := map[string]struct {
		ctx  context.Context
		want bool
	}{
		"a live context":      {context.Background(), false},
		"the user stopped it": {stopped, true},
		"a deadline ran out":  {expired, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := interrupted(c.ctx); got != c.want {
				t.Errorf("interrupted() = %v, want %v (ctx.Err() = %v)", got, c.want, c.ctx.Err())
			}
		})
	}
}

func TestRunConfigInitWritesNothingAfterTheUserStopped(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	if got := run(ctx, []string{"config", "--init"}, nil, &stdout, &stderr); got != exitInterrupted {
		t.Fatalf("config --init on a stopped run = %d, want %d (stdout: %s, stderr: %s)",
			got, exitInterrupted, stdout.String(), stderr.String())
	}
	home := os.Getenv("XDG_CONFIG_HOME")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read %s: %v", home, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s holds %d entry(s) after a run that was stopped, want none", home, len(entries))
	}
}

func TestEveryReportLineIsOneLineInsideTheWidth(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})

	title := "pay\nthe\trent\a " + strings.Repeat("and file the receipt ", 12)
	steps := []struct {
		name string
		args []string

		check bool
	}{
		{"add", []string{"add", "--", title}, true},
		{"ls", []string{"ls"}, false},
		{"due", []string{"due", "1", "fri"}, true},
		{"pri", []string{"pri", "1", "high"}, true},
		{"done", []string{"done", "1"}, true},
		{"undo", []string{"undo"}, true},
		{"rm", []string{"rm", "1", "-y"}, true},
	}
	for _, s := range steps {
		var stdout, stderr bytes.Buffer
		if got := run(ctx, s.args, strings.NewReader(""), &stdout, &stderr); got != exitOK {
			t.Fatalf("run(%v) = %d, want %d (stderr: %s)", s.args, got, exitOK, stderr.String())
		}
		if !s.check {
			continue
		}
		if stdout.Len() == 0 {
			t.Errorf("%s printed nothing about the task it acted on", s.name)
		}
		for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > cli.Width {
				t.Errorf("%s: a report line is %d columns wide:\n%q", s.name, n, line)
			}
			for _, r := range line {
				if unicode.IsControl(r) {
					t.Errorf("%s: a report line carries %U raw:\n%q", s.name, r, line)
				}
			}
		}
	}
}

func TestRunRefusesAMarkerThatCameAfterTheOptions(t *testing.T) {
	for _, args := range [][]string{
		{"add", "note", "here", "-p", "high", "--", "-weird"},
		{"done", "-A", "--", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitUsage, stderr.String())
			}
			if strings.Contains(stderr.String(), "unknown option") {
				t.Errorf("stderr = %q, want it not to call the marker an option tt has never heard of", stderr.String())
			}
			for _, want := range []string{`"--" goes in front of the data`, endOfOptionsExample} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to carry %q", stderr.String(), want)
				}
			}

			for _, line := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("a line of the refusal is %d columns wide:\n%q", n, line)
				}
			}
		})
	}
}

func TestRunStillHonoursAMarkerThatCameFirst(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	if got := run(context.Background(), []string{"add", "--", "-5 min plank"}, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "-5 min plank") {
		t.Errorf("stdout = %q, want the task created under the title behind the marker", stdout.String())
	}
}

func TestRunSaysWhereTheMarkerGoesWhenItCameBeforeTheVerb(t *testing.T) {
	for _, args := range [][]string{
		{"--"},
		{"--", "add", "foo"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitUsage, stderr.String())
			}
			if strings.Contains(stderr.String(), "unknown flag") {
				t.Errorf("stderr = %q, want it not to call the marker a flag tt has never heard of", stderr.String())
			}
			for _, want := range []string{`"--" goes after the command`, endOfOptionsExample} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to carry %q", stderr.String(), want)
				}
			}

			for _, line := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("a line of the refusal is %d columns wide:\n%q", n, line)
				}
			}
		})
	}
}

func TestRunRefusesAMarkerBehindTheColorOption(t *testing.T) {
	for _, args := range [][]string{
		{"add", "1", "--color=never", "--", "-x"},
		{"add", "1", "--color", "never", "--", "-x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitUsage {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitUsage, stderr.String())
			}
			for _, want := range []string{`"--" goes in front of the data`, endOfOptionsExample} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want the refusal every other option on that line earns: %q",
						stderr.String(), want)
				}
			}
		})
	}
}

type boundWordCase struct {
	name     string
	args     []string
	exit     int
	onStdout bool
	file     string
	before   string
}

var boundWordExpectedWords = map[string]string{
	"a repeated timer note option":       `"--note"`,
	"a missing timer note value":         `"--note"`,
	"a repeated indicator format option": `"--format"`,
	"a missing indicator format value":   `"--format"`,
}

func boundWordCases() []boundWordCase {
	long := strings.Repeat("z", 200)
	return []boundWordCase{
		{"a flag that names no command", []string{"--" + long}, exitUsage, false, "dispatch.go", "tt: unknown flag "},
		{"a word that names no command", []string{long}, exitUsage, false, "dispatch.go", "tt: unknown command "},
		{"an option help does not take", []string{"help", "--" + long}, exitUsage, false, "registry.go", "unknown option "},
		{"a command help has never heard of", []string{"help", long}, exitUsage, false, "registry.go", "no command called "},
		{"an option add does not take", []string{"add", "milk", "--" + long}, exitUsage, false, "add.go", "unknown option "},
		{"an option item does not take", []string{"item", "1", "done", "1", "--" + long}, exitUsage, false, "item.go", "unknown option "},
		{"an action item does not know", []string{"item", "1", long}, exitUsage, false, "item.go", "unknown action "},
		{"an item position that is not an integer", []string{"item", "1", "done", long}, exitUsage, false, "item.go", "position must be a positive integer: "},
		{"an option repeat does not take", []string{"repeat", "1", "daily", "--" + long}, exitUsage, false, "repeat.go", "unknown option "},
		{"an option remind does not take", []string{"remind", "1", "TRIGGER:PT0S", "--" + long}, exitUsage, false, "remind.go", "unknown option "},
		{"an argument timer status does not take", []string{"pomodoro", "status", long}, exitUsage, false, "timer.go", "unexpected argument "},
		{"an operation timer does not know", []string{"pomodoro", long}, exitUsage, false, "timer.go", "unknown operation "},
		{"a repeated timer note option", []string{"pomodoro", "start", "--note", long, "--note"}, exitUsage, false, "timer.go", "repeated option "},
		{"an option timer does not take", []string{"pomodoro", "start", "--" + long}, exitUsage, false, "timer.go", "unknown option "},
		{"a missing timer note value", []string{"pomodoro", "start", "--note"}, exitUsage, false, "timer.go", "missing value for "},
		{"a timer duration that is not one", []string{"pomodoro", "start", "-d", long}, exitUsage, false, "timer.go", "expected a whole-second duration from 1s to 24h, got "},
		{"an invalid session indicator mode", []string{"timer", "start", "--indicator", long}, exitUsage, false, "timer.go", "expected --indicator on or off, got "},
		{"an unknown indicator option", []string{"timer", "indicator", "--" + long}, exitUsage, false, "timer_indicator.go", "unknown indicator option "},
		{"a repeated indicator format option", []string{"timer", "indicator", "--format", "plain", "--format"}, exitUsage, false, "timer_indicator.go", "repeated option "},
		{"a missing indicator format value", []string{"timer", "indicator", "--format"}, exitUsage, false, "timer_indicator.go", "missing value for "},
		{"an invalid indicator format", []string{"timer", "indicator", "--format", long}, exitUsage, false, "timer_indicator.go", "expected plain, json or tmux, got "},
		{"an invalid indicator width", []string{"timer", "indicator", "--width", long}, exitUsage, false, "timer_indicator.go", "expected indicator width from 32 to 512, got "},
		{"an option done does not take", []string{"done", "1", "--" + long}, exitUsage, false, "done.go", "unknown option "},
		{"an option due does not take", []string{"due", "1", "fri", "--" + long}, exitUsage, false, "due.go", "unknown option "},
		{"an option mv does not take", []string{"mv", "1", "Работа", "--" + long}, exitUsage, false, "mv.go", "unknown option "},
		{"an option pri does not take", []string{"pri", "1", "high", "--" + long}, exitUsage, false, "pri.go", "unknown option "},
		{"an option show does not take", []string{"show", "1", "--" + long}, exitUsage, false, "show.go", "unknown option "},
		{"an option rm does not take", []string{"rm", "1", "--" + long}, exitUsage, false, "rm.go", "unknown option "},
		{"an option ls does not take", []string{"ls", "--" + long}, exitUsage, false, "ls.go", "unknown option "},
		{"an option ui does not take", []string{"ui", "--" + long}, exitUsage, false, "ui.go", "unknown option "},
		{"an option s does not take", []string{"s", "milk", "--" + long}, exitUsage, false, "s.go", "unknown option "},
		{"an option undo does not take", []string{"undo", "--" + long}, exitUsage, false, "undo.go", "unknown option "},
		{"an argument today does not take", []string{"today", long}, exitUsage, false, "today.go", "unexpected argument "},
		{"an option today does not take", []string{"today", "--" + long}, exitUsage, false, "today.go", "unknown option "},
		{"an argument config does not take", []string{"config", long}, exitUsage, false, "config_cmd.go", "tt: config: unexpected argument "},
		{"an option config does not take", []string{"config", "--" + long}, exitUsage, false, "config_cmd.go", "tt: config: unknown option "},
		{"an argument doctor does not take", []string{"doctor", long}, exitUsage, false, "doctor.go", "tt: doctor: unexpected argument "},
		{"an option doctor does not take", []string{"doctor", "--" + long}, exitUsage, false, "doctor.go", "tt: doctor: unknown option "},
		{"an argument sync does not take", []string{"sync", long}, exitUsage, false, "sync.go", "unexpected argument "},
		{"an option sync does not take", []string{"sync", "--" + long}, exitUsage, false, "sync.go", "unknown option "},
		{"an argument auto status does not take", []string{"auto", "status", long}, exitUsage, false, "background.go", "unexpected argument "},
		{"an option auto does not take", []string{"auto", "run", "--" + long}, exitUsage, false, "background.go", "unknown option "},
		{"an operation auto does not know", []string{"auto", long}, exitUsage, false, "background.go", "unknown operation "},
		{"an option login does not take", []string{"login", "--" + long}, exitUsage, false, "login.go", "tt: login: unknown option "},
		{"a port that is not one", []string{"login", "--port", long}, exitUsage, false, "login.go", "tt: login: invalid --port value "},

		{"an empty result s reports", []string{"s", long}, exitOK, true, "s.go", "no tasks match "},
	}
}

type heldElsewhereSite struct {
	file string
	fn   string

	calls  int
	what   string
	holder string
}

// Forty-one sites below account for fifty-two bounding calls and their focused tests.
var heldElsewhere = []heldElsewhereSite{
	{"auth.go", "authText", 1,
		"Local authorization labels and scopes are escaped and bounded as complete final lines",
		"TestAuthTextBoundsAndEscapes"},
	{"background.go", "backgroundStatus", 1,
		"the saved background error, escaped and bounded after its final label",
		"TestBackgroundStatusBoundsLastError"},
	{"schedule.go", "intervalInputError", 1,
		"Interval parser and option errors are escaped and bounded after the command prefix",
		"TestIntervalInputErrorsFitAndEscape"},
	{"timer_indicator.go", "renderIndicator", 2,
		"the bounded JSON task label and the final plain/tmux line's task label; controls are quoted, tmux hashes are escaped and both widths are measured",
		"TestIndicatorVisibilityFormattingAndHostileTitles"},
	{"ui.go", "uiFailure", 1,
		"a UI startup or query error, escaped and bounded after the final command prefix",
		"TestUIFailureBoundsFinalStderr"},
	{"add.go", "cmdAdd", 2,
		"the cached list name on ordinary add and Markdown add, cut to a quarter of the width. " +
			"The holder runs both paths with long titles and hostile long list names, and measures final stdout",
		"TestEditorReportsBoundTitlesAndProjectNames"},
	{"config_cmd.go", "printConfigError", 2,
		"the two answer bodies \"tt config\", \"tt notify test\", or \"tt doctor --fix\" has about " +
			"a config file it could not load: the parser's sentence about the line it choked on, which " +
			"carries the key it choked on and so that file's own bytes, and the selected error of a read " +
			"that never reached a line. The problem is cut to the terminal's width less the two columns " +
			"every problem stands behind, less again whatever tt's own \"line N: \" takes in front of it; " +
			"the read's error is escaped whole with a budget it cannot reach. The config path in the " +
			"header is handled by fullReportAtom and accounted for by that helper's own row. The line is " +
			"finished here: this function writes the whole of it and nothing folds it afterwards",
		"TestConfigEscapesWhatItDidNotWrite"},
	{"doctor.go", "apiDetail", 1,
		"the answer a server or a proxy gave the one call the API check makes: the bounded raw prefix " +
			"of that answer's own body on one line with no " +
			"space anywhere in it, going into a verdict the report folds. Everything tt wrote itself " +
			"- the status, the word http, the limit, the fixed decode category and the sentence about an answer carrying nothing - " +
			"stands outside the quotes, so what the width has to pay for is the bracket, the comma " +
			"and that lead in front of it. The value tt sent is taken out of the answer before the " +
			"bound is applied, an answer being free to echo the request it is answering. The direct " +
			"round-trip holder requires the non-truncated quote to recover only the retained raw bytes",
		"TestCheckAPIKeepsAServerBodyInsideTheWidth"},
	{"doctor.go", "cacheCompareFailure", 1,
		"what the schema comparison came back with where what gave way was the reference schema " +
			"and not the cache: an in-memory database this build fills with its own migration " +
			"bodies, so the answer is the store's wording with SQLite's refusal of a body inside " +
			"it. Not tt's prose, and not written to a width - SQLite quotes what it choked on, at " +
			"whatever length, line breaks included. The width is what the note leaves once the " +
			"store's lead is counted, and the sentinel's own sentence is taken off the front of " +
			"the answer first, the summary having just said it in tt's own words",
		"TestCacheCompareFailureTellsThisBuildsOwnSchemaFromTheUsersFile"},
	{"doctor.go", "cacheReadFailure", 1,
		"what came back from one of the six reads this check makes once the pages have been " +
			"vouched for - the schema the file holds, the task count, the list names, the outbox " +
			"counts, the parked creates, the rows made offline. The driver names the object it " +
			"choked on, and `malformed database schema (<name>)` carries a table or index name out " +
			"of somebody else's cache at whatever length whoever wrote the file gave it. The width " +
			"is what the note leaves once the lead is counted, and here the lead is one of two: " +
			"two of the six hand the driver's answer on as it was given and four come through the " +
			"store, which words the trouble first, so the caller passes the lead it is entitled to " +
			"and the budget is worked out from that. Both arithmetics are the two rows below. The " +
			"holder asks this verdict for directly, once behind each lead: no fixture is known " +
			"that makes one of the six fail after the integrity check over the same pages has " +
			"passed, and a bound on a path nothing reaches is the one that would be lost quietly",
		"TestCheckCacheBoundsAReadThatWouldNotFinish"},
	{"doctor.go", "cacheUnreadable", 1,
		"what SQLite said about a cache that could not be opened at all: the driver's own words, at " +
			"what the note leaves once the lead tt writes in front of them is taken off. They stand in " +
			"a note and not inside doctor's own sentence because in two measured states they say " +
			"\"write\" about a check that never writes, and a reader given both has to choose which " +
			"half to believe",
		"TestCacheUnreadableKeepsDriverAnswerOnOneBoundedLine"},
	{"doctor.go", "cacheVersionUnreadable", 1,
		"what answered when the cache's schema version could not be got out of it: the driver's words " +
			"where the read itself failed, and the store's where the row was found with something in " +
			"it that is not a number. The store's complaint carries that something whole - meta.value " +
			"is TEXT and what stands in it belongs to whatever wrote the file - and it went into " +
			"doctor's own summary until this call was put in front of it, four hundred characters of " +
			"it in one word. The width is what the note leaves once the lead is counted, the same " +
			"arithmetic the cacheUnreadable row above rests on",
		"TestCheckCacheBoundsAVersionNothingCanRead"},
	{"doctor.go", "configListNames", 1,
		"the names of the lists the cache holds, in the line that says which names the configured " +
			"default_project could have been. They are the server's text or the user's, at whatever " +
			"length and with whatever in them, and they reach doctor out of the file the check above " +
			"this one exists to disbelieve. The width is the prose width less the one column the " +
			"joining comma stands in: the names are printed one behind another, so the word cli.Wrap " +
			"measures is a closing quote with a comma against it and nothing in it to break at. The " +
			"line is finished by whichever note asked for the names",
		"TestCheckConfigEscapesWhatItDidNotWrite"},
	{"doctor.go", "configUnusable", 1,
		"the parser's sentence about the line it choked on, which carries the key it choked on and so " +
			"that file's own bytes. It is cut to the prose width less whatever tt's own \"line N: \" " +
			"takes in front of it. The selected reason of a read that reached no line and both summary " +
			"paths go through fullReportAtom instead and are accounted for by that helper's row",
		"TestCheckConfigEscapesWhatItDidNotWrite"},
	{"default_project.go", "cmdDefaultProject", 2,
		"Cached list names in picker rows and the saved result are escaped and bounded to 48 cells; validated opaque IDs remain complete",
		"TestDefaultListOutputEscapesNamesAndPaths"},
	{"default_focus.go", "pickDefaultFocus", 2,
		"Cached list/task labels fit the remaining prompt width after numbers and complete validated IDs; opaque IDs are never truncated",
		"TestDefaultFocusOutputEscapesNamesAndPaths"},
	{"default_focus_doctor.go", "defaultFocusVerdict", 3,
		"Cached titles and errors are escaped and bounded before doctor wraps them; validated opaque references remain complete",
		"TestDefaultFocusOutputEscapesNamesAndPaths"},
	{"doctor.go", "defaultProjectVerdict", 1,
		"the effective default_project - the name \"tt add\" reaches for when no list is given - out of " +
			"the user's own config file, or the name tt ships with where nothing tt can read sets one. " +
			"One call for every sentence this verdict can print, escaped once at the top of it, because " +
			"the same value goes into the line that says it matches nothing, the line that says it " +
			"matches several, and the two that say the question was not put; a bound on one of those " +
			"and not the others is a report that stays inside the width until the day the cache is the " +
			"thing that is wrong. The width is the prose width whole, the value ending its clause with " +
			"a space after it",
		"TestCheckConfigEscapesWhatItDidNotWrite"},
	{"doctor.go", "doubleOpenNotes", 1,
		"the dotted key names of a table the config file opens twice, which have no length at all " +
			"until the file is read. Not the closed vocabulary they look like, either: what this block " +
			"reads is a second read of that path, and under the race that read documents a key of any " +
			"shape arrives here. What the escaping buys was measured rather than assumed - the decoder " +
			"refuses a file holding a raw control byte outright, and a key written as an escape comes " +
			"back from toml.Key.String still escaped, so it is not buying protection from either; it " +
			"buys U+202E, the bidi override, which decodes and comes back raw, and a width for a key as " +
			"long as somebody cares to write. The width is the prose width less the column the joining " +
			"comma stands in, the keys being printed one behind another",
		"TestCheckConfigEscapesWhatItDidNotWrite"},
	{"doctor.go", "fullReportAtom", 1,
		"the complete path, error, selected reason, symbolic-link target, or probed config anchor " +
			"passed by any of its fifty-three callers. The " +
			"helper gives cli.Foreign a budget large enough for every escaped rune, so the whole value " +
			"remains one opaque atom without truncation; a caller may then fold its surrounding prose, " +
			"and the atom may remain wider than the terminal by design",
		"TestFullReportAtomSurvivesDoctorWrapping"},
	{"doctor.go", "integrityProblemLines", 1,
		"one row of PRAGMA integrity_check: SQLite's words carrying index and table names out of a " +
			"file this check has just decided it cannot vouch for. The width is the nested listing's, " +
			"and the rows stand one to a line, so a name with a break in it would turn one row into " +
			"two and the count above the listing would stop describing it",
		"TestCheckCacheReportsADamagedFile"},
	{"doctor.go", "lastSyncAge", 3,
		"the last_sync stamp read out of the cache, whose length belongs to whatever wrote it - " +
			"a build that spelled the stamp differently, or something that is not tt at all. Three " +
			"calls over one column: the stamp is quoted where it cannot be read at all, and again at " +
			"either end of the range a duration reaches, and a bound put on one branch and not the " +
			"other two is a report that stays inside the width until the day the clock is the thing " +
			"that is wrong",
		"TestCheckCacheBoundsEveryAnswerAboutTheLastSync"},
	{"doctor.go", "lastSyncReadFailure", 1,
		"the answer returned instead of last_sync: store.Meta words what the driver said as " +
			"\"read meta last_sync: ...\". SQLite pre-escapes some object names, so the holder also " +
			"calls this raw-error boundary directly; the width is what the note leaves once the store " +
			"lead is counted",
		"TestCheckCacheBoundsEveryAnswerAboutTheLastSync"},
	{"doctor.go", "orphanedTaskLine", 2,
		"the two halves of a row made offline that nothing will ever send, both of them out of the " +
			"cache: the title, and the id beside it. The row is where the two halves of the rule " +
			"come apart, which is why one line holds two calls. The title is cut - the width is " +
			"the nested note's and comes from checkCache, which is where the line is finished - " +
			"and the id is not, being unbreakable and useless halved, so it gets a budget it " +
			"cannot reach and the call is there to escape it and nothing else. tasks.id is TEXT " +
			"in a file this check exists to disbelieve and the query behind it selects on local = 1 " +
			"and never on the shape of an id, so a line break in one turned the row into two lines " +
			"and the count over the listing stopped describing it",
		"TestCheckCacheHoldsBothHalvesOfAnOrphanedRow"},
	{"doctor.go", "schemaObjectLines", 1,
		"the name of one object a cache holds that this build's migrations do not produce, or the " +
			"other way round: whatever somebody wrote in a CREATE TABLE, a line break inside the " +
			"quoted name included. The width is the nested line's less the kind standing in front " +
			"of the name, and both halves of the difference go through this one call so that moving " +
			"an entry between them cannot lose the bound. The kind is not tt's word, whatever an " +
			"earlier version of this row said: for an object the cache has and the migrations do " +
			"not it is the type column of the user's own sqlite_schema. It is printed unbounded " +
			"because SQLite refuses to load a schema whose type is not one of its own four words, " +
			"so a file carrying a fifth never reaches this listing - it fails at the open and comes " +
			"back through cacheUnreadable, which quotes the driver and bounds what it quotes. That " +
			"is measured rather than assumed",
		"TestCheckCacheBoundsAnObjectNameOutOfTheCache"},
	{"doctor.go", "unreachableAnswerLines", 1,
		"the whole text of an error handed to the API-unreachable renderer. The api package's current " +
			"ordinary errors are finite safe categories, so hostile width and escape-boundary inputs " +
			"come from direct defensive helper fixtures. The row is where the width is " +
			"not what the call is for: cutToColumns folds it onto the three nested lines, and " +
			"what cli.Foreign does first is escape the answer and cut it to the budget those " +
			"lines come to. The value tt sent is taken out of the error before either of them",
		"TestUnreachableAnswerLosesNoneOfItsBoundedRendering"},
	{"doctor.go", "unresolvedProgramNotice", 1,
		"the first word of a command out of the user's config, in the sentence saying nothing on this " +
			"machine answers to it: somebody else's word at whatever length they gave it, going into " +
			"prose the notify check folds on spaces",
		"TestCheckNotifyBoundsAProgramNameOutOfTheConfig"},
	{"doctor_fix.go", "checkOnEndEdit", 1,
		"the timer.on_end value parsed from the exact prospective candidate before --fix writes " +
			"anything: whatever the text edit produced in among the captured source bytes",
		"TestCheckOnEndEditBoundsWhatTheFileHolds"},
	{"doctor_fix.go", "fixValue", 1,
		"the old and new timer.on_end values shown in --fix's replacement question, and the value " +
			"named by its unchanged outcome: bytes from the user's config and a platform command. The " +
			"budget is deliberately large enough for every escaped rune because cutting the suffix " +
			"could hide the destructive part of the value the user is being asked to replace",
		"TestDoctorFixEscapesBothCompleteReplacementValues"},
	{"edit.go", "editorFailure", 1,
		"the external editor, parser or store error rendered after an editor attempt, escaped and " +
			"bounded to the terminal width minus the fixed command prefix; the holder measures final stderr",
		"TestEditorFailureBoundsAndEscapesFinalError"},
	{"edit.go", "editorDraftPath", 1,
		"the complete recovery pathname, escaped as one quoted atom on its own physical line. " +
			"It deliberately exceeds width when necessary; wrapping or clipping would corrupt a copyable path. " +
			"The holder decodes final stderr and requires every original path byte",
		"TestEditorRecoveryPathFinalOutputKeepsExactBytes"},
	{"mv.go", "moveName", 1,
		"a task title or a list name going into one of mv's paragraphs; the width is the paragraph's " +
			"and comes from the caller, one column of it held back for the mark written against the name",
		"TestMvBoundsEveryNameItPutsOnScreen"},
	{"sync.go", "printDroppedMutations", 1,
		"the op at the head of a drop record, out of a queue entry another build may have written",
		"TestSyncKeepsADropRecordInsideTheWidthWhateverTheOpIs"},
	{"sync.go", "droppedTaskProject", 1,
		"the name of the list a dropped entry's task was in; the width is what the record's line " +
			"leaves, worked out by printDroppedMutations, which finishes it",
		"TestSyncKeepsWhatItSaysAboutADropInsideTheWidth"},
	{"sync.go", "droppedTaskName", 2,
		"the title of a dropped entry's task, covered directly at an ordinary caller width and in " +
			"the defensive nonempty-title, empty-id fallback below droppedTitleFloor. The record printer's " +
			"current width arithmetic clamps at that floor, so it cannot reach the fallback itself",
		"TestDroppedTaskNameEscapesAndBoundsBothTitleBranches"},
	{"undo.go", "cmdUndo", 1,
		"the op out of a record this build has no reversal for, in the refusal that names it",
		"TestUndoOfAForeignOpStaysInsideTheWidth"},
	{"timer.go", "timerFailure", 1,
		"a store, watcher, notifier or uploader error, escaped and bounded after the actual timer or pomodoro diagnostic prefix; the holder measures final stderr for both verbs",
		"TestTimerGuardFieldsAndFailuresKeepFinalLinesBounded"},
	{"timer.go", "timerField", 1,
		"the session ID, note, outcome, cached task title or missing-task ID, escaped and bounded after each final field prefix; the holder measures final stdout for both timer renderers",
		"TestTimerGuardFieldsAndFailuresKeepFinalLinesBounded"},
	{"timer_note.go", "resolveTimerNote", 1,
		"the upload wake error after a completion note was saved, escaped and bounded as the complete final stderr line; the holder verifies long and hostile warnings cannot overflow or emit terminal controls and do not undo the saved note",
		"TestTimerCompletionNoteWakeWarningBoundsAndEscapes"},
	{"undo.go", "undoSkipEntry", 1,
		"the same op, in the line --skip prints about the record it dropped",
		"TestUndoOfAForeignOpStaysInsideTheWidth"},
	{"undo.go", "undoName", 1,
		"the title of the task a refusal or a dropped record names, at the whole width because nothing " +
			"tt writes in these sentences stands hard against the name",
		"TestUndoSkipStaysInsideTheWidth"},
}

type reportTitleException struct {
	file  string
	fn    string
	calls int
	why   string
}

var reportTitleExceptions = []reportTitleException{
	{"registry.go", "quoteWord", 1,
		"quoteWord itself, which is the arithmetic the first half of this inventory is built on. " +
			"The string it bounds is the word off the command line, so its account is boundWordCases " +
			"above - every caller has a row there - and the arithmetic itself is held by " +
			"TestQuoteWordMeasuresThePrefixInRunes in registry_test.go"},
}

type quotedVerbException struct {
	file   string
	format string
	why    string
}

var quotedVerbExceptions = []quotedVerbException{
	{
		file:   "add.go",
		format: "unknown option %q",
		why: "the Error text of addUnknownOption, which is a description of the failure for " +
			"whoever prints the error rather than the word; the line the user reads is built " +
			"by cmdAdd through misuseWord and is in the inventory above",
	},
}

func TestLinesBoundTheWordsTheyQuote(t *testing.T) {
	for _, c := range boundWordCases() {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			var stdout, stderr bytes.Buffer
			if got := run(context.Background(), c.args, strings.NewReader(""), &stdout, &stderr); got != c.exit {
				t.Fatalf("run(%v) = %d, want %d (stderr: %s)", c.args, got, c.exit, stderr.String())
			}

			quoted, where := stderr.String(), "stderr"
			if c.onStdout {
				quoted, where = stdout.String(), "stdout"
			}
			expectedWord := "zzz"
			if fixed, ok := boundWordExpectedWords[c.name]; ok {
				expectedWord = fixed
			}
			if !strings.Contains(quoted, expectedWord) {
				t.Errorf("%s = %q, want it to quote back %q", where, quoted, expectedWord)
			}

			for _, out := range []struct{ name, text string }{
				{"stdout", stdout.String()},
				{"stderr", stderr.String()},
			} {
				for _, line := range strings.Split(strings.TrimRight(out.text, "\n"), "\n") {
					if n := utf8.RuneCountInString(line); n > cli.Width {
						t.Errorf("a line of %s is %d columns wide:\n%q", out.name, n, line)
					}
				}
			}
		})
	}
}

func TestEveryQuotedWordIsAccountedFor(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackage(t, fset, false)

	inventory := map[string]string{}
	for _, c := range boundWordCases() {
		key := c.file + " " + strconv.Quote(c.before)
		if other, dup := inventory[key]; dup {
			t.Errorf("two rows name the same site %s: %q and %q", key, other, c.name)
		}
		inventory[key] = c.name
	}
	found := map[string]bool{}
	for name, f := range files {
		forEachCall(f, func(fn string, call *ast.CallExpr) {
			callee := calleeName(call.Fun)
			if callee != "quoteWord" && callee != "misuseWord" {
				return
			}

			if name == "registry.go" && fn == "misuseWord" {
				return
			}
			before := literalText(call.Args[0])
			key := name + " " + strconv.Quote(before)
			found[key] = true
			if _, ok := inventory[key]; !ok {
				t.Errorf("%s writes a word behind %q and no row of boundWordCases names it; "+
					"add the command line that reaches it", fset.Position(call.Pos()), before)
			}
		})
	}
	for key, name := range inventory {
		if !found[key] {
			t.Errorf("boundWordCases has %q for site %s, and nothing in the package writes it any more",
				name, key)
		}
	}

	exceptions := map[string]bool{}
	for _, e := range quotedVerbExceptions {
		exceptions[e.file+" "+strconv.Quote(e.format)] = true
	}
	seen := map[string]bool{}
	for name, f := range files {
		forEachString(f, func(lit *ast.BasicLit, s string) {
			if !strings.Contains(s, "%q") {
				return
			}
			key := name + " " + strconv.Quote(s)
			seen[key] = true
			if !exceptions[key] {
				t.Errorf("%s prints %%q on a string of somebody else's without bounding it first; "+
					"hand the word to quoteWord, or say in quotedVerbExceptions what it is instead:\n\t%s",
					fset.Position(lit.Pos()), s)
			}
		})
	}
	for _, e := range quotedVerbExceptions {
		if !seen[e.file+" "+strconv.Quote(e.format)] {
			t.Errorf("quotedVerbExceptions still excuses %s %q (%s), and the package no longer has it",
				e.file, e.format, e.why)
		}
	}

	const heldRows = 41
	const heldCalls = 52
	if len(heldElsewhere) != heldRows {
		t.Errorf("heldElsewhere has %d rows and the account over it is written for %d; the sentences "+
			"there spell the number out in words and have to be read again", len(heldElsewhere), heldRows)
	}
	accounted := 0
	for _, site := range heldElsewhere {
		accounted += site.calls
	}
	if accounted != heldCalls {
		t.Errorf("heldElsewhere answers for %d bounding calls and the account over it is written for "+
			"%d; that number is spelled out in words there as well", accounted, heldCalls)
	}
	tests := map[string]bool{}
	for _, f := range parsePackage(t, fset, true) {
		forEachFunc(f, func(name string, _ *ast.FuncDecl) { tests[name] = true })
	}
	for _, problem := range boundingInventoryProblems(files, heldElsewhere, reportTitleExceptions, tests) {
		t.Error(problem)
	}
}

func boundingInventoryProblems(files map[string]*ast.File, sites []heldElsewhereSite,
	exceptions []reportTitleException, tests map[string]bool) []string {
	held := map[string]heldElsewhereSite{}
	for _, site := range sites {
		held[site.file+" "+site.fn] = site
	}
	excused := map[string]reportTitleException{}
	var problems []string
	for _, e := range exceptions {
		key := e.file + " " + e.fn
		if _, both := held[key]; both {
			problems = append(problems, fmt.Sprintf("%s is in heldElsewhere and in reportTitleExceptions at once", key))
		}
		excused[key] = e
	}
	bounds := map[string]int{}
	for name, f := range files {
		forEachCall(f, func(scope string, call *ast.CallExpr) {
			if boundingCall(call.Fun) {
				bounds[name+" "+scope]++
			}
		})
	}
	for key, made := range bounds {
		site, isHeld := held[key]
		e, isExcused := excused[key]
		switch {
		case isHeld && site.calls != made:
			problems = append(problems, fmt.Sprintf("%s makes %d bounding call(s) - cli.ReportTitle or cli.Foreign - and "+
				"heldElsewhere accounts for %d; say what the new one bounds, or take the gone one out",
				key, made, site.calls))
		case isHeld && !tests[site.holder]:
			problems = append(problems, fmt.Sprintf("heldElsewhere leaves %s to %s, and there is no such test",
				key, site.holder))
		case isExcused && e.calls != made:
			problems = append(problems, fmt.Sprintf("%s makes %d bounding call(s) - cli.ReportTitle or cli.Foreign - and "+
				"reportTitleExceptions accounts for %d", key, made, e.calls))
		case !isHeld && !isExcused:
			problems = append(problems, fmt.Sprintf("%s makes %d bounding call(s) - cli.ReportTitle or cli.Foreign - and "+
				"nothing accounts for it; add a row of heldElsewhere saying what it bounds and which test "+
				"measures the line, or say in reportTitleExceptions what it is instead", key, made))
		}
	}
	for key := range held {
		if bounds[key] == 0 {
			problems = append(problems, fmt.Sprintf("heldElsewhere accounts for %s, and nothing there calls "+
				"cli.ReportTitle or cli.Foreign any more", key))
		}
	}
	for key := range excused {
		if bounds[key] == 0 {
			problems = append(problems, fmt.Sprintf("reportTitleExceptions excuses %s, and nothing there calls "+
				"cli.ReportTitle or cli.Foreign any more", key))
		}
	}
	return problems
}

func TestForEachCallWalksPackageInitializersOnce(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", `package fixture
var direct = cli.Foreign("direct", 20)
var literal = func() string { return cli.Foreign("literal", 20) }
var handler = makeHandler(func() string { return cli.ReportTitle("nested", 20) })
func ordinary() {
	cli.Foreign("ordinary", 20)
	_ = func() string { return cli.ReportTitle("inside ordinary", 20) }
}
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	forEachCall(f, func(scope string, call *ast.CallExpr) {
		if boundingCall(call.Fun) {
			got[scope]++
		}
	})
	want := map[string]int{"var:direct": 1, "var:literal": 1, "var:handler": 1, "ordinary": 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bounding call scopes = %v, want %v", got, want)
	}
}

func TestBoundingInventoryRejectsUnlistedAndStaleScopes(t *testing.T) {
	parse := func(t *testing.T, src string) map[string]*ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]*ast.File{"fixture.go": f}
	}
	for _, c := range []struct {
		name  string
		src   string
		scope string
	}{
		{
			name:  "direct package initializer",
			src:   `package fixture; var direct = cli.Foreign("x", 20)`,
			scope: "var:direct",
		},
		{
			name:  "literal inside package initializer",
			src:   `package fixture; var handler = makeHandler(func() string { return cli.ReportTitle("x", 20) })`,
			scope: "var:handler",
		},
		{
			name:  "top-level function literal",
			src:   `package fixture; var literal = func() string { return cli.Foreign("x", 20) }`,
			scope: "var:literal",
		},
		{
			name:  "ordinary function",
			src:   `package fixture; func ordinary() { cli.Foreign("x", 20) }`,
			scope: "ordinary",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			problems := boundingInventoryProblems(parse(t, c.src), nil, nil, nil)
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, "fixture.go "+c.scope+" makes 1 bounding call") ||
				!strings.Contains(joined, "nothing accounts for it") {
				t.Errorf("problems = %v, want the unlisted %s call rejected", problems, c.scope)
			}
		})
	}

	t.Run("nested initializer is not counted twice", func(t *testing.T) {
		files := parse(t, `package fixture; var handler = makeHandler(func() string { return cli.ReportTitle("x", 20) })`)
		sites := []heldElsewhereSite{{file: "fixture.go", fn: "var:handler", calls: 1, holder: "TestHolder"}}
		if problems := boundingInventoryProblems(files, sites, nil, map[string]bool{"TestHolder": true}); len(problems) != 0 {
			t.Errorf("one nested initializer call produced problems: %v", problems)
		}
	})

	t.Run("removed call leaves a stale row", func(t *testing.T) {
		sites := []heldElsewhereSite{{file: "fixture.go", fn: "ordinary", calls: 1, holder: "TestHolder"}}
		problems := boundingInventoryProblems(parse(t, `package fixture; func ordinary() {}`),
			sites, nil, map[string]bool{"TestHolder": true})
		joined := strings.Join(problems, "\n")
		if !strings.Contains(joined, "heldElsewhere accounts for fixture.go ordinary") ||
			!strings.Contains(joined, "nothing there calls") {
			t.Errorf("problems = %v, want the stale ordinary row rejected", problems)
		}
	})
}

func parsePackage(t *testing.T, fset *token.FileSet, tests bool) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") != tests {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("parsed no files in the package directory; tests=%v", tests)
	}
	return out
}

func forEachFunc(f *ast.File, visit func(name string, decl *ast.FuncDecl)) {
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			visit(fn.Name.Name, fn)
		}
	}
}

func forEachCall(f *ast.File, visit func(scope string, call *ast.CallExpr)) {
	walk := func(scope string, n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 0 {
				visit(scope, call)
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			walk(d.Name.Name, d)
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, raw := range d.Specs {
				spec, ok := raw.(*ast.ValueSpec)
				if !ok || len(spec.Names) == 0 {
					continue
				}
				allNames := make([]string, len(spec.Names))
				for i, name := range spec.Names {
					allNames[i] = name.Name
				}
				for i, value := range spec.Values {
					name := strings.Join(allNames, ",")
					if len(spec.Names) == len(spec.Values) {
						name = spec.Names[i].Name
					}
					walk("var:"+name, value)
				}
			}
		}
	}
}

func forEachString(n ast.Node, visit func(lit *ast.BasicLit, text string)) {
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if text, err := strconv.Unquote(lit.Value); err == nil {
			visit(lit, text)
		}
		return true
	})
}

func boundingCall(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "cli" {
		return false
	}
	return sel.Sel.Name == "ReportTitle" || sel.Sel.Name == "Foreign"
}

func calleeName(e ast.Expr) string {
	switch fn := e.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func literalText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if text, err := strconv.Unquote(lit.Value); err == nil {
				b.WriteString(text)
			}
		}
		return true
	})
	return b.String()
}

func TestAddNamesTheListItAddedToInsideTheWidth(t *testing.T) {
	isolate(t)

	list := strings.Repeat("Р", 300)
	seedProjects(t,
		model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"},
		model.Project{Id: "p2", Name: list, Kind: "TASK"})

	var stdout, stderr bytes.Buffer
	args := []string{"add", strings.Repeat("Забрать посылку ", 20), addFlagProject, list}
	if got := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); got != exitOK {
		t.Fatalf("run(add -P <a list named at 300 letters>) = %d, want %d (stderr: %s)",
			got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added ") {
		t.Errorf("stdout = %q, want the line saying what was added and where", stdout.String())
	}
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("the line naming the list is %d columns wide:\n%q", n, line)
		}
	}
}
