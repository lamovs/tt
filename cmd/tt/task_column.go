package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const taskColumnOrigin = "Official Open API (column write live-verified, undocumented)"

type taskColumnEnvelope struct {
	Version int                      `json:"version"`
	Kind    string                   `json:"kind"`
	Preview *store.TaskColumnPreview `json:"preview"`
}

func (inv *invocation) previewTaskColumn(st *store.Store, original model.Task, destination string) int {
	actions := app.NewActions(st, config.Config{}, nil)
	preview, err := actions.PreviewTaskColumn(inv.ctx, original, destination)
	if err != nil {
		return inv.fail(err)
	}
	raw, err := json.Marshal(taskColumnEnvelope{Version: 1, Kind: "task_column", Preview: &preview})
	if err != nil {
		return inv.fail(err)
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if err := st.SetMeta(inv.ctx, "task_column_preview_"+id, string(raw)); err != nil {
		return inv.fail(err)
	}
	effect := "Acceptance queues only this column assignment. Task status and ordering stay unchanged. Sync is separate."
	if preview.Task.ColumnId == preview.DestinationID {
		effect = "The task is already in this column. Acceptance makes no task changes and queues no operation."
	}
	if inv.jsonOutput {
		return printFocusPreview(inv, id, map[string]any{"id": id, "preview": preview, "api_origin": taskColumnOrigin, "effect": effect})
	}
	for _, line := range []string{
		"Task: " + fullReportAtom(preview.Task.Title) + " [" + fullReportAtom(preview.Task.Id) + "]",
		"Project ID: " + fullReportAtom(preview.Task.ProjectId),
		"Source column: " + fullReportAtom(preview.SourceName) + " [" + fullReportAtom(preview.Task.ColumnId) + "]",
		"Destination column: " + fullReportAtom(preview.DestinationName) + " [" + fullReportAtom(preview.DestinationID) + "]",
		taskColumnOrigin,
		effect,
	} {
		cli.WriteLines(inv.stdout, strings.Split(ansi.Hardwrap(line, cli.Width, true), "\n"))
	}
	return printFocusPreview(inv, id, nil)
}

func (inv *invocation) acceptTaskColumn(st *store.Store, id string) (int, bool) {
	raw, exists, err := st.Meta(inv.ctx, "task_column_preview_"+id)
	if err != nil {
		return inv.fail(err), true
	}
	if !exists {
		return exitOK, false
	}
	sum := sha256.Sum256([]byte(raw))
	if hex.EncodeToString(sum[:]) != id {
		return inv.misuse("column preview failed validation"), true
	}
	var envelope taskColumnEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil || envelope.Version != 1 || envelope.Kind != "task_column" || envelope.Preview == nil {
		return inv.misuse("invalid column preview; prepare it again"), true
	}
	actions := app.NewActions(st, config.Config{}, nil)
	outcome, err := actions.ApplyTaskColumn(inv.ctx, *envelope.Preview)
	if err != nil {
		return inv.fail(err), true
	}
	if inv.jsonOutput {
		result := app.Result(inv.verb, outcome, app.ResultMeta{Source: "local"})
		if outcome.Changed {
			result.Status, result.Meta.Pending = "queued", 1
		}
		return inv.writeResult(result), true
	}
	if !outcome.Changed {
		fmt.Fprintln(inv.stdout, "No task changes.")
		return exitOK, true
	}
	return printMutationTask(inv, outcome.Task), true
}
