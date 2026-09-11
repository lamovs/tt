package config

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCommandAcceptsShellBraces(t *testing.T) {
	for _, command := range []string{
		`notify-send "${TT_TASK}"`,
		`echo "$TT_TASK" | awk '{print $1}'`,
		`notify-send "{task}" "${TT_PROJECT}"`,
		`osascript -e "on run argv" -e "display notification (item 1 of argv)" -e "end run" "${TT_TASK}"`,
		`notify-send "${task}"`,
		`notify-send "${tsak}"`,
	} {
		src := "[timer]\non_end = " + strconv.Quote(command) + "\n"
		if _, err := parse("shell.toml", []byte(src)); err != nil {
			t.Errorf("on_end = %s was rejected: %v", command, err)
		}
	}
}

func TestCommandStillRejectsAMisspelledPlaceholder(t *testing.T) {
	src := "[timer]\non_end = \"notify-send {tsak}\"\n"
	_, err := parse("typo.toml", []byte(src))
	if err == nil {
		t.Fatal("a misspelled placeholder was accepted")
	}
	if !strings.Contains(err.Error(), `unknown placeholder "{tsak}"`) {
		t.Fatalf("error = %v, want it to name the unknown placeholder", err)
	}
}

func TestPlaceholderPatternMatchesTheAcceptedNames(t *testing.T) {
	re := PlaceholderPattern()
	for _, name := range Placeholders() {
		if got := re.FindString("x {" + name + "} y"); got != "{"+name+"}" {
			t.Errorf("PlaceholderPattern did not match {%s}, found %q", name, got)
		}
	}
	for _, s := range []string{"${TT_TASK}", "{print $1}", "{}", "{TASK}", "{a-b}"} {
		if got := re.FindString(s); got != "" {
			t.Errorf("PlaceholderPattern matched %q in %q", got, s)
		}
	}
}

func TestPlaceholderPatternTakesInALeadingDollar(t *testing.T) {
	got := PlaceholderPattern().FindAllString(`say ${task} x{task}{project}`, -1)
	want := []string{"${task}", "{task}", "{project}"}
	if !slices.Equal(got, want) {
		t.Fatalf("matches = %q, want %q", got, want)
	}
}

func TestPlaceholderName(t *testing.T) {
	for match, want := range map[string]string{
		"{task}":  "task",
		"{tsak}":  "tsak",
		"${task}": "",
		"${tsak}": "",
	} {
		name, ok := PlaceholderName(match)
		if ok != (want != "") {
			t.Errorf("PlaceholderName(%q) ok = %v, want %v", match, ok, want != "")
		}
		if name != want {
			t.Errorf("PlaceholderName(%q) = %q, want %q", match, name, want)
		}
	}
}

func TestDottedTimerKeys(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []string
	}{
		"dotted at the root":                  {"timer.focus = '25m'\n", []string{"timer.focus"}},
		"more than one":                       {"timer.focus = '25m'\ntimer.long_every = 3\n", []string{"timer.focus", "timer.long_every"}},
		"quoted the long way":                 {`"timer".focus = '25m'` + "\n", []string{"timer.focus"}},
		"both spellings at once":              {"timer.focus = '25m'\n[timer]\nlong_every = 3\n", []string{"timer.focus"}},
		"under a header":                      {"[timer]\nfocus = '25m'\n", nil},
		"dotted inside the table":             {"[timer]\nsub.x = 1\n", nil},
		"a table of its own":                  {"[timer.sub]\nx = 1\n", nil},
		"a table of its own, then the header": {"[timer.sub]\nx = 1\n[timer]\nfocus = '25m'\n", nil},
		"an array of tables":                  {"[[timer]]\nx = 1\n", nil},
		"someone else's timer":                {"[other]\ntimer.focus = '25m'\n", nil},
		"a root key merely alike":             {"other.timer = 1\n", nil},
		"nothing of ours":                     {"default_project = 'Work'\n", nil},
		"not TOML at all":                     {"= broken\n", nil},
		"empty":                               {"", nil},

		"deeper than the table, then the header": {"timer.a.b = 1\n[timer]\nfocus = '25m'\n", nil},

		"both spellings, and the parser refuses": {"timer.focus = '25m'\ntimer.warn_before = '0'\n[timer]\nlong_every = 3\n", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DottedTimerKeys([]byte(c.src)); !slices.Equal(got, c.want) {
				t.Errorf("DottedTimerKeys(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestTableNames(t *testing.T) {
	want := []string{"timer", "sync", "focus_upload"}
	if got := TableNames(); !slices.Equal(got, want) {
		t.Fatalf("TableNames() = %v, want %v", got, want)
	}

	if !slices.Contains(TableNames(), TimerTable) {
		t.Errorf("TableNames() = %v, want it to hold %q", TableNames(), TimerTable)
	}
}

func TestDoubleOpenedTables(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []DoubleOpen
	}{
		"timer, both spellings": {
			"timer.focus = '25m'\n[timer]\nlong_every = 3\n",
			[]DoubleOpen{{"timer", []string{"timer.focus"}}},
		},
		"sync, both spellings": {
			"sync.interval = '1m'\n[sync]\nmove_by_recreate = 'never'\n",
			[]DoubleOpen{{"sync", []string{"sync.interval"}}},
		},
		"focus_upload, both spellings": {
			"focus_upload.enabled = true\n[focus_upload]\n",
			[]DoubleOpen{{"focus_upload", []string{"focus_upload.enabled"}}},
		},

		"two tables, the file's order and not the struct's": {
			"sync.interval = '1m'\ntimer.focus = '25m'\n[sync]\nmove_by_recreate = 'never'\n[timer]\nlong_every = 3\n",
			[]DoubleOpen{{"sync", []string{"sync.interval"}}, {"timer", []string{"timer.focus"}}},
		},
		"dotted only":                   {"timer.focus = '25m'\n", nil},
		"a header only":                 {"[sync]\ninterval = '1m'\n", nil},
		"a dotted key under the header": {"[timer]\nsub.x = 1\n", nil},

		"a header inside a multi-line string": {
			"timer.focus = '25m'\ndefault_project = \"\"\"\n[timer]\n\"\"\"\n",
			nil,
		},

		"somebody else's table": {"other.x = 1\n[other]\ny = 2\n", nil},
		"not TOML at all":       {"= broken\n", nil},
		"empty":                 {"", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := DoubleOpenedTables([]byte(c.src))
			if !slices.EqualFunc(got, c.want, func(a, b DoubleOpen) bool {
				return a.Table == b.Table && slices.Equal(a.Dotted, b.Dotted)
			}) {
				t.Errorf("DoubleOpenedTables(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestSetsRootKey(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"set at the root":           {"default_project = 'Work'\n", true},
		"quoted the long way":       {`"default_project" = 'Work'` + "\n", true},
		"another key, not this one": {"color = 'never'\n[timer]\nfocus = '25m'\n", false},

		"the same name inside a table": {"[timer]\ndefault_project = 'Work'\n", false},
		"not TOML at all":              {"default_project =\n", false},
		"empty":                        {"", false},

		"the file tt itself writes": {Template(), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := SetsRootKey([]byte(c.src), "default_project"); got != c.want {
				t.Errorf("SetsRootKey(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}
