package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
)

func TestConfigNamesTheOptionItDidNotUnderstand(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"beside the option it takes", []string{"config", "--init", "--bogus"}, `tt: config: unknown option "--bogus"`},
		{"before the option it takes", []string{"config", "--bogus", "--init"}, `tt: config: unknown option "--bogus"`},
		{"on its own", []string{"config", "--bogus"}, `tt: config: unknown option "--bogus"`},
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
		})
	}
}

func TestConfigTakesInitTwice(t *testing.T) {
	isolate(t)
	var stdout, stderr bytes.Buffer
	args := []string{"config", "--init", "--init"}
	if got := run(context.Background(), args, nil, &stdout, &stderr); got != exitOK {
		t.Fatalf("run(%v) = %d, want %d (stderr: %s)", args, got, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "wrote ") {
		t.Errorf("stdout = %q, want the config file to have been written", stdout.String())
	}
}

func TestConfigEscapesWhatItDidNotWrite(t *testing.T) {

	hostileHome := "\x1b[32m\u202e\n" + strings.Repeat("q", 200)
	assertNothingRaw := func(t *testing.T, line string) {
		t.Helper()
		if strings.ContainsAny(line, "\x1b\n\u202e") {
			t.Errorf("the line carries somebody else's bytes as they stand: %q", line)
		}
		if !strings.Contains(line, `\x1b`) {
			t.Errorf("line = %q, want the escape sequence written out rather than dropped", line)
		}
	}

	t.Run("the parser's sentence about a line of the file", func(t *testing.T) {
		isolate(t)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), hostileHome))
		key := "\"\\u202E\\u001B[31m" + strings.Repeat("z", 200) + "\""
		path := writeConfig(t, key+" = 1\n"+key+" = 2\n")

		var stdout, stderr bytes.Buffer
		if got := run(context.Background(), []string{"config"}, nil, &stdout, &stderr); got != exitError {
			t.Fatalf("run(config) = %d, want %d (stderr: %q)", got, exitError, stderr.String())
		}
		lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")

		if len(lines) != 2 {
			t.Fatalf("stderr = %q, want the head and one problem under it", stderr.String())
		}
		head := lines[0]
		assertNothingRaw(t, head)
		marker := "\": 1 problem(s):"
		end := strings.Index(head, marker)
		if end < 0 {
			t.Fatalf("config error head = %q, want one quoted path and problem count", head)
		}
		gotPath, err := strconv.Unquote(head[:end+1])
		if err != nil || gotPath != path {
			t.Fatalf("config error path recovered %q, %v; want exact path %q", gotPath, err, path)
		}
		problem := lines[1]
		assertNothingRaw(t, problem)
		if n := utf8.RuneCountInString(problem); n > cli.Width {
			t.Errorf("the problem is %d columns wide, want at most %d:\n%q", n, cli.Width, problem)
		}
		if !strings.HasPrefix(problem, "  line 2: ") {
			t.Errorf("problem = %q, want tt's own line number in front of it", problem)
		}
	})

	t.Run("the error of a read that reached no line", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file at mode 000 anyway")
		}
		isolate(t)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), hostileHome))
		path := writeConfig(t, "default_project = 'Work'\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o600) })

		var stdout, stderr bytes.Buffer
		if got := run(context.Background(), []string{"config"}, nil, &stdout, &stderr); got != exitError {
			t.Fatalf("run(config) = %d, want %d (stderr: %q)", got, exitError, stderr.String())
		}
		lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("stderr = %q, want one line", stderr.String())
		}
		assertNothingRaw(t, lines[0])

		if !strings.Contains(lines[0], "permission denied") {
			t.Errorf("stderr = %q, want the reason, which is what a cut takes first", lines[0])
		}
	})
}

func TestEffectiveConfigHeaderKeepsAHostilePathAndTheOutputRoundTrips(t *testing.T) {
	for _, found := range []bool{false, true} {
		name := "missing"
		if found {
			name = "found"
		}
		t.Run(name, func(t *testing.T) {
			isolate(t)
			hostileHome := filepath.Join(t.TempDir(), "  config\u00a0 home\n\x1b\u202e  ")
			t.Setenv("XDG_CONFIG_HOME", hostileHome)
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			want := config.Default()
			if found {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				const src = "default_project = \"Работа \\u001B\\u0085\\u202E\"\ncolor = 'always'\n[timer]\non_end = \"say \\\"done\\\" \\\\ \\u001B\\u009B\\u2066\"\n[sync]\nmove_by_recreate = 'never'\n"
				if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
					t.Fatal(err)
				}
				want, err = config.LoadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := printEffectiveConfig(&stdout, &stderr); code != exitOK {
				t.Fatalf("printEffectiveConfig = %d, stderr %q", code, stderr.String())
			}
			printed := stdout.String()
			lineEnd := strings.IndexByte(printed, '\n')
			if lineEnd < 0 {
				t.Fatalf("effective config has no header line: %q", printed)
			}
			header := printed[:lineEnd]
			prefix := "# config file: "
			suffix := " (not found, using defaults)"
			if found {
				suffix = " (found)"
			}
			if !strings.HasPrefix(header, prefix) || !strings.HasSuffix(header, suffix) {
				t.Fatalf("header = %q, want the %s classification", header, name)
			}
			gotPath, err := strconv.Unquote(header[len(prefix) : len(header)-len(suffix)])
			if err != nil {
				t.Fatalf("header path is not one quoted field: %v", err)
			}
			if gotPath != path {
				t.Fatalf("header path recovered %q, want exact path %q", gotPath, path)
			}
			if strings.ContainsAny(header, "\x1b\u202e") {
				t.Fatalf("header retained a raw terminal control: %q", header)
			}
			for _, r := range printed {
				if r < 0x20 && r != '\n' || r >= 0x7f && r <= 0x9f {
					t.Fatalf("effective config contains raw control U+%04X: %q", r, printed)
				}
				switch r {
				case 0x061c, 0x200e, 0x200f,
					0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
					0x2066, 0x2067, 0x2068, 0x2069:
					t.Fatalf("effective config contains raw directional control U+%04X: %q", r, printed)
				}
			}

			decodedPath := filepath.Join(t.TempDir(), "effective.toml")
			if err := os.WriteFile(decodedPath, stdout.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := config.LoadFile(decodedPath)
			if err != nil {
				t.Fatalf("printed effective config is not valid TOML: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded effective config = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConfigPathReasonRemovesOnlyTheAlreadyPrintedExactPath(t *testing.T) {
	path := "/config/a  b/settings.toml"
	other := "/config/other  place/settings.toml"
	reason := errors.New("  too many\u00a0 links  ")
	nested := &fs.PathError{Op: "stat", Path: other, Err: reason}
	cases := map[string]struct {
		err  error
		want string
	}{
		"generic":          {err: reason, want: reason.Error()},
		"matching":         {err: &fs.PathError{Op: "stat", Path: path, Err: reason}, want: reason.Error()},
		"wrapped matching": {err: fmt.Errorf("outer: %w", &fs.PathError{Op: "stat", Path: path, Err: reason}), want: reason.Error()},
		"unmatched":        {err: &fs.PathError{Op: "stat", Path: other, Err: reason}, want: (&fs.PathError{Op: "stat", Path: other, Err: reason}).Error()},
		"nested":           {err: &fs.PathError{Op: "stat", Path: path, Err: nested}, want: nested.Error()},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := strconv.Unquote(configPathReason(path, c.err))
			if err != nil {
				t.Fatalf("reason is not one quoted field: %v", err)
			}
			if got != c.want {
				t.Fatalf("reason recovered %q, want %q", got, c.want)
			}
		})
	}
}

func TestConfigStatReportsAnAncestorLoopWithOnePath(t *testing.T) {
	for _, init := range []bool{false, true} {
		name := "effective"
		if init {
			name = "init"
		}
		t.Run(name, func(t *testing.T) {
			isolate(t)
			root := t.TempDir()
			loop := filepath.Join(root, "loop  here")
			if err := os.Symlink(filepath.Base(loop), loop); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", loop)
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			var syscallErr error
			if init {
				_, syscallErr = os.Lstat(path)
			} else {
				_, syscallErr = os.Stat(path)
			}
			var pathErr *fs.PathError
			if !errors.As(syscallErr, &pathErr) || pathErr.Path != path {
				t.Fatalf("fixture did not produce the expected exact PathError: %v", syscallErr)
			}

			var stdout, stderr bytes.Buffer
			var code int
			if init {
				code = initConfig(context.Background(), &stdout, &stderr)
			} else {
				code = printEffectiveConfig(&stdout, &stderr)
			}
			if code != exitError || stdout.Len() != 0 {
				t.Fatalf("config command = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
			line := stderr.String()
			if len(line) == 0 || line[len(line)-1] != '\n' {
				t.Fatalf("stat diagnostic is not one finished line: %q", line)
			}
			line = line[:len(line)-1]
			const prefix = "tt: stat "
			if !strings.HasPrefix(line, prefix) {
				t.Fatalf("stat diagnostic changed prefix: %q", line)
			}
			pathToken, reasonToken, ok := strings.Cut(line[len(prefix):], ": ")
			if !ok {
				t.Fatalf("stat diagnostic has no path/reason boundary: %q", line)
			}
			gotPath, err := strconv.Unquote(pathToken)
			if err != nil || gotPath != path {
				t.Fatalf("path recovered %q, %v; want %q", gotPath, err, path)
			}
			gotReason, err := strconv.Unquote(reasonToken)
			if err != nil || gotReason != pathErr.Err.Error() {
				t.Fatalf("reason recovered %q, %v; want %q", gotReason, err, pathErr.Err.Error())
			}
		})
	}
}

func TestPrintConfigErrorDoesNotReuseAnUncertainSettingLine(t *testing.T) {
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
			path := writeConfig(t, tc.src)
			_, loadErr := config.LoadFile(path)
			if loadErr == nil {
				t.Fatal("fixture did not produce a config error")
			}
			var out bytes.Buffer
			printConfigError(&out, loadErr)
			printed := out.String()
			for _, want := range []string{tc.unknown, tc.invalid} {
				if !strings.Contains(printed, want) {
					t.Fatalf("renderer omitted the associated problem %q: %q", want, printed)
				}
			}
			if tc.forbiddenLine > 0 {
				if bad := fmt.Sprintf("line %d:", tc.forbiddenLine); strings.Contains(printed, bad) {
					t.Fatalf("renderer assigned a problem to unrelated %s %q", bad, printed)
				}
			}
		})
	}
}

func TestInitConfigKeepsEverySelectedOutputAtomInert(t *testing.T) {
	for _, name := range []string{"already exists", "directory failure", "write failure", "success"} {
		t.Run(name, func(t *testing.T) {
			if name == "write failure" && os.Geteuid() == 0 {
				t.Skip("root can create a temporary file in a read-only directory")
			}
			isolate(t)
			base := filepath.Join(t.TempDir(), "config  \x1b\u202e\u2003")
			t.Setenv("XDG_CONFIG_HOME", base)
			path, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "already exists":
				writeConfig(t, "")
			case "directory failure":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), base); err != nil {
					t.Fatal(err)
				}
			case "write failure":
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(filepath.Dir(path), 0o700) })
			}

			var stdout, stderr bytes.Buffer
			code := initConfig(context.Background(), &stdout, &stderr)
			printed := stdout.String() + stderr.String()
			if strings.Contains(printed, "\x1b") || strings.Contains(printed, "\u202e") || strings.Contains(printed, "\u2003") {
				t.Fatalf("selected config atom reached the terminal active: %q", printed)
			}
			switch name {
			case "already exists":
				if code != exitError {
					t.Fatalf("exit = %d, want %d", code, exitError)
				}
				line := strings.TrimSuffix(stderr.String(), "\n")
				const prefix = "tt: config file already exists: "
				got, err := strconv.Unquote(strings.TrimPrefix(line, prefix))
				if err != nil || got != path {
					t.Fatalf("existing path recovered %q, %v; want %q", got, err, path)
				}
			case "directory failure":
				if code != exitError {
					t.Fatalf("exit = %d, want %d", code, exitError)
				}
				line := strings.TrimSuffix(stderr.String(), "\n")
				got, err := strconv.Unquote(strings.TrimPrefix(line, "tt: "))
				if err != nil {
					t.Fatalf("failure is not one quoted reason: %v in %q", err, line)
				}
				wantErr := os.MkdirAll(filepath.Dir(path), 0o700)
				if wantErr == nil || got != wantErr.Error() {
					t.Fatalf("directory failure recovered %q, want exact independent reason %v", got, wantErr)
				}
			case "write failure":
				if code != exitError {
					t.Fatalf("exit = %d, want %d", code, exitError)
				}
				line := strings.TrimSuffix(stderr.String(), "\n")
				got, err := strconv.Unquote(strings.TrimPrefix(line, "tt: "))
				if err != nil {
					t.Fatalf("failure is not one quoted reason: %v in %q", err, line)
				}
				dir := filepath.Dir(path)
				prefix := "create a temporary file in " + dir + ": open "
				const suffix = ": permission denied"
				if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
					t.Fatalf("write failure recovered %q, want complete create/open refusal", got)
				}
				tempPath := strings.TrimSuffix(strings.TrimPrefix(got, prefix), suffix)
				tempPrefix := filepath.Base(path) + ".tmp"
				tempBase := filepath.Base(tempPath)
				tempName := strings.TrimPrefix(tempBase, tempPrefix)
				if filepath.Dir(tempPath) != dir || !strings.HasPrefix(tempBase, tempPrefix) ||
					tempName == "" || strings.Trim(tempName, "0123456789") != "" {
					t.Fatalf("dynamic temporary path = %q, want %s/config.toml.tmp followed by digits", tempPath, dir)
				}
				want := "create a temporary file in " + dir + ": " + (&fs.PathError{
					Op: "open", Path: tempPath, Err: fs.ErrPermission,
				}).Error()
				if got != want {
					t.Fatalf("write failure recovered %q, want exact reason %q", got, want)
				}
			case "success":
				if code != exitOK {
					t.Fatalf("exit = %d, want %d: %q", code, exitOK, stderr.String())
				}
				line := strings.TrimSuffix(stdout.String(), "\n")
				got, err := strconv.Unquote(strings.TrimPrefix(line, "wrote "))
				if err != nil || got != path {
					t.Fatalf("written path recovered %q, %v; want %q", got, err, path)
				}
			}
		})
	}
}

func TestSharedConfigErrorConsumersPreserveOneExactEscapingPass(t *testing.T) {
	cases := map[string]func(context.Context, *bytes.Buffer, *bytes.Buffer) int{
		"config": func(ctx context.Context, stdout, stderr *bytes.Buffer) int {
			return run(ctx, []string{"config"}, nil, stdout, stderr)
		},
		"notify test": func(ctx context.Context, stdout, stderr *bytes.Buffer) int {
			return run(ctx, []string{"notify", "test"}, nil, stdout, stderr)
		},
		"doctor --fix": func(ctx context.Context, stdout, stderr *bytes.Buffer) int {
			return doctorFix(ctx, strings.NewReader("n\n"), stdout, stderr)
		},
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			base := filepath.Join(t.TempDir(), "config  \x1b\u202e\u2003")
			t.Setenv("XDG_CONFIG_HOME", base)
			key := "\"\\u001B\""
			path := writeConfig(t, key+" = 1\n"+key+" = 2\n")
			_, loadErr := config.LoadFile(path)
			var cerr *config.Error
			if !errors.As(loadErr, &cerr) || len(cerr.Problems) != 1 {
				t.Fatalf("fixture error = %#v, want one config problem", loadErr)
			}

			var stdout, stderr bytes.Buffer
			if code := command(context.Background(), &stdout, &stderr); code != exitError {
				t.Fatalf("%s exit = %d, want %d", name, code, exitError)
			}
			printed := stderr.String()
			if strings.Contains(printed, "\x1b") || strings.Contains(printed, "\u202e") || strings.Contains(printed, "\u2003") {
				t.Fatalf("shared config error reached the terminal active: %q", printed)
			}
			lines := strings.Split(strings.TrimSuffix(printed, "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("shared config error = %q, want one head and one problem", printed)
			}
			marker := "\": 1 problem(s):"
			end := strings.Index(lines[0], marker)
			if end < 0 {
				t.Fatalf("config error head = %q, want one quoted path", lines[0])
			}
			wantPath := path
			if name == "doctor --fix" {
				resolved, resolveErr := resolveConfigPath(path)
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				wantPath = resolved
			}
			gotPath, err := strconv.Unquote(lines[0][:end+1])
			if err != nil || gotPath != wantPath {
				t.Fatalf("%s config path recovered %q, %v; want %q", name, gotPath, err, wantPath)
			}
			lead := fmt.Sprintf("  line %d: ", cerr.Problems[0].Line)
			if !strings.HasPrefix(lines[1], lead) {
				t.Fatalf("problem line = %q, want lead %q", lines[1], lead)
			}
			gotProblem, err := strconv.Unquote(strings.TrimPrefix(lines[1], lead))
			if err != nil || gotProblem != cerr.Problems[0].Msg {
				t.Fatalf("%s problem recovered %q, %v; want exact raw problem %q", name, gotProblem, err, cerr.Problems[0].Msg)
			}
		})
	}
}
