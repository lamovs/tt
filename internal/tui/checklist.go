package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type itemIntent struct {
	original model.Task
	change   store.ItemChange
	position int
}

type itemForm struct {
	intent             itemIntent
	title              searchInput
	initial, err       string
	discard, quitAfter bool
}

func (f *itemForm) dirty() bool { return string(f.title.value) != f.initial }

type checklistPanel struct {
	taskID, key string
	lost        bool
	form        *itemForm
	confirm     *itemIntent
	inspect     *itemIntent
	offset      int
	pending     itemIntent
}

func (m *browserModel) openChecklist() {
	m.checklist = &checklistPanel{taskID: m.taskID}
	m.reconcileChecklist()
	m.notice = "Checklist: changes save locally; s explicitly syncs."
}

func selectedItem(items []model.Item, key string) int {
	index := -1
	if key == "" {
		return -1
	}
	for i, item := range items {
		if item.Key == key {
			if index >= 0 {
				return -1
			}
			index = i
		}
	}
	return index
}

func (m *browserModel) reconcileChecklist() {
	p := m.checklist
	if p == nil || m.detail == nil || m.detail.Id != p.taskID {
		return
	}
	if p.key != "" && selectedItem(m.detail.Items, p.key) < 0 {
		p.key, p.lost = "", true
		m.notice = "Selected item disappeared. Select an item to continue."
	} else if p.key == "" && !p.lost && len(m.detail.Items) > 0 {
		p.key = m.detail.Items[0].Key
	}
}

func (m browserModel) checklistProblem() string {
	if m.checklist == nil || m.detail == nil || m.taskID != m.checklist.taskID || m.detail.Id != m.checklist.taskID {
		return "Task unavailable. Esc returns to the browser."
	}
	if m.loading || m.detailLoading {
		return "Refreshing checklist; wait before changing it."
	}
	if m.err != nil {
		return m.err.Error()
	}
	if m.detailErr != nil {
		return m.detailErr.Error()
	}
	if m.checklistErr != nil {
		return m.checklistErr.Error()
	}
	return m.checklistState.Refusal
}

func (m browserModel) checklistKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, s := m.checklist, msg.String()
	if p.form != nil {
		return m.itemFormKey(msg)
	}
	if p.inspect != nil {
		if s == "esc" || s == "enter" {
			p.inspect = nil
		} else {
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.itemInspectionLines()))
		}
		return m, nil
	}
	if p.confirm != nil {
		switch s {
		case "esc":
			p.confirm = nil
		case "enter":
			return m.submitItem(*p.confirm)
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.itemTargetLines(*p.confirm))+2)
		}
		return m, nil
	}
	switch s {
	case "esc":
		m.checklist = nil
		return m, nil
	case "q":
		return m, tea.Quit
	case "?":
		m.help.ShowAll, m.help.offset = true, 0
		return m, nil
	case "ctrl+o":
		m.jump = true
		return m, nil
	case "tab", "shift+tab":
		m.checklist = nil
		if s == "tab" {
			m.focus = (m.focus + 1) % 3
		} else {
			m.focus = (m.focus + 2) % 3
		}
		m.reflow()
		return m, nil
	case "r":
		return m.load()
	case "u":
		return m.beginUndo()
	case "s":
		return m.beginSync()
	}
	if problem := m.checklistProblem(); problem != "" {

		if s == "a" || s == "e" || s == "space" || s == "ctrl+d" || s == "alt+up" || s == "alt+down" {
			m.notice = "Not saved: " + problem
			return m, nil
		}
	}
	if m.detail == nil || m.detail.Id != p.taskID {
		return m, nil
	}
	items := m.detail.Items
	i := selectedItem(items, p.key)
	intent := itemIntent{original: *m.detail, position: i + 1, change: store.ItemChange{Key: p.key}}
	if s == "a" {
		intent.change.Action = store.ItemAdd
		p.form = &itemForm{intent: intent}
		return m, nil
	}
	if i < 0 && (s == "e" || s == "space" || s == "ctrl+d" || s == "alt+up" || s == "alt+down") {
		m.notice = "Select an item first; no action was applied."
		return m, nil
	}
	switch s {
	case "enter":
		p.inspect, p.offset = &intent, 0
	case "e":
		if !utf8.ValidString(items[i].Title) {
			m.notice = "Not saved: item title is not UTF-8; original data retained."
			return m, nil
		}
		intent.change.Action = store.ItemRename
		p.form = &itemForm{intent: intent, initial: items[i].Title}
		p.form.title.set(items[i].Title)
	case "space":
		intent.change.Action, intent.change.Done = store.ItemSetDone, !items[i].Status.Done()
		return m.submitItem(intent)
	case "ctrl+d":
		intent.change.Action = store.ItemRemove
		p.confirm, p.offset = &intent, 0
	case "alt+up", "alt+down":
		destination := i - 1
		if s == "alt+down" {
			destination = i + 1
		}
		if destination < 0 || destination >= len(items) {
			m.notice = "Unchanged; item is already at the edge."
			return m, nil
		}
		intent.change.Action, intent.change.DestinationKey = store.ItemMove, items[destination].Key
		return m.submitItem(intent)
	default:
		next := scroll(i, s, max(1, m.height-9), len(items))
		if next >= 0 && next < len(items) && next != i {
			p.key, p.lost = items[next].Key, false
		}
	}
	return m, nil
}

func (m browserModel) submitItem(intent itemIntent) (browserModel, tea.Cmd) {
	if intent.change.Action == store.ItemAdd || intent.change.Action == store.ItemRename {
		if err := app.ValidateItemTitle(intent.change.Title); err != nil {
			m.checklist.form.err = err.Error()
			return m, nil
		}
	}
	m.checklist.pending = intent
	actions, ctx := m.actions, m.ctx
	return m.actionCommand("checklist", func() actionFinished {
		out, err := actions.Checklist(ctx, intent.original, intent.change)
		return actionFinished{outcome: out, err: err}
	})
}

func (m browserModel) finishChecklist(msg actionFinished) (browserModel, tea.Cmd) {
	p := m.checklist
	if p == nil {
		return m, nil
	}
	if msg.err != nil {
		m.notice = "Not saved: " + msg.err.Error()
		if p.form != nil {
			p.form.err, p.form.quitAfter, p.form.discard = msg.err.Error(), false, false
		}
		p.confirm = nil
		return m.load()
	}
	quit := p.form != nil && p.form.quitAfter
	p.form, p.confirm = nil, nil
	m.notice = "Unchanged; no local mutation."
	if msg.outcome.Changed {
		m.notice = "Saved locally (checklist " + string(p.pending.change.Action) + "). Remote confirmation requires sync."
		if p.pending.change.Action == store.ItemAdd {
			for _, item := range msg.outcome.Task.Items {
				if selectedItem(p.pending.original.Items, item.Key) < 0 {
					p.key, p.lost = item.Key, false
				}
			}
		}
		if p.pending.change.Action == store.ItemRemove {
			p.key, p.lost = "", true
			m.notice += " Select an item to continue."
		}
	}
	if quit {
		return m, tea.Quit
	}
	return m.load()
}

func (m *browserModel) pasteItem(text string) {
	f := m.checklist.form
	if f.discard {
		return
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) || len(text)+len(string(f.title.value)) > 4<<20 {
		f.err = "Paste refused: use UTF-8 without NUL, at most 4 MiB."
		return
	}
	f.title.insertExact(text)
	f.err = ""
}

func (m browserModel) itemFormKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	f, s := m.checklist.form, msg.String()
	if f.discard {
		switch s {
		case "s", "ctrl+s":
			f.discard = false
			return m.saveItemForm()
		case "d":
			quit := f.quitAfter
			m.checklist.form, m.notice = nil, "Draft discarded; no local mutation."
			if quit {
				return m, tea.Quit
			}
		case "esc":
			f.discard, f.quitAfter = false, false
		}
		return m, nil
	}
	switch s {
	case "esc":
		if f.dirty() {
			f.discard = true
		} else {
			m.checklist.form = nil
		}
	case "ctrl+s":
		return m.saveItemForm()
	default:

		f.title.update(msg)
	}
	return m, nil
}

func (m browserModel) saveItemForm() (browserModel, tea.Cmd) {
	f := m.checklist.form
	intent := f.intent
	intent.change.Title = string(f.title.value)
	return m.submitItem(intent)
}

func (m browserModel) itemTargetLines(intent itemIntent) []string {
	text := "Task: " + intent.original.Title + "\nTask ID: " + intent.original.Id
	if intent.change.Action != store.ItemAdd && intent.position > 0 && intent.position <= len(intent.original.Items) {
		text += fmt.Sprintf("\nItem %d of %d: %s", intent.position, len(intent.original.Items), intent.original.Items[intent.position-1].Title)
	}
	return wrapText(text, max(1, m.width-4))
}

func (m browserModel) checklistView() (string, []string) {
	p, width := m.checklist, max(1, m.width-4)
	if p.inspect != nil {
		lines := m.itemInspectionLines()
		start := min(p.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Checklist details", lines[start:]
	}
	if f := p.form; f != nil {
		if f.discard {
			return "Unsaved item changes", wrapText("s: save | d: discard | Esc: continue", width)
		}
		title := "Add checklist item"
		if f.intent.change.Action == store.ItemRename {
			title = "Rename checklist item"
		}
		lines := []string{fit("Task: "+display(f.intent.original.Title), width), "Title: " + f.title.view(max(1, width-7))}
		if f.err != "" {
			lines = append(lines, wrapText("Not saved: "+f.err, width)...)
		}
		return title, lines
	}
	if p.confirm != nil {
		lines := append(m.itemTargetLines(*p.confirm), "", "Enter removes this item locally; Esc cancels.")
		start := min(p.offset, max(0, len(lines)-max(1, m.height-6)))
		return "Remove checklist item", lines[start:]
	}
	if m.detail == nil || m.detail.Id != p.taskID || m.taskID != p.taskID {
		return "Checklist unavailable", wrapText(m.checklistProblem(), width)
	}
	items := m.detail.Items
	i := selectedItem(items, p.key)
	lines := []string{fit("Task: "+display(m.detail.Title), width)}
	if problem := m.checklistProblem(); problem != "" {
		lines = append(lines, fit("Read-only: "+display(problem), width), "Enter: full details and refusal reason")
	}
	if m.checklistState.Uncertain || m.checklistState.RecoveryPending {
		lines = append(lines, "Identity pending; Enter: details")
	}
	if len(items) == 0 {
		return "Checklist (0)", append(lines, "No items. a adds an item.")
	}
	if i < 0 {
		lines = append(lines, "No item selected; use j/k or arrows.")
	}
	lines = lines[:min(len(lines), max(1, m.height-7))]
	rows := max(1, m.height-6-len(lines))
	start := max(0, i-rows+1)
	for n := start; n < min(len(items), start+rows); n++ {
		status := "[ ]"
		if items[n].Status.Done() {
			status = "[x]"
		}
		lines = append(lines, fmt.Sprintf("%s%d %s %s", marker(n == i), n+1, status, display(items[n].Title)))
	}
	return fmt.Sprintf("Checklist (%d) - item %d", len(items), i+1), lines
}

func (m browserModel) checklistFooters() (string, string) {
	if m.checklist.inspect != nil {
		return "Checklist details | Esc back", "j/k scroll | Enter back"
	}
	if m.checklist.form != nil {
		if m.checklist.form.discard {
			return "Unsaved item changes", "s save d discard Esc continue"
		}
		return "Item title | Ctrl+S save", "Esc back | shortcuts type as text"
	}
	if m.checklist.confirm != nil {
		return "Enter remove | Esc cancel", "j/k scroll exact target"
	}
	if m.width < 80 {
		return "^O Tab Esc | r u s ? q", "j/k a e Space ^D Alt+up/down"
	}
	return "Checklist: ^O jump | Tab focus | Esc back | r refresh | u undo | s sync | ? help | q quit", "j/k select | a add | e rename | Space done/open | Ctrl+D remove | Alt+up/down move"
}

func (m browserModel) itemInspectionLines() []string {
	lines := m.itemTargetLines(*m.checklist.inspect)
	if problem := m.checklistProblem(); problem != "" {
		lines = append(lines, wrapText("Read-only: "+problem, max(1, m.width-4))...)
	}
	if m.checklistState.Uncertain || m.checklistState.RecoveryPending {
		lines = append(lines, wrapText("Identity confirmation pending. Sync uses read-only recovery for uncertain allocations; it does not blindly repeat their POST.", max(1, m.width-4))...)
	}
	return lines
}

func checklistHelp() []string {
	return []string{"Checklist keys", "Enter: read the full selected title and refusal reason", "j/k or arrows: select an item, including completed items", "a: append an item; TEXT becomes CHECKLIST", "e: rename the selected item", "Ctrl+S: save a title locally; Esc handles unsaved input", "Space: complete or reopen the selected item only", "Ctrl+D: preview removal; Enter confirms, Esc cancels", "Alt+up/down: move the selected item one position", "Each change is one local operation with global undo", "u: preview global undo; s: explicit sync; r: refresh", "Ctrl+O then 1/2/3: return to that browser panel", "Tab/Shift+Tab: return and change focus; Esc: return", "?: help; Esc closes help; q: quit", "", "Selection follows a private key, never a title or row", "Missing items clear selection; choose again explicitly", "Refresh keeps drafts; stale snapshots refuse writes", "Uncertain allocation is recovered by reading, never blind replay", "Unsupported raw properties refuse edits; source data stays intact", "Last-item removal keeps local CHECKLIST; confirmed pull may converge to TEXT", "Markdown checkboxes stay body text; NOTE cannot become a checklist"}
}
