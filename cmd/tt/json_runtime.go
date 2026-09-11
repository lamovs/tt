package main

import (
	"errors"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func (inv *invocation) setResultPart(key string, value any) {
	if !inv.jsonOutput {
		return
	}
	data, ok := inv.resultData.(map[string]any)
	if !ok {
		data = make(map[string]any)
		inv.resultData = data
	}
	data[key] = value
}

func jsonErrorMessages(errs []error) []string {
	values := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			values = append(values, uiPrivateText(err.Error()))
		}
	}
	return values
}

func privateMessages(values []string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = uiPrivateText(value)
	}
	return result
}

func (inv *invocation) jsonNotify() int {
	if inv.rawOutput {
		return inv.misuse("notify does not expose retained server data")
	}
	if len(inv.refinements) != 0 || len(inv.data) != 1 || inv.data[0] != "test" {
		return inv.misuse("expected notify test")
	}
	cfg, err := config.Load()
	if err != nil {
		return inv.fail(err)
	}
	if cfg.Timer.OnEnd == "" {
		return inv.fail(errors.New("timer.on_end is not configured"))
	}
	res, err := RunNotify(inv.ctx, cfg.Timer.OnEnd, NotifyVars{Kind: "focus", Note: "Deep work", Task: "Test task", Project: "Inbox", Duration: "25m", Cycle: "1"})
	inv.resultData = map[string]any{"exit_code": res.ExitCode, "timed_out": res.TimedOut, "killed": res.Killed, "stdout": uiPrivateText(res.Stdout), "stderr": uiPrivateText(res.Stderr), "visual_receipt_verified": false}
	if err != nil {
		return inv.fail(err)
	}
	if inv.interrupted() {
		return inv.jsonFailure("cancelled", "notification test was interrupted", exitInterrupted)
	}
	if res.TimedOut || res.Killed != "" || res.ExitCode != 0 {
		return inv.jsonFailure("notification_failed", "notification command did not exit successfully", exitError)
	}
	return inv.output(inv.resultData, app.ResultMeta{Source: "local"})
}

func timerStateData(state store.TimerState) map[string]any {
	return map[string]any{
		"session_id": state.SessionID, "task_id": state.TaskID, "topic_id": state.TopicID, "topic_name": state.TopicName, "kind": state.Kind, "note": state.Note,
		"focus_type": state.FocusType, "started_at": state.StartedAt, "paused_at": state.PausedAt,
		"deadline": state.Deadline, "last_event_at": state.LastEventAt,
		"planned_ms": state.PlannedDuration.Milliseconds(), "paused_ms": state.PauseDuration.Milliseconds(),
		"elapsed_ms": state.ActiveDuration.Milliseconds(), "indicator": state.Indicator,
	}
}

func timerSessionData(session store.TimerSession) map[string]any {
	return map[string]any{
		"session_id": session.ID, "task_id": session.TaskID, "topic_id": session.TopicID, "topic_name": session.TopicName, "kind": session.Kind, "note": session.Note,
		"focus_type": session.FocusType, "outcome": session.Outcome, "started_at": session.StartedAt,
		"ended_at": session.EndedAt, "synced_at": session.SyncedAt,
		"planned_ms": session.PlannedDuration.Milliseconds(), "paused_ms": session.PauseDuration.Milliseconds(),
		"elapsed_ms": session.ActiveDuration.Milliseconds(), "notification_claimed": session.NotificationClaimed,
		"note_review_pending": session.NoteReviewPending,
	}
}
