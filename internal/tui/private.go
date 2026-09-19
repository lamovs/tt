package tui

import "strings"

// Private mode keeps task content off a shared screen while the lists are
// browsed. The placeholders are fixed strings: a per-character mask would still
// report how long the hidden title is.
const (
	hiddenTitle   = "(hidden)"
	hiddenPreview = "Task content is hidden. Press Ctrl+K to show it."
)

// conceal is the only place that reads the private mode, so masking one more
// surface is another call to it instead of another condition inside a renderer.
// A surface hands it the task text it would paint and the placeholder to paint
// instead; a surface that keeps nothing of its own passes no text and paints
// whatever comes back.
func (m browserModel) conceal(text, placeholder string) string {
	if !m.private {
		return text
	}
	return placeholder
}

// A notice is assembled when the action finishes, so it cannot decide there
// whether to keep a task title: private mode can be switched on afterwards
// while the notice still stands. The handler marks the title instead and the
// status bar resolves the mark through conceal as it paints.
const (
	titleMarkOpen  = "\x01"
	titleMarkClose = "\x02"
)

// markTitle marks task text inside a notice. Marks carried by the text itself
// are dropped: they would end the span early and paint the rest of the title.
func markTitle(text string) string {
	return titleMarkOpen + strings.NewReplacer(titleMarkOpen, "", titleMarkClose, "").Replace(text) + titleMarkClose
}

// revealTitles resolves the marked titles of s: the title itself outside
// private mode, the placeholder inside it. Text without marks is returned
// unchanged, so notices that carry no task content stay readable either way.
func (m browserModel) revealTitles(s string) string {
	var b strings.Builder
	for {
		open := strings.Index(s, titleMarkOpen)
		if open < 0 {
			break
		}
		b.WriteString(s[:open])
		rest := s[open+len(titleMarkOpen):]
		end := strings.Index(rest, titleMarkClose)
		if end < 0 {
			return b.String() + m.conceal(rest, hiddenTitle)
		}
		b.WriteString(m.conceal(rest[:end], hiddenTitle))
		s = rest[end+len(titleMarkClose):]
	}
	return b.String() + strings.ReplaceAll(s, titleMarkClose, "")
}

// searchView paints the search input, or its placeholder while private mode is
// on: the filter is typed from task titles and stays on screen once applied.
func (m browserModel) searchView(width int) string {
	if hidden := m.conceal("", hiddenTitle); hidden != "" {
		return fit(hidden, width)
	}
	return m.input.view(width)
}
