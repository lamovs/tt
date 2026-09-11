package config

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

const insideAString = `default_project = """
[timer]
focus = "oops"
"""

[timer]
focus = "not a duration"
`

func TestIndexLinesSkipsMultiLineStrings(t *testing.T) {
	cases := map[string]struct {
		src  string
		want map[string]int
	}{

		"no strings at all": {
			"[timer]\nfocus = '25m'\n",
			map[string]int{"timer": 1, "timer.focus": 2},
		},

		"a quoted header names its children": {
			"[\"timer]\"]\nfocus = '25m'\nshort_break = '5m'\n[timer]\nfocus = 'invalid'\n",
			map[string]int{"timer": 4, "timer.focus": 5, "timer]": 1, "timer].focus": 2, "timer].short_break": 3},
		},

		"a literal dot in a quoted root key stays distinct": {
			"\"timer.focus\" = '25m'\n[timer]\nfocus = 'invalid'\n",
			map[string]int{"timer": 2, "timer.focus": 3, "=timer.focus": 1},
		},
		"a quoted segment keeps unknown placement": {
			"\"timer\".focus = 'invalid'\n",
			map[string]int{"timer.focus": 1},
		},
		"a table inside a basic multiline": {
			insideAString,
			map[string]int{"default_project": 1, "timer": 6, "timer.focus": 7},
		},
		"a table inside a literal multiline": {
			"default_project = '''\n[timer]\nfocus = \"oops\"\n'''\n\n[timer]\nfocus = \"not a duration\"\n",
			map[string]int{"default_project": 1, "timer": 6, "timer.focus": 7},
		},

		"a comment holding three quotes": {
			"# \"\"\"\ndefault_project = 'Work' # \"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 2, "timer.focus": 3},
		},
		"a hash inside a string is no comment": {
			"default_project = 'a # b'\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"three quotes inside a single-line string": {
			"default_project = \"'''\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"opened and closed on one line": {
			"default_project = \"\"\"Work\"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"an escaped quote does not close a basic string": {
			"default_project = \"a \\\" b\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"an escaped quote does not close a basic multiline": {
			"default_project = \"\"\"\na \\\"\"\" b\n\"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 4},
		},

		"a backslash escapes nothing in a literal string": {
			"default_project = 'a\\'\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"a backslash escapes nothing in a literal multiline": {
			"default_project = '''\na \\'''\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 3},
		},

		"a closing run of four on the line that opened": {
			"default_project = \"\"\"a\"\"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"a closing run of five on the line that opened": {
			"default_project = \"\"\"a\"\"\"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 2},
		},
		"a closing run of four on a line of its own": {
			"default_project = \"\"\"\na\"\"\"\"\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 3},
		},
		"a closing run of five on a line of its own": {
			"default_project = '''\na'''''\ntimer.focus = '25m'\n",
			map[string]int{"default_project": 1, "timer.focus": 3},
		},

		"an unterminated single-line quote": {
			"default_project = \"oops\ntimer.focus = '25m'\n",
			map[string]int{"timer.focus": 2},
		},

		"the source ends inside a string": {
			"default_project = \"\"\"\n[timer]\n",
			map[string]int{},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			want := make(lineIndex)
			for key, line := range c.want {
				if strings.HasPrefix(key, "=") {
					want[identity([]string{strings.TrimPrefix(key, "=")})] = line
					continue
				}
				want[identity(semanticParts(key))] = line
			}
			if got := indexLines([]byte(c.src)); !maps.Equal(got, want) {
				t.Errorf("indexLines(%q) = %v, want %v", c.src, got, want)
			}
		})
	}
}

func TestLoadFilePlacesAProblemOutsideAString(t *testing.T) {
	_, err := LoadFile(write(t, insideAString))
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("LoadFile err = %v, want a *config.Error", err)
	}
	if len(cerr.Problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", cerr.Problems)
	}
	if got := cerr.Problems[0].Line; got != 7 {
		t.Errorf("problem line = %d, want 7 - the line the value is on", got)
	}
}

func TestLoadFileDoesNotAttributeAQuotedHeaderChildToAnotherTable(t *testing.T) {
	src := "[\"timer]\"]\nfocus = '25m'\n[timer]\nfocus = 'invalid'\n"
	line := loadProblemLine(t, src, "timer.focus:")
	if line != 4 {
		t.Errorf("problem line = %d, want actual line 4", line)
	}
}

func TestLoadFileDoesNotTreatALiteralDotAsAKeyPath(t *testing.T) {
	src := "\"timer.focus\" = '25m'\n[timer]\nfocus = 'invalid'\n"
	line := loadProblemLine(t, src, "timer.focus:")
	if line != 3 {
		t.Errorf("problem line = %d, want actual line 3", line)
	}
}

func loadProblemLine(t *testing.T, src, prefix string) int {
	t.Helper()
	_, err := LoadFile(write(t, src))
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("LoadFile err = %v, want a *config.Error", err)
	}
	for _, problem := range cerr.Problems {
		if strings.HasPrefix(problem.Msg, prefix) {
			return problem.Line
		}
	}
	t.Fatalf("problems = %v, want one beginning %q", cerr.Problems, prefix)
	return 0
}
