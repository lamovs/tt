package tui

import tea "charm.land/bubbletea/v2"

func (m browserModel) helpKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	switch msg.String() {
	case "f1", "?", "esc":
		m.help.ShowAll = false
	case "ctrl+o":
		if m.usable() && m.queries != nil && m.form == nil && m.dialog == nil && m.sync == nil &&
			m.resource == nil && m.palette == nil && m.serverTasks == nil &&
			m.taskOperation == nil && m.recovery == nil && !m.busy &&
			(m.checklist == nil || m.checklist.form == nil && m.checklist.confirm == nil) &&
			(m.timer == nil || m.timer.form == nil && m.timer.confirm == nil && m.timer.review == nil) &&
			(m.systemPanel == nil || m.systemPanel.preview == nil) {
			m.help.ShowAll, m.jump = false, true
		}
	case "q":
		if m.form == nil && m.checklist == nil && m.timer == nil && m.systemPanel == nil && m.sync == nil && m.resource == nil && m.palette == nil && m.serverTasks == nil {
			return m, tea.Quit
		}
	default:
		rows, count := max(1, m.height-6), len(m.helpLines())
		m.help.offset = scroll(min(m.help.offset, max(0, count-rows)), msg.String(), rows, count)
	}
	return m, nil
}

func (m browserModel) modalHelp() []string {
	if m.serverTasks != nil {
		return []string{"Read-only server queries", "Tab selects query fields. Ctrl+S reads the exact cached query; Ctrl+R explicitly fetches.", "Dates require RFC3339 offsets. Completed uses completion time; filter uses start time; search uses due time.", "j/k selects source rows; Enter shows raw fields; e edits the query; r cache; R remote.", "Rows never enter the local task store, numbered references, undo, or mutation queue.", "Coverage is unknown, or partial at the provider limit. Missing rows never prove deletion."}
	}
	if m.resource != nil {
		if m.resource.form != nil && m.resource.form.discard {
			return []string{"Unsaved resource fields", "s: review; d: discard; Esc: continue editing"}
		}
		if m.resource.focusPreview != nil {
			return []string{"Completed focus record preview", "Enter imports this exact completed session locally once; Esc returns to retained fields.", "The active timer is unchanged. Upload is separate through Local focus timer or tt timer sync.", "No provider write runs from this preview or its acceptance."}
		}
		if m.resource.form != nil && (m.resource.form.kind == "focus-record" || m.resource.form.kind == "focus-history") {
			return []string{"Focus record/history fields", "Tab selects fields; Ctrl+S reviews a record or reads the chosen history range locally.", "Type 0 is Pomodoro; type 1 is stopwatch timing. Dates require RFC3339 offsets.", "Manual completed records require whole seconds; pause is whole seconds smaller than the interval.", "Task is optional and must name an exact cached ID. No title-based retargeting.", "Esc asks about changed fields; ordinary q and ? remain text. F1 opens help."}
		}
		if m.resource.preview != nil {
			return []string{"Exact resource preview", "Enter queues only this target and displayed revision locally; Esc returns to fields", "j/k and PageUp/PageDown scroll the before snapshot and changed fields", "Sending is separate. Unknown remote outcomes are recovered read-only."}
		}
		if m.resource.form != nil {
			lines := []string{"Resource fields", "Tab/Shift+Tab: fields; Ctrl+S: create exact preview; Esc: unsaved choice", "Unchanged fields are omitted; zero and empty editable values remain explicit", "Ordinary keys including q and ? are field text. F1 opens help."}
			if m.resource.form.kind == "checkin" {
				lines = append(lines, "Ctrl+R explicitly fetches the selected habit/date baseline before the first check-in.", "Value is an absolute daily value, not an increment.")
			}
			return lines
		}
		if m.resource.queue != nil {
			return []string{"Resource operation queue", "j/k: select; Enter: details; r: local refresh", "c: preview cancellation; f: preview recovery of one exact sequence and revision", "Enter confirms; Esc cancels. Unknown writes remain read-back only.", "Task and focus queues remain available through the workspace palette."}
		}
		if m.resource.query.Kind == "focus" {
			return []string{"Remote focus history", "0/1 chooses Pomodoro/stopwatch type using local cache; e edits the date range locally.", "r reads cached history; R explicitly fetches remote history; Enter shows source details.", "a prepares a manually completed record using the existing focus session/uploader path.", "Ctrl+D previews deletion of the exact focus type/ID. Sync sends the queued deletion.", "Manual records do not create a second uploader or change the active timer."}
		}
		return []string{"Resource workspace", "r: local cache; R: explicit remote refresh; Enter: details", "a: create; e: edit; Ctrl+S: preview; Enter: queue locally", "Q: resource queue; cancellation and recovery name one exact operation", "Habits: c check-in; h history; Projects: b board", "Columns support create and rename. The board previews card assignment; column reorder/delete remain unavailable.", "Countowns are read-only; tags have create only; comments support create/delete.", "Ctrl+D previews deletion only where the provider contract permits it."}
	}
	if m.palette != nil {
		return []string{"Workspace palette", "Type to filter; arrows choose; Enter opens; Esc closes", "Opening a resource workspace reads the local cache. R explicitly requests server data."}
	}
	if m.boardContext() {
		return []string{"Kanban board", "j/k: cards in the current column; Alt+Left/Right: navigate columns", "Alt+Shift+Left/Right: preview moving the selected card to the adjacent real column", "From No column, right chooses the first real column and left chooses the last", "Enter confirms the frozen preview; Esc cancels; assignment does not complete a task", "Official Open API (column write live-verified, undocumented)", "Enter opens existing task details; task edit, checklist and completion keep the same task identity", "R explicitly fetches column metadata; r reads local tasks and columns", "C opens create/rename column controls; b or Esc returns to the list view", "Narrow terminals show one column; unknown column IDs stay visible in a separate group", "Column ordering and deletion are unavailable."}
	}
	if m.systemPanel != nil && m.systemPanel.preview != nil {
		return []string{"Settings and diagnostics - exact action preview", "Enter: confirm this frozen action; Esc: cancel without starting it", "j/k/arrows/PageUp/PageDown/Home/End: read target, version and consequences", "Other settings actions require closing this preview first"}
	}
	if m.queue != nil && m.queue.confirmation != nil {
		return []string{"Sync queue recovery confirmation", "Enter: recover only the displayed entry/version; Esc: cancel", "j/k/arrows/PageUp/PageDown/Home/End: scroll every consequence", "This does not send requests; task and focus recovery remain separate"}
	}
	if m.checklist != nil && m.checklist.form != nil {
		if m.checklist.form.discard {
			return []string{"Checklist unsaved title", "s: save locally; d: discard; Esc: continue editing"}
		}
		return []string{"Checklist title input", "Ctrl+S: validate and save exact title; Esc: unsaved changes choice", "Arrows/Home/End move; Enter inserts a newline; Ctrl+U clears", "? and ordinary shortcuts are text; F1 opens help", "Concurrent item/task changes refuse the saved snapshot"}
	}
	if m.checklist != nil && m.checklist.confirm != nil {
		return []string{"Checklist removal confirmation", "Enter: remove the exact private item; Esc: cancel", "j/k/arrows/PageUp/PageDown/Home/End: scroll complete identity and title"}
	}
	if m.timer != nil && m.timer.review != nil {
		if m.timer.review.discard {
			return []string{"Unsaved completion note", "s: save addition; d: discard only the addition; Esc: continue editing", "Discard preserves the stored start note and leaves review pending"}
		}
		return []string{"Focus completion note", "Type an addition to the existing focus-history note; this is not a task comment", "Tab/Shift+Tab selects input, Save addition, Skip; Ctrl+S saves", "Enter in empty input skips; with text it inserts a newline; ordinary task shortcuts remain text", "Skip preserves the start note; Esc defers without releasing the upload hold", "Changed input asks Save/Discard/Continue on Esc or Ctrl+C; only submitted notes persist", "PageUp/PageDown reads full session identity and existing note; pending review survives restart"}
	}
	if m.timer != nil && m.timer.confirm != nil {
		return []string{"Focus confirmation", "Enter: apply this exact session/target/version; Esc: cancel preview", "j/k/arrows/PageUp/PageDown/Home/End: scroll all consequences", "Unknown focus POSTs are never repeated; task retry does not grant focus retry"}
	}
	if m.timer != nil && m.timer.form != nil {
		if m.timer.form.discard {
			return []string{"Focus unsaved form", "s: review/save; d: discard; Esc: continue editing"}
		}
		return []string{"Focus session form", "Tab/Shift+Tab: Mode/Task/Duration/Note/Indicator", "Arrows choose mode/task; Indicator cycles config/on/off", "Ctrl+S: preview start; Esc: unsaved choice; Enter: newline in note", "Timer counts up; Pomodoro counts down; duration uses 25m, 90s, 1h30m", "Task may be explicitly unassigned; starting never creates a task", "Indicator on/off persists for this session; config follows timer.indicator", "Hiding external status never stops the timer or changes upload/notifications", "? in a text field is literal; F1 opens help"}
	}

	if m.searching {
		return []string{"Search input", "Enter: apply phrase in this view; Esc: cancel input", "Ctrl+U clears; arrows/Home/End move; Backspace/Delete edit", "All ordinary shortcuts, including ?, are text. F1 opens help."}
	}
	if m.recovery != nil {
		return []string{"Editor recovery", "j/k/arrows/PageUp/PageDown/Home/End: scroll full diagnostic and draft path", "Esc: return to the retained form/browser; complete draft paths also print on exit", "The private draft is retained after failures; no automatic resubmission."}
	}
	if f := m.form; f != nil {
		if !f.discard && f.interval != nil && (f.intervalField() >= 0 || f.interval.review != nil) {
			return intervalHelp()
		}
		if f.discard {
			return []string{"Unsaved changes", "s: save locally; d: discard; Esc: continue editing", "Ctrl+C requests exit; the current unsaved draft still requires this choice."}
		}
		if f.schedule != nil {
			return append([]string{"Schedule form", "Tab/Shift+Tab: Date/Repeat/Reminders/Start/Duration/Zone; Ctrl+S: review/save", "Date: empty keeps; none or Ctrl+X clears start/due and all-day", "Existing non-recurring timed interval: Date changes end only", "in 2h, +2h, fri 18:00; +1m means a calendar month; minutes use min", "Repeat: Ctrl+P cycles presets; supported raw RRULE is accepted", "Reminders: at, -10min or raw TRIGGER; Ctrl+N add; Ctrl+D remove", "Up/down select reminder; Alt+up/down reorder; Ctrl+X clears active section", "Untouched raw fields remain exact; changed duplicate reminders refuse save"}, intervalHelp()...)
		}
		return append([]string{"Task/note form", "Tab/Shift+Tab: fields; Ctrl+S: validate and save locally; Esc: back", "Title Enter advances to body; Body Enter inserts a newline", "Arrows/Home/End, Backspace/Delete edit; Ctrl+U clears the active text field", "Priority/List: arrows choose; Remind at accepts in 2h or tmr 09:00", "New forms also offer Start/Duration/Zone after Remind at", "Current list overrides default_project (ID or legacy name)", "No valid default: choose List explicitly; no first-list fallback", "tt config default-project sets it; S then r reloads settings", "Ctrl+E: open this draft in VISUAL then EDITOR; use a waiting editor", "Unchanged editor document does not save the form; errors retain a private draft", "? and ordinary shortcut keys remain field text; F1 opens help", "Unsaved choice: s save, d discard, Esc continue; stale snapshots refuse save"}, intervalHelp()...)
	}
	if m.dialog != nil {
		if m.dialog.kind == "column move" {
			return []string{"Column assignment confirmation", "Enter: apply exactly the displayed task and source/destination columns; Esc: cancel", "j/k/arrows/PageUp/PageDown/Home/End: read the full preview", "Task status, project and ordering stay unchanged; sync is separate", "Official Open API (column write live-verified, undocumented)", "Concurrent task or column changes require a new preview"}
		}
		return []string{"Task confirmation", "Enter: apply exactly the displayed target/version; Esc: cancel", "j/k/arrows/PageUp/PageDown/Home/End: read every target and consequence", "Completion: Tab selects complete-items or keep-items policy", "Undo is global history, including CLI changes; it is not a selected-task reopen"}
	}
	if m.taskOperation != nil {
		return []string{"Delete/move confirmation", "j/k/arrows: choose destination or scroll preview; Enter: preview, then confirm", "Esc: cancel/back; ?: toggle operation help", "Exact task/list identities, counts and versions are checked again", "Native move preserves task ID; unresolved relationships and queued edits refuse"}
	}
	if m.sync != nil {
		return []string{"Explicit sync", "Esc: request cancellation while running, or close the final report", "j/k/arrows/PageUp/PageDown/Home/End: scroll the completed result", "Confirmed remote outcomes and durable local changes survive cancellation", "Task recovery and focus retry remain separate; uncertain POSTs never repeat", "Ctrl+C exits after started work settles; closing help does not cancel sync"}
	}
	return nil
}
