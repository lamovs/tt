# tt

`tt` is a TickTick CLI and TUI for tasks, projects, checklists, habits, planned
intervals, sync recovery, and local or remote focus history.

Edits are saved locally first. The optional background service sends queued
changes automatically. `tt sync` still runs a pass immediately.

## Install

macOS 13+ and Linux, ARM64 or x86-64. Native Windows is not supported.
Go is not required for release packages.

With Homebrew:

```sh
brew install lamovs/tap/tt
```

Update with `brew upgrade lamovs/tap/tt`. Installing does not enable background
sync, log in, or change shell configuration.

Without Homebrew, download the matching archive and `checksums.txt` from
[Releases](https://github.com/lamovs/tt/releases). Verify its SHA-256 against
`checksums.txt`, extract it, and run `./tt setup install` from the extracted
directory. This installs into `~/.local/bin` and keeps old binaries. Keep that
directory on `PATH`. Repeat with a newer archive to update.

To build from source, use the Go version in `go.mod`:

```sh
go build -trimpath -buildvcs=false -o tt ./cmd/tt
```

Source builds do not include the prebuilt macOS notification app.

## Optional setup

```sh
tt setup notifications
tt doctor --fix
tt notify test
tt setup background
```

Notifications and background sync are independent. `tt setup background`
enables a user service and can immediately send queued changes using the saved
login. `tt setup background --remove` stops it and retains its files as backups.

macOS uses a bundled notification app; Swift is not required. The app is
ad-hoc signed, not notarized, so macOS may require approval before opening it.
Allow notifications for `tt` in System Settings. Linux uses `notify-send`
from your distribution's packages and requires a desktop session.

`tt setup herdr` and `tt setup tmux` print configuration fragments to merge
into your existing settings. They do not modify or reload personal configs.

## Login

Choose one Open API login:

| Method | Command | Required |
| --- | --- | --- |
| API token (recommended) | `tt login token` | Token from TickTick Settings > Account > API Token |
| OAuth | `tt login --legacy` | Developer app client ID, secret and matching redirect URI |
| OAuth PKCE | `tt login --pkce --client-id CLIENT_ID` | Registered public client with S256 PKCE support |

`tt login` also opens OAuth login. OAuth uses callback port 9977 by default;
add `--port N` to change it. Paste secrets only into the hidden prompts.
Open API login saves the credential and pulls lists and tasks without sending
queued changes. Then run `tt ui`.

Timer topics require a separate Browser login for the same account:

```sh
tt login web --same-account
tt sync
tt timer topic ls
```

Sign in to `ticktick.com`, open the browser's site-storage or cookie view, and
copy the value of cookie `t` into the hidden prompt. This is an optional second
credential, not a replacement for Open API login. tt does not extract or renew
the cookie automatically. Never put tokens or cookies in command arguments.

`tt auth status` and `tt auth web status` read local status; add `--check` for
a server check. `tt auth logout` signs out of Open API locally;
`tt auth web logout` removes Browser login. Local tasks and queues are retained.
For renewal, see [Sync and recovery](#sync-and-recovery).

## Quick reference

Tasks:

```sh
tt ui
tt today
tt ls
tt s report
tt show 3
tt add "Send report" --in 2h
tt done 3
tt undo
tt sync
tt auto status
```

Pomodoro and timer:

```sh
tt pomodoro start
tt pomodoro start -d 50m
tt pomodoro start 28 -d 25m --note "Work"
tt pomodoro start --topic Work -d 25m
tt timer start --topic Study
tt timer status
tt timer pause
tt timer resume
tt timer stop
tt timer cancel
tt timer history
tt notify test
```

Timer and Pomodoro share one active focus session. `tt timer status` shows
either mode. Run `tt help <command>` for complete syntax and examples.

Commands by area:

- browse: `ui`, `today`, `ls`, `s`, `show`
- change tasks: `add`, `edit`, `item`, `done`, `pri`, `due`, `schedule`,
  `repeat`, `remind`, `mv`, `rm`, `undo`
- focus: `timer`, `pomodoro`
- setup and maintenance: `setup`, `login`, `sync`, `auto`, `doctor`, `config`, `notify`
- documentation: `help`, `version`

## Dates and planned intervals

Common date forms include:

```text
today
tmr
fri
next mon
2026-09-15
21.10.2026
14:30
tmr 09:00
in 2h
+30min
+1m
```

`min` means minutes. In date arithmetic, `m` means a calendar month, so `+1m`
does not mean one minute.

A timed task uses `startDate` as the start and `dueDate` as the end. The first
supported interval form is non-recurring. Planned duration is scheduling data;
it is not focus time.

Examples:

```sh
tt add "Review" --schedule "tmr 10:00 + 45min" --preview
tt schedule 3 "fri 14:00 + 2h" --preview
tt schedule 3 "fri 14:00 + 2h"
```

Times are interpreted in the configured time zone. Invalid or ambiguous DST
times are rejected instead of being silently shifted. A save checks cached
tasks for overlapping planned intervals and shows a preview before continuing.

## TUI

Start the interface in a normal terminal:

```sh
tt ui
```

WezTerm works directly; tmux and Herdr are optional and are not required.
The minimum terminal size is 32 columns by 10 rows.

Navigation:

- arrows or `j`/`k`: move; `PgUp`/`PgDn` and `Home`/`End`: move farther
- `1`/`2`/`3`: select a panel; `Tab`/`l` and `Shift+Tab`/`h`: switch panels
- `+`/`_`: change panel sizes; `r`: refresh the local cache
- `Enter`: open or confirm; `Esc`: go back or cancel
- `/`: search; `?`: help outside text fields; `F1`: help everywhere
- `q`: quit; `Ctrl+C`: interrupt current work and exit safely

Task actions:

- `a`/`n`: add a task or note
- `e`/`Shift+E`: edit in a form or in `VISUAL`/`EDITOR`
- `c`/`d`: edit checklist items or dates, repeat, and reminders
- `Space`: complete or reopen; `Ctrl+D`/`m`: preview deletion or a move
- `u`: global undo; `s`: sync

Forms and checklists:

- `Tab` and `Shift+Tab`: move between fields
- `Ctrl+S`: preview or save; `Ctrl+E`: edit the draft in `VISUAL` or `EDITOR`
- schedule form: `Ctrl+X` clears, `Ctrl+P` cycles repeat presets, `Ctrl+N`
  adds a reminder, `Ctrl+D` removes one, and `Alt+Up`/`Alt+Down` reorders
- checklist: `a` adds, `e` renames, `Space` toggles, `Ctrl+D` removes, and
  `Alt+Up`/`Alt+Down` reorders

Other workspaces:

- `t`: Focus; `Tab` switches the active session and history
- `Q`: queue; `Tab` switches task and focus uploads, `Enter` opens details,
  `f` previews recovery, and `s` syncs
- `S`: settings; `r` reloads, `g` chooses default focus, `i` initializes the
  config, `d` runs doctor, `n` tests notifications, and `l`/`L` previews login
- `Ctrl+O`: show the `1`/`2`/`3` panel hints

Forms save locally with `Ctrl+S`. Destructive and remote-sensitive actions show
a preview and require confirmation. If data changes concurrently, `tt` refuses
the stale action and requires a new preview.

## Focus timers

CLI controls:

```sh
tt timer start
tt pomodoro start
tt pomodoro start -d 50m
tt timer topic ls --remote
tt timer start --topic Work
tt pomodoro start --topic Study -d 25m
tt timer status
tt timer pause
tt timer resume
tt timer stop
tt timer cancel
tt timer history
tt timer sync
tt notify test
tt timer note SESSION_ID --note "What I learned"
tt timer note SESSION_ID --skip
```

End notifications are opt-in. On macOS, run `tt setup notifications`,
then `tt doctor --fix`, accept the suggested notifier, and run
`tt notify test`. The helper uses the `tt` logo and plays the built-in `Glass`
sound three times without starting three players. Start `--note` supplies a
session note, not a named TickTick Timer destination. If macOS plays the sound
but hides the banner, open
`System Settings -> Notifications -> tt`, enable `Allow notifications`,
`Desktop`, and `Play sound for notification`, then run `tt notify test` again.

TUI controls:

- `t`: open Focus
- `a`: use the selected task or `default_focus`; `A`: force `default_focus`;
  `N`: unassigned
- arrows: choose Timer or Pomodoro and task/Timer topic/unassigned; `Tab`:
  change fields
- `Ctrl+S`: preview the start; `Enter`: confirm it
- `p`: pause or resume; `x`: preview stop; `Ctrl+D`: preview cancel
- `Tab`: open history; `Enter`: details; `U`: upload; `f`: retry a definitive
  rejection; `r`: refresh local state; `R`: refresh Timer topics; `s`: sync;
  `u`: global task undo

Interactive CLI and TUI starts ask for an optional completion note after the
timer stops. The new thought is appended to the start note in focus history;
it is not a task comment. Enter with no text skips in both CLI and TUI, keeping
the start note. CLI EOF or interruption defers it. In the TUI, Ctrl+S saves text;
Enter with text inserts a newline, and Escape defers an empty form.
Unsaved text asks before discard or quit. The combined
note can contain at most 5000 characters. In Focus, `e` reopens a pending review.

Closing the TUI does not stop an active timer. Pending note reviews survive
closing and reopening it. Detached countdowns cannot ask in the old terminal;
their review is offered on the next interactive timer command or TUI visit.
`timer note SESSION_ID --note TEXT` or `--skip` resolves an exact pending review
without prompting. `timer status --json` includes pending `note_reviews`,
independent of the latest-20 history limit. CLI `--no-prompt` suppresses automatic
offers; on start it
also disables review for that session. JSON and headless starts never prompt
and preserve automatic upload behavior. Existing pending reviews stay held
until resolved explicitly. Old sessions are not retroactively opted in.

With `focus_upload.enabled = true`, completion wakes background task sync and
focus upload. Sessions awaiting a note stay local while other eligible work
continues. Saving or skipping the note releases its upload without repeating
the completion notification. Offline uploads stay queued. An uncertain upload
is read back and is never blindly repeated; its frozen note cannot be edited.

Existing named TickTick Timer topics such as Work, Study or Reading can be selected
for both Timer and Pomodoro. `tt sync` and `s` in the TUI refresh the catalog.
Use `tt timer topic ls --remote` or `R` in Focus for a topic-only refresh. Starts accept
exact topic IDs or case-insensitive exact names; ambiguous names require an ID.
Task, Timer topic and unassigned are mutually exclusive destinations. A session
freezes the selected topic ID, name and Browser credential identity.

Timer topics use TickTick's Browser API, which is not part of the documented
Open API. The Browser session can expire independently. Topic uploads retain the
same no-blind-retry and exact read-back rules as other focus history. A
user-confirmed same-account binding is required because TickTick exposes no
cross-channel identity comparison endpoint.

`tt timer indicator` prints the current timer or Pomodoro for terminal status
bars. It reads local state without changing the session or using the network.

For Herdr, merge [integrations/herdr/tt.toml](integrations/herdr/tt.toml) into
the existing `[ui]` section of `~/.config/herdr/config.toml`, then run
`herdr config check` and `herdr server reload-config`. This uses Herdr's own
top tab row, including with one tab, and does not need a WezTerm status bar.
Herdr must support `tab_bar_right` command entries (verified with 0.8.2).
The same adapter can be used when Herdr runs inside Ghostty or WezTerm.

For tmux, source [integrations/tmux/tt.conf](integrations/tmux/tt.conf) from
your tmux config. Both adapters use the multiplexer's status row, with no
separate outer-terminal integration or duplicate indicator.
The command runs on the multiplexer server, not necessarily the client machine
when using a remote session. Herdr may hide the status area when tabs need the
available width. No active timer, or a hidden indicator, produces no status text.
System completion notifications are separate from these status bars.

## Sync and recovery

Use `tt sync` for routine synchronization. The same pass is used by `s` in the
TUI and the background service:

- Refresh Timer topics when Browser authorization is configured.
- Send resource and task queues and refresh tasks and lists.
- Refresh project, folder, tag, habit, countdown and Kanban-column catalogs.
- Repeat previously fetched comment scopes and dated histories recorded by
  this version, keeping their exact bounds. Older cache metadata without a
  saved query needs one individual `--remote` fetch to register its scope.
- Upload eligible Timer/Pomodoro sessions when `focus_upload.enabled` is true.
  If disabled, the report explains that `tt timer sync` still uploads explicitly.

Each catalog reports refreshed, skipped or failed. A missing optional Browser
login is skipped. Expired Browser access leaves cached topics intact and does
not stop eligible Open API work; any failed stage returns a partial result and
a nonzero exit status. Task-sync failure holds the subsequent focus upload.

Individual commands remain available: `tt timer sync` / `tt pomodoro sync` for
focus uploads, `tt timer topic ls --remote` for topics, and resource `--remote`
commands for their own scope. `tt auth status --check` and
`tt auth web status --check` are optional diagnostics.

Authorization failures name the Open API token or Browser session and give a
recovery command. HTTP 401 means rejected access, possibly expired or revoked;
HTTP 403 means denied access and does not establish expiry. Network failures
are reported separately. The next server request detects rejection; there is
no guaranteed advance expiry notification.

For a new personal token of the same account, run
`tt login token --same-account`. For OAuth, repeat the original login with
`--same-account`, the same client and callback port. A bounded read verifies
access before replacement. The flag is your explicit same-account confirmation,
not provider identity proof. Local queues survive renewal; another account must
use a separate `XDG_DATA_HOME`. Unset `TT_TOKEN` before replacing a saved token.

After an Open API token change, run `tt login web --same-account` again. For an
expired Browser session, sign in to `ticktick.com`, copy the current `t` cookie
and use that same command. Confirmed renewal lets frozen topic sessions use the
replacement credential without changing destination, timing, request IDs or
upload phases. Recovery itself never sends queued writes.

Writes that may have reached the server are not retried blindly. Rejected focus
writes still require `tt timer sync --retry-failed` after authorization recovery.
Inspect failed or uncertain
operations in the TUI with `Q`, or use the recovery commands shown by
`tt help sync`.

The installed background service checks once a minute. It syncs waiting task or
focus work at `sync.interval` with a one-minute minimum, and pulls server changes
every five minutes when idle.
Run `tt auto status` for its latest result. A timed task with an explicit `at`,
`-Nmin` or `-Nh` reminder is left to TickTick after its current schedule has
been confirmed by the server. If it is still local when the reminder is due,
`tt` uses the configured notifier. After sleep, reminders up to 15 minutes old
are recovered once. Unknown raw triggers remain provider-only.

Run `tt doctor --fix` after installing a notification helper. On Linux this
requires `notify-send` in the desktop session. Remove the background service
with `tt setup background --remove` on either platform.

Task numbers refer to the current local listing. Exact task and project IDs are
used internally for writes, previews, undo, and recovery.

## Projects, habits and other resources

```sh
tt project ls --remote
tt folder ls --remote
tt tag ls --remote
tt habit ls --remote
tt countdown ls --remote
tt folder add Work --preview
tt folder add --accept PREVIEW_ID
tt sync
tt sync queue --json
tt sync cancel OPERATION_SEQ --preview
tt sync cancel --accept PREVIEW_ID
```

Without `--remote`, resource lists use the local cache and report its coverage.
Writes create an exact local preview; copy its ID into the same action with
`--accept`. Acceptance queues the operation, not a network write. A changed
target invalidates the preview. `sync recover` uses the same preview/accept
workflow as `sync cancel`; uncertain writes remain read-back only. An object
creation with no proven returned ID stays held rather than being retried by name.
Global undo cancels the latest proven-unsent resource operation; it refuses to
invent a remote reversal after sending or confirmation.

Projects support add/edit; folders support add/rename; tags support add; habits
support add/edit. Tag rename/delete, habit delete, project-folder membership,
column delete/reorder and container cascade are unavailable until their provider
contracts are verified. This also applies to apparently empty containers.
`countdown` is read-only calendar data, not a local timer.
Use `--resource-color` where a resource supports a color field; global `--color`
continues to control terminal rendering only.

```sh
tt project column ls --project PROJECT_ID --remote
tt project column add Doing --project PROJECT_ID --preview
tt comment ls TASK --remote
tt comment add TASK "Review notes" --preview
tt comment rm TASK COMMENT_ID --preview
tt habit history HABIT_ID --from 2026-09-11 --to 2026-09-11 --remote
tt habit checkin HABIT_ID --date 2026-09-11 --value 1 --preview
```

A check-in sets an absolute value for one date, never increments it. Refresh the
exact date before the first check-in preview. Existing-day edits compare the
remote baseline before sending. Comment deletion cannot restore its history.

In the TUI, `:` opens resource and server-query workspaces. Resource `R` fetches
explicitly, `a`/`e` open supported forms, `Ctrl+S` previews and `Enter` confirms.
Resource `Q` shows the same outbox's resource operations. In a project task view,
`b` opens the Kanban layout; `Alt+Left`/`Alt+Right` select columns. Column create
and rename are supported. `Alt+Shift+Left`/`Alt+Shift+Right` preview moving the
selected card to an adjacent column; Enter queues that exact move and Esc
cancels. The move uses the Official Open API and does not complete the task or
change its checklist, tags, reminders or sort order. Unknown column
IDs remain visible rather than being silently assigned elsewhere.

## Task fields and native moves

```sh
tt edit TASK --parent PARENT_TASK --preview
tt edit TASK --parent none --preview
tt edit TASK --estimated-duration 30m --estimated-pomo 2 --preview
tt edit TASK --sort-order 100 --preview
tt edit --accept PREVIEW_ID
tt mv TASK id:PROJECT_ID --preview
tt mv --accept PREVIEW_ID
```

Native moves preserve the task ID and use the provider's move endpoint. A move
currently previews one confirmed task at a time; pending edits, parent/child
relationships and column bindings must be resolved first. Global undo checks
whether cancellation or a confirmed reverse move is safe. `mv --recreate` is an
explicit legacy fallback controlled by `sync.move_by_recreate`; it creates a new
task ID, loses server history and has no move undo. Existing queued legacy moves
keep their original protocol.

Task estimates use seconds and Pomodoro counts from 0 to 60. Writes use the
provider's nested `focusSummaries` format without sending observed counters.
Ambiguous multiple summaries are retained but not edited. Parent edits reject
cycles and cross-project relationships. `columnId`, child IDs, sort order and
unknown server fields survive caching. To assign an existing confirmed column:

```sh
tt project column ls --project PROJECT_ID --remote
tt edit TASK --column COLUMN_ID --preview
tt edit --accept PREVIEW_ID
tt sync
```

Card assignment requires a synchronized open task without parent/child links,
no pending task edits, and a confirmed column in the same project. Column writes
cannot be mixed with other edits. Undo cancels only an unsent column operation;
after a confirmed send, prepare a new move back. An uncertain reply is recovered
by reading the same task, never by blindly resending the write. Clearing a
column, reordering columns and deleting columns are not supported.

The Official Open API endpoint accepts a minimal `columnId` update in verified
live checks, although this writable field is not currently documented. There
is no Browser API dependency. Server preflight detects an already changed
column, but does not provide atomic compare-and-set: a simultaneous move in
another client can still race. Later pulls reconcile the server's column.

## Server queries and structured output

```sh
tt ls --completed --remote
tt ls --filter --tag work --remote
tt s report --remote
tt s report --server
tt ls --json
tt show TASK --json --raw
tt project ls --json --raw
```

Server query results have a separate exact-query cache. They never prune the
task cache, change task numbering or overwrite queued edits. Completed/filter
responses can stop at 200 records; search coverage is unknown. Cached server
queries use the same arguments without `--remote` (`s` also needs `--server`).

`--json` emits one versioned result with `schema_version`, `command`, `status`,
typed `data`, `meta`, `warnings` and `error`. Metadata identifies source, fetch
time when known, coverage and pending state. Errors retain nonzero exit status.
`--raw` is available only for supported reads and remains inside this envelope.
Machine mode never opens a picker or editor; interactive-only operations return
a structured refusal before changing state. Text output remains the default.

## Remote focus and reminder interpretation

```sh
tt timer focus ls --type 1 --from 2026-09-01T00:00:00Z --to 2026-09-11T00:00:00Z --remote
tt timer focus show FOCUS_ID --type 1 --remote
tt timer focus add --type 1 --from 2026-09-11T10:00:00Z --to 2026-09-11T10:30:00Z --pause 30 --preview
tt timer focus add --accept PREVIEW_ID
tt timer sync
tt timer focus rm FOCUS_ID --type 1 --preview
tt timer focus rm --accept PREVIEW_ID
tt sync
tt remind TASK 'TRIGGER;RELATED=START:-P1DT30S'
tt remind TASK --explain --json
```

Focus type 0 is Pomodoro; type 1 is Timing. List requests are split into bounded
30-day windows. Manual records enter the existing focus uploader and do not
replace an active timer. Reaccepting the same record preview does not duplicate
its local session. Confirmed remote deletions suppress re-upload of linked local
history; uncertain deletion is resolved by an exact addressed read.

In the TUI's remote focus workspace, `0`/`1` select the focus type, `e` edits the
history range, and `R` explicitly fetches it. `a` previews a completed manual
record; `Ctrl+D` previews deletion of the selected exact remote record. Opening
the workspace reads only its cache. The local timer workspace remains separate.

The reminder parser understands START/END relations and seconds, minutes, hours,
days, weeks, months and years. `remind --explain` separates parsed offsets,
computable timestamps and local delivery eligibility. Missing reference policy,
calendar month/day behavior, all-day conventions and ambiguous civil times are
reported explicitly. The accepted local delivery allowlist is unchanged; an
interpreted timestamp does not establish provider semantics or device receipt.

## Configuration and files

Configuration is optional:

```sh
tt config
tt config --init
tt config default-project
tt config default-focus
tt doctor
```

Default paths are:

```text
~/.config/tt/config.toml
~/.local/share/ticktick/token
~/.local/share/ticktick/oauth.json
~/.local/share/ticktick/web-auth.json
~/.local/share/ticktick/cache.db
```

`XDG_CONFIG_HOME` and `XDG_DATA_HOME` override these locations. Secret files and
their directory must not be accessible to other users.

`default_project` controls where new tasks are created when no list is chosen.
`default_focus` controls the task used by timer starts that omit a task. Neither
setting silently falls back to another task or list when its saved target is no
longer usable.
