package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func hasTaskFieldOptions(inv *invocation) bool {
	for _, arg := range inv.refinements {
		switch arg {
		case "--parent", "--estimated-duration", "--estimated-pomo", "--sort-order", "--column", "--preview", "--accept":
			return true
		}
	}
	return false
}

func cmdTaskFields(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("--raw is read-only")
	}
	edit := model.TaskEdit{}
	column := ""
	accept := ""
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		flag := inv.refinements[i]
		if seen[flag] {
			return inv.misuse("repeated task field option")
		}
		seen[flag] = true
		if flag == "--preview" {
			continue
		}
		i++
		if i >= len(inv.refinements) {
			return inv.misuse("missing task field value")
		}
		value := inv.refinements[i]
		switch flag {
		case "--accept":
			accept = value
		case "--column":
			column = value
			if strings.TrimSpace(column) == "" {
				return inv.misuse("column needs an exact column ID")
			}
		case "--parent":
			if value == "none" {
				value = ""
			}
			edit.ParentId = model.Ptr(value)
		case "--estimated-duration":
			duration, err := time.ParseDuration(value)
			if err != nil || duration < 0 || duration%time.Second != 0 {
				return inv.misuse("estimated duration needs nonnegative whole seconds, e.g. 30m or 0s")
			}
			edit.EstimatedDuration = model.Ptr(int64(duration / time.Second))
		case "--estimated-pomo":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 || n > 60 {
				return inv.misuse("estimated Pomodoros must be 0..60")
			}
			edit.EstimatedPomo = &n
		case "--sort-order":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return inv.misuse("sort order must be an int64")
			}
			edit.SortOrder = &n
		default:
			return inv.misuse("unknown task field option")
		}
	}
	if seen["--column"] && !edit.IsEmpty() {
		return inv.misuse("column assignment uses a separate preview from other task fields")
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	if accept != "" {
		if len(seen) != 1 || len(inv.data) != 0 {
			return inv.misuse("edit --accept applies the saved preview without new fields or references")
		}
		if len(accept) != 64 {
			return inv.misuse("invalid preview ID")
		}
		if code, handled := inv.acceptTaskColumn(st, accept); handled {
			return code
		}
		text, exists, err := st.Meta(inv.ctx, "task_fields_preview_"+accept)
		if err != nil {
			return inv.fail(err)
		}
		if !exists {
			return inv.misuse("preview is unavailable")
		}
		sum := sha256.Sum256([]byte(text))
		if hex.EncodeToString(sum[:]) != accept {
			return inv.misuse("preview failed validation")
		}
		var preview store.TaskExtensionsPreview
		if err := json.Unmarshal([]byte(text), &preview); err != nil {
			return inv.fail(err)
		}
		var outcome store.TaskMutationOutcome
		if preview.Edit.SortOrder != nil {
			outcome, err = st.UpdateTaskIfUnchanged(inv.ctx, preview.Task, preview.Edit)
		} else {
			outcome, err = st.ApplyTaskExtensions(inv.ctx, preview)
		}
		if err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			result := app.Result(inv.verb, outcome, app.ResultMeta{Source: "local"})
			if outcome.Changed {
				result.Status = "queued"
				result.Meta.Pending = 1
			}
			return inv.writeResult(result)
		}
		if !outcome.Changed {
			fmt.Fprintln(inv.stdout, "No task changes.")
			return exitOK
		}
		return printMutationTask(inv, outcome.Task)
	}
	if len(inv.data) == 0 || edit.IsEmpty() && !seen["--column"] {
		return inv.misuse("edit needs one task and at least one field")
	}
	id, code, ok := inv.oneFeatureTask(st, strings.Join(inv.data, " "))
	if !ok {
		return code
	}
	original, err := st.Task(inv.ctx, id)
	if err != nil {
		return inv.fail(err)
	}
	if seen["--column"] {
		return inv.previewTaskColumn(st, original, column)
	}
	if edit.ParentId != nil && *edit.ParentId != "" {
		parent, code, ok := inv.oneFeatureTask(st, *edit.ParentId)
		if !ok {
			return code
		}
		edit.ParentId = &parent
	}
	preview := store.TaskExtensionsPreview{Task: original, Edit: edit}
	if edit.SortOrder != nil {
		if edit.ParentId != nil || edit.EstimatedDuration != nil || edit.EstimatedPomo != nil {
			return inv.misuse("sort order uses a separate preview from task extensions")
		}
	} else {
		preview, err = st.PreviewTaskExtensions(inv.ctx, original, edit)
		if err != nil {
			return inv.fail(err)
		}
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		return inv.fail(err)
	}
	sum := sha256.Sum256(raw)
	previewID := hex.EncodeToString(sum[:])
	if err := st.SetMeta(inv.ctx, "task_fields_preview_"+previewID, string(raw)); err != nil {
		return inv.fail(err)
	}
	return printFocusPreview(inv, previewID, map[string]any{"id": previewID, "task": original, "edit": edit, "undo": "existing task undo; preview is checked again at acceptance"})
}

func printMutationTask(inv *invocation, task model.Task) int {
	fmt.Fprintln(inv.stdout, cli.ReportLine("queued change: ", task.Title, "; run tt sync", nil))
	return exitOK
}
