package tui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type searchInput struct {
	value []rune
	pos   int
}

func (s *searchInput) set(value string) { s.value, s.pos = []rune(value), len([]rune(value)) }

func (s *searchInput) insert(text string) {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	chars := []rune(text)
	value := append([]rune(nil), s.value[:s.pos]...)
	value = append(value, chars...)
	s.value = append(value, s.value[s.pos:]...)
	s.pos += len(chars)
}

func (s *searchInput) update(msg tea.KeyPressMsg) {
	switch msg.String() {
	case "left":
		s.pos = max(0, s.pos-1)
	case "right":
		s.pos = min(len(s.value), s.pos+1)
	case "home", "ctrl+a":
		s.pos = 0
	case "end", "ctrl+e":
		s.pos = len(s.value)
	case "backspace":
		if s.pos > 0 {
			s.value = append(append([]rune(nil), s.value[:s.pos-1]...), s.value[s.pos:]...)
			s.pos--
		}
	case "delete":
		if s.pos < len(s.value) {
			s.value = append(append([]rune(nil), s.value[:s.pos]...), s.value[s.pos+1:]...)
		}
	case "ctrl+u":
		s.set("")
	default:
		if msg.Text != "" {
			s.insert(msg.Text)
		} else if msg.Mod & ^tea.ModShift == 0 && msg.Code >= 32 && msg.Code <= unicode.MaxRune {
			s.insert(string(msg.Code))
		}
	}
}

func (s searchInput) view(width int) string {

	span := max(1, width)*8 + 32
	left, right := display(string(s.value[max(0, s.pos-span):s.pos])), display(string(s.value[s.pos:min(len(s.value), s.pos+span)]))
	available := max(1, width-1)
	if ansi.StringWidth(left) > available {
		left = ansi.Cut(left, ansi.StringWidth(left)-available, ansi.StringWidth(left))
	}
	return fit(left+"|"+right, width)
}

func (s searchInput) bodyView(width, rows int) []string {
	width, rows = max(1, width), max(1, rows)
	span := width*rows*8 + 32
	start, end := max(0, s.pos-span), min(len(s.value), s.pos+span)
	left, right := string(s.value[start:s.pos]), string(s.value[s.pos:end])
	before := wrapText(left, width)
	all := wrapText(left+"|"+right, width)
	first := min(max(0, len(before)-rows), len(all))
	return all[first:min(len(all), first+rows)]
}

func (s searchInput) summary(width int) string {
	end := min(len(s.value), max(1, width)*8+32)
	text := string(s.value[:end])
	if end < len(s.value) {
		text += "..."
	}
	return displayClipped(text, width)
}
