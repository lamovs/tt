package tui

import (
	"context"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
)

type SystemActions interface {
	Settings(context.Context) (SettingsResult, error)
	Doctor(context.Context) ([]string, error)
	Prepare(context.Context, string) (SystemPreview, error)
	FocusChoices(context.Context) (app.FocusChoices, error)
	PrepareDefaultFocus(context.Context, config.FocusReference) (SystemPreview, error)
	Login(context.Context, bool, *os.File, *os.File) string
}

type SettingsResult struct {
	ServerTasks    ServerTaskActions
	Resources      ResourceActions
	Lines          []string
	Queries        Queries
	Actions        Actions
	Timers         TimerActions
	DefaultProject string
	Color          bool
}

type SystemPreview struct {
	Action string
	Lines  []string
	Apply  func(context.Context) (string, error)
}

type systemPanel struct {
	lines   []string
	offset  int
	preview *SystemPreview
	focus   *focusPicker
}

type systemFinished struct {
	generation uint64
	action     string
	settings   SettingsResult
	lines      []string
	preview    SystemPreview
	choices    *app.FocusChoices
	err        error
}

type loginHandoff struct {
	token bool
	check func(context.Context) (string, error)
}

func (m browserModel) systemCommand(action string, fn func(context.Context) systemFinished) (browserModel, tea.Cmd) {
	if m.busy || m.timerWorking {
		m.notice = "Wait for the current operation before changing settings or handing off the terminal."
		return m, nil
	}
	m.systemGeneration++
	generation := m.systemGeneration
	ctx, cancel := context.WithCancel(m.ctx)
	m.systemCancel = cancel
	m.busy, m.systemWorking = true, true
	m.systemPanel.offset = 0
	m.systemPanel.lines = []string{"Working: " + action + ". Esc requests cancellation."}
	var once sync.Once
	var result systemFinished
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			result = fn(ctx)
			result.generation, result.action = generation, action
		})
		return result
	})
}

func (m browserModel) openSystem() (browserModel, tea.Cmd) {
	m.systemPanel = &systemPanel{}
	return m.readSettings()
}

func (m browserModel) readSettings() (browserModel, tea.Cmd) {
	system := m.system
	return m.systemCommand("validated settings reload", func(ctx context.Context) systemFinished {
		result, err := system.Settings(ctx)
		return systemFinished{settings: result, err: err}
	})
}

func (m browserModel) finishSystem(msg systemFinished) (browserModel, tea.Cmd) {
	if !m.systemWorking || msg.generation != m.systemGeneration || m.systemPanel == nil {
		return m, nil
	}
	m.systemCancel()
	m.systemCancel = nil
	m.systemWorking, m.busy = false, false
	p := m.systemPanel
	p.offset, p.preview = 0, nil
	p.lines = msg.lines
	if msg.err != nil {
		p.lines = append(p.lines, "Operation did not complete: "+msg.err.Error())
		if msg.action == "validated settings reload" {
			p.lines = append(msg.settings.Lines, p.lines...)
			p.lines = append(p.lines, "Previous validated settings remain active. Correct the file and press r.")
		}
	} else if msg.preview.Apply != nil {
		p.focus = nil
		p.preview = &msg.preview
		p.lines = msg.preview.Lines
	} else if msg.choices != nil {
		p.focus = &focusPicker{choices: *msg.choices, projectID: msg.choices.InitialProjectID}
	} else if msg.action == "validated settings reload" {
		p.lines = msg.settings.Lines
		m.queries, m.actions, m.timers = msg.settings.Queries, msg.settings.Actions, msg.settings.Timers
		m.resources = msg.settings.Resources
		m.serverTaskActions = msg.settings.ServerTasks
		m.closeServerTasks()
		m.closeResources()
		m.board, m.palette = nil, nil
		m.boardGeneration++
		m.defaultProject = msg.settings.DefaultProject
		m.accent, m.muted = lipgloss.NewStyle(), lipgloss.NewStyle()
		if msg.settings.Color {
			m.accent = m.accent.Foreground(lipgloss.Color("2")).Bold(true)
			m.muted = m.muted.Foreground(lipgloss.Color("8"))
		}
		m.err = nil
		m.timerGeneration++
		m.timerTickGeneration++
		m.timerEnsured = ""
		if m.queries != nil {
			next, cmd := m.load()
			return next, tea.Batch(cmd, next.timerReadCmd(), next.timerTickCmd())
		}
	}
	return m, nil
}

func (m browserModel) systemKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	s := msg.String()
	p := m.systemPanel
	if p.focus != nil {
		return m.focusPickerKey(msg)
	}
	if p.preview != nil {
		switch s {
		case "esc":
			p.preview = nil
			p.lines, p.offset = []string{"Canceled preview; no action started. Press r for settings."}, 0
		case "enter":
			preview := *p.preview
			if preview.Action == "login" || preview.Action == "login token" {
				return m.beginLogin(preview.Action == "login token")
			}
			if m.timerWorking {
				m.notice = "Wait for the focus operation to finish."
				return m, nil
			}
			p.preview = nil
			return m.systemCommand(preview.Action, func(ctx context.Context) systemFinished {
				text, err := preview.Apply(ctx)
				return systemFinished{lines: []string{text, "Press r to reload validated settings."}, err: err}
			})
		default:
			p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.systemLines()))
		}
		return m, nil
	}
	switch s {
	case "esc":
		m.systemPanel = nil
	case "q":
		return m, tea.Quit
	case "ctrl+o":
		m.jump = m.queries != nil
	case "?":
		m.help.ShowAll, m.help.offset = true, 0
	case "r":
		return m.readSettings()
	case "g":
		system := m.system
		return m.systemCommand("choose default focus", func(ctx context.Context) systemFinished {
			choices, err := system.FocusChoices(ctx)
			return systemFinished{choices: &choices, err: err}
		})
	case "d":
		system := m.system
		return m.systemCommand("doctor (includes one API check)", func(ctx context.Context) systemFinished {
			lines, err := system.Doctor(ctx)
			return systemFinished{lines: lines, err: err}
		})
	case "i", "f", "n", "l", "L":
		action := map[string]string{"i": "config init", "f": "notification fix", "n": "notifier test", "l": "login", "L": "login token"}[s]
		system := m.system
		return m.systemCommand("prepare "+action, func(ctx context.Context) systemFinished {
			preview, err := system.Prepare(ctx, action)
			return systemFinished{preview: preview, err: err}
		})
	default:
		p.offset = scroll(p.offset, s, max(1, m.height-6), len(m.systemLines()))
	}
	return m, nil
}

func (m browserModel) systemLines() []string {
	if m.systemPanel.focus != nil {
		return m.focusPickerLines()
	}
	var lines []string
	for _, line := range m.systemPanel.lines {
		lines = append(lines, wrapText(line, max(1, m.width-4))...)
	}
	if m.systemPanel.preview != nil {
		lines = append(lines, "", "Enter confirms this exact preview; Esc cancels.")
	}
	return lines
}

func systemHelp() []string {
	return []string{
		"Settings and diagnostics (S)",
		"r: read and apply validated effective settings; invalid files keep previous settings",
		"i: preview creation of a commented config; existing files are never overwritten",
		"g: choose default_focus from cached open tasks or none; default_project initializes the list",
		"Focus picker: Tab switches lists/tasks; arrows or j/k select; Enter reviews; n chooses none",
		"Enter confirms the exact default; r reloads it. Active sessions and saved uploads keep their targets",
		"d: explicitly run doctor, including its API check; Esc requests cancellation",
		"f: preview the supported timer.on_end fix with exact target and backup",
		"n: preview one notifier test; execution status does not prove visual receipt",
		"l: preview existing browser login handoff; L: existing hidden token input",
		"Browser OAuth needs a TickTick developer app client ID and secret",
		"Enter confirms a preview; Esc cancels. Ctrl+C during login exits and restores the terminal",
		"No arbitrary settings editor, custom keybindings or default list picker",
		"Version appears with settings. j/k/arrows/PageUp/PageDown/Home/End scroll",
		"? or Esc closes help; Esc returns to browser; Ctrl+O jumps to panels; q quits",
	}
}

func (m browserModel) beginLogin(token bool) (browserModel, tea.Cmd) {
	if m.timerWorking || m.busy {
		m.notice = "Wait for the current operation before login."
		return m, nil
	}
	m.generation++
	m.detailGeneration++
	m.timerGeneration++
	m.timerTickGeneration++
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	if m.cancelDetail != nil {
		m.cancelDetail()
	}
	m.loading = false
	m.login = &loginHandoff{token: token, check: m.systemPanel.preview.Apply}
	return m, tea.Quit
}
