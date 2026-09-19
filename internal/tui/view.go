package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
)

func display(s string) string {
	s = strconv.Quote(s)
	return s[1 : len(s)-1]
}

func displayClipped(s string, width int) string {
	if width <= 0 {
		return ""
	}
	limit := width*8 + 32
	if len(s) > limit {
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		s = s[:limit] + "..."
	}
	return ansi.Truncate(display(s), width, "...")
}

func fit(s string, width int) string {
	width = max(0, width)
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

type listEntry struct {
	name  string
	query app.BrowseQuery
}

func (m browserModel) entries() []listEntry {
	entries := []listEntry{{"Today", app.BrowseQuery{View: app.TodayView}},
		{"All open", app.BrowseQuery{View: app.OpenView}}, {"Completed", app.BrowseQuery{View: app.CompletedView}}}
	for _, p := range m.projects {
		name := p.Name
		if p.Closed {
			name += " (closed)"
		}
		entries = append(entries, listEntry{name, app.BrowseQuery{View: app.ProjectView, ProjectID: p.Id}})
	}
	return entries
}

func (m browserModel) entryIndex(entries []listEntry) int {
	for i, e := range entries {
		if e.query.View == m.query.View && e.query.ProjectID == m.query.ProjectID {
			return i
		}
	}
	return -1
}

func (m browserModel) queryTitle() string {
	entries := m.entries()
	if i := m.entryIndex(entries); i >= 0 {
		return displayClipped(entries[i].name, max(1, m.width))
	}
	return "Missing list: " + displayClipped(m.query.ProjectID, max(1, m.width))
}

func (m browserModel) View() tea.View {
	mode := []string{"normal", "half", "full"}[m.mode]
	header := "tt ui | " + m.queryTitle() + " | " + mode
	if m.stateErr != nil {
		header += " | Queue unavailable"
	} else if m.stateKnown {
		q := m.cacheState.Queue
		header += fmt.Sprintf(" | Queue %d pending / %d in flight / %d held", q.Pending, q.Inflight, q.Failed)
		if q.Unknown > 0 {
			header += fmt.Sprintf(" / %d unknown", q.Unknown)
		}
	}
	if m.width < 120 && m.stateKnown {
		q := m.cacheState.Queue
		suffix := fmt.Sprintf(" | %s | Q:%d/%d/%d", mode, q.Pending, q.Inflight, q.Failed)
		if q.Unknown > 0 {
			suffix += fmt.Sprintf("/?%d", q.Unknown)
		}
		header = "tt ui | " + ansi.Truncate(m.queryTitle(), max(0, m.width-8-ansi.StringWidth(suffix)), "") + suffix
	}
	lines := []string{m.accent.Render(header)}
	status := "Local cache | Auto refresh 2s"
	if m.notice != "" {
		status = displayClipped(m.revealTitles(m.notice), max(1, m.width))
	}
	if m.loading {
		status = "Loading cache..."
	}
	if m.query.Search != "" {
		status = "Search: " + displayClipped(m.conceal(m.query.Search, hiddenTitle), max(1, m.width))
	}
	if m.selectionNotice != "" {
		status = m.selectionNotice
	}
	if m.searching {
		status = "Search: " + m.searchView(max(1, m.width-8))
	}
	if m.actions != nil && m.notice != "" && !m.searching {
		status = displayClipped(m.revealTitles(m.notice), max(1, m.width))
	}
	if m.form != nil && m.form.err != "" {
		status = "Not saved: " + displayClipped(m.form.err, max(1, m.width))
	}
	if m.checklist != nil && m.checklist.form != nil && m.checklist.form.err != "" {
		status = "Not saved: " + displayClipped(m.checklist.form.err, max(1, m.width))
	}
	if m.busy {
		status = "Saving local operation..."
	}
	if m.sync != nil {
		status = m.sync.summary()
	}
	if m.boardContext() && !m.loading && m.notice == "" && m.selectionNotice == "" && m.query.Search == "" {
		status = "Kanban | Local cards | R fetch columns | C manage columns"
		if m.board.loading {
			status = "Loading Kanban columns..."
		}
		if m.board.err != "" {
			status = displayClipped(m.board.err, max(1, m.width))
		}
	}
	if m.resource != nil {
		status = "Local resources | R explicitly refreshes from TickTick"
		if m.notice != "" {
			status = displayClipped(m.revealTitles(m.notice), max(1, m.width))
		}
		if m.resource.err != "" {
			status = displayClipped(m.resource.err, max(1, m.width))
		}
		if m.resource.working {
			status = "Preparing or applying the exact local operation..."
		}
	}
	if m.serverTasks != nil {
		status = "Read-only server query | Local tasks and references are unchanged"
		if m.serverTasks.loading {
			status = "Loading exact server query..."
		}
		if m.serverTasks.err != "" {
			status = displayClipped(m.serverTasks.err, max(1, m.width))
		}
	}
	if focus := m.focusStatus(); focus != "" && !m.searching && m.form == nil {
		status = focus + " | " + status
	}
	lines = append(lines, m.muted.Render(status))
	if !m.usable() {
		lines = append(lines, "Terminal too small (need 32x10).")
		if m.form != nil || m.dialog != nil {
			lines = append(lines, "Resize to review the open form.")
		} else if m.sync != nil {
			lines = append(lines, "Esc cancels sync; Ctrl+C exits.")
		} else {
			lines = append(lines, "Resize or q to quit.")
		}
	} else {
		switch {
		case m.help.ShowAll:
			body := m.helpLines()
			start := min(m.help.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel("Help - "+paneName(m.focus), body[start:], m.width, m.height-4, m.focus)...)
		case m.palette != nil:
			title, body := m.paletteView()
			lines = append(lines, m.panel(title, body, m.width, m.height-4, m.focus)...)
		case m.serverTasks != nil:
			title, body := m.serverTasksView()
			lines = append(lines, m.panel(title, body, m.width, m.height-4, m.focus)...)
		case m.resource != nil:
			title, body := m.resourceView()
			lines = append(lines, m.panel(title, body, m.width, m.height-4, m.focus)...)
		case m.recovery != nil:
			body := m.recoveryLines()
			start := min(m.recovery.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel("Editor result", body[start:], m.width, m.height-4, m.focus)...)
		case m.form != nil:
			title, body := m.formView()
			lines = append(lines, m.panel(title, body, m.width, m.height-4, m.focus)...)
		case m.dialog != nil:
			title, body := m.dialogView()
			start := min(m.dialog.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel(title, body[start:], m.width, m.height-4, m.focus)...)
		case m.sync != nil:
			body := m.syncLines()
			start := min(m.sync.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel("Explicit sync", body[start:], m.width, m.height-4, m.focus)...)
		case m.taskOperation != nil:
			title, body := m.taskOperationView()
			start := min(m.taskOperation.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel(title, body[start:], m.width, m.height-4, m.focus)...)
		case m.jump:
			lines = append(lines, m.panel("Jump to panel", []string{
				"[1] Lists", "[2] Tasks", "[3] Preview", "Esc or Ctrl+O cancels.",
				"Panel digits are not CLI task numbers.",
			}, m.width, m.height-4, m.focus)...)

		case m.systemPanel != nil:
			body := m.systemLines()
			start := min(m.systemPanel.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel("Settings and diagnostics", body[start:], m.width, m.height-4, m.focus)...)
		case m.timer != nil:
			title, body := m.timerView()
			start := min(m.timer.offset, max(0, len(body)-(m.height-6)))
			if m.timer.review != nil {
				start = 0
			}
			if f := m.timer.form; f != nil && !f.discard && m.timer.confirm == nil && m.timer.report == "" {
				start = max(0, f.field-max(1, m.height-6)+1)
			}
			lines = append(lines, m.panel(title, body[start:], m.width, m.height-4, m.focus)...)
		case m.checklist != nil:
			title, body := m.checklistView()
			lines = append(lines, m.panel(title, body, m.width, m.height-4, m.focus)...)
		case m.queue != nil:
			title, body := m.queueView()
			start := min(m.queue.offset, max(0, len(body)-(m.height-6)))
			lines = append(lines, m.panel(title, body[start:], m.width, m.height-4, m.focus)...)
		case m.queries == nil && m.err != nil:
			body := append([]string{"Cannot read the cache:", ""}, wrapText(m.err.Error(), m.width-4)...)
			lines = append(lines, m.panel("Cache error", body[min(m.errorOffset, max(0, len(body)-(m.height-6))):], m.width, m.height-4, m.focus)...)
		case m.board != nil && m.focus == tasksPane:
			lines = append(lines, m.boardView()...)
		default:
			lines = append(lines, m.panels()...)
		}
		global, local := m.footers()
		lines = append(lines, m.muted.Render(global), m.accent.Render(local))
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = fit(lines[i], m.width)
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	v.ReportFocus = true
	return v
}

func (m browserModel) panel(title string, body []string, width, height int, target pane) []string {
	style := m.muted
	if m.focus == target {
		title = "* " + title
		style = m.accent
	}
	label := ansi.Truncate(" "+title+" ", width-2, "")
	lines := []string{style.Render("+" + label + strings.Repeat("-", max(0, width-2-ansi.StringWidth(label))) + "+")}
	for i := 0; i < height-2; i++ {
		text := ""
		if i < len(body) {
			text = body[i]
		}
		lines = append(lines, style.Render("|")+" "+fit(text, width-4)+" "+style.Render("|"))
	}
	return append(lines, style.Render("+"+strings.Repeat("-", width-2)+"+"))
}

func (m browserModel) paneContent(p pane, width, rows int) (string, []string) {
	switch p {
	case listsPane:
		entries := m.entries()
		selected := m.entryIndex(entries)
		start := max(0, selected-rows+1)
		var lines []string
		for i := start; i < min(len(entries), start+rows); i++ {
			text := marker(i == selected) + displayClipped(entries[i].name, max(1, m.width))
			if i == selected && m.focus == listsPane {
				text = m.accent.Render(text)
			}
			lines = append(lines, text)
		}
		return "Lists", lines
	case tasksPane:
		title := fmt.Sprintf("Tasks (%d) - %s", len(m.tasks), m.queryTitle())
		if m.err != nil {
			body := append([]string{"Cannot read tasks:", ""}, wrapText(m.err.Error(), width)...)
			return title, body[min(m.errorOffset, max(0, len(body)-rows)):]
		}
		if m.loading && len(m.tasks) == 0 {
			return title, []string{"Loading cache..."}
		}
		if len(m.tasks) == 0 {
			if len(m.projects) == 0 {
				return title, []string{"Cache is empty.", "Press s to sync and populate it."}
			}
			return title, []string{"No matching tasks."}
		}
		var lines []string
		start := max(0, m.taskIndex()-rows+1)
		for _, t := range m.tasks[start:min(len(m.tasks), start+rows)] {
			box := "[ ]"
			if t.Status.Done() {
				box = "[x]"
			}
			if t.Kind == "NOTE" {
				box = "[N]"
			}
			prefix := marker(t.Id == m.taskID) + box + " "
			due := m.dues[t.Id]
			suffix := ""
			if due != "" && due != "--" {
				suffix = " | " + display(due)
			}
			if ansi.StringWidth(suffix) > width/2 {
				suffix = ansi.Truncate(suffix, width/2, "...")
			}
			title := m.conceal(t.Title, hiddenTitle)
			text := fit(prefix+displayClipped(title, max(0, width-len(prefix)-ansi.StringWidth(suffix))), max(0, width-ansi.StringWidth(suffix))) + suffix
			if t.Id == m.taskID && m.focus == tasksPane {
				text = m.accent.Render(text)
			}
			lines = append(lines, text)
		}
		return title, lines
	default:
		if hidden := m.conceal("", hiddenPreview); hidden != "" {
			return "Preview", wrapText(hidden, width)
		}
		if m.selectionNotice != "" {
			return "Preview", wrapText(m.selectionNotice, width)
		}
		if m.detailErr != nil {
			body := append([]string{"Cannot read task:", ""}, wrapText(m.detailErr.Error(), width)...)
			return "Preview", body[min(m.detailOffset, max(0, len(body)-rows)):]
		}
		if m.detail == nil {
			if m.detailLoading {
				return "Preview", []string{"Loading task..."}
			}
			return "Preview", []string{"Select a task for details."}
		}
		start := m.previewOffset()
		return fmt.Sprintf("Preview %d-%d/%d", start+1, min(start+rows, len(m.detailLines)), len(m.detailLines)), m.detailLines[start:]
	}
}

func paneName(p pane) string { return []string{"Lists", "Tasks", "Preview"}[p] }
func marker(selected bool) string {
	if selected {
		return "> "
	}
	return "  "
}

func (m browserModel) boardContext() bool {
	return m.board != nil && m.focus == tasksPane && !m.busy && !m.searching && !m.jump &&
		m.form == nil && m.dialog == nil && m.sync == nil && m.timer == nil &&
		m.queue == nil && m.checklist == nil && m.systemPanel == nil && m.taskOperation == nil &&
		m.recovery == nil && m.resource == nil && m.serverTasks == nil && m.palette == nil
}

func (m browserModel) footers() (string, string) {
	if !m.help.ShowAll {
		if m.palette != nil {
			return "Workspaces: type to filter", "Up/down choose Enter open Esc back"
		}
		if m.serverTasks != nil {
			if m.serverTasks.editing {
				return "Tab fields ^S cache ^R remote", "Esc close | RFC3339 dates"
			}
			return "j/k rows Enter source e query", "r cache R remote Esc back"
		}
		if m.resource != nil {
			return m.resourceFooters()
		}
		if m.boardContext() {
			return "b back R fetch C columns ? help", "Alt+Left/Right view Alt+Shift+Left/Right move"
		}
	}
	global := "Global: 1/2/3 pane | Tab focus | ^O hints | +/_ size | / search | r refresh | ? help | q quit"
	if m.width < 80 {
		global = "1/2/3 pane ^O hints +/_ size ? help q quit"
	}
	local := paneName(m.focus) + ": j/k move | Enter open | PgUp/PgDn page | Home/End | Esc back"
	if m.focus == previewPane {
		local = "Preview: j/k scroll | PgUp/PgDn page | Home/End | Esc back"
	}
	if m.width < 80 {
		local = paneName(m.focus) + ": j/k move Enter open"
		if m.focus == previewPane {
			local = "Preview: j/k PgUp/PgDn Home/End"
		}
	}
	if m.jump {
		local = "Jump: 1 Lists | 2 Tasks | 3 Preview | Esc cancel"
		if m.width < 60 {
			local = "Jump: 1/2/3 select | Esc cancel"
		}
	}
	if m.help.ShowAll {
		local = "Help: j/k scroll | PgUp/PgDn | ?/Esc close"
		if m.width < 60 {
			local = "Help: j/k scroll ?/Esc close"
		}
	}
	if m.searching {
		global = "Search input | Ctrl+C interrupt"
		local = "Enter apply | Esc cancel | Ctrl+U clear"
		if m.width < 40 {
			local = "Enter apply Esc cancel ^U clear"
		}
	}
	if m.actions != nil && !m.searching && !m.jump && !m.help.ShowAll {
		if m.width >= 120 {
			global = "Global: 1/2/3 pane | Tab focus | ^O hints | +/_ size | / search | r refresh | a task n note | u undo | t focus s sync Q queue | S settings | ? help | q quit"
		} else if m.width >= 80 {
			global = "1/2/3 pane ^O hints +/_ size / search r refresh a task n note u undo t focus s sync Q queue S settings ? help q quit"
		} else {
			global = "1/2/3 ^O +/_ a n t u s Q ? q"
		}
		if m.width >= 80 {
			local = paneName(m.focus) + ": j/k | Enter open | c checklist | e edit E editor | d schedule | Space done | ^D delete m move"
		} else {
			local = paneName(m.focus) + ": j/k c e E d ^D m Space"
		}
	}
	if m.checklist != nil {
		global, local = m.checklistFooters()
	}
	if m.queue != nil && !m.jump && !m.help.ShowAll {
		global, local = "Queue: Tab section r refresh s sync ? help", "j/k select Enter details f recover Esc back"
		if m.width < 60 {
			global, local = "Tab tasks/focus r s ?", "j/k Enter f Esc"
		}
		if m.queue.confirmation != nil {
			global, local = "Recovery: 1 exact operation", "Enter confirm Esc cancel j/k scroll"
		}
	}
	if m.timer != nil && !m.jump && !m.help.ShowAll {
		global, local = "Focus: a selected A default N none p pause x stop ^D cancel", "Tab history j/k Enter e note U upload f retry r refresh s sync u undo ? Esc"
		if m.width < 80 {
			global, local = "Focus: Tab a p x ^D r ?", "j/k Enter e U f s u Esc"
		}
		if m.timer.history {
			global, local = "Focus history: Tab active | U upload | f retry | r refresh | ? help", "j/k select | Enter details | e note | s sync | u undo | Esc back"
			if m.width < 80 {
				global, local = "History: Tab U f r ?", "j/k Enter e s u Esc"
			}
		}
		if m.timer.form != nil {
			global, local = "Tab fields | arrows choose | ^S preview", "Enter adds note newline | Esc back"
		}
		if m.timer.confirm != nil {
			global, local = "Exact focus target: 1", "Enter confirm | Esc cancel | j/k scroll"
		}
		if m.timer.report != "" {
			global, local = "Focus result", "j/k scroll | Esc close"
		}
		if m.timer.review != nil {
			global, local = "Tab input/Save/Skip | ^S save", "Esc later | PgUp/PgDn details"
			if m.timer.review.discard {
				global, local = "Unsaved completion note", "s save | d discard | Esc edit"
			}
		}
	}
	if m.taskOperation != nil {
		global, local = "Task operation | Exact target", "Enter preview/confirm Esc back j/k scroll"
	}
	if m.form != nil {
		global = "Form: Tab fields | Ctrl+S save | Ctrl+E editor | Esc back"
		local = "Title: Enter body | Body: Enter newline | Priority/List: arrows"
		if m.width < 80 {
			global, local = "Tab fields ^S save ^E editor Esc", "Body: Enter newline; arrows move"
		}
		if m.form.schedule != nil {
			global = "Schedule: Tab fields | Ctrl+S save | Esc back"
			if m.width < 80 {
				global = "Tab fields ^S save Esc back"
			}
			local = "Date: ^X clear | Repeat: ^P preset | Reminders: ^N add ^D remove ^X clear, Alt+arrows move"
			if m.width < 100 {
				local = "^N add ^D drop ^X clear ^P preset"
			}
		} else if m.form.field == 4 {
			local = "in 2h / +2h / tmr 09:00; empty skips"
		}
		if m.form.discard {
			global, local = "Unsaved changes", "s save d discard Esc continue"
		}
		if f := m.form.interval; f != nil && !m.form.discard {
			if m.form.intervalField() >= 0 {
				global, local = "Tab fields ^S review Esc back", "Start + h/min; end is due"
			}
			if f.review != nil {
				global, local = "s save Esc cancel", "j/k scroll Home/End; cached only"
			}
		}
	}
	if m.dialog != nil {
		global, local = "Confirm: Enter apply | Esc cancel", "j/k scroll"
		if m.dialog.kind == "complete" {
			local = "Tab: close checklist items too"
			if m.dialog.keepItems {
				local = "Tab: leave checklist unchanged"
			}
		}
	}
	if m.sync != nil {
		global, local = "Explicit sync | Local work is durable", "j/k scroll | Esc close"
		if m.sync.running {
			local = "Esc cancel | Ctrl+C exit"
		}
	}
	if m.busy {
		local = "Working... repeated keys ignored"
	}
	if m.systemPanel != nil && !m.jump && !m.help.ShowAll {
		global, local = "Settings: r reload g focus i init d doctor f fix n test l/L login ? help", "j/k scroll | Esc back | q quit"
		if m.systemPanel.focus != nil {
			global, local = "Choose default focus", "Tab lists/tasks | j/k select | Enter review | n none | Esc cancel"
		}
		if m.systemPanel.preview != nil {
			global, local = "Review exact action", "j/k scroll | Enter confirm | Esc cancel"
		}
		if m.systemWorking {
			global, local = "Settings operation running", "Esc cancel | Ctrl+C exit"
		}
	}
	if m.recovery != nil {
		global, local = "Editor result | Esc returns", "j/k scroll | Draft paths print on exit"
	}
	if m.help.ShowAll {
		return "Help: j/k PgUp/PgDn Home/End", "F1 / ? / Esc: close help"
	}
	if m.width < 80 && m.form == nil && m.dialog == nil && m.sync == nil && m.timer == nil && m.queue == nil && m.checklist == nil && m.systemPanel == nil && m.taskOperation == nil && !m.searching && !m.jump {
		global = "1/2/3 ^O +/_ a n t u s Q S ? q"
	}
	if m.actions != nil && m.form == nil && m.dialog == nil && m.sync == nil && m.timer == nil && m.queue == nil && m.checklist == nil && m.systemPanel == nil && m.taskOperation == nil && m.recovery == nil && !m.searching && !m.jump {
		if m.width >= 120 {
			return "F1 help | : workspaces b board | 1/2/3 panels Tab ^O +/_ / r | a n u | t focus s sync Q queue S settings | q quit", local
		}
		if m.width >= 60 {
			return "F1 help | : workspaces b board | 1/2/3 ^O / r a n u t s Q S q", paneName(m.focus) + ": j/k Enter | c checklist e edit E editor d dates | ^D delete m move"
		}
		return "F1 help | : b 1/2/3 t Q S s q", paneName(m.focus) + ": j/k Enter Esc back"
	}
	if m.width < 60 {
		if m.form != nil && !m.form.discard {
			global = "^S save Esc back"
			if f := m.form.interval; f != nil {
				if m.form.intervalField() >= 0 {
					global = "^S review Esc back"
				}
				if f.review != nil {
					global = "s save Esc cancel"
				}
			}
		}
		if m.systemPanel != nil && m.systemPanel.preview == nil && !m.systemWorking {
			global = "r i d f n l/L"
		}
	}
	return "F1 help | " + global, local
}

func (m browserModel) helpLines() []string {
	if lines := m.modalHelp(); lines != nil {
		var wrapped []string
		for _, line := range append(lines, "F1 or Esc: close help and return without changing the draft/action") {
			wrapped = append(wrapped, wrapText(line, max(1, m.width-4))...)
		}
		return wrapped
	}

	if m.systemPanel != nil {
		var lines []string
		for _, line := range systemHelp() {
			lines = append(lines, wrapText(line, max(1, m.width-4))...)
		}
		return lines
	}
	if m.timer != nil {
		var lines []string
		for _, line := range timerHelp() {
			lines = append(lines, wrapText(line, max(1, m.width-4))...)
		}
		return lines
	}

	if m.queue != nil {
		var lines []string
		for _, line := range queueHelp() {
			lines = append(lines, wrapText(line, max(1, m.width-4))...)
		}
		return lines
	}
	if m.checklist != nil {
		var lines []string
		for _, line := range checklistHelp() {
			lines = append(lines, wrapText(line, max(1, m.width-4))...)
		}
		return lines
	}
	lines := []string{
		"Global keys", ": opens project/folder/tag/habit/countdown/comment/queue/focus workspaces", "b: toggle Kanban for the selected project; C manages its columns", "1/2/3: select Lists, Tasks or Preview directly", "Ctrl+O: show panel hints; Esc or Ctrl+O cancels them",
		"Ctrl+K: private mode hides task titles, Kanban cards and details",
		"Private mode also hides the search filter; forms keep their own draft",
		"Ctrl+K answers in every context; the mode is never kept between runs",
		"Tab/l/right: next panel", "Shift+Tab/h/left: previous panel",
		"+: next size (normal -> half -> full)", "_: previous size; Esc restores normal size",
		"/: search this view; Enter applies, Esc cancels", "Empty search removes the search filter",
		"r: refresh local cache; automatic refresh every 2s", "?: help outside text input; F1: help in every context; F1/?/Esc closes help",
		"q: quit; Ctrl+C: interrupt", "", "Local keys - " + paneName(m.focus),
		"Up/k, down/j: previous/next row", "PgUp/PgDn: previous/next page", "Home/End: first/last row",
	}
	if m.focus == previewPane {
		lines = append(lines, "Scroll reads the complete body and checklist.")
	} else {
		lines = append(lines, "Enter: open next panel; Esc: back")
	}
	lines = append(lines, "", "Search input: arrows, Home/End, Backspace/Delete", "Ctrl+U clears; shortcuts type as text while searching.",
		"Search matches title, body, tags and list as one phrase.", "Panel digits never change CLI task numbers.",
		"S: effective settings, local account status, version, explicit doctor/fix/test/login",
		"t: local timer/Pomodoro, history and selected focus upload; ? there shows controls",
		"c: open the selected task checklist; ? there shows item keys",
		"a: create a TEXT task; n: create a NOTE; e: edit title/body/priority",
		"E (Shift+E): selected task/note in VISUAL, then EDITOR",
		"Ctrl+E in a basic form: current draft in the external editor; End moves the cursor",
		"Save and close the editor to apply locally; unchanged document returns without saving",
		"Existing kind/id/project are read-only; Markdown checkboxes remain body text",
		"Editor errors/conflicts keep a private draft; recovery paths also print on TUI exit",
		"Create: Remind at field accepts in 2h, +2h, tmr 09:00 or 18:00",
		"Remind at sets a due time and one reminder at that time; empty skips",
		"d: schedule the selected task; Tab switches Date/Repeat/Reminders",
		"Date: in 2h / +2h / fri 18:00; no time means all-day; empty keeps dates",
		"Ctrl+X clears start/due and all-day in Date, repeat in Repeat, or all reminders",
		"Repeat: Ctrl+P cycles none/daily/weekly/monthly/yearly; raw RRULE: is supported",
		"Reminders: Ctrl+N adds, Ctrl+D removes, up/down selects, Alt+up/down reorders",
		"Reminder values: at, -10min, -1h or raw TRIGGER:; order is preserved",
		"Relative date previews are frozen on input; saving uses that exact time",
		"Changed reminder lists reject duplicates; untouched raw fields stay intact", "Space: confirm task completion; Tab chooses checklist policy",
		"u: preview and confirm the last global change, including CLI changes",
		"Undo is not a selected-task reopen and does not promise remote rollback",
		"Forms: Tab/Shift+Tab fields; Ctrl+S saves locally; Esc save/discard/continue",
		"Body: Enter newline; arrows move; pasted shortcuts remain text",
		"s: explicit sync; Esc cancels the running pass or closes its report",
		"Q (Shift+Q): task sync queue; Tab switches to read-only focus uploads",
		"Queue: Enter reads details; f previews recovery of 1 exact operation; s sends separately",
		"Ctrl+D in browser panels: preview deletion of 1 task and its queued operations; Enter confirms",
		"m: choose destination, then confirm recreate/delete with new IDs and no undo",
		"Concurrent changes invalidate confirmations; new search matches never join the target set",
		"Sync reports remote confirmations, pending/held work, errors and cancellation separately",
		"Queue p/i/h (or Q: pending/inflight/held) counts all local task operations",
		"No background HTTP. Local edits do not automatically sync.")
	var wrapped []string
	for _, line := range lines {
		for _, paragraph := range cli.Wrap(line, max(1, m.width-4)) {
			wrapped = append(wrapped, wrapText(paragraph, max(1, m.width-4))...)
		}
	}
	return wrapped
}
