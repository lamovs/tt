package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type rectangle struct{ x, y, width, height int }

func (m browserModel) usable() bool { return m.width >= 32 && m.height >= 10 }

func (m browserModel) layout() map[pane]rectangle {
	w, h := m.width, max(1, m.height-4)
	if m.mode == fullMode || w < 80 {
		return map[pane]rectangle{m.focus: {0, 0, w, h}}
	}
	if m.mode == normalMode && w >= 120 && h >= 13 {
		left := min(42, max(30, w/3))
		top := max(5, min(8, h/3))
		return map[pane]rectangle{
			listsPane: {0, 0, left, top}, tasksPane: {0, top, left, h - top},
			previewPane: {left + 1, 0, w - left - 1, h},
		}
	}
	left := max(26, w/3)
	if m.mode == halfMode {
		left = (w - 1) / 2
	}
	a, b := tasksPane, previewPane
	if m.focus == listsPane {
		a, b = listsPane, tasksPane
	}
	return map[pane]rectangle{a: {0, 0, left, h}, b: {left + 1, 0, w - left - 1, h}}
}

func (m browserModel) paneRows(p pane) int {
	return max(1, m.layout()[p].height-2)
}

func (m browserModel) previewOffset() int {
	return min(max(0, m.detailOffset), max(0, len(m.detailLines)-m.paneRows(previewPane)))
}

type detailRendered struct {
	id                           string
	generation, snapshot, render uint64
	width                        int
	lines                        []string
}

func (m *browserModel) reflow() tea.Cmd {
	if m.detail == nil || !m.usable() {
		return nil
	}
	rect, visible := m.layout()[previewPane]
	if !visible {
		return nil
	}
	width := max(1, rect.width-4)
	if m.detailWidth == width || m.renderWidth == width {
		return nil
	}
	m.renderGeneration++
	m.renderWidth = width
	copy := *m
	return m.gate.command(func() tea.Msg {
		if copy.ctx != nil && copy.ctx.Err() != nil {
			return nil
		}
		return detailRendered{id: copy.detail.Id, generation: copy.detailGeneration, snapshot: copy.generation, render: copy.renderGeneration, width: width, lines: copy.fullDetailLines(width)}
	})
}

func (m browserModel) panels() []string {
	area := make([]string, max(1, m.height-4))
	for i := range area {
		area[i] = fit("", m.width)
	}
	layout := m.layout()
	for _, p := range []pane{listsPane, tasksPane, previewPane} {
		r, visible := layout[p]
		if !visible {
			continue
		}
		title, body := m.paneContent(p, r.width-4, r.height-2)
		lines := m.panel(title, body, r.width, r.height, p)
		for i, line := range lines {
			y := r.y + i
			area[y] = ansi.Cut(area[y], 0, r.x) + line + ansi.Cut(area[y], r.x+r.width, m.width)
		}
	}
	return area
}
