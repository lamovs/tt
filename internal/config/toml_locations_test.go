package config

import (
	"errors"
	"slices"
	"testing"
)

func TestLoadFileKeepsLiteralDottedRootDistinctFromKnownPath(t *testing.T) {
	src := "\"timer.focus\" = 1\n[\"timer\"]\n\"focus\" = \"invalid\"\n"
	_, err := LoadFile(write(t, src))
	var configErr *Error
	if !errors.As(err, &configErr) {
		t.Fatalf("LoadFile err = %v, want a *config.Error", err)
	}
	want := []Problem{
		{Line: 1, Msg: `unknown key "timer.focus"`},
		{Line: 3, Msg: `timer.focus: invalid duration "invalid" (want "25m", "1h30m", "90s" or "0")`},
	}
	if !slices.Equal(configErr.Problems, want) {
		t.Fatalf("problems = %#v, want %#v", configErr.Problems, want)
	}
}

func TestIndexLinesKeepsDecodedComponentCollisionsDistinct(t *testing.T) {
	src := "\"a.b\" = 1\n[a]\nb = 2\n\"timer\".\"focus\" = \"invalid\"\n"
	got := indexLines([]byte(src))
	want := lineIndex{
		identity([]string{"a.b"}):                 1,
		identity([]string{"a"}):                   2,
		identity([]string{"a", "b"}):              3,
		identity([]string{"a", "timer", "focus"}): 4,
	}
	if !mapsEqual(got, want) {
		t.Fatalf("indexLines = %v, want %v", got, want)
	}
}

func TestLoadFilePlacesQuotedEscapedAndBracketedNames(t *testing.T) {
	cases := map[string]struct {
		src    string
		prefix string
		line   int
	}{
		"escaped known table": {
			"[\"tim\\u0065r\"]\n\"focus\" = \"invalid\"\n",
			"timer.focus:",
			2,
		},
		"escaped known key": {
			"[timer]\n\"foc\\u0075s\" = \"invalid\"\n",
			"timer.focus:",
			2,
		},
		"closing bracket in literal table name": {
			"['odd].name']\nvalue = 1\n",
			`unknown table "odd].name"`,
			1,
		},
		"escaped quote and dot in table name": {
			"[\"odd\\\".name\"]\nvalue = 1\n",
			`unknown table "odd\".name"`,
			1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := loadProblemLine(t, tc.src, tc.prefix); got != tc.line {
				t.Errorf("problem line = %d, want %d", got, tc.line)
			}
		})
	}
}

func TestUnknownTableDedupUsesComponentIdentity(t *testing.T) {
	src := "[\"a.b\"]\nvalue = 1\n[a.b]\nvalue = 2\n"
	var first []Problem
	for i := 0; i < 20; i++ {
		_, err := LoadFile(write(t, src))
		var configErr *Error
		if !errors.As(err, &configErr) {
			t.Fatalf("run %d: LoadFile err = %v, want a *config.Error", i, err)
		}
		if i == 0 {
			first = append([]Problem(nil), configErr.Problems...)
		}
		if !slices.Equal(configErr.Problems, first) {
			t.Fatalf("run %d: problems = %#v, first = %#v", i, configErr.Problems, first)
		}
	}
	want := []Problem{
		{Line: 1, Msg: `unknown table "a.b"`},
		{Line: 3, Msg: `unknown table "a.b"`},
	}
	if !slices.Equal(first, want) {
		t.Fatalf("problems = %#v, want %#v", first, want)
	}
}

func TestIndexLinesShieldsMultilineContainers(t *testing.T) {
	src := `payload = [
  { focus = "text", "timer.focus" = "also text" },
  "focus = 'text'",
  """multiline
[timer]
focus = "text"
""",
]
[timer]
focus = "invalid"
`
	got := indexLines([]byte(src))
	want := lineIndex{
		identity([]string{"payload"}):        1,
		identity([]string{"timer"}):          9,
		identity([]string{"timer", "focus"}): 10,
	}
	if !mapsEqual(got, want) {
		t.Fatalf("indexLines = %v, want %v", got, want)
	}
	if line := loadProblemLine(t, src, "timer.focus:"); line != 10 {
		t.Fatalf("timer.focus line = %d, want 10", line)
	}
}

func TestCollectorSortsKnownLinesTiesAndUnknownLinesDeterministically(t *testing.T) {
	lines := lineIndex{
		identity([]string{"later"}):    7,
		identity([]string{"tie", "b"}): 3,
		identity([]string{"tie", "a"}): 3,
	}
	build := func(reverse bool) []Problem {
		c := collector{lines: lines}
		adds := []struct {
			parts []string
			msg   string
		}{
			{[]string{"missing", "z"}, "line zero z"},
			{[]string{"tie", "b"}, "tie b"},
			{[]string{"later"}, "later"},
			{[]string{"tie", "a"}, "tie a"},
			{[]string{"missing", "a"}, "line zero a second"},
			{[]string{"missing", "a"}, "line zero a first"},
		}
		if reverse {
			slices.Reverse(adds)
		}
		for _, add := range adds {
			c.addAtParts(add.parts, add.msg)
		}
		return c.sorted()
	}
	want := []Problem{
		{Line: 3, Msg: "tie a"},
		{Line: 3, Msg: "tie b"},
		{Line: 7, Msg: "later"},
		{Line: 0, Msg: "line zero a first"},
		{Line: 0, Msg: "line zero a second"},
		{Line: 0, Msg: "line zero z"},
	}
	for i := 0; i < 20; i++ {
		got := build(i%2 == 1)
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: sorted = %#v, want %#v", i, got, want)
		}
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
