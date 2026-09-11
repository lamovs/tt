package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/store"
)

func (inv *invocation) jsonMaintenance() int {
	if inv.rawOutput {
		return inv.misuse("maintenance commands do not expose provider raw data")
	}
	switch inv.verb {
	case "doctor":
		if len(inv.args) != 0 {
			if len(inv.args) == 1 && inv.args[0] == "--fix" {
				return inv.jsonFailure("unsupported_mode", "doctor --fix requires an interactive confirmation; use the TUI settings preview", exitUsage)
			}
			return inv.misuse("doctor takes no arguments in JSON mode")
		}
		type check struct {
			Status  string   `json:"status"`
			Summary string   `json:"summary"`
			Notes   []string `json:"notes"`
		}
		rows := []check{}
		failed := false
		for _, result := range collectDoctorChecks(inv.ctx, observeConfigWrite) {
			notes := []string{}
			for _, note := range result.notes {
				notes = append(notes, strings.TrimSpace(uiPrivateText(note)))
			}
			rows = append(rows, check{result.status.label(), uiPrivateText(result.summary), notes})
			failed = failed || result.status == statusFail
		}
		inv.resultData = map[string]any{"checks": rows}
		if inv.interrupted() {
			return inv.jsonFailure("cancelled", "doctor was interrupted", exitInterrupted)
		}
		if failed {
			return inv.jsonFailure("checks_failed", "one or more diagnostic checks failed", exitError)
		}
		return inv.output(inv.resultData, app.ResultMeta{Source: "local"})
	case "config":
		if len(inv.data) == 1 && (inv.data[0] == "default-project" || inv.data[0] == "default-focus") {
			return inv.jsonFailure("unsupported_mode", "the picker requires a terminal; supply an exact configured reference", exitUsage)
		}
		if len(inv.args) != 0 {
			code := commands["config"].run(inv)
			if inv.resultWritten {
				return code
			}
			if code != exitOK {
				return inv.jsonFailure("operation_failed", "configuration command did not complete", code)
			}
		}
		cfg, err := config.Load()
		if err != nil {
			return inv.fail(err)
		}
		path, err := config.Path()
		if err != nil {
			return inv.fail(err)
		}
		_, err = os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return inv.fail(err)
		}
		return inv.output(map[string]any{"path": path, "exists": err == nil, "effective": cfg}, app.ResultMeta{Source: "local"})
	case "auto":
		return inv.runStructured(commands["auto"])
	}
	return inv.misuse("unknown maintenance command")
}

func backgroundJSONStatus(inv *invocation, st *store.Store, cfg config.Config, counts store.OutboxCounts, focus, unsupported int, lastError string) int {
	timestamps := map[string]*time.Time{}
	for name, key := range map[string]string{"last_check": store.BackgroundLastTickKey, "last_attempt": store.BackgroundLastAttemptKey, "last_success": store.BackgroundLastSuccessKey} {
		stamp, exists, err := st.BackgroundTime(inv.ctx, key)
		if err != nil {
			return inv.fail(err)
		}
		if exists {
			timestamps[name] = &stamp
		} else {
			timestamps[name] = nil
		}
	}
	return inv.output(map[string]any{"queue": counts, "focus_pending": focus, "provider_only_reminders": unsupported, "last_error": uiPrivateText(lastError), "timestamps": timestamps,
		"policy": map[string]int64{"retry_seconds": int64(backgroundSyncInterval(cfg) / time.Second), "pull_seconds": int64(backgroundPullInterval / time.Second), "catch_up_seconds": int64(backgroundCatchUp / time.Second)}}, app.ResultMeta{Source: "local"})
}
