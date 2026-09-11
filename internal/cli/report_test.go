package cli

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestReportTitleEscapesBeforeItCuts(t *testing.T) {

	title := strings.Repeat("\t", 10)
	got := ReportTitle(title, 12)
	if n := DisplayWidth(got); n > 12 {
		t.Errorf("ReportTitle(%q, 12) = %q, %d columns wide", title, got, n)
	}
	if strings.ContainsRune(got, '\t') {
		t.Errorf("ReportTitle(%q, 12) = %q, want the tabs escaped", title, got)
	}
}

func TestReportTitle(t *testing.T) {
	cases := []struct {
		name  string
		title string
		w     int
		want  string
	}{

		{"a plain title", "buy milk", Width, `"buy milk"`},

		{"multi-byte letters", "Забрать посылку", 17, `"Забрать посылку"`},
		{"multi-byte letters, one column short", "Забрать посылку", 16, `"Забрать пос..."`},

		{"a combining character", "éclair", Width, "\"éclair\""},

		{"only control characters", "\x00\x01\x7f", Width, `"\x00\x01\x7f"`},
		{"a newline", "two\nlines", Width, `"two\nlines"`},

		{"exactly at the bound", "0123456789", 12, `"0123456789"`},
		{"one column over", "0123456789", 11, `"012345..."`},

		{"no room for the mark", "0123456789", 5, `"012"`},
		{"no room for anything", "0123456789", 2, ""},
		{"no width at all", "0123456789", 0, ""},

		{"an empty title", "", Width, `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReportTitle(c.title, c.w)
			if got != c.want {
				t.Errorf("ReportTitle(%q, %d) = %q, want %q", c.title, c.w, got, c.want)
			}
			if n := DisplayWidth(got); n > c.w {
				t.Errorf("ReportTitle(%q, %d) is %d columns wide: %q", c.title, c.w, n, got)
			}
		})
	}
}

func TestEscapedCutsKeepRawGraphemesAndCompleteEscapes(t *testing.T) {
	zwj := "\U0001F469\u200d\U0001F4BB"
	cases := []struct {
		name string
		got  string
		want string
		max  int
	}{
		{name: "quoted combining grapheme", got: ReportTitle("e\u0301x", 3), want: "\"e\u0301\"", max: 3},
		{name: "quoted CJK", got: ReportTitle("界x", 4), want: "\"界\"", max: 4},
		{name: "quoted ZWJ escape", got: ReportTitle(zwj+"xxxx", 15), want: "\"\U0001F469\\u200d\U0001F4BB...\"", max: 15},
		{name: "cell combining grapheme", got: escapeCell("e\u0301x", 1), want: "e\u0301", max: 1},
		{name: "cell ZWJ escape", got: escapeCell(zwj+"xxxx", 13), want: "\U0001F469\\u200d\U0001F4BB...", max: 13},
		{name: "complete control escape", got: escapeCell("\x00xxxx", 7), want: `\x00...`, max: 7},
		{name: "invalid byte spelling", got: escapeCell("\xffxxxx", 7), want: `\xff...`, max: 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
			if width := DisplayWidth(c.got); width > c.max {
				t.Errorf("got %d cells, want at most %d: %q", width, c.max, c.got)
			}
		})
	}
}

func TestUncutEscapingKeepsExistingDialectsExactly(t *testing.T) {
	raw := "e\u0301 \U0001F469\u200d\U0001F4BB \\\"\n\x00\xff"
	if got, want := ReportTitle(raw, 200), strconv.Quote(raw); got != want {
		t.Errorf("ReportTitle uncut = %q, want %q", got, want)
	}
	wantCell := "e\u0301 \U0001F469\\u200d\U0001F4BB \\\"\\n\\x00\\xff"
	if got := escape(raw); got != wantCell {
		t.Errorf("escape uncut = %q, want %q", got, wantCell)
	}
}

func TestReportTitleNeverLetsAControlCharacterThrough(t *testing.T) {
	for r := rune(0); r < 0x300; r++ {
		title := strings.Repeat(string(r), 12)
		for _, w := range []int{2, 3, 5, 8, 13, 21, Width} {
			got := ReportTitle(title, w)
			if n := DisplayWidth(got); n > w {
				t.Fatalf("ReportTitle(%d x %U, %d) is %d columns wide: %q", 12, r, w, n, got)
			}
			for _, c := range got {
				if unicode.IsControl(c) {
					t.Fatalf("ReportTitle(%d x %U, %d) = %q, which carries %U", 12, r, w, got, c)
				}
			}

			if got != "" && (!strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`)) {
				t.Fatalf("ReportTitle(%d x %U, %d) = %q, want it quoted at both ends", 12, r, w, got)
			}
		}
	}
}

func TestReportLineFitsTheWidth(t *testing.T) {
	titles := []string{
		strings.Repeat("word ", 60),
		strings.Repeat("Забрать посылку ", 20),
		strings.Repeat("\n", 100),
		strings.Repeat("́", 100),
		"",
	}
	fixtures := []struct{ before, after string }{
		{"done: ", ""},
		{"", ": due overdue 12d"},
		{"undid completing ", ": it is open again"},
		{"added ", " to " + strings.Repeat("l", Width/4)},
	}
	for _, title := range titles {
		for _, f := range fixtures {
			got := ReportLine(f.before, title, f.after, nil)
			if n := DisplayWidth(got); n > Width {
				t.Errorf("ReportLine(%q, ..., %q) is %d columns wide: %q", f.before, f.after, n, got)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("ReportLine(%q, ..., %q) is more than one line: %q", f.before, f.after, got)
			}
			if !strings.HasPrefix(got, f.before) || !strings.HasSuffix(got, f.after) {
				t.Errorf("ReportLine(%q, ..., %q) = %q, want tt's own words kept whole", f.before, f.after, got)
			}
		}
	}
}

func TestReportLineStylesAfterItMeasures(t *testing.T) {
	title := strings.Repeat("word ", 60)
	bold := NewPalette(true).Bold
	plain := ReportLine("done: ", title, "", nil)
	styled := ReportLine("done: ", title, "", bold)
	if styled != "done: "+bold(strings.TrimPrefix(plain, "done: ")) {
		t.Errorf("styled = %q, want the same line with the title alone dressed", styled)
	}
	if !strings.Contains(styled, "\x1b[") {
		t.Errorf("styled = %q, want the escape sequences a palette that is on adds", styled)
	}
}

func TestEscape(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"nothing to escape", "buy milk", "buy milk"},

		{"a newline", "pay\nthe rent", `pay\nthe rent`},
		{"a tab", "pay\tthe bill", `pay\tthe bill`},

		{"multi-byte letters", "Забрать посылку", "Забрать посылку"},

		{"a double quote and a backslash", `say "hi" about C:\temp`, `say "hi" about C:\temp`},

		{"a backslash and an n", `pay\nthe rent`, `pay\nthe rent`},
		{"a newline again", "pay\nthe rent", `pay\nthe rent`},
		{"a byte that is not a character", "ok\xff", `ok\xff`},
		{"nothing at all", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escape(c.title); got != c.want {
				t.Errorf("escape(%q) = %q, want %q", c.title, got, c.want)
			}

			for _, r := range escape(c.title) {
				if unicode.IsControl(r) {
					t.Errorf("escape(%q) = %q, which carries %U", c.title, escape(c.title), r)
				}
			}
		})
	}
}

func TestReportTitleKeepsTheQuotedDialect(t *testing.T) {
	const title = `say "hi" about C:\temp`
	if got, want := ReportTitle(title, Width), `"say \"hi\" about C:\\temp"`; got != want {
		t.Errorf("ReportTitle(%q, Width) = %q, want %q", title, got, want)
	}
	if got, want := escape(title), title; got != want {
		t.Errorf("escape(%q) = %q, want the title byte for byte", title, got)
	}
}

func TestEscapeCellCutsBetweenEscapeSequences(t *testing.T) {
	cases := []struct {
		name  string
		title string
		w     int
		want  string
	}{
		{"nothing to escape or cut", "buy milk", Width, "buy milk"},

		{"a double quote and a backslash", `say "hi" about C:\temp`, Width, `say "hi" about C:\temp`},
		{"a double quote and a backslash, cut", `say "hi" about C:\temp`, 10, `say "hi...`},
		{"exactly at the bound", "0123456789", 10, "0123456789"},
		{"one column over", "0123456789", 9, "012345..."},

		{"tabs", strings.Repeat("\t", 12), 6, `\t...`},
		{"tabs, one column more", strings.Repeat("\t", 12), 7, `\t\t...`},
		{"tabs, two columns more", strings.Repeat("\t", 12), 8, `\t\t...`},

		{"control characters", strings.Repeat("\x00", 12), 10, `\x00...`},

		{"no room for the mark", "0123456789", 3, "012"},
		{"no room at all", "0123456789", 0, ""},
		{"nothing at all", "", Width, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeCell(c.title, c.w)
			if got != c.want {
				t.Errorf("escapeCell(%q, %d) = %q, want %q", c.title, c.w, got, c.want)
			}
			if n := DisplayWidth(got); n > c.w {
				t.Errorf("escapeCell(%q, %d) is %d columns wide: %q", c.title, c.w, n, got)
			}
		})
	}
}

func TestEscapeCellNeverLetsAControlCharacterThrough(t *testing.T) {
	for r := rune(0); r < 0x300; r++ {
		title := strings.Repeat(string(r), 12)
		for _, w := range []int{0, 1, 2, 3, 5, 8, 13, 21, Width} {
			got := escapeCell(title, w)
			if n := DisplayWidth(got); n > w {
				t.Fatalf("escapeCell(%d x %U, %d) is %d columns wide: %q", 12, r, w, n, got)
			}
			for _, c := range got {
				if unicode.IsControl(c) {
					t.Fatalf("escapeCell(%d x %U, %d) = %q, which carries %U", 12, r, w, got, c)
				}
			}
			body, unit := strings.TrimSuffix(got, ellipsis), escape(string(r))
			if len(body)%len(unit) != 0 || strings.Repeat(unit, len(body)/len(unit)) != body {
				t.Fatalf("escapeCell(%d x %U, %d) = %q, cut inside the escaping of %q", 12, r, w, got, unit)
			}
		}
	}
}

func TestInvalidBytesAreSpelledTheSameWayAtEveryWidth(t *testing.T) {
	const title = "ok\xff\xfe tail"
	for _, got := range []string{ReportTitle(title, Width), ReportTitle(title, 14), escape(title), escapeCell(title, 9)} {
		if !strings.Contains(got, `\xff`) {
			t.Errorf("%q does not spell the invalid byte the way strconv.Quote does", got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%q carries the replacement character, so the byte was lost", got)
		}
	}
}

func TestEscapeBlock(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"a paragraph break is structure", "one\n\ntwo", "one\n\ntwo"},

		{"a carriage return is not", "one\rtwo", `one\rtwo`},
		{"an escape sequence is not", "one\x1b[31mtwo", `one\x1b[31mtwo`},
		{"a tab is not", "one\ttwo", `one\ttwo`},

		{"a quote and a backslash stand as they are", `say "hi" C:\temp`, `say "hi" C:\temp`},
		{"nothing at all", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escapeBlock(c.text); got != c.want {
				t.Errorf("escapeBlock(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}

	if got, want := escape("one\ntwo"), `one\ntwo`; got != want {
		t.Errorf("escape(%q) = %q, want %q - the exemption is the block's alone", "one\ntwo", got, want)
	}
}

func TestEscapeBlockNormalisesTheCRLFPair(t *testing.T) {
	const text = "first line\r\nsecond line\r\n\r\nthird"
	got := escapeBlock(text)
	if want := "first line\nsecond line\n\nthird"; got != want {
		t.Errorf("escapeBlock(%q) = %q, want %q", text, got, want)
	}
	if strings.Contains(got, `\r`) {
		t.Errorf("escapeBlock(%q) = %q, which still writes out a carriage return", text, got)
	}
	if a, b := strings.Count(got, "\n"), 3; a != b {
		t.Errorf("escapeBlock(%q) = %q: %d line breaks, want %d - the paragraph break is one of them", text, got, a, b)
	}
	const lone = "one\rtwo\r\nthree"
	if got, want := escapeBlock(lone), "one"+`\r`+"two\nthree"; got != want {
		t.Errorf("escapeBlock(%q) = %q, want %q - a lone return is not a line break", lone, got, want)
	}
}

func TestEscapeBlockKeepsTheLineBreakAndNothingElse(t *testing.T) {
	for r := rune(0); r < 0x300; r++ {
		text := "a" + strings.Repeat(string(r), 3) + "b\nc"
		got := escapeBlock(text)
		for _, c := range got {
			if unicode.IsControl(c) && c != '\n' {
				t.Fatalf("escapeBlock(%q) = %q, which carries %U", text, got, c)
			}
		}
		if a, b := strings.Count(got, "\n"), strings.Count(text, "\n"); a != b {
			t.Fatalf("escapeBlock(%q) = %q: %d line breaks where the text had %d", text, got, a, b)
		}
	}
}

func TestForeignHoldsOneAnswerToOneLine(t *testing.T) {
	answer := "upstream said:\r\n\t\x1b[2Jgone\xff"
	got := Foreign(answer, Width)
	for _, r := range got {
		if r < 0x20 || r == 0x1b {
			t.Fatalf("Foreign(%q, Width) = %q, which carries %U", answer, got, r)
		}
	}
	back, err := strconv.Unquote(got)
	if err != nil {
		t.Fatalf("Unquote(%q): %v", got, err)
	}
	if back != answer {
		t.Errorf("Unquote(Foreign(%q, Width)) = %q, want the answer as it arrived", answer, back)
	}
}

func TestForeignCutsInsideItsOwnQuotes(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		want   int
		exact  bool
	}{
		{"plain text fills the width", strings.Repeat("a", 1000), 40, true},
		{"escapes stop short of it", strings.Repeat("\x1b", 1000), 40, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Foreign(c.answer, c.want)
			n := DisplayWidth(got)
			if n > c.want || (c.exact && n != c.want) {
				t.Errorf("Foreign(..., %d) is %d columns wide: %q", c.want, n, got)
			}
			if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `..."`) {
				t.Errorf("Foreign(..., %d) = %q, want the cut marked inside the quotes", c.want, got)
			}
		})
	}
}

func TestForeignEscapesTheQuoteThatMarksIt(t *testing.T) {
	answer := `{"error":"gone"} C:\temp`
	if got, want := Foreign(answer, Width), `"{\"error\":\"gone\"} C:\\temp"`; got != want {
		t.Errorf("Foreign(%q, Width) = %q, want %q", answer, got, want)
	}
}

func TestForeignNeverPrintsNothing(t *testing.T) {
	for _, w := range []int{0, 1, 5, 8} {
		if got := Foreign("upstream is gone", w); got == "" {
			t.Errorf("Foreign(..., %d) came back empty", w)
		}
	}
	if got, want := Foreign("\x1b[2Jhello", 8), `"..."`; got != want {
		t.Errorf("Foreign(%q, 8) = %q, want %q - the floor buys the quotes and the mark and none of the answer", "\x1b[2Jhello", got, want)
	}

	if got, want := Foreign("", 40), `""`; got != want {
		t.Errorf("Foreign(%q, 40) = %q, want %q", "", got, want)
	}
}
