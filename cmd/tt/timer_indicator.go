package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

type indicatorOutput struct {
	Version     int    `json:"version"`
	Visible     bool   `json:"visible"`
	State       string `json:"state"`
	SessionID   string `json:"session_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	TaskLabel   string `json:"task_label,omitempty"`
	Kind        string `json:"kind,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	RemainingMS *int64 `json:"remaining_ms,omitempty"`
	ObservedMS  int64  `json:"observed_ms"`
	Text        string `json:"text"`
}

func cmdTimerIndicator(inv *invocation) int {
	if len(inv.data) != 1 {
		return inv.misuse("indicator takes no positional arguments")
	}
	format, width := "plain", 80
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		flag := inv.refinements[i]
		if flag != "--format" && flag != "--width" {
			return inv.misuseWord("unknown indicator option ", flag)
		}
		if seen[flag] {
			return inv.misuseWord("repeated option ", flag)
		}
		seen[flag] = true
		if i+1 == len(inv.refinements) {
			return inv.misuseWord("missing value for ", flag)
		}
		i++
		value := inv.refinements[i]
		if flag == "--format" {
			if value != "plain" && value != "json" && value != "tmux" {
				return inv.misuseWord("expected plain, json or tmux, got ", value)
			}
			format = value
		} else {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 32 || parsed > 512 {
				return inv.misuseWord("expected indicator width from 32 to 512, got ", value)
			}
			width = parsed
		}
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	cfg, err := inv.config()
	if err != nil {
		return timerFailure(inv, err)
	}
	ctx, cancel := context.WithTimeout(inv.ctx, 250*time.Millisecond)
	defer cancel()
	snapshot, err := app.ReadIndicator(ctx, "", time.Now())
	if err != nil {
		return timerFailure(inv, err)
	}
	out := renderIndicator(snapshot, cfg.Timer.Indicator, format, width)
	if inv.jsonOutput {
		inv.resultData = out
		return exitOK
	}
	if format == "json" {
		err = json.NewEncoder(inv.stdout).Encode(out)
	} else if out.Visible {
		_, err = fmt.Fprintln(inv.stdout, out.Text)
	}
	if err != nil {
		return timerFailure(inv, err)
	}
	return exitOK
}

func renderIndicator(snapshot store.TimerSnapshot, enabled bool, format string, width int) indicatorOutput {
	out := indicatorOutput{Version: 1, State: "idle", ObservedMS: snapshot.ObservedAt.UnixMilli()}
	state := snapshot.State
	if state == nil {
		return out
	}
	if !state.Indicator.Visible(enabled) {
		out.State = "hidden"
		return out
	}
	out.Visible, out.State, out.Kind = true, "running", "timer"
	out.SessionID, out.TaskID = state.SessionID, state.TaskID
	out.ElapsedMS = max(0, state.ActiveDuration.Milliseconds())
	if state.PausedAt != nil {
		out.State = "paused"
	}
	clock := out.ElapsedMS
	if state.FocusType == 0 {
		out.Kind = "pomodoro"
		remaining := max(0, state.PlannedDuration.Milliseconds()-out.ElapsedMS)
		out.RemainingMS, clock = &remaining, remaining
		if state.PausedAt == nil && remaining == 0 {
			out.State = "expired"
		}
	}
	label := snapshot.TaskTitle
	if state.TaskID == "" {
		label = "Unassigned"
	} else if !snapshot.TaskFound {
		label = "Task unavailable"
	} else if label == "" {
		label = "Untitled task"
	}

	if len(label) > 2048 {
		label = label[:2048]
	}
	if format == "tmux" {

		label = strings.ReplaceAll(label, "#", "\\x23")
	}
	out.TaskLabel = cli.ReportTitle(label, min(80, width))
	seconds := clock / 1000
	prefix := fmt.Sprintf("%s %02d:%02d:%02d | %s | ", out.Kind, seconds/3600, seconds/60%60, seconds%60, out.State)
	out.Text = prefix + cli.ReportTitle(label, max(1, width-len(prefix)))
	return out
}
