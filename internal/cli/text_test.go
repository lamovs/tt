package cli

import (
	"strings"
	"testing"
)

func TestDisplayWidthUsesTerminalCells(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{name: "ASCII", text: "abc", want: 3},
		{name: "Cyrillic", text: "да", want: 2},
		{name: "CJK", text: "界面", want: 4},
		{name: "combining grapheme", text: "e\u0301", want: 1},
		{name: "emoji", text: "\U0001F642", want: 2},
		{name: "ZWJ emoji", text: "\U0001F469\u200d\U0001F4BB", want: 2},
		{name: "invalid byte", text: "a\xffb", want: 3},
		{name: "foreign ANSI is not stripped", text: "\x1b[31m", want: 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DisplayWidth(c.text); got != c.want {
				t.Errorf("DisplayWidth(%q) = %d, want %d", c.text, got, c.want)
			}
		})
	}
}

func TestWrapUsesCellsAndKeepsLongWordsWhole(t *testing.T) {
	if got, want := Wrap("界 界", 4), []string{"界", "界"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Wrap CJK = %q, want %q", got, want)
	}
	if got, want := Wrap("e\u0301 e\u0301", 3), []string{"e\u0301 e\u0301"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Wrap combining text = %q, want %q", got, want)
	}
	word := "\U0001F469\u200d\U0001F4BB\U0001F469\u200d\U0001F4BB"
	if got := Wrap(word, 3); len(got) != 1 || got[0] != word {
		t.Errorf("Wrap long word = %q, want the complete opaque word", got)
	}
}

func TestPadUsesCellsBeforeStyling(t *testing.T) {
	if got, want := pad("界", 4, nil), "界  "; got != want {
		t.Errorf("pad plain = %q, want %q", got, want)
	}
	p := NewPalette(true)
	got := pad("e\u0301", 3, p.Bold)
	if want := p.Bold("e\u0301") + "  "; got != want {
		t.Errorf("pad styled = %q, want %q", got, want)
	}
	if width := DisplayWidth(unstyle(got)); width != 3 {
		t.Errorf("unstyled padded value is %d cells, want 3: %q", width, got)
	}
}

func TestWrapBeginsEveryParagraphOnALineOfItsOwn(t *testing.T) {
	const quoted = `"tt login"`
	const para = quoted + " is what to run first"

	for _, w := range []int{Width, Width - 12, 20} {
		for n := 0; n <= 2*Width; n++ {
			lead := "tt looks for the config at /" + strings.Repeat("d", n) + "/config.toml"
			leadLines := Wrap(lead, w)
			paraLines := Wrap(para, w)
			lines := Wrap(lead+"\n"+para, w)

			if len(leadLines) == 0 || len(paraLines) == 0 {
				t.Fatalf("at %d columns, a lead of %d columns wraps to %d lines and the paragraph to %d, and neither of them is empty text",
					w, runeLen(lead), len(leadLines), len(paraLines))
			}
			if len(lines) != len(leadLines)+len(paraLines) {
				t.Fatalf("at %d columns, a lead of %d columns and a paragraph wrap to %d lines together and %d + %d apart: %q",
					w, runeLen(lead), len(lines), len(leadLines), len(paraLines), lines)
			}
			if got := strings.Join(lines[:len(leadLines)], "|"); got != strings.Join(leadLines, "|") {
				t.Errorf("at %d columns, the lead of %d columns wraps to %q with a paragraph after it and to %q without one",
					w, runeLen(lead), lines[:len(leadLines)], leadLines)
			}
			if got := strings.Join(lines[len(leadLines):], "|"); got != strings.Join(paraLines, "|") {
				t.Errorf("at %d columns, the paragraph after a lead of %d columns wraps to %q and on its own to %q",
					w, runeLen(lead), lines[len(leadLines):], paraLines)
			}

			first := lines[len(leadLines)]
			if !strings.HasPrefix(first, quoted) {
				t.Errorf("at %d columns, with %d columns of text ahead of it, the paragraph begins %q, want it to begin with %s",
					w, runeLen(lead), first, quoted)
			}
			if times := strings.Count(strings.Join(lines, "\n"), quoted); times != 1 {
				t.Errorf("at %d columns, the message carries %s %d times, want the once it was written: %q", w, quoted, times, lines)
			}
		}
	}
}
