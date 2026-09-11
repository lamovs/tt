package main

import (
	"fmt"
	"slices"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
)

func timerUpload(inv *invocation, st *store.Store) int {
	cfg, err := inv.config()
	if err != nil {
		return timerFailure(inv, err)
	}
	result, pending, err := app.UploadPendingFocus(inv.ctx, st, func() (*api.Client, error) {
		token, err := syncToken()
		if err != nil {
			return nil, err
		}
		return managedAPIClient(token), nil
	}, cfg.Timer.UploadAborted, slices.Contains(inv.refinements, "--retry-failed"), func() (focus.TopicClient, error) {
		client, _, err := webClient(inv.ctx)
		return client, err
	})
	if err != nil {
		return timerFailure(inv, err)
	}
	if inv.jsonOutput {
		inv.resultSource = "server"
		inv.setResultPart("focus_upload", map[string]any{"pending": pending, "uploaded": result.Uploaded, "held": result.Held, "errors": jsonErrorMessages(result.Errors)})
		inv.resultChanged = inv.resultChanged || result.Uploaded != 0
	}
	if !pending {
		fmt.Fprintln(inv.stdout, "no focus sessions waiting for upload")
		return exitOK
	}
	fmt.Fprintf(inv.stdout, "focus: uploaded %d, held %d\n", result.Uploaded, result.Held)
	for _, err := range result.Errors {
		timerFailure(inv, err)
	}
	if inv.ctx.Err() != nil {
		return exitInterrupted
	}
	if len(result.Errors) != 0 {
		return exitError
	}
	return exitOK
}

func runSyncWithFocus(inv *invocation) int {
	if isResourceRecovery(inv) {
		return cmdResourceRecovery(inv)
	}
	return cmdSync(inv.ctx, inv.stdout, inv.stderr, inv.args, inv)
}
