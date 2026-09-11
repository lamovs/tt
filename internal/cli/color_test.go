package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/config"
)

func TestColorEnabled(t *testing.T) {
	cases := []struct {
		name    string
		mode    config.ColorMode
		tty     bool
		noColor bool
		term    string
		want    bool
	}{
		{"auto on a terminal", config.ColorAuto, true, false, "xterm-256color", true},
		{"auto down a pipe", config.ColorAuto, false, false, "xterm-256color", false},
		{"auto with NO_COLOR", config.ColorAuto, true, true, "xterm-256color", false},
		{"auto on a dumb terminal", config.ColorAuto, true, false, "dumb", false},
		{"auto with no TERM at all", config.ColorAuto, true, false, "", true},

		{"always down a pipe", config.ColorAlways, false, false, "xterm-256color", true},

		{"always with NO_COLOR", config.ColorAlways, true, true, "xterm-256color", true},
		{"always on a dumb terminal", config.ColorAlways, true, false, "dumb", true},

		{"never on a terminal", config.ColorNever, true, false, "xterm-256color", false},
		{"never down a pipe", config.ColorNever, false, false, "xterm-256color", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := colorEnabled(c.mode, c.tty, c.noColor, c.term); got != c.want {
				t.Errorf("colorEnabled(%q, tty=%v, noColor=%v, term=%q) = %v, want %v",
					c.mode, c.tty, c.noColor, c.term, got, c.want)
			}
		})
	}
}

func TestNoColorEmptyIsNotSet(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{"unset", "", false, false},
		{"empty", "", true, false},
		{"NO_COLOR=1", "1", true, true},

		{"NO_COLOR=0", "0", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setNoColor(t, c.value, c.set)
			if got := noColorSet(); got != c.want {
				t.Errorf("noColorSet() with NO_COLOR %s = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func setNoColor(t *testing.T, value string, set bool) {
	t.Helper()
	t.Setenv(noColorVar, value)
	if !set {
		os.Unsetenv(noColorVar)
	}
}

func TestColorAlwaysOutranksNoColor(t *testing.T) {
	setNoColor(t, "1", true)
	var buf bytes.Buffer
	if p := PaletteFor(config.ColorAlways, &buf); !p.Enabled() {
		t.Error("--color=always must outrank a NO_COLOR that is set")
	}
	if p := PaletteFor(config.ColorNever, &buf); p.Enabled() {
		t.Error("--color=never colours nothing")
	}
}

func TestPaletteForAStreamThatIsNotATerminal(t *testing.T) {
	setNoColor(t, "", false)
	var buf bytes.Buffer
	if p := PaletteFor(config.ColorAuto, &buf); p.Enabled() {
		t.Error("auto coloured a bytes.Buffer")
	}
	if p := PaletteFor(config.ColorAlways, &buf); !p.Enabled() {
		t.Error("always must colour whatever the stream is")
	}
}

func TestPaletteWritesNothingWhenOff(t *testing.T) {
	p := PlainPalette()
	for name, got := range map[string]string{
		"bold":   p.Bold("x"),
		"dim":    p.Dim("x"),
		"accent": p.Accent("x"),
	} {
		if got != "x" {
			t.Errorf("%s on a plain palette = %q, want the text untouched", name, got)
		}
	}
}

func TestPaletteClosesWhatItOpens(t *testing.T) {
	p := NewPalette(true)
	cases := map[string]struct{ got, open, close string }{
		"bold":   {p.Bold("x"), sgrBold, sgrWeight},
		"dim":    {p.Dim("x"), sgrDim, sgrWeight},
		"accent": {p.Accent("x"), sgrAccent, sgrDefaultText},
	}
	for name, c := range cases {
		if want := c.open + "x" + c.close; c.got != want {
			t.Errorf("%s = %q, want %q", name, c.got, want)
		}
		if strings.Contains(c.got, "\x1b[0m") {
			t.Errorf("%s uses the blanket reset", name)
		}
	}
	if got := p.Bold(""); got != "" {
		t.Errorf("styling an empty string produced %q, want nothing at all", got)
	}
}
