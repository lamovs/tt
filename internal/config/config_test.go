package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if c.DefaultProject != "Личное" || c.EditorHints != model.HintsFull {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.Timer.Focus.String() != "25m" || c.Timer.ShortBreak.String() != "5m" || c.Timer.LongBreak.String() != "15m" {
		t.Errorf("unexpected timer defaults: %+v", c.Timer)
	}
	if c.Timer.LongEvery != 4 || !c.Timer.AutoBreak || c.Timer.AutoFocus {
		t.Errorf("unexpected timer defaults: %+v", c.Timer)
	}
	if !c.Timer.WarnBefore.IsOff() || c.Timer.UploadAborted || c.FocusUpload.Enabled {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.Sync.Interval.String() != "1m" {
		t.Errorf("sync interval = %q", c.Sync.Interval)
	}
}

func TestPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path()
	if err != nil || got != "/xdg/tt/config.toml" {
		t.Fatalf("Path() = %q, %v", got, err)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err = Path()
	if err != nil || got != filepath.Join(home, ".config", "tt", "config.toml") {
		t.Fatalf("Path() = %q, %v", got, err)
	}

	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got, _ = Path(); !strings.HasPrefix(got, home) {
		t.Fatalf("Path() = %q, want the home fallback", got)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("got %+v, want the defaults", got)
	}
}

func TestLoadFileMissingIsAnError(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("an explicitly named file must not be optional")
	}
}

func TestParseBytesUsesTheCapturedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[timer]\non_end = 'from disk'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ParseBytes(path, []byte("[timer]\non_end = 'captured'\n"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got.Timer.OnEnd != "captured" {
		t.Errorf("timer.on_end = %q, want the value from src rather than a second read of path", got.Timer.OnEnd)
	}
}

func TestLoadFullFile(t *testing.T) {
	src := `
default_project = "Работа"
editor_hints = "short"
color = "never"

[timer]
focus = "50m"
short_break = "10m"
long_break = "1h"
long_every = 3
auto_break = false
auto_focus = true
warn_before = "30s"
on_end = "notify-send {kind} {task}"
on_break_end = "say done"
upload_aborted = true

[sync]
interval = "5m"
move_by_recreate = "always"

[focus_upload]
enabled = true
`
	path := write(t, src)
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	want := Config{
		DefaultProject: "Работа",
		EditorHints:    model.HintsShort,
		Color:          ColorNever,
		Timer: Timer{
			Focus:         model.Duration(50 * time.Minute),
			ShortBreak:    model.Duration(10 * time.Minute),
			LongBreak:     model.Duration(time.Hour),
			LongEvery:     3,
			AutoBreak:     false,
			AutoFocus:     true,
			WarnBefore:    model.Duration(30 * time.Second),
			OnEnd:         "notify-send {kind} {task}",
			OnBreakEnd:    "say done",
			UploadAborted: true,
			Indicator:     true,
		},
		Sync:        Sync{Interval: model.Duration(5 * time.Minute), MoveByRecreate: MoveAlways},
		FocusUpload: FocusUpload{Enabled: true},
		AI:          defaultAI(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	got, err := LoadFile(write(t, "[timer]\nfocus = \"30m\"\n"))
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	want := Default()
	want.Timer.Focus = model.Duration(30 * time.Minute)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestUnknownKey(t *testing.T) {
	path := write(t, "default_project = \"x\"\neditor_hint = \"full\"\n")
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("an unknown key must be an error")
	}
	want := path + `:2: unknown key "editor_hint" (did you mean "editor_hints"?)`
	if err.Error() != want {
		t.Errorf("got %q\nwant %q", err, want)
	}
}

func TestUnknownNestedKey(t *testing.T) {
	_, err := LoadFile(write(t, "[timer]\nfocu = \"25m\"\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), `unknown key "timer.focu" (did you mean "timer.focus"?)`) {
		t.Errorf("got %q", err)
	}
	if !strings.Contains(err.Error(), ":2:") {
		t.Errorf("the line is missing: %q", err)
	}
}

func TestUnknownTable(t *testing.T) {
	_, err := LoadFile(write(t, "[timr]\nfocus = \"25m\"\nlong_every = 2\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("an unknown table must be reported once, got %q", err)
	}
	if !strings.Contains(err.Error(), `unknown table "timr" (did you mean "timer"?)`) {
		t.Errorf("got %q", err)
	}
}

func TestUnknownKeyWithoutSuggestion(t *testing.T) {
	_, err := LoadFile(write(t, "whatever = 1\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), `unknown key "whatever"`) || strings.Contains(err.Error(), "did you mean") {
		t.Errorf("got %q", err)
	}
}

func TestAllProblemsAtOnce(t *testing.T) {
	src := `default_project = ""
editor_hints = "ful"

[timer]
focus = "0"
long_every = 20
on_end = "run {tsk}"
short_brek = "5m"

[sync]
interval = "1min"
`
	_, err := LoadFile(write(t, src))
	if err == nil {
		t.Fatal("want error")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 7 {
		t.Fatalf("want 7 problems, got %d:\n%s", len(lines), err)
	}
	wants := []string{
		`:1: default_project: must not be empty`,
		`:2: editor_hints: unknown editor hints "ful" (allowed: full, short, none)`,
		`:5: timer.focus: must be greater than 0`,
		`:6: timer.long_every: must be between 1 and 12, got 20`,
		`:7: timer.on_end: unknown placeholder "{tsk}" (allowed: {kind}, {note}, {task}, {project}, {duration}, {cycle})`,
		`:8: unknown key "timer.short_brek" (did you mean "timer.short_break"?)`,
		`:11: sync.interval: invalid duration "1min" (want "25m", "1h30m", "90s" or "0")`,
	}
	for i, want := range wants {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("problem %d:\n got %q\nwant suffix %q", i+1, lines[i], want)
		}
	}
}

func TestProblemsCarryThePath(t *testing.T) {
	path := write(t, "editor_hints = \"nope\"\n")
	_, err := LoadFile(path)
	if err == nil || !strings.HasPrefix(err.Error(), path+":1: ") {
		t.Fatalf("got %v", err)
	}
}

func TestBrokenSyntax(t *testing.T) {
	_, err := LoadFile(write(t, "default_project = \"x\"\neditor_hints = \n"))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), ":2:") {
		t.Errorf("a syntax error must carry its line: %q", err)
	}
}

func TestWrongType(t *testing.T) {
	_, err := LoadFile(write(t, "[timer]\nlong_every = \"4\"\n"))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "timer.long_every") {
		t.Errorf("got %q", err)
	}
}

func TestZeroBreaksRejected(t *testing.T) {
	for _, key := range []string{"focus", "short_break", "long_break"} {
		_, err := LoadFile(write(t, "[timer]\n"+key+" = \"0\"\n"))
		if err == nil || !strings.Contains(err.Error(), "must be greater than 0") {
			t.Errorf("%s = 0: got %v", key, err)
		}
	}
	got, err := LoadFile(write(t, "[timer]\nwarn_before = \"0\"\n"))
	if err != nil || !got.Timer.WarnBefore.IsOff() {
		t.Errorf("warn_before = 0 must be allowed: %v", err)
	}
}

func TestPlaceholders(t *testing.T) {
	src := "[timer]\non_end = \"say {kind} {note} {task} {project} {duration} {cycle}\"\non_break_end = \"say {stop}\"\n"
	_, err := LoadFile(write(t, src))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("only on_break_end is wrong, got %q", err)
	}
	if !strings.Contains(err.Error(), `timer.on_break_end: unknown placeholder "{stop}"`) {
		t.Errorf("got %q", err)
	}
	if want := strings.Join(Placeholders(), ","); want != "kind,note,task,project,duration,cycle" {
		t.Errorf("Placeholders() = %v", Placeholders())
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	src, err := Default().Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse("memory.toml", src)
	if err != nil {
		t.Fatalf("the encoded config must load back: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("got %+v\nwant %+v", got, Default())
	}
	if !strings.Contains(string(src), `focus = '25m'`) {
		t.Errorf("durations must be encoded canonically:\n%s", src)
	}
	if want := "default_project = '" + Default().DefaultProject + "'"; !strings.Contains(string(src), want) {
		t.Errorf("a non-ASCII default must survive encoding as %s:\n%s", want, src)
	}
}

func TestEncodeRoundTripsEveryStringFieldFamily(t *testing.T) {
	cfg := Config{
		DefaultProject: "Работа wide 漢字",
		EditorHints:    model.HintsShort,
		Color:          ColorNever,
		Timer: Timer{
			Focus:         model.Duration(50 * time.Minute),
			ShortBreak:    model.Duration(10 * time.Minute),
			LongBreak:     model.Duration(time.Hour),
			LongEvery:     3,
			AutoBreak:     false,
			AutoFocus:     true,
			WarnBefore:    model.Duration(30 * time.Second),
			OnEnd:         `notify-send -- "done" 'task' back\slash`,
			OnBreakEnd:    `say "break"`,
			UploadAborted: true,
		},
		Sync:        Sync{Interval: model.Duration(5 * time.Minute), MoveByRecreate: MoveAlways},
		FocusUpload: FocusUpload{Enabled: true},
		AI:          defaultAI(),
	}

	src, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse("memory.toml", src)
	if err != nil {
		t.Fatalf("the encoded config must load back: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("the full config changed in the round trip:\ngot  %+v\nwant %+v", got, cfg)
	}
	for _, key := range []string{
		"default_project", "editor_hints", "color", "focus", "short_break",
		"long_break", "warn_before", "on_end", "on_break_end", "interval",
		"move_by_recreate",
	} {
		if !strings.Contains(string(src), key+" = ") {
			t.Errorf("encoded config is missing string field %q", key)
		}
	}
}

func TestEncodeEscapesTerminalUnsafeValuesAndPreservesIdentity(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"ordinary ASCII", `plain "quotes" and back\slash`},
		{"Cyrillic and wide printable", "Работа 漢字"},
		{"apostrophe", "it's done"},
		{"line break", "first\nsecond"},
		{"ASCII controls and C1", "x\x1b[31m\x7f\u0085\u009b31m"},
		{"directional controls", "x\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069y"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.DefaultProject = "project " + tc.value
			cfg.Timer.OnEnd = "focus " + tc.value
			cfg.Timer.OnBreakEnd = "break " + tc.value

			src, err := cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			assertNoRawUnsafeTOMLValueRunes(t, src)

			got, err := parse("memory.toml", src)
			if err != nil {
				t.Fatalf("the encoded config must remain valid TOML: %v", err)
			}
			if !reflect.DeepEqual(got, cfg) {
				t.Errorf("the terminal-safe round trip changed the config:\ngot  %+v\nwant %+v", got, cfg)
			}
		})
	}
}

func assertNoRawUnsafeTOMLValueRunes(t *testing.T, src []byte) {
	t.Helper()
	for _, r := range string(src) {
		if r < 0x20 && r != '\n' || r >= 0x7f && r <= 0x9f {
			t.Fatalf("encoded TOML contains raw control U+%04X", r)
		}
		switch r {
		case 0x061c, 0x200e, 0x200f,
			0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
			0x2066, 0x2067, 0x2068, 0x2069:
			t.Fatalf("encoded TOML contains raw directional control U+%04X", r)
		}
	}
}

func TestQuoteTOMLString(t *testing.T) {
	cases := map[string]string{
		`plain`:        `'plain'`,
		`has "quotes"`: `'has "quotes"'`,
		`back\slash`:   `'back\slash'`,
		``:             `''`,

		`it's`:              `"it's"`,
		"tab\tand\nnewline": `"tab\tand\nnewline"`,
		"bell\a":            `"bell\u0007"`,
	}
	for in, want := range cases {
		if got := QuoteTOMLString(in); got != want {
			t.Errorf("QuoteTOMLString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeQuotesLikeQuoteTOMLString(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"quotes and backslashes", `osascript -e "say \"$TT_TASK\""`, `on_end = 'osascript -e "say \"$TT_TASK\""'`},
		{"lone backslash", `back\slash`, `on_end = 'back\slash'`},
		{"empty", "", "on_end = ''"},
		{"single quote falls back", "echo it's done", `on_end = "echo it's done"`},
		{"tab falls back", "echo a\tb", `on_end = "echo a\tb"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			cfg.Timer.OnEnd = c.value
			src, err := cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(src), c.want) {
				t.Errorf("want a %q line in:\n%s", c.want, src)
			}
			got, err := parse("memory.toml", src)
			if err != nil {
				t.Fatalf("the encoded config must load back: %v\n%s", err, src)
			}
			if !reflect.DeepEqual(got, cfg) {
				t.Errorf("the round trip changed the config:\ngot  %+v\nwant %+v", got, cfg)
			}
		})
	}
}

func TestTemplateReadsLikeWhatTtPrints(t *testing.T) {
	printed, err := Default().Encode()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(Template(), "\n")
	checked := 0
	for _, printedLine := range strings.Split(string(printed), "\n") {
		setting := strings.TrimSpace(printedLine)
		if setting == "" {
			continue
		}
		checked++
		found := false
		for _, l := range lines {
			if strings.HasPrefix(l, "# "+setting) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the template does not show %q the way tt config prints it", setting)
		}
	}
	if checked < 15 {
		t.Fatalf("only %d settings were compared; the encoder printed almost nothing", checked)
	}
}

func TestTemplateShowsTheReadyCommands(t *testing.T) {
	for _, command := range []string{NotifyMacOS, NotifyLinux} {
		line := "on_end = " + QuoteTOMLString(command)
		if !strings.Contains(Template(), line) {
			t.Errorf("the template does not show %s", line)
		}
		got, err := parse("example.toml", []byte("[timer]\n"+line+"\n"))
		if err != nil {
			t.Errorf("the example does not load: %v", err)
			continue
		}
		if got.Timer.OnEnd != command {
			t.Errorf("Timer.OnEnd = %q, want %q", got.Timer.OnEnd, command)
		}
	}
}

func TestNotifyMacOSUsesHelperApp(t *testing.T) {
	for _, want := range []string{"tt-notify --", `"$TT_KIND" "$TT_NOTE" "$TT_TASK" "$TT_DURATION"`} {
		if !strings.Contains(NotifyMacOS, want) {
			t.Errorf("NotifyMacOS = %q, want %q", NotifyMacOS, want)
		}
	}
}

func TestOpensTimerTable(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"plain header":        {"[timer]\nfocus = '25m'\n", true},
		"spaces inside":       {"[ timer ]\nfocus = '25m'\n", true},
		"tabs inside":         {"[\ttimer\t]\nfocus = '25m'\n", true},
		"basic quoted key":    {"[\"timer\"]\nfocus = '25m'\n", true},
		"literal quoted key":  {"['timer']\nfocus = '25m'\n", true},
		"trailing comment":    {"[timer] # session lengths\nfocus = '25m'\n", true},
		"no keys of its own":  {"[timer]\n", true},
		"after another table": {"[sync]\ninterval = '1m'\n\n[timer]\n", true},
		"commented out":       {"# [timer]\n", false},
		"inside a value":      {"default_project = '[timer]'\n", false},
		"inside a multiline":  {"default_project = \"\"\"\n[timer]\n\"\"\"\n", false},

		"dotted key only": {"timer.focus = '25m'\n", false},

		"an array of tables": {"[[timer]]\nx = 1\n", false},
		"no timer at all":    {"default_project = 'Work'\n", false},
		"empty file":         {"", false},
		"not even TOML":      {"[timer\n", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := OpensTimerTable(write(t, c.src)); got != c.want {
				t.Errorf("OpensTimerTable(%q) = %v, want %v", c.src, got, c.want)
			}
			if got := OpensTimerTableIn([]byte(c.src)); got != c.want {
				t.Errorf("OpensTimerTableIn(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}

	if OpensTimerTable(filepath.Join(t.TempDir(), "not-there.toml")) {
		t.Error("a file that is not there opens no table")
	}
}

func TestIsTimerHeader(t *testing.T) {
	cases := map[string]bool{
		"[timer]":                   true,
		"[ timer ]":                 true,
		"[\ttimer\t]":               true,
		`["timer"]`:                 true,
		`['timer']`:                 true,
		"[timer] # session lengths": true,
		"[timer]   ":                true,
		"[sync]":                    false,
		"[timer.foo]":               false,
		"[TIMER]":                   false,
		"timer.focus = '25m'":       false,
		"focus = '25m'":             false,
		"on_end = 'x'":              false,
		"# [timer]":                 false,
		"[timer":                    false,
		"":                          false,
		"   ":                       false,
	}
	for line, want := range cases {
		t.Run(line, func(t *testing.T) {
			if got := IsTimerHeader(line); got != want {
				t.Errorf("IsTimerHeader(%q) = %v, want %v", line, got, want)
			}
		})
	}
}

func TestTemplateMatchesDefaults(t *testing.T) {
	got, err := parse("template.toml", []byte(uncomment(Template())))
	if err != nil {
		t.Fatalf("the template must load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("the template drifted from the defaults:\ngot  %+v\nwant %+v", got, Default())
	}
}

func uncomment(s string) string {
	setting := regexp.MustCompile(`^# (\[|[a-z_]+ = )`)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if setting.MatchString(line) {
			out = append(out, strings.TrimPrefix(line, "# "))
		}
	}
	if len(out) < 15 {
		panic("the template lost its settings")
	}
	return strings.Join(out, "\n")
}

func write(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
