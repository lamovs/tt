package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func init() {
	for _, verb := range []string{"timer", "pomodoro"} {
		example := "tt timer start 3"
		summary := "measure local focus time and keep completed sessions"
		if verb == "pomodoro" {
			example = "tt pomodoro start 3 -d 25m"
			summary = "run one local focus countdown"
		}
		register(command{help: cli.Help{
			Verb: verb, Summary: summary,
			Examples: []cli.Example{
				{Cmd: example, What: "start for task 3; omit the task to use default_focus"},
				{Cmd: "tt " + verb + " start --topic Work", What: "start for one exact cached Timer topic"},
				{Cmd: "tt " + verb + " topic ls --remote", What: "refresh and list Timer topics through Browser authorization"},
				{Cmd: "tt " + verb + " start --none", What: "start explicitly unassigned, overriding default_focus"},
				{Cmd: "tt " + verb + " start --note \"Read chapter 2\"", What: "attach a note to a new session"},
				{Cmd: "tt " + verb + " pause", What: "pause the active session without counting the pause"},
				{Cmd: "tt " + verb + " resume", What: "continue the active session"},
				{Cmd: "tt " + verb + " stop", What: "save the active time as a completed session"},
				{Cmd: "tt " + verb + " note SESSION_ID --note \"Finished chapter 2\"", What: "append a completion note to one pending focus session"},
				{Cmd: "tt " + verb + " note SESSION_ID --skip", What: "keep the existing note and release one pending review"},
				{Cmd: "tt " + verb + " status --no-prompt", What: "show state without offering a completion-note prompt"},
				{Cmd: "tt " + verb + " cancel", What: "save an aborted session locally"},
				{Cmd: "tt " + verb + " status", What: "show the active session; this is also the bare command"},
				{Cmd: "tt " + verb + " indicator", What: "read one external status line without completing or recovering the timer"},
				{Cmd: "tt " + verb + " indicator --format json", What: "read versioned local display data; plain and tmux formats are also available"},
				{Cmd: "tt " + verb + " start --indicator off", What: "hide only this new session's external indicator, even after leaving CLI/TUI"},
				{Cmd: "tt " + verb + " history", What: "show the latest 20 local sessions without a network request"},
				{Cmd: "tt " + verb + " sync", What: "upload saved sessions to TickTick"},
				{Cmd: "tt " + verb + " sync --retry-failed", What: "retry definitive rejections; uncertain sends are only read back"},
				{Cmd: "tt timer focus ls --type 1 --from FROM --to TO --remote", What: "fetch an RFC3339 range into the remote history cache"},
				{Cmd: "tt timer focus show FOCUS_ID --type 1 --remote", What: "read one exact remote record; type 0 is Pomodoro, 1 is Timing"},
				{Cmd: "tt timer focus add --type 1 --from FROM --to TO --preview", What: "preview one completed RFC3339 interval"},
				{Cmd: "tt timer focus add --accept PREVIEW_ID", What: "save the reviewed record for the existing focus uploader"},
				{Cmd: "tt timer focus rm FOCUS_ID --type 1 --preview", What: "preview deletion of an exact cached remote record; accept queues it for tt sync"},
			},
			Sections: []cli.HelpSection{
				{Title: "Timer modes and destination", Items: []string{
					"timer counts up and pomodoro counts down. Both commands share one active session.",
					"An explicit task, --topic NAME_OR_ID or --none overrides default_focus. These destinations are mutually exclusive.",
					"Timer topics use a separately bound Browser session. Run tt login web --same-account, then use tt sync for routine refresh or topic ls --remote for topics only.",
					"Configure the default with tt config default-focus. It must be none or an open task in a usable cached list.",
					"Later configuration changes, task renames and task deletion never redirect saved sessions or queued uploads.",
					"Pomodoro uses timer.focus unless -d provides a positive whole-second duration up to 24h, such as 25m or 90s.",
				}},
				{Title: "Session lifecycle", Items: []string{
					"Pauses survive process restarts. A countdown watcher runs in the background; status recovers it when needed.",
					"An expired countdown reached zero but has not yet been reconciled. Run status to save the completion and restart any missing watcher.",
					"stop saves actual active time, including a partial Pomodoro. There is no automatic break or next focus interval.",
				}},
				{Title: "External indicator", Items: []string{
					"indicator is read-only. timer.indicator defaults to true; start --indicator on|off overrides visibility for one session.",
					"Hiding the indicator never stops a session or changes upload and notification policy.",
					"indicator supports plain, json and tmux formats and width 32..512. Plain and tmux output is empty while idle or hidden; JSON still reports state.",
				}},
				{Title: "Completion and notifications", Items: []string{
					"Pomodoro completion and timer stop run timer.on_end. With focus_upload.enabled, automatic task sync runs before focus upload.",
					"Notification delivery is at most once: process loss may lose a notification, but not the saved session.",
					"Use tt doctor --fix to configure the system notifier and tt notify test to test it.",
				}},
				{Title: "Completion notes", Items: []string{
					"Notes must be valid UTF-8 and at most 5000 characters.",
					"Interactive starts and stops may request a focus-history completion note. New text is appended to the start note; Enter skips without erasing it. This is not a task comment.",
					"An unresolved review persists and holds only that session's upload. EOF or Escape defers it.",
					"--no-prompt disables CLI prompts and review intent for a new start. JSON, redirected input and background watchers never prompt.",
					"Use note SESSION_ID --note TEXT or --skip to resolve one pending review without prompting.",
				}},
				{Title: "TickTick history", Items: []string{
					"TickTick receives completed history records, not control of a remotely running timer.",
					"FROM and TO are increasing RFC3339 timestamps with offsets.",
					"Remote focus reads use type 0 for Pomodoro and type 1 for Timing. Manual add and delete use exact preview and accept steps.",
					"Uncertain uploads are checked by read-back instead of repeating the POST.",
				}},
				{Title: "TUI", Items: []string{
					"In tt ui, t opens Focus. a starts for the selected task, A uses default_focus and N starts unassigned.",
					"p pauses or resumes, x previews stop and Ctrl+D previews cancel. Tab opens history; U uploads or checks one session and f retries a definitive rejection.",
					"Closing the TUI leaves the session running.",
				}},
			},
			SeeAlso: []string{"timer", "pomodoro", "ui", "notify", "config"},
		}, run: cmdTimer})
	}
}

type timerArguments struct {
	action    string
	task      []string
	note      string
	planned   time.Duration
	duration  bool
	session   string
	indicator store.IndicatorMode
	none      bool
	noPrompt  bool
	topic     string
}

type timerRuntime struct {
	now    func() time.Time
	watch  func(string) error
	upload func(*invocation, *store.Store) int
	notify func(context.Context, string, NotifyVars) (NotifyResult, error)
	wake   func() error
}

func defaultTimerRuntime() timerRuntime {
	return timerRuntime{now: time.Now, watch: startTimerWatcher, upload: timerUpload, notify: RunNotify, wake: startBackgroundWorker}
}

func cmdTimer(inv *invocation) int {
	if len(inv.data) > 0 && inv.data[0] == "topic" {
		return cmdTimerTopic(inv)
	}
	if inv.verb == "timer" && len(inv.data) > 0 && inv.data[0] == "focus" {
		return cmdRemoteFocus(inv)
	}
	return cmdTimerWithRuntime(inv, defaultTimerRuntime())
}

func parseTimerArguments(inv *invocation) (timerArguments, int) {
	out := timerArguments{action: "status"}
	if len(inv.data) > 0 {
		out.action = inv.data[0]
	}
	switch out.action {
	case "start":
		out.task = inv.data[1:]
	case "pause", "resume", "stop", "cancel", "status", "history", "sync":
		if len(inv.data) > 1 {
			return out, inv.misuseWord("unexpected argument ", inv.data[1])
		}
	case "_watch":
		if len(inv.data) != 2 || inv.data[1] == "" || len(inv.refinements) != 0 {
			return out, inv.misuse("invalid watcher invocation")
		}
		out.session = inv.data[1]
		return out, exitOK
	default:
		return out, inv.misuseWord("unknown operation ", out.action)
	}
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		flag := inv.refinements[i]
		if seen[flag] {
			return out, inv.misuseWord("repeated option ", flag)
		}
		seen[flag] = true
		if flag == "--no-prompt" {
			out.noPrompt = true
			continue
		}
		if out.action == "start" && flag == "--none" {
			if len(out.task) != 0 || out.topic != "" {
				return out, inv.misuse("--none cannot be combined with a task or --topic")
			}
			out.none = true
			continue
		}
		if out.action == "sync" && flag == "--retry-failed" {
			continue
		}
		if out.action != "start" || (flag != "--note" && flag != "--indicator" && flag != "--topic" && !(flag == "-d" && inv.verb == "pomodoro")) {
			return out, inv.misuseWord("unknown option ", flag)
		}
		if i+1 >= len(inv.refinements) {
			return out, inv.misuseWord("missing value for ", flag)
		}
		i++
		value := inv.refinements[i]
		if flag == "--indicator" {
			switch value {
			case "on":
				out.indicator = store.IndicatorOn
			case "off":
				out.indicator = store.IndicatorOff
			default:
				return out, inv.misuseWord("expected --indicator on or off, got ", value)
			}
			continue
		}
		if flag == "--note" {
			out.note = value
			continue
		}
		if flag == "--topic" {
			if len(out.task) != 0 || out.none {
				return out, inv.misuse("--topic cannot be combined with a task or --none")
			}
			out.topic = value
			continue
		}
		duration, err := model.ParseDuration(value)
		if err != nil || duration <= 0 || duration.Duration() > 24*time.Hour {
			return out, inv.misuseWord("expected a whole-second duration from 1s to 24h, got ", value)
		}
		out.planned, out.duration = duration.Duration(), true
	}
	return out, exitOK
}

func cmdTimerWithRuntime(inv *invocation, runtime timerRuntime) (code int) {
	if len(inv.data) > 0 && inv.data[0] == "indicator" {
		return cmdTimerIndicator(inv)
	}
	if len(inv.data) > 0 && inv.data[0] == "note" {
		return cmdTimerNote(inv, runtime)
	}
	args, code := parseTimerArguments(inv)
	if code != exitOK {
		return code
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	if args.action == "_watch" && inv.jsonOutput {
		return inv.misuse("the internal timer watcher does not support --json")
	}
	cfg, err := inv.config()
	if err != nil {
		return timerFailure(inv, err)
	}
	st, err := inv.openStore()
	if err != nil {
		return timerFailure(inv, err)
	}
	defer st.Close()
	if args.action == "_watch" {
		return timerWatch(inv, st, args.session, runtime)
	}
	interactiveReview := !inv.jsonOutput && !args.noPrompt && canAsk(inv)
	reviewSession := ""
	if interactiveReview {
		defer func() {
			if code == exitOK && !inv.interrupted() {
				code = offerTimerNote(inv, st, reviewSession, runtime)
			}
		}()
	}
	if args.action == "history" {
		history, err := st.TimerHistory(inv.ctx, 20)
		if err != nil {
			return timerFailure(inv, err)
		}
		if inv.jsonOutput {
			rows := make([]any, 0, len(history))
			for _, session := range history {
				rows = append(rows, timerSessionData(session))
			}
			inv.resultData = rows
		}
		if len(history) == 0 {
			fmt.Fprintln(inv.stdout, "No local focus sessions.")
		}
		for _, session := range history {
			printTimerSession(inv, st, session)
		}
		return exitOK
	}
	if args.action == "sync" {
		return runtime.upload(inv, st)
	}
	var result store.TimerResult
	switch args.action {
	case "start":
		options := store.TimerStartOptions{FocusType: 1, Note: args.note, Indicator: args.indicator, ReviewNote: interactiveReview}
		if inv.verb == "pomodoro" {
			options.FocusType, options.Planned = 0, cfg.Timer.Focus.Duration()
			if args.duration {
				options.Planned = args.planned
			}
		}
		if len(args.task) != 0 {
			ids, code, ok := inv.resolveTasks(st, args.task, store.StatusAll)
			if !ok {
				return code
			}
			if len(ids) != 1 {
				return inv.misuse(oneTaskOnly, len(ids))
			}
			options.TaskID = ids[0]
		}
		if args.topic != "" {
			topic, topicCode, ok := resolveTimerTopic(inv, st, args.topic)
			if !ok {
				return topicCode
			}
			options.TopicID, options.TopicName, options.TopicCredential = topic.ID, topic.Name, topic.CredentialFingerprint
		}
		if len(args.task) == 0 && args.topic == "" && !args.none && cfg.DefaultFocus.TaskID != "" {

			var snapshot store.TimerSnapshot
			snapshot, err = st.ReadTimer(inv.ctx, runtime.now())
			if err == nil {
				var guard store.TimerGuard
				guard, err = app.BindDefaultFocus(inv.ctx, st, snapshot.Guard, cfg.DefaultFocus)
				if err == nil {
					options.TaskID = guard.TaskID
					result, err = st.ControlTimer(inv.ctx, "start", options, &guard, runtime.now())
				}
			}
		} else {
			result, err = st.StartTimer(inv.ctx, options, runtime.now())
		}
	case "pause":
		result, err = st.PauseTimer(inv.ctx, runtime.now())
	case "resume":
		result, err = st.ResumeTimer(inv.ctx, runtime.now())
	case "stop":
		result, err = st.ControlTimer(inv.ctx, "stop", store.TimerStartOptions{ReviewNote: interactiveReview}, nil, runtime.now())
	case "cancel":
		result, err = st.CancelTimer(inv.ctx, runtime.now())
	case "status":
		result, err = st.TimerStatus(inv.ctx, runtime.now())
	}
	if err != nil {
		return timerFailure(inv, err)
	}
	var noteReviews []store.FocusTarget
	if args.action == "status" {
		noteReviews, err = st.PendingFocusNoteReviews(inv.ctx)
		if err != nil {
			return timerFailure(inv, err)
		}
	}
	if inv.jsonOutput {
		var state, completed any
		if result.State != nil {
			state = timerStateData(*result.State)
		}
		if result.Completed != nil {
			completed = timerSessionData(*result.Completed)
		}
		inv.resultData = map[string]any{"operation": args.action, "state": state, "completed": completed}
		if args.action == "status" {
			reviews := make([]any, 0, len(noteReviews))
			for _, target := range noteReviews {
				entry := timerSessionData(target.Session)
				entry["version"] = target.Version
				reviews = append(reviews, entry)
			}
			inv.setResultPart("note_reviews", reviews)
		}
		inv.resultChanged = args.action != "status" || result.Completed != nil
	}
	code = exitOK
	if result.State != nil {
		printTimerState(inv, st, *result.State)
		if result.State.FocusType == 0 && (args.action == "start" || args.action == "resume" || args.action == "status") {
			if err := runtime.watch(result.State.SessionID); err != nil {
				timerFailure(inv, err)
				fmt.Fprintln(inv.stderr, "The session is saved; run status to recover its countdown watcher.")
				code = exitError
			}
		}
	} else if result.Completed == nil {
		fmt.Fprintln(inv.stdout, "No active timer.")
	}
	if args.action == "status" && !inv.jsonOutput && len(noteReviews) != 0 {
		fmt.Fprintf(inv.stdout, "Pending completion-note reviews: %d\n", len(noteReviews))
		for _, target := range noteReviews {
			timerField(inv, "Session: ", target.Session.ID)
		}
		fmt.Fprintln(inv.stdout, "Resolve an exact session with timer note SESSION_ID --note TEXT or --skip.")
	}
	if result.Completed != nil {
		reviewSession = result.Completed.ID
		printTimerSession(inv, st, *result.Completed)
		if completedCode := timerAfterCompletion(inv, st, *result.Completed, runtime); completedCode != exitOK {
			code = completedCode
		}
	}
	return code
}

func timerFailure(inv *invocation, err error) int {
	if inv.jsonOutput {
		return inv.fail(err)
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	prefix := "tt: " + inv.verb + ": "
	if writeAuthorizationAdvice(inv.stderr, prefix, err) {
		return exitError
	}
	fmt.Fprintln(inv.stderr, prefix+cli.Foreign(err.Error(), cli.Width-cli.DisplayWidth(prefix)))
	return exitError
}

func timerField(inv *invocation, prefix, value string) {
	fmt.Fprintln(inv.stdout, prefix+cli.Foreign(value, cli.Width-cli.DisplayWidth(prefix)))
}

func timerMode(focusType int) string {
	switch focusType {
	case 0:
		return "Pomodoro"
	case 1:
		return "Timer"
	default:
		return "Focus"
	}
}

func timerDuration(duration time.Duration) string {
	return model.Duration(duration.Truncate(time.Second)).String()
}

func printTimerState(inv *invocation, st *store.Store, state store.TimerState) {
	status := "running"
	if state.PausedAt != nil {
		status = "paused"
	}
	line := timerMode(state.FocusType) + " " + status + ": " + timerDuration(state.ActiveDuration) + " active"
	if state.FocusType == 0 {
		line += ", " + timerDuration(max(time.Duration(0), state.PlannedDuration-state.ActiveDuration)) + " remaining"
	}
	fmt.Fprintln(inv.stdout, line)
	timerField(inv, "Session: ", state.SessionID)
	printTimerTask(inv, st, state.TaskID)
	printTimerTopic(inv, state.TopicID, state.TopicName)
	if state.Note != "" {
		timerField(inv, "Note: ", state.Note)
	}
}

func printTimerSession(inv *invocation, st *store.Store, session store.TimerSession) {
	status := "saved locally"
	if session.SyncedAt != nil {
		status = "uploaded"
	}
	line := timerMode(session.FocusType) + ": " + timerDuration(session.ActiveDuration) + " active, " +
		timerDuration(session.PauseDuration) + " paused, " + status
	fmt.Fprintln(inv.stdout, line)
	timerField(inv, "Outcome: ", session.Outcome)
	timerField(inv, "Session: ", session.ID)
	if session.EndedAt.IsZero() {
		fmt.Fprintln(inv.stdout, "Ended: unavailable")
	} else {
		fmt.Fprintln(inv.stdout, "Ended: "+session.EndedAt.Format(time.RFC3339))
	}
	printTimerTask(inv, st, session.TaskID)
	printTimerTopic(inv, session.TopicID, session.TopicName)
	if session.Note != "" {
		timerField(inv, "Note: ", session.Note)
	}
	if session.NoteReviewPending {
		fmt.Fprintln(inv.stdout, "Completion note pending; this session's upload waits for note or skip.")
	}
}

func printTimerTopic(inv *invocation, id, name string) {
	if id == "" {
		return
	}
	timerField(inv, "Timer topic: ", name)
	timerField(inv, "Topic ID: ", id)
}

func printTimerTask(inv *invocation, st *store.Store, id string) {
	if id == "" {
		return
	}
	task, err := st.Task(inv.ctx, id)
	if err != nil {
		timerField(inv, "Task ID: ", id)
		return
	}
	timerField(inv, "Task: ", task.Title)
}

func notifyTimerSession(ctx context.Context, st *store.Store, session store.TimerSession, command string, notify func(context.Context, string, NotifyVars) (NotifyResult, error)) error {
	vars := NotifyVars{Kind: "focus", Note: session.Note, Duration: timerDuration(session.ActiveDuration), Cycle: "1"}
	if session.FocusType == 1 {
		vars.Kind = "timing"
	}
	if session.TaskID != "" {
		if task, err := st.Task(ctx, session.TaskID); err == nil {
			vars.Task = task.Title
			if projects, err := cli.ProjectNames(ctx, st); err == nil {
				vars.Project = projects[task.ProjectId]
			}
		}
	}
	result, err := notify(ctx, command, vars)
	switch {
	case err != nil:
		return err
	case result.TimedOut:
		return errors.New("notification timed out; the session remains saved")
	case result.Killed != "":
		return errors.New("notification was killed; the session remains saved")
	case result.ExitCode != 0:
		return fmt.Errorf("notification exited with status %d; the session remains saved", result.ExitCode)
	}
	return nil
}

func timerAfterCompletion(inv *invocation, st *store.Store, session store.TimerSession, runtime timerRuntime) int {
	if session.Outcome != "done" {
		return exitOK
	}
	cfg, err := inv.config()
	if err != nil {
		return timerFailure(inv, err)
	}
	var notify, upload func() error
	if cfg.Timer.OnEnd != "" {
		notify = func() error { return notifyTimerSession(inv.ctx, st, session, cfg.Timer.OnEnd, runtime.notify) }
	}
	code := exitOK
	if cfg.FocusUpload.Enabled {
		if runtime.wake != nil {
			upload = runtime.wake
		} else if runtime.upload != nil {
			upload = func() error { code = runtime.upload(inv, st); return nil }
		}
	}
	for _, err := range app.CompleteTimer(inv.ctx, st, session, notify, upload) {
		timerFailure(inv, err)
	}
	return code
}

func timerWatch(inv *invocation, st *store.Store, sessionID string, runtime timerRuntime) int {
	digest := sha256.Sum256([]byte(sessionID))
	unlock, acquired, err := lockTimerWatcher(fmt.Sprintf("%s.timer-watch-%x", st.Path(), digest))
	if err != nil {
		return timerFailure(inv, err)
	}
	if !acquired {
		return exitOK
	}
	defer unlock()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := st.TimerStatus(inv.ctx, runtime.now())
		if err != nil {
			return timerFailure(inv, err)
		}
		completedCode := exitOK
		if result.Completed != nil {
			completedCode = timerAfterCompletion(inv, st, *result.Completed, runtime)
		}
		if result.State == nil || result.State.SessionID != sessionID {
			if result.Completed != nil && result.Completed.ID == sessionID {
				return completedCode
			}

			if session, err := st.TimerSessionByID(inv.ctx, sessionID); err == nil {
				return timerAfterCompletion(inv, st, session, runtime)
			}
			return exitOK
		}
		select {
		case <-inv.ctx.Done():
			return exitInterrupted
		case <-ticker.C:
		}
	}
}

func startTimerWatcher(sessionID string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate countdown watcher executable: %w", err)
	}
	return startTimerWatchProcess(executable, []string{"timer", "_watch", sessionID})
}

func startTimerWatchProcess(executable string, args []string) error {
	streams, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open countdown watcher streams: %w", err)
	}
	defer streams.Close()
	cmd := exec.Command(executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = streams, streams, streams
	if err := detachTimerWatcher(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start countdown watcher: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release countdown watcher: %w", err)
	}
	return nil
}
