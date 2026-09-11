package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
)

const (
	backgroundMinInterval  = time.Minute
	backgroundPullInterval = 5 * time.Minute
	backgroundCatchUp      = 15 * time.Minute
	backgroundRetention    = 45 * 24 * time.Hour
	backgroundUnsupported  = "background_unsupported_reminders"
)

type backgroundRuntime struct {
	now    func() time.Time
	notify func(context.Context, string, NotifyVars) (NotifyResult, error)
	sync   func(context.Context, *store.Store, config.Config) error
}

func defaultBackgroundRuntime() backgroundRuntime {
	return backgroundRuntime{
		now:    time.Now,
		notify: RunNotify,
		sync: func(ctx context.Context, st *store.Store, cfg config.Config) error {
			out := app.RunSync(ctx, st, cfg, func() (*api.Client, error) {
				token, err := syncToken()
				if err != nil {
					return nil, err
				}
				return managedAPIClient(token), nil
			}, nil, func() (focus.TopicClient, error) {
				client, _, err := webClient(ctx)
				return client, err
			})
			problems := append([]error(nil), out.Tasks.Errors...)
			for _, message := range out.Resources.Errors {
				problems = append(problems, errors.New(message))
			}
			problems = append(problems, out.Focus.Errors...)
			for _, refresh := range out.Refreshes {
				if refresh.State == "failed" {
					problems = append(problems, errors.New(refresh.Summary()))
				}
			}
			problems = append(problems, out.Err)
			return errors.Join(problems...)
		},
	}
}

func startBackgroundWorker() error {
	executable, err := os.Executable()
	if err != nil {
		return errors.New("cannot locate background worker executable")
	}
	streams, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return errors.New("cannot open background worker streams")
	}
	defer streams.Close()
	cmd := exec.Command(executable, "auto", "run", "--now", "--quiet")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = streams, streams, streams
	if err := detachTimerWatcher(cmd); err != nil {
		return errors.New("cannot detach background worker")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("cannot start background worker")
	}
	if err := cmd.Process.Release(); err != nil {
		return errors.New("cannot release background worker")
	}
	return nil
}

func cmdBackground(inv *invocation) int {
	return cmdBackgroundWithRuntime(inv, defaultBackgroundRuntime())
}

func cmdBackgroundWithRuntime(inv *invocation, runtime backgroundRuntime) int {
	action := "status"
	if len(inv.data) > 0 {
		action = inv.data[0]
	}
	if len(inv.data) > 1 {
		return inv.misuseWord("unexpected argument ", inv.data[1])
	}
	force, quiet := false, false
	for _, flag := range inv.refinements {
		switch flag {
		case "--now":
			force = true
		case "--quiet":
			quiet = true
		default:
			return inv.misuseWord("unknown option ", flag)
		}
	}
	if action != "run" && (force || quiet) {
		return inv.misuse("--now and --quiet are only valid with run")
	}
	if action != "run" && action != "status" {
		return inv.misuseWord("unknown operation ", action)
	}
	cfg, err := inv.config()
	if err != nil {
		return inv.fail(err)
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	if action == "status" {
		return backgroundStatus(inv, st, cfg, runtime.now())
	}
	unlock, err := acquireSyncLock(st)
	if errors.Is(err, store.ErrLocked) {
		inv.resultData = map[string]any{"state": "already_running", "synced": false}
		return exitOK
	}
	if err != nil {
		return inv.fail(err)
	}
	defer unlock()
	result := runBackgroundPass(inv.ctx, st, cfg, runtime, force)
	inv.resultData = map[string]any{"local_reminders": result.local, "provider_reminders": result.provider, "synced": result.synced}
	if result.err != nil {
		if !quiet {
			return inv.fail(result.err)
		}
		return exitError
	}
	if !quiet {
		fmt.Fprintf(inv.stdout, "background: reminders local %d, provider %d; sync %s\n",
			result.local, result.provider, map[bool]string{true: "ran", false: "not needed"}[result.synced])
	}
	return exitOK
}

type backgroundResult struct {
	local, provider int
	synced          bool
	err             error
}

func runBackgroundPass(ctx context.Context, st *store.Store, cfg config.Config, runtime backgroundRuntime, force bool) backgroundResult {
	now := runtime.now()
	result := backgroundResult{}
	occurrences, unsupported, err := st.DueReminderOccurrences(ctx, now.Add(-backgroundCatchUp), now)
	if err != nil {
		result.err = err
	} else {
		_ = st.SetMeta(ctx, backgroundUnsupported, fmt.Sprintf("%d", unsupported))
		for _, occurrence := range occurrences {
			decision := "provider"
			if !occurrence.ServerConfirmed {
				if cfg.Timer.OnEnd == "" {
					continue
				}
				decision = "local"
			}
			claimed, claimErr := st.ClaimReminderOccurrence(ctx, occurrence, decision, now)
			if claimErr != nil {
				result.err = errors.Join(result.err, claimErr)
				continue
			}
			if !claimed {
				continue
			}
			if decision == "provider" {
				result.provider++
				continue
			}
			vars := NotifyVars{Kind: "task", Note: occurrence.Project, Project: occurrence.Project, Task: occurrence.Title}
			notification, notifyErr := runtime.notify(ctx, cfg.Timer.OnEnd, vars)
			if notifyErr != nil || notification.TimedOut || notification.Killed != "" || notification.ExitCode != 0 {
				result.err = errors.Join(result.err, errors.New("task reminder notification failed"))
				continue
			}
			result.local++
		}
	}
	if err := st.SetBackgroundTime(ctx, store.BackgroundLastTickKey, now); err != nil {
		result.err = errors.Join(result.err, err)
	}
	if _, err := st.PruneReminderDeliveries(ctx, now.Add(-backgroundRetention)); err != nil {
		result.err = errors.Join(result.err, err)
	}

	taskWork, _, workErr := st.AutomaticTaskWork(ctx)
	if workErr != nil {
		result.err = errors.Join(result.err, workErr)
		return finishBackgroundPass(ctx, st, now, result)
	}
	focusWork := false
	if cfg.FocusUpload.Enabled {
		ids, focusErr := st.PendingFocusSessionIDs(ctx, cfg.Timer.UploadAborted)
		if focusErr != nil {
			result.err = errors.Join(result.err, focusErr)
			return finishBackgroundPass(ctx, st, now, result)
		}
		focusWork = len(ids) > 0
	}
	lastAttempt, attempted, attemptErr := st.BackgroundTime(ctx, store.BackgroundLastAttemptKey)
	if attemptErr != nil {
		result.err = errors.Join(result.err, attemptErr)
		return finishBackgroundPass(ctx, st, now, result)
	}
	retryDue := !attempted || now.Sub(lastAttempt) >= backgroundSyncInterval(cfg)
	pullDue := !attempted || now.Sub(lastAttempt) >= backgroundPullInterval
	if !force && !((taskWork || focusWork) && retryDue) && !pullDue {
		return finishBackgroundPass(ctx, st, now, result)
	}
	result.synced = true
	if err := st.SetBackgroundTime(ctx, store.BackgroundLastAttemptKey, now); err != nil {
		result.err = errors.Join(result.err, err)
		return finishBackgroundPass(ctx, st, now, result)
	}
	if err := runtime.sync(ctx, st, cfg); err != nil {
		result.err = errors.Join(result.err, err)
		return finishBackgroundPass(ctx, st, now, result)
	}
	if err := st.SetBackgroundTime(ctx, store.BackgroundLastSuccessKey, now); err != nil {
		result.err = errors.Join(result.err, err)
	}
	return finishBackgroundPass(ctx, st, now, result)
}

func finishBackgroundPass(ctx context.Context, st *store.Store, now time.Time, result backgroundResult) backgroundResult {
	if !result.synced && result.err == nil {
		return result
	}
	message := ""
	if result.err != nil {
		message = result.err.Error()
	}
	if err := st.SetMeta(ctx, store.BackgroundLastErrorKey, message); err != nil {
		result.err = errors.Join(result.err, err)
	}
	return result
}

func backgroundStatus(inv *invocation, st *store.Store, cfg config.Config, now time.Time) int {
	_, counts, err := st.AutomaticTaskWork(inv.ctx)
	if err != nil {
		return inv.fail(err)
	}
	focus := 0
	if cfg.FocusUpload.Enabled {
		ids, err := st.PendingFocusSessionIDs(inv.ctx, cfg.Timer.UploadAborted)
		if err != nil {
			return inv.fail(err)
		}
		focus = len(ids)
	}
	lastError, _, err := st.Meta(inv.ctx, store.BackgroundLastErrorKey)
	if err != nil {
		return inv.fail(err)
	}
	unsupportedText, _, err := st.Meta(inv.ctx, backgroundUnsupported)
	if err != nil {
		return inv.fail(err)
	}
	unsupported := 0
	if unsupportedText != "" {
		unsupported, err = strconv.Atoi(unsupportedText)
		if err != nil || unsupported < 0 {
			return inv.fail(errors.New("invalid provider-only reminder count"))
		}
	}
	if inv.jsonOutput {
		return backgroundJSONStatus(inv, st, cfg, counts, focus, unsupported, lastError)
	}
	fmt.Fprintf(inv.stdout, "Task queue: %d pending, %d in flight, %d held\n", counts.Pending, counts.Inflight, counts.Failed)
	fmt.Fprintf(inv.stdout, "Focus queue: %d waiting\n", focus)
	fmt.Fprintf(inv.stdout, "Provider-only reminders: %d\n", unsupported)
	for _, item := range []struct {
		label, key string
	}{{"Last check", store.BackgroundLastTickKey}, {"Last sync attempt", store.BackgroundLastAttemptKey}, {"Last successful sync", store.BackgroundLastSuccessKey}} {
		stamp, ok, err := st.BackgroundTime(inv.ctx, item.key)
		if err != nil {
			return inv.fail(err)
		}
		value := "never"
		if ok {
			value = stamp.Local().Format(time.RFC3339) + " (" + elapsedLabel(now.Sub(stamp)) + ")"
		}
		fmt.Fprintf(inv.stdout, "%s: %s\n", item.label, value)
	}
	if lastError == "" {
		lastError = "none"
	}
	fmt.Fprintln(inv.stdout, "Last error: "+cli.Foreign(lastError, cli.Width-len("Last error: ")))
	fmt.Fprintf(inv.stdout, "Policy: retry local work every %s; pull every %s; reminder catch-up %s\n",
		timerDuration(backgroundSyncInterval(cfg)), timerDuration(backgroundPullInterval), timerDuration(backgroundCatchUp))
	return exitOK
}

func backgroundSyncInterval(cfg config.Config) time.Duration {
	interval := cfg.Sync.Interval.Duration()
	if interval < backgroundMinInterval {
		return backgroundMinInterval
	}
	return interval
}

func elapsedLabel(elapsed time.Duration) string {
	if elapsed < 0 {
		return "clock is earlier"
	}
	return timerDuration(elapsed) + " ago"
}
