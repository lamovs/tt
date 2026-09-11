package cli

import (
	"io"
	"os"

	"github.com/movsar/tt/internal/config"
)

const noColorVar = "NO_COLOR"

const (
	termVar  = "TERM"
	dumbTerm = "dumb"
)

func PaletteFor(mode config.ColorMode, w io.Writer) Palette {
	return NewPalette(colorEnabled(mode, IsTerminal(w), noColorSet(), os.Getenv(termVar)))
}

func noColorSet() bool {
	return os.Getenv(noColorVar) != ""
}

func colorEnabled(mode config.ColorMode, isTTY, noColor bool, term string) bool {
	switch mode {
	case config.ColorNever:
		return false
	case config.ColorAlways:
		return true
	}
	if noColor || term == dumbTerm {
		return false
	}
	return isTTY
}

func IsTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(f)
}

type Palette struct{ on bool }

func NewPalette(on bool) Palette { return Palette{on: on} }

func (p Palette) Enabled() bool { return p.on }

const (
	sgrBold   = "\x1b[1m"
	sgrDim    = "\x1b[2m"
	sgrWeight = "\x1b[22m"

	sgrAccent      = "\x1b[33m"
	sgrDefaultText = "\x1b[39m"
)

func (p Palette) Bold(s string) string { return p.wrap(s, sgrBold, sgrWeight) }

func (p Palette) Dim(s string) string { return p.wrap(s, sgrDim, sgrWeight) }

func (p Palette) Accent(s string) string { return p.wrap(s, sgrAccent, sgrDefaultText) }

func (p Palette) wrap(s, on, off string) string {
	if !p.on || s == "" {
		return s
	}
	return on + s + off
}

type styler func(string) string

func (p Palette) accentIf(cond bool) styler {
	if !cond {
		return nil
	}
	return p.Accent
}

func PlainPalette() Palette { return Palette{} }
