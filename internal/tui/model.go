package tui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type Queries interface {
	Projects(context.Context) ([]model.Project, error)
	Tasks(context.Context, app.BrowseQuery, time.Time) ([]model.Task, error)
	Task(context.Context, string) (model.Task, error)
}

type snapshotLoaded struct {
	selection  string
	query      app.BrowseQuery
	generation uint64
	projects   []model.Project
	tasks      []model.Task
	dues       map[string]string
	now        time.Time
	err        error
	state      app.CacheState
	stateErr   error
}

type detailLoaded struct {
	checklist            store.ChecklistState
	checklistErr         error
	id                   string
	generation, snapshot uint64
	task                 model.Task
	due                  string
	err                  error
}

type refreshDue struct{ generation uint64 }
type pane int

const (
	listsPane pane = iota
	tasksPane
	previewPane
)

type screenMode int

const (
	normalMode screenMode = iota
	halfMode
	fullMode
)

type helpState struct {
	ShowAll bool
	offset  int
}

type browserModel struct {
	serverTaskActions    ServerTaskActions
	serverTasks          *serverTaskPanel
	serverTaskGeneration uint64
	serverTaskCancel     context.CancelFunc
	resources            ResourceActions
	resource             *resourcePanel
	resourceGeneration   uint64
	resourceCancel       context.CancelFunc
	palette              *resourcePalette
	board                *kanbanBoard
	boardGeneration      uint64
	errorOffset          int
	focused              bool
	renderWidth          int
	renderGeneration     uint64
	system               SystemActions
	systemPanel          *systemPanel
	systemGeneration     uint64
	systemWorking        bool
	systemCancel         context.CancelFunc
	login                *loginHandoff

	timers                                               TimerActions
	timer                                                *timerPanel
	timerData                                            app.TimerData
	timerKnown, timerWorking, timerForeground            bool
	timerGeneration, timerOperation, timerTickGeneration uint64
	timerErr                                             error
	timerNow                                             time.Time
	timerEnsured                                         string
	timerReport                                          string
	timerCancel                                          context.CancelFunc
	timerAction                                          string
	timerNoteDeferred                                    map[string]bool

	ctx                              context.Context
	actions                          Actions
	defaultProject                   string
	gate                             *commandGate
	form                             *taskForm
	checklist                        *checklistPanel
	queue                            *queuePanel
	queueGeneration                  uint64
	taskOperation                    *taskOperationPanel
	checklistState                   store.ChecklistState
	checklistErr                     error
	recovery                         *recoveryPanel
	drafts                           *draftJournal
	editor                           *editorCommand
	dialog                           *actionDialog
	sync                             *syncPanel
	actionGeneration, syncGeneration uint64
	busy                             bool
	cacheState                       app.CacheState
	stateErr                         error
	stateKnown                       bool
	queries                          Queries
	query                            app.BrowseQuery
	projects                         []model.Project
	tasks                            []model.Task
	dues                             map[string]string
	taskID                           string
	selections                       map[app.BrowseQuery]string
	selectionLost                    bool
	generation, detailGeneration     uint64
	loadContext                      context.Context
	cancelLoad, cancelDetail         context.CancelFunc
	now                              time.Time
	detail                           *model.Task
	detailDue                        string
	detailLines                      []string
	detailWidth, detailOffset        int
	detailErr                        error
	detailLoading                    bool
	focus                            pane
	mode                             screenMode
	jump, loading, searching         bool
	input                            searchInput
	interrupted                      bool
	err                              error
	notice, selectionNotice          string
	width, height                    int
	help                             helpState
	keys                             keyMap
	accent, muted                    lipgloss.Style
}

func newModel(ctx context.Context, queries Queries, opts Options) browserModel {
	accent, muted := lipgloss.NewStyle(), lipgloss.NewStyle()
	if opts.Color {
		accent = accent.Foreground(lipgloss.Color("2")).Bold(true)
		muted = muted.Foreground(lipgloss.Color("8"))
	}
	loadCtx, cancel := context.WithCancel(ctx)
	return browserModel{
		serverTaskActions: opts.ServerTasks,
		resources:         opts.Resources,
		system:            opts.System, focused: true,
		timers: opts.Timers, timerGeneration: 1, timerTickGeneration: 1,
		drafts: opts.drafts, ctx: ctx, queries: queries, actions: opts.Actions, defaultProject: opts.DefaultProject, gate: opts.gate, query: app.BrowseQuery{View: app.TodayView},
		generation: 1, loadContext: loadCtx, cancelLoad: cancel,
		selections: make(map[app.BrowseQuery]string),
		loading:    opts.Err == nil, err: opts.Err, notice: opts.Notice,
		width: 80, height: 24, keys: newKeyMap(), accent: accent, muted: muted,
	}
}

func (m browserModel) Init() tea.Cmd {
	if m.err != nil {
		return nil
	}
	if m.timers == nil {
		return m.snapshotCmd()
	}
	return tea.Batch(m.snapshotCmd(), m.timerReadCmd(), m.timerTickCmd())
}

func (m browserModel) snapshotCmd() tea.Cmd {
	ctx, queries, query, generation := m.loadContext, m.queries, m.query, m.generation
	actions := m.actions
	selection := m.taskID
	ids := []string{selection}
	for _, id := range m.selections {
		ids = append(ids, id)
	}
	return m.gate.command(func() tea.Msg {
		now := time.Now()
		msg := snapshotLoaded{selection: selection, query: query, generation: generation, now: now}
		msg.projects, msg.err = queries.Projects(ctx)
		if msg.err == nil {
			msg.tasks, msg.err = queries.Tasks(ctx, query, now)
		}
		msg.dues = make(map[string]string, len(msg.tasks))
		zones := make(map[string]*time.Location)
		for _, task := range msg.tasks {
			zone := zones[task.TimeZone]
			if zone == nil {
				zone = dates.Zone(task.TimeZone)
				zones[task.TimeZone] = zone
			}
			msg.dues[task.Id] = dates.Format(task.DueDate, task.IsAllDay, zone, now)
		}
		if actions != nil {
			msg.state, msg.stateErr = actions.State(ctx, ids)
		}
		return msg
	})
}

func (m browserModel) refreshCmd() tea.Cmd {
	generation := m.generation
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return refreshDue{generation} })
}

func (m browserModel) load() (browserModel, tea.Cmd) {
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	if m.cancelDetail != nil {
		m.cancelDetail()
	}
	m.loadContext, m.cancelLoad = context.WithCancel(m.ctx)
	m.generation++
	m.loading = true
	m.stateKnown = false
	return m, m.snapshotCmd()
}

func (m browserModel) changeQuery(query app.BrowseQuery) (browserModel, tea.Cmd) {
	if query == m.query {
		return m, nil
	}
	if query.View != app.ProjectView || query.ProjectID != m.query.ProjectID {
		m.board = nil
		m.boardGeneration++
	}
	m.selections[m.query] = m.taskID
	m.query, m.tasks, m.err = query, nil, nil
	m.taskID = m.selections[query]
	m.selectionLost, m.selectionNotice = false, ""
	m.clearDetail()
	return m.load()
}

func (m *browserModel) clearDetail() {
	if m.cancelDetail != nil {
		m.cancelDetail()
	}
	m.detailGeneration++
	m.renderGeneration++
	m.renderWidth = 0
	m.detail, m.detailLines, m.detailErr = nil, nil, nil
	m.detailOffset, m.detailWidth, m.detailLoading = 0, 0, false
}

func (m browserModel) loadDetail() (browserModel, tea.Cmd) {
	if m.cancelDetail != nil {
		m.cancelDetail()
	}
	m.detailGeneration++
	if m.taskID == "" || m.taskIndex() < 0 {
		m.clearDetail()
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancelDetail, m.detailLoading = cancel, true
	actions := m.actions
	queries, id, generation, snapshot, now := m.queries, m.taskID, m.detailGeneration, m.generation, m.now
	return m, m.gate.command(func() tea.Msg {
		task, err := queries.Task(ctx, id)
		due := ""
		if err == nil {
			due = dates.Format(task.DueDate, task.IsAllDay, dates.Zone(task.TimeZone), now)
		}
		msg := detailLoaded{id: id, generation: generation, snapshot: snapshot, task: task, due: due, err: err}
		if err == nil && actions != nil {
			msg.checklist, msg.checklistErr = actions.ChecklistState(ctx, task)
		}
		return msg
	})
}

func (m browserModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case focusRecordPrepared:
		return m.finishFocusRecordPreview(msg)
	case focusRecordApplied:
		return m.finishFocusRecordApply(msg)
	case serverTasksLoaded:
		return m.finishServerTasks(msg)
	case resourceLoaded:
		return m.finishResourceLoad(msg)
	case resourcePrepared:
		return m.finishResourcePreview(msg)
	case resourceApplied:
		return m.finishResourceApply(msg)
	case kanbanLoaded:
		return m.finishKanbanLoad(msg)
	case resourceQueueLoaded:
		return m.finishResourceQueueLoad(msg)
	case resourceQueueFinished:
		return m.finishResourceQueueAction(msg)
	case resourceBaselineLoaded:
		return m.finishResourceBaseline(msg)
	case detailRendered:
		if msg.render != m.renderGeneration || msg.generation != m.detailGeneration || msg.snapshot != m.generation || msg.id != m.taskID {
			return m, nil
		}
		m.renderWidth = 0
		m.detailWidth, m.detailLines = msg.width, msg.lines
	case tea.BlurMsg:
		m.focused = false
		return m, nil
	case tea.FocusMsg:
		if m.focused {
			return m, nil
		}
		m.focused = true
		if m.queries != nil && !m.loading && m.editor == nil && m.login == nil {
			return m.load()
		}
		return m, nil

	case systemFinished:
		return m.finishSystem(msg)
	case timerLoaded:
		return m.finishTimerRead(msg)
	case timerFinished:
		return m.finishTimerOperation(msg)
	case timerRefreshDue:
		if msg.generation == m.timerGeneration {
			return m.loadTimer()
		}
		return m, nil
	case timerTick:
		if msg.generation != m.timerTickGeneration {
			return m, nil
		}
		m.timerNow = msg.now
		return m, m.timerTickCmd()

	case queueLoaded:
		return m.finishQueue(msg)
	case queueRefreshDue:
		if m.queue != nil && !m.queue.loading && msg.generation == m.queueGeneration {
			return m.loadQueue()
		}
		return m, nil
	case editorFinished:
		return m.finishEditor(msg)
	case actionFinished:
		return m.finishAction(msg)
	case syncProgress:
		if m.sync != nil && m.sync.running && msg.generation == m.sync.generation {
			m.sync.phase = msg.phase
			return m, m.sync.progressCommand()
		}
		return m, nil
	case syncFinished:
		if m.sync == nil || !m.sync.running || msg.generation != m.sync.generation {
			return m, nil
		}
		m.sync.running, m.sync.result = false, msg.result
		m.notice = m.sync.summary()
		m.sync.cancel()
		m, reload := m.load()
		if m.timer != nil {
			var timerReload tea.Cmd
			m, timerReload = m.loadTimer()
			return m, tea.Batch(reload, timerReload)
		}
		return m, reload
	case tea.WindowSizeMsg:
		m.width, m.height = max(0, msg.Width), max(0, msg.Height)
		if !m.usable() {
			m.jump = false
		}
	case snapshotLoaded:
		if msg.query != m.query || msg.generation != m.generation {
			return m, nil
		}

		if (m.err == nil) != (msg.err == nil) || m.err != nil && msg.err != nil && m.err.Error() != msg.err.Error() {
			m.errorOffset = 0
		}
		m.loading, m.err = false, msg.err
		if msg.err != nil {
			m.stateErr, m.stateKnown = msg.err, false
			m.jump = false
			return m, m.refreshCmd()
		}

		if m.actions != nil && msg.selection != m.taskID {
			return m.load()
		}
		m.cacheState, m.stateErr, m.stateKnown = msg.state, msg.stateErr, m.actions != nil && msg.stateErr == nil

		for query, id := range m.selections {
			if next := msg.state.Replacements[id]; next != "" {
				m.selections[query] = next
			}
		}
		if next := msg.state.Replacements[m.taskID]; next != "" {
			if m.checklist != nil && m.checklist.taskID == m.taskID {
				m.checklist.taskID = next
			}
			m.taskID = next
			m.clearDetail()
			found := false
			for _, task := range msg.tasks {
				if task.Id == next {
					found = true
				}
			}
			if !found {

				return m.load()
			}
		}
		m.projects, m.tasks, m.dues, m.now = msg.projects, msg.tasks, msg.dues, msg.now
		m.reconcileBoardSelection()
		if m.taskID != "" && m.taskIndex() < 0 {
			m.taskID, m.selectionLost = "", true
			m.selectionNotice = "Selected task is no longer in this view. Select a task to continue."
			m.clearDetail()
		} else if m.taskID == "" && !m.selectionLost && len(m.tasks) > 0 {
			m.taskID = m.tasks[0].Id
		}
		next, cmd := m.loadDetail()
		return next, tea.Batch(cmd, m.refreshCmd())
	case detailLoaded:
		if msg.id != m.taskID || msg.generation != m.detailGeneration || msg.snapshot != m.generation {
			return m, nil
		}
		m.detailLoading, m.detailErr = false, msg.err
		if msg.err == nil && msg.task.Id == m.taskID {
			m.detail, m.detailDue = &msg.task, msg.due
			m.detailWidth, m.renderWidth = 0, 0
			m.renderGeneration++
			m.checklistState, m.checklistErr = msg.checklist, msg.checklistErr
			m.reconcileChecklist()
		} else {
			m.detail, m.detailLines = nil, nil
		}
	case refreshDue:
		if msg.generation == m.generation && !m.loading && m.queries != nil {
			return m.load()
		}
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		if m.help.ShowAll {
			return m, nil
		}
		if m.palette != nil {
			m.palette.input.insert(msg.Content)
			m.palette.index = 0
			return m, nil
		}
		if m.serverTasks != nil {
			m.editServerQuery(msg.Content, nil)
			return m, nil
		}
		if m.resource != nil {
			m.pasteResource(msg.Content)
			return m, nil
		}
		if m.systemPanel != nil {
			return m, nil
		}
		if m.timer != nil && m.timer.review != nil {
			if !m.busy && !m.timer.review.discard && m.timer.review.field == 0 {
				m.timer.review.addition.insertExact(msg.Content)
				m.timer.review.err = ""
			}
			return m, nil
		}
		if m.timer != nil && m.timer.form != nil {
			if !m.busy && m.timer.confirm == nil && !m.timer.form.discard {
				switch m.timer.form.field {
				case 2:
					m.timer.form.duration.insert(msg.Content)
				case 3:
					m.timer.form.note.insertExact(msg.Content)
				}
			}
			return m, nil
		}

		if m.recovery != nil {
			return m, nil
		}
		if m.checklist != nil && m.checklist.form != nil && !m.busy {
			m.pasteItem(msg.Content)
			return m, nil
		}
		if m.form != nil && !m.busy && !m.form.discard {
			if m.form.interval != nil && m.form.interval.review != nil {
				return m, nil
			}
			if m.form.intervalField() >= 0 {
				m.intervalPaste(msg.Content)
			} else if m.form.schedule != nil {
				m.schedulePaste(msg.Content)
			} else if m.form.field == 4 {
				m.form.quick.insertExact(msg.Content)
				m.form.parseQuickReminder(time.Now())
			} else if m.form.field == 0 {
				m.form.title.insert(msg.Content)
			}
			if m.form.schedule == nil && m.form.field == 1 {
				m.form.body.insertExact(msg.Content)
			}
		} else if m.searching {
			m.input.insert(msg.Content)
		}
	}
	cmd := m.reflow()
	return m, cmd
}

func (m browserModel) handleKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	s := msg.String()
	if key.Matches(msg, m.keys.interrupt) {
		m.help.ShowAll = false
		if m.timer != nil && m.timer.review != nil && len(m.timer.review.addition.value) != 0 && !m.busy {
			m.timer.review.discard, m.timer.review.quitAfter = true, true
			return m, nil
		}
		if m.resource != nil && m.resource.form != nil && m.resource.form.dirty() && !m.busy {
			m.resource.preview = nil
			m.resource.focusPreview = nil
			m.resource.form.discard, m.resource.form.quitAfter = true, true
			return m, nil
		}
		if m.timer != nil && m.timer.form != nil && !m.busy {
			m.timer.confirm = nil
			m.timer.form.discard, m.timer.form.quitAfter = true, true
			return m, nil
		}

		if m.checklist != nil && m.checklist.form != nil && m.checklist.form.dirty() && !m.busy {
			m.checklist.form.discard, m.checklist.form.quitAfter = true, true
			return m, nil
		}
		if m.form != nil && m.form.dirty() && !m.busy {
			m.form.discard, m.form.quitAfter = true, true
			return m, nil
		}
		m.interrupted = true
		return m, tea.Quit
	}
	if s == "f1" {
		m.help.ShowAll = !m.help.ShowAll
		m.help.offset = 0
		return m, nil
	}
	if m.help.ShowAll {
		return m.helpKey(msg)
	}
	if s == "esc" && m.systemWorking && m.systemCancel != nil {
		m.systemCancel()
		return m, nil
	}
	if s == "esc" && m.timerForeground && m.timerCancel != nil && (m.timerAction == "upload" || m.timerAction == "retry") {
		m.timerCancel()
		return m, nil
	}
	if !m.usable() && (m.form != nil || m.dialog != nil || m.sync != nil || m.checklist != nil || m.queue != nil || m.taskOperation != nil || m.timer != nil || m.systemPanel != nil || m.resource != nil || m.palette != nil || m.serverTasks != nil) {
		if m.sync != nil && s == "esc" {
			return m.syncKey(msg)
		}
		return m, nil
	}
	if m.busy {
		if m.systemWorking && s == "esc" && m.systemCancel != nil {
			m.systemCancel()
		}
		if m.timerForeground && s == "esc" && m.timerCancel != nil && (m.timerAction == "upload" || m.timerAction == "retry") {
			m.timerCancel()
			m.notice = "Canceling focus upload; waiting for its durable outcome."
		}
		return m, nil
	}
	if m.palette != nil {
		return m.paletteKey(msg)
	}
	if m.serverTasks != nil {
		return m.serverTasksKey(msg)
	}
	if m.resource != nil {
		return m.resourceKey(msg)
	}
	if m.recovery != nil {
		if s == "esc" {
			m.recovery = nil
		} else {
			m.recovery.offset = scroll(m.recovery.offset, s, max(1, m.height-6), len(m.recoveryLines()))
		}
		return m, nil
	}
	if m.form != nil {
		return m.formKey(msg)
	}
	if m.dialog != nil {
		switch s {
		case "esc":
			m.dialog = nil
		case "enter":
			return m.confirmAction()
		case "tab":
			if m.dialog.kind == "complete" {
				m.dialog.keepItems = !m.dialog.keepItems
			}
		default:
			_, lines := m.dialogView()
			rows := max(1, m.height-6)
			m.dialog.offset = scroll(min(m.dialog.offset, max(0, len(lines)-rows)), s, rows, len(lines))
		}
		return m, nil
	}
	if m.sync != nil {
		return m.syncKey(msg)
	}
	if m.taskOperation != nil {
		return m.taskOperationKey(msg)
	}
	if m.systemPanel != nil && !m.jump && !m.help.ShowAll {
		return m.systemKey(msg)
	}
	if m.timer != nil && !m.jump && !m.help.ShowAll {
		return m.timerKey(msg)
	}
	if m.queue != nil && !m.jump && !m.help.ShowAll {
		return m.queueKey(msg)
	}
	if m.checklist != nil && !m.jump && !m.help.ShowAll {
		return m.checklistKey(msg)
	}
	if m.searching {
		switch s {
		case "esc":
			m.searching = false
		case "enter":
			m.searching = false
			query := m.query
			query.Search = strings.TrimSpace(string(m.input.value))
			return m.changeQuery(query)
		default:
			m.input.update(msg)
		}
		return m, nil
	}
	if key.Matches(msg, m.keys.quit) {
		return m, tea.Quit
	}
	if m.jump {
		switch s {
		case "1", "2", "3":
			m.focus, m.jump, m.checklist = pane(s[0]-'1'), false, nil
			m.queue = nil
			m.systemPanel = nil
			m.timer = nil
			m.queueGeneration++
		case "esc", "ctrl+o":
			m.jump = false
		}
		cmd := m.reflow()
		return m, cmd
	}

	if s == "S" && m.system != nil && m.usable() {
		return m.openSystem()
	}
	if s == ":" && m.usable() {
		m.palette = &resourcePalette{}
		return m, nil
	}
	if s == "b" && m.usable() && m.queries != nil {
		return m.toggleKanban()
	}
	if m.board != nil && m.usable() {
		if next, cmd, handled := m.kanbanKey(msg); handled {
			return next, cmd
		}
	}
	if m.queries != nil && m.usable() && (s == "1" || s == "2" || s == "3") {
		m.focus = pane(s[0] - '1')
		return m, m.reflow()
	}
	if m.usable() && m.err != nil && (m.queries == nil || m.focus == tasksPane) && s != "q" && s != "esc" && s != "?" && s != "r" && s != "ctrl+o" && s != "tab" && s != "shift+tab" {
		m.errorOffset = scroll(m.errorOffset, s, max(1, m.height-6), len(wrapText(m.err.Error(), max(1, m.width-4)))+2)
		return m, nil
	}
	if m.queries != nil && m.usable() {
		if next, cmd, handled := m.mutationKey(s); handled {
			return next, cmd
		}
	}
	switch {
	case key.Matches(msg, m.keys.help):
		m.help.ShowAll, m.help.offset = true, 0
	case key.Matches(msg, m.keys.jump):
		m.jump = m.usable() && m.queries != nil
	case key.Matches(msg, m.keys.back):
		if m.mode != normalMode {
			m.mode = normalMode
		} else if m.focus > listsPane && m.queries != nil {
			m.focus--
		} else {
			return m, tea.Quit
		}
	case m.queries == nil || !m.usable():
		return m, nil
	case key.Matches(msg, m.keys.search):
		m.searching = true
		m.input.set(m.query.Search)
	case key.Matches(msg, m.keys.refresh):
		return m.load()
	case key.Matches(msg, m.keys.nextSize):
		m.mode = (m.mode + 1) % 3
	case key.Matches(msg, m.keys.prevSize):
		m.mode = (m.mode + 2) % 3
	case key.Matches(msg, m.keys.nextPane):
		m.focus = (m.focus + 1) % 3
	case key.Matches(msg, m.keys.previousPane):
		m.focus = (m.focus + 2) % 3
	case key.Matches(msg, m.keys.open):
		m.focus = min(m.focus+1, previewPane)
	default:
		switch m.focus {
		case listsPane:
			entries := m.entries()
			i := m.entryIndex(entries)
			next := scroll(i, s, m.paneRows(listsPane), len(entries))
			if next != i {
				query := entries[next].query
				query.Search = m.query.Search
				return m.changeQuery(query)
			}
		case tasksPane:
			if len(m.tasks) > 0 {
				i := m.taskIndex()
				next := scroll(i, s, m.paneRows(tasksPane), len(m.tasks))
				if next != i {
					m.taskID, m.selectionLost, m.selectionNotice = m.tasks[next].Id, false, ""
					m.clearDetail()
					return m.loadDetail()
				}
			}
		case previewPane:
			if m.detailErr != nil {
				m.detailOffset = scroll(m.detailOffset, s, m.paneRows(previewPane), len(wrapText(m.detailErr.Error(), max(1, m.layout()[previewPane].width-4)))+2)
			} else {
				m.detailOffset = scroll(m.previewOffset(), s, m.paneRows(previewPane), len(m.detailLines))
			}
		}
	}
	cmd := m.reflow()
	return m, cmd
}

func scroll(current int, key string, page, count int) int {
	next := current
	switch key {
	case "up", "k":
		next--
	case "down", "j":
		next++
	case "pgup":
		next -= max(1, page)
	case "pgdown":
		next += max(1, page)
	case "home":
		next = 0
	case "end":
		next = count - 1
	default:
		return current
	}
	return min(max(0, next), max(0, count-1))
}

func (m browserModel) taskIndex() int {
	for i, t := range m.tasks {
		if t.Id == m.taskID {
			return i
		}
	}
	return -1
}

type keyMap struct {
	up, down, nextPane, previousPane, open, back, jump, help, quit, interrupt key.Binding
	search, refresh, nextSize, prevSize                                       key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		up: key.NewBinding(key.WithKeys("up", "k")), down: key.NewBinding(key.WithKeys("down", "j")),
		nextPane:     key.NewBinding(key.WithKeys("tab", "right", "l")),
		previousPane: key.NewBinding(key.WithKeys("shift+tab", "left", "h")),
		open:         key.NewBinding(key.WithKeys("enter")), back: key.NewBinding(key.WithKeys("esc")),
		jump: key.NewBinding(key.WithKeys("ctrl+o")), help: key.NewBinding(key.WithKeys("?")),
		quit: key.NewBinding(key.WithKeys("q")), interrupt: key.NewBinding(key.WithKeys("ctrl+c")),
		search: key.NewBinding(key.WithKeys("/")), refresh: key.NewBinding(key.WithKeys("r")),
		nextSize: key.NewBinding(key.WithKeys("+")), prevSize: key.NewBinding(key.WithKeys("_")),
	}
}
