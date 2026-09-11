package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func cmdNativeMv(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("--raw is read-only")
	}
	accept := ""
	if len(inv.refinements) > 0 {
		if len(inv.refinements) == 1 && inv.refinements[0] == "--preview" {
		} else if len(inv.refinements) == 2 && inv.refinements[0] == "--accept" {
			accept = inv.refinements[1]
		} else {
			return inv.misuse("use --preview or --accept ID")
		}
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	if accept != "" {
		if len(inv.data) != 0 || len(accept) != 64 {
			return inv.misuse("mv --accept needs only the saved preview ID")
		}
		text, exists, err := st.Meta(inv.ctx, "native_move_preview_"+accept)
		if err != nil {
			return inv.fail(err)
		}
		if !exists {
			return inv.misuse("move preview is unavailable")
		}
		sum := sha256.Sum256([]byte(text))
		if hex.EncodeToString(sum[:]) != accept {
			return inv.misuse("move preview failed validation")
		}
		var preview store.NativeMovePreview
		if err := json.Unmarshal([]byte(text), &preview); err != nil {
			return inv.fail(err)
		}
		outcome, err := st.ApplyNativeMove(inv.ctx, preview)
		if err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			result := app.Result(inv.verb, outcome, app.ResultMeta{Source: "local", Pending: 1})
			result.Status = "queued"
			return inv.writeResult(result)
		}
		return printMutationTask(inv, outcome.Task)
	}
	if len(inv.data) < 2 {
		return inv.misuse("name a task and the list to move it to")
	}
	resolver := inv.resolver(st)
	var project model.Project
	var projectErr error
	namesList := func(query string) error {
		if strings.HasPrefix(query, "id:") {
			project, projectErr = st.Project(inv.ctx, strings.TrimPrefix(query, "id:"))
			return nil
		}
		p, err := resolver.Project(inv.ctx, query)
		var unknown *cli.NoMatchError
		if errors.As(err, &unknown) {
			return err
		}
		project, projectErr = p, err
		return nil
	}
	ids, _, code, ok := inv.resolveTasksAndValue(st, inv.data, store.StatusOpen, "list", namesList)
	if !ok {
		return code
	}
	if projectErr != nil {
		return inv.fail(projectErr)
	}
	if len(ids) != 1 {
		return inv.misuse("native move previews one exact task at a time")
	}
	task, err := st.Task(inv.ctx, ids[0])
	if err != nil {
		return inv.fail(err)
	}
	preview, err := st.PreviewNativeMove(inv.ctx, task, project.Id)
	if err != nil {
		return inv.fail(err)
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		return inv.fail(err)
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if err := st.SetMeta(inv.ctx, "native_move_preview_"+id, string(raw)); err != nil {
		return inv.fail(err)
	}
	return printFocusPreview(inv, id, map[string]any{"id": id, "preview": preview, "identity": "unchanged task ID", "undo": "cancel before send or reverse after confirmation"})
}

func legacyMoveRequested(inv *invocation) bool {
	for _, flag := range inv.refinements {
		if strings.EqualFold(flag, "--recreate") {
			return true
		}
	}
	return false
}
