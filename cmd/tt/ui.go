package main

import (
	"errors"
	"io/fs"
	"os"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/tui"
)

func init() {
	register(command{
		help: cli.Help{
			Verb: "ui", Summary: "interactive task workspace with context help, timers and settings",
			Examples: []cli.Example{
				{Cmd: "tt ui", What: "open the interactive task workspace"},
				{Cmd: "EDITOR=nvim tt ui", What: "use Neovim when VISUAL is unset"},
			},
			Sections: []cli.HelpSection{
				{Title: "Requirements and navigation", Items: []string{
					"Requires terminal stdin and stdout and a window of at least 32 columns by 10 rows.",
					"Use arrows or j/k to move, Tab and Shift+Tab to switch panels, / to search, + and _ to resize panels, r to refresh and q to quit.",
					"Press 1, 2 or 3 for Lists, Tasks or Preview. Ctrl+O shows those panel hints.",
					"Press ? for help outside text input or F1 for context help anywhere. F1 or Esc closes help without changing a draft or confirmation.",
					"Today includes overdue tasks. CLI changes refresh automatically; returning terminal focus requests one local refresh.",
				}},
				{Title: "Tasks and forms", Items: []string{
					"a creates a TEXT task, n creates a NOTE, e edits the selected record, d opens dates/repeat/reminders, Space completes or reopens, u opens undo and s synchronizes.",
					"Ctrl+S saves a form locally. CLI task numbering is not changed by TUI navigation.",
					"New tasks use the selected project or default_project. Missing, ambiguous or unusable defaults require an explicit project.",
					"Every write checks its original snapshot. Unsupported raw task data and stale edits are refused.",
				}},
				{Title: "Dates, intervals and reminders", Items: []string{
					"New-task and date forms support Start, Duration and Zone for open, non-all-day, non-recurring tasks. Duration uses positive h or min; end becomes due and DST validation is strict.",
					"Ctrl+S opens cached overlap review. Use arrows, j/k or Home/End to inspect it, s to save despite reviewed overlaps, or Esc to return to the draft.",
					"For an existing interval, editing Date changes only the end. Empty input keeps dates; none clears start, due and all-day state.",
					"In a new task, Remind at accepts in 2h, +2h or tmr 09:00 to set due time and a reminder together.",
					"Ctrl+P cycles repeat presets. Ctrl+N adds a reminder, Ctrl+D removes one, Alt+up/down reorders and Ctrl+X clears.",
				}},
				{Title: "Checklist", Items: []string{
					"Press c to open checklist mode. j/k selects; a adds; e renames; Space completes or reopens; Ctrl+D removes; Alt+Up/Down reorders; Ctrl+S saves titles.",
					"Enter shows full item details. Selection follows private checklist identity through refresh; a missing item clears the selection.",
				}},
				{Title: "Task deletion and list moves", Items: []string{
					"Ctrl+D in the task browser previews deletion of one exact task and its queued changes. m selects a destination and previews a native list move.",
					"Enter confirms and Esc cancels. Exact versions, targets and counts are rechecked; concurrent changes require a new preview.",
					"drop-parked and project-drop are intentionally unavailable in the TUI.",
				}},
				{Title: "Kanban", Items: []string{
					"Choose a specific project in Lists, then press b to open its Kanban board. b or Esc returns to the normal task view.",
					"Press uppercase R to fetch column metadata from the server; lowercase r refreshes local tasks and cached columns. C opens column management.",
					"Alt+Left/Right selects columns. Alt+Shift+Left/Right previews moving the selected card to an adjacent real column; Enter queues the exact move.",
					"Column assignment does not complete a task. Column deletion and reordering are unavailable.",
				}},
				{Title: "Sync queue and recovery", Items: []string{
					"Q opens task operations; Tab switches to the separate read-only focus upload queue. Enter shows the exact operation and error.",
					"f previews recovery of one failed or parked operation; Enter returns it to pending. s runs sync separately.",
					"Uncertain allocating requests are never blindly repeated. Task recovery does not upload or rearm focus sessions.",
				}},
				{Title: "Markdown editor", Items: []string{
					"Shift+E opens the selected task or note in VISUAL, then EDITOR. Ctrl+E opens the current basic-form draft. Use an editor that waits, such as nvim or code --wait.",
					"Save and close to apply locally. An unchanged document leaves the form unsaved; errors and conflicts preserve a private recovery draft.",
					"Existing kind, ID, project and checklist items remain unchanged. Markdown checkboxes are body text, not checklist items.",
				}},
				{Title: "Focus timer", Items: []string{
					"t opens Focus; Tab switches active session and history. a starts for the selected task, A uses default_focus and N starts explicitly unassigned.",
					"Choose Timer or Pomodoro and any task with arrows. Tab moves through duration and note; Ctrl+S previews and Enter confirms.",
					"p pauses or resumes, x previews stop and Ctrl+D previews cancel. Stop records actual active time; closing the TUI leaves the timer running.",
					"History shows 20 recent sessions. U previews upload or read-back; f retries only a definitive rejection. Unknown sends are not repeated.",
					"r in Focus recovers the countdown watcher. There is no automatic break loop or remote active-timer control.",
					"Indicator accepts config, on or off. Read external status with tt timer indicator.",
				}},
				{Title: "Settings and account", Items: []string{
					"S opens settings, local authorization status and version. r validates and reloads configuration; invalid files leave previous settings active.",
					"g chooses default_focus from cached open tasks or none. Saved sessions and queued uploads never follow later default changes.",
					"i previews config initialization, d runs doctor, f previews the supported notification fix and n previews one notification test.",
					"l previews browser login and L previews hidden token input after restoring the terminal. Opening settings alone never starts login.",
					"Notifier output remains private. A successful command exit does not prove that a notification was visible.",
				}},
				{Title: "Storage and rendering", Items: []string{
					"Startup opens and migrates the local cache. Missing configuration uses defaults.",
					"Details are formatted asynchronously with identity and width checks. Only visible rows and bounded fields are painted; stored raw text remains intact.",
				}},
			},
			SeeAlso: []string{"add", "edit", "item", "show", "sync", "rm", "mv", "timer", "pomodoro", "config", "doctor", "login", "notify", "version"},
		},
		run: cmdUI,
	})
}

func cmdUI(inv *invocation) int {
	if len(inv.refinements) > 0 {
		return inv.misuseWord("unknown option ", inv.refinements[0])
	}
	if len(inv.data) > 0 {
		return inv.misuse("tt ui takes no arguments")
	}
	if !cli.IsTerminal(inv.stdin) || !cli.IsTerminal(inv.stdout) {
		return inv.fail(errors.New("tt ui needs an interactive terminal on stdin and stdout; use tt ls for piped output"))
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	system := &uiSystem{output: inv.stdout, color: inv.color, colorOverride: inv.colorKnown}
	defer system.close()
	opts := tui.Options{Color: inv.outPalette().Enabled(), System: system}
	cfg, cfgErr := inv.config()
	opts.Err, opts.DefaultProject = cfgErr, cfg.DefaultProject
	var queries tui.Queries
	if cfgErr == nil {
		system.store, opts.Err = inv.openStore()
		if system.store != nil {
			runtime := system.runtime(cfg)
			queries, opts.Actions, opts.Timers = runtime.Queries, runtime.Actions, runtime.Timers
			opts.Resources = runtime.Resources
			opts.ServerTasks = runtime.ServerTasks
		}
		if path, err := config.Path(); err == nil {
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				opts.Notice = "No config file; using defaults. Local cache only."
			}
		}
	}

	if inv.interrupted() {
		return exitInterrupted
	}
	err := tui.Run(inv.ctx, inv.stdin.(*os.File), inv.stdout.(*os.File), queries, opts)
	if inv.interrupted() || errors.Is(err, tui.ErrInterrupted) {
		return exitInterrupted
	}
	if err != nil {
		return inv.uiFailure(err)
	}
	return exitOK
}

func (inv *invocation) uiFailure(err error) int {
	return inv.fail(errors.New(cli.ReportTitle(err.Error(), cli.Width-len("tt: ui: "))))
}
