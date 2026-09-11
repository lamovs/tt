package cli

import (
	"io"
	"strings"

	"github.com/rivo/uniseg"
)

const Width = 80

const ellipsis = "..."

func DisplayWidth(s string) int { return uniseg.StringWidth(s) }

func runeLen(s string) int { return DisplayWidth(s) }

func Wrap(s string, w int) []string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		fields := strings.Fields(para)
		if len(fields) == 0 {

			out = append(out, "")
			continue
		}
		line := fields[0]
		for _, word := range fields[1:] {
			if DisplayWidth(line)+1+DisplayWidth(word) > w {
				out = append(out, line)
				line = word
				continue
			}
			line += " " + word
		}
		out = append(out, line)
	}
	return out
}

func pad(s string, w int, style styler) string {
	fill := w - DisplayWidth(s)
	if style != nil {
		s = style(s)
	}
	if fill <= 0 {
		return s
	}
	return s + strings.Repeat(" ", fill)
}

func WriteLines(w io.Writer, lines []string) error {
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	return nil
}
