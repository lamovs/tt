package main

import "github.com/movsar/tt/internal/cli"

func init() {
	register(command{
		help: cli.Help{
			Verb:    "config",
			Summary: "show settings or choose the default list and focus task",
			Examples: []cli.Example{
				{Cmd: "tt config", What: "print every setting that applies, defaults included"},
				{Cmd: "tt config --init", What: "write a starter file with every key commented out"},
				{Cmd: "tt config default-project", What: "choose a cached list and save its ID"},
				{Cmd: "tt config default-project id:abc123", What: "save an exact cached ID without a prompt"},
				{Cmd: "tt config default-focus", What: "choose a cached open task; default_project initializes its list"},
				{Cmd: "tt config default-focus task:abc123", What: "save an exact cached focus task for both counting modes"},
				{Cmd: "tt config default-focus none", What: "make future default starts unassigned"},
			},
			Sections: []cli.HelpSection{
				{Title: "Configuration file", Items: []string{
					"The file is $XDG_CONFIG_HOME/tt/config.toml, or ~/.config/tt/config.toml when XDG_CONFIG_HOME is unset.",
					"Every key is optional. Unknown keys are errors, so uncomment a documented line instead of inventing a key.",
					"Changes made by the picker keep a backup of the previous configuration.",
				}},
				{Title: "Default project", Items: []string{
					"default-project reads cached lists only and requires terminal stdin and stderr for its picker. Empty input cancels.",
					"default_project accepts id:ID, legacy names and name:NAME. IDs match exactly; names use case-insensitive exact or substring lookup.",
					"Explicit command targets override the default. Missing, ambiguous or unusable defaults never select another list automatically.",
				}},
				{Title: "Default focus task", Items: []string{
					"default_focus is none by default or task:ID. Its picker lists cached open tasks in usable lists; its list and task numbers exist only inside the prompt.",
					"At the list step, Enter uses default_project, q cancels and 0 selects none. Empty task input cancels.",
					"An explicit start task or --none overrides the default. A missing, completed or unavailable default refuses a new default start; cached state is not a server check.",
					"Active sessions, history and queued uploads keep their original destinations. If a local task receives a new server ID, select the default again.",
				}},
				{Title: "Timer indicator", Items: []string{
					"timer.indicator defaults to true. start --indicator on|off overrides external visibility for that session only.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, S opens settings, g chooses default_focus, i previews initialization and r applies a validated reload.",
				}},
			},
			SeeAlso: []string{"doctor"},
		},
		run: func(inv *invocation) int {
			if len(inv.args) > 0 && inv.args[0] == "default-focus" {
				return cmdDefaultFocus(inv)
			}
			if len(inv.args) > 0 && inv.args[0] == "default-project" {
				return cmdDefaultProject(inv)
			}
			return cmdConfig(inv.ctx, inv.stdout, inv.stderr, inv.args)
		},
	})

	register(command{
		help: cli.Help{
			Verb:    "doctor",
			Summary: "check the token, one API call, cache and configuration",
			Examples: []cli.Example{
				{Cmd: "tt doctor", What: "report what is wrong, change nothing"},
				{Cmd: "tt doctor --fix", What: "offer to write a notification command into the config"},
			},
			Sections: []cli.HelpSection{
				{Title: "Checks", Items: []string{
					"Default project and default_focus diagnostics use cached IDs and availability. An empty or stale cache does not prove that a server target is invalid.",
					"The default focus target must be an open task in a usable cached list.",
				}},
				{Title: "Fix mode", Items: []string{
					"--fix asks before writing and keeps a copy of the previous configuration. Answering no writes nothing and still exits successfully.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, S then d runs the checks. f previews the supported notification fix and its backup.",
				}},
			},
			SeeAlso: []string{"config", "sync"},
		},
		run: func(inv *invocation) int {
			return cmdDoctor(inv.ctx, inv.stdin, inv.stdout, inv.stderr, inv.args)
		},
	})

	register(command{
		help: cli.Help{
			Verb:    "login",
			Summary: "authorize Open API access and optional Timer topics",
			Examples: []cli.Example{
				{Cmd: "tt login token", What: "recommended: save a personal Open API token through a hidden prompt"},
				{Cmd: "tt login", What: "alternative: obtain the Open API bearer through OAuth"},
				{Cmd: "tt login --port 8080", What: "listen for the redirect on another port"},
				{Cmd: "tt login --pkce --client-id OWN_ID", What: "use your registered public client with S256 PKCE"},
				{Cmd: "tt login web --same-account", What: "add the same account's Browser session and cache Timer topics"},
				{Cmd: "tt login token --same-account", What: "replace an expired personal token while preserving this account's queued work"},
				{Cmd: "tt auth status --check", What: "verify the Open API read channel"},
				{Cmd: "tt auth web status --check", What: "verify the Browser read channel and refresh Timer topics"},
			},
			Sections: []cli.HelpSection{
				{Title: "Which logins are needed", Items: []string{
					"Choose one Open API login: tt login token is recommended for a personal tt; tt login and tt login --pkce are alternatives that obtain the same kind of bearer.",
					"Open API access handles lists, tasks, projects and task-linked or unassigned focus history.",
					"Add tt login web --same-account only when you want Timer topics such as Work, Study or Reading. It does not replace the Open API login.",
					"The normal setup therefore stores two credentials: one Open bearer plus one Browser session. It does not require three simultaneous authorizations.",
				}},
				{Title: "Recommended setup", Items: []string{
					"1. Run tt login token and paste a personal Open API token at the hidden prompt. Alternatively complete one OAuth login.",
					"2. Sign in to ticktick.com in your browser with that same TickTick account.",
					"3. In browser developer tools, open site storage or cookies for ticktick.com and copy only the value of the cookie named t.",
					"4. Run tt login web --same-account and paste that value at the hidden prompt. Never put it in command arguments, shell history, screenshots or messages.",
					"5. Use tt sync for routine synchronization, then tt timer topic ls to see available topics. Separate auth status --check commands are optional diagnostics.",
				}},
				{Title: "Browser OAuth", Items: []string{
					"Default browser OAuth uses the client ID and secret from a TickTick developer app. --legacy selects that flow explicitly.",
					"--pkce --client-id OWN_ID uses S256 without a secret. It never borrows the official CLI identity or silently falls back; public-client registration support must be verified separately.",
					"The selected port must appear in the app registration as a redirect URI.",
				}},
				{Title: "Existing access token", Items: []string{
					"Normally, tt login obtains the access token through browser authorization.",
					"Another tt installation normally stores its token at ~/.local/share/ticktick/token, or $XDG_DATA_HOME/ticktick/token when that variable is set.",
					"Transfer the token securely, run tt login token and paste it only into the hidden prompt; do not put the token in the command itself.",
					"The token is stored in the data directory, never in the configuration file.",
				}},
				{Title: "After login", Items: []string{
					"Open API login methods pull lists and tasks without sending older queued changes.",
				}},
				{Title: "Renewing access", Items: []string{
					"For a replacement personal token, run tt login token --same-account. For OAuth, repeat the original login with --same-account and the same client and callback port.",
					"This flag confirms that the new credential belongs to this cache's account. A bounded server read must succeed before replacement; it proves access, not account identity. Another account needs a separate XDG_DATA_HOME.",
					"Queued work is preserved. Changing the Open API token requires tt login web --same-account again before Timer topics can be used. Unset TT_TOKEN before replacing a saved credential.",
					"For an expired Browser session, sign in to ticktick.com and repeat tt login web --same-account with its current t cookie. Previously saved topic sessions retain their destination and timing.",
					"Login never retries parked or rejected writes. After recovery, use tt sync; explicitly rejected focus writes still need tt timer sync --retry-failed. Uncertain writes are only read back.",
				}},
				{Title: "Timer topic session", Items: []string{
					"web reads only the value of the TickTick cookie named t through a hidden prompt. It never extracts a browser cookie or converts the Open API bearer.",
					"--same-account records your explicit confirmation that both credentials are for the same account. TickTick provides no automatic comparison route.",
					"The Browser credential is stored separately and is used only for Timer topics and topic-linked focus records.",
					"Changing the Open API credential blocks topic operations until web login is repeated. Explicit same-account Browser renewal also authorizes the existing frozen sessions without changing their requests or replaying uncertain writes.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, S then l previews browser login. L previews hidden token input after the terminal is restored.",
				}},
			},
			SeeAlso: []string{"auth", "timer", "doctor"},
		},
		run: func(inv *invocation) int {
			return cmdLogin(inv.ctx, inv.stdin, inv.stdout, inv.stderr, inv.args)
		},
	})

	register(command{
		help: cli.Help{
			Verb:    "notify",
			Summary: "run the configured end-of-session command",
			Examples: []cli.Example{
				{Cmd: "tt notify test", What: "run timer.on_end right now, with a made-up session"},
			},
			Sections: []cli.HelpSection{
				{Title: "Environment", Items: []string{
					"The configured command receives $TT_KIND, $TT_NOTE, $TT_TASK, $TT_PROJECT, $TT_DURATION and $TT_CYCLE.",
				}},
				{Title: "macOS helper", Items: []string{
					"Run tt setup notifications before tt doctor --fix. The bundled helper uses the tt logo and plays Glass three times; no Swift compiler is needed.",
				}},
				{Title: "Verification", Items: []string{
					"In tt ui, S then n previews a notification test. Command output stays private, and a successful exit does not prove that a notification was visible.",
				}},
			},
			SeeAlso: []string{"config", "doctor"},
		},
		run: func(inv *invocation) int {
			return cmdNotify(inv.ctx, inv.stdout, inv.stderr, inv.args)
		},
	})

	register(command{
		help: cli.Help{
			Verb:    "sync",
			Summary: "send queued changes and refresh the cache",
			Examples: []cli.Example{
				{Cmd: "tt sync", What: "refresh topics and catalogs, sync queued work and enabled focus uploads"},
				{Cmd: "tt sync --retry-failed", What: "put parked changes back in line, then sync"},
				{Cmd: "tt sync --drop-parked", What: "throw parked changes away, then sync"},
				{Cmd: "tt sync --allow-project-drop", What: "let the cache follow a server that no longer has some of your lists"},
				{Cmd: "tt sync queue --json", What: "inspect task, resource and focus operations"},
				{Cmd: "tt sync recover OPERATION_SEQ --preview", What: "preview exact resource recovery without sending"},
				{Cmd: "tt sync recover --accept PREVIEW_ID", What: "schedule the reviewed recovery for a separate sync"},
				{Cmd: "tt sync cancel OPERATION_SEQ --preview", What: "preview cancellation of a proven-unsent resource operation"},
			},
			Sections: []cli.HelpSection{
				{Title: "One routine sync", Items: []string{
					"tt sync refreshes Timer topics when Browser authorization is configured, sends resource and task queues, refreshes tasks and resource catalogs, then uploads eligible focus history when focus_upload.enabled is true.",
					"Catalogs include projects, folders, tags, habits, countdowns and project columns. Comments and dated histories repeat previously fetched scopes recorded by this version; sync never invents an unbounded history range.",
					"Each refresh reports success, skip or failure. Missing optional Browser login is a skip. Failed Browser access retains cached topics and does not stop eligible Open API work. Any failed stage makes the overall result partial with a nonzero exit status.",
				}},
				{Title: "Individual operations", Items: []string{
					"tt timer sync and tt pomodoro sync upload only focus history, even when automatic focus upload is disabled. tt timer topic ls --remote refreshes only Timer topics.",
					"Resource ls/show --remote commands still refresh their own scope. auth status --check and auth web status --check remain optional authorization diagnostics.",
				}},
				{Title: "Queue safety", Items: []string{
					"A change that cannot be sent is parked instead of retried forever. It remains parked until explicit recovery.",
					"--drop-parked permanently discards parked work and cannot be undone.",
					"Uncertain allocating requests are read back rather than blindly repeated. Task recovery never rearms focus uploads.",
				}},
				{Title: "Automatic and TUI sync", Items: []string{
					"The optional background service sends pending work at sync.interval and pulls server changes every five minutes.",
					"In tt ui, Q shows task operations and a separate read-only focus queue. f previews recovery of one failed or parked entry; s runs sync separately.",
				}},
			},
			SeeAlso: []string{"auto", "doctor", "ui"},
		},
		run: runSyncWithFocus,
	})

	register(command{
		help: cli.Help{
			Verb:    "auto",
			Summary: "run or inspect automatic sync and local reminder fallback",
			Examples: []cli.Example{
				{Cmd: "tt auto status", What: "show queues and the latest background sync result"},
				{Cmd: "tt auto run", What: "run one due background pass now"},
				{Cmd: "tt auto run --now", What: "force one sync after checking reminders"},
			},
			Sections: []cli.HelpSection{
				{Title: "Schedule", Items: []string{
					"The macOS installer registers a LaunchAgent that runs once a minute.",
					"Each pass handles due local fallback reminders first, then synchronizes pending task and focus work. With no local work, it pulls every five minutes.",
					"The reminder catch-up window after sleep is 15 minutes.",
				}},
				{Title: "Reminder fallback", Items: []string{
					"A clean server-confirmed task is left to TickTick. An unsynchronized timed task uses timer.on_end with TT_KIND=task.",
					"Supported local triggers are at, -Nmin, -Nh and their generated combined form.",
					"Current cached occurrences of repeating tasks are covered. tt does not invent future occurrences while offline.",
				}},
				{Title: "Failure handling", Items: []string{
					"Failed or uncertain task operations remain held for explicit recovery.",
				}},
			},
			SeeAlso: []string{"sync", "remind", "notify", "doctor"},
		},
		run: cmdBackground,
	})

	register(command{
		help: cli.Help{
			Verb:    "version",
			Summary: "print the version",
			Sections: []cli.HelpSection{
				{Title: "TUI", Items: []string{
					"In tt ui, S also shows the running version.",
				}},
			},
			Examples: []cli.Example{
				{Cmd: "tt version", What: "the version this binary was built at"},
				{Cmd: "tt --version", What: "the same thing"},
			},
		},
		run: func(inv *invocation) int {
			return cmdVersion(inv.stdout, inv.stderr, inv.args)
		},
	})
}
