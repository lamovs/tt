package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func cmdRemoteFocus(inv *invocation) int {
	if len(inv.data) < 2 {
		return inv.misuse("timer focus needs ls, show, add, or rm")
	}
	action := inv.data[1]
	if action != "ls" && action != "show" && action != "add" && action != "rm" {
		return inv.misuse("unknown remote focus action")
	}
	values := map[string]string{}
	for i := 0; i < len(inv.refinements); i++ {
		flag := inv.refinements[i]
		if _, ok := values[flag]; ok {
			return inv.misuse("repeated focus option")
		}
		switch flag {
		case "--remote", "--preview":
			values[flag] = "true"
			continue
		case "--type", "--from", "--to", "--pause", "--note", "--task", "--accept":
		default:
			return inv.misuse("unknown focus option %s", flag)
		}
		i++
		if i >= len(inv.refinements) {
			return inv.misuse("missing focus option value")
		}
		values[flag] = inv.refinements[i]
	}
	if values["--preview"] != "" && (action == "ls" || action == "show") {
		return inv.misuse("--preview requires focus add or rm")
	}
	if action == "show" || action == "rm" {
		for _, flag := range []string{"--from", "--to"} {
			if _, exists := values[flag]; exists {
				return inv.misuse("exact focus show/rm does not accept date range options")
			}
		}
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	service := inv.resourceService(st)
	if id := values["--accept"]; id != "" {
		if len(values) != 1 || len(inv.data) != 2 || inv.rawOutput {
			return inv.misuse("--accept applies the exact preview without new fields")
		}
		var outcome any
		if action == "add" {
			outcome, err = service.ApplyFocusRecord(inv.ctx, id)
		} else if action == "rm" {
			outcome, err = service.Apply(inv.ctx, id, "focus", "delete")
		} else {
			return inv.misuse("accept needs add or rm")
		}
		if err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			result := app.Result(inv.verb, outcome, app.ResultMeta{Source: "local"})
			if action == "add" {
				result.Status = "recorded"
				result.Data = timerSessionData(outcome.(store.TimerSession))
			} else {
				result.Status = "queued"
				result.Meta.Pending = 1
			}
			return inv.writeResult(result)
		}
		fmt.Fprintln(inv.stdout, "Saved locally. Use tt timer sync for additions or tt sync for deletions.")
		return exitOK
	}
	kind, err := strconv.Atoi(values["--type"])
	if err != nil || (kind != 0 && kind != 1) {
		return inv.misuse("focus requires --type 0 (Pomodoro) or 1 (Timing)")
	}
	query := app.ResourceQuery{Kind: "focus", FocusType: api.FocusType(kind), From: values["--from"], To: values["--to"]}
	if action == "show" || action == "rm" {
		if len(inv.data) != 3 {
			return inv.misuse("focus show/rm needs an exact server ID")
		}
		query.ID = inv.data[2]
	} else if len(inv.data) != 2 {
		return inv.misuse("unexpected focus argument")
	}
	if action == "add" {
		if inv.rawOutput || values["--remote"] != "" {
			return inv.misuse("focus add is a local preview")
		}
		start, e1 := time.Parse(time.RFC3339, query.From)
		end, e2 := time.Parse(time.RFC3339, query.To)
		if e1 != nil || e2 != nil {
			return inv.misuse("focus add requires RFC3339 --from and --to")
		}
		pause := int64(0)
		if text := values["--pause"]; text != "" {
			pause, err = strconv.ParseInt(text, 10, 64)
			if err != nil {
				return inv.misuse("pause is whole seconds")
			}
		}
		record := store.FocusRecord{FocusType: kind, Start: start, End: end, PauseSeconds: pause, Note: values["--note"]}
		if reference := values["--task"]; reference != "" {
			id, code, ok := inv.oneFeatureTask(st, reference)
			if !ok {
				return code
			}
			record.TaskID = id
		}
		preview, err := service.PrepareFocusRecord(inv.ctx, record)
		if err != nil {
			return inv.fail(err)
		}
		return printFocusPreview(inv, preview.ID, preview)
	}
	for _, flag := range []string{"--task", "--note", "--pause"} {
		if values[flag] != "" {
			return inv.misuse("%s requires focus add", flag)
		}
	}
	if action == "rm" && (inv.rawOutput || values["--remote"] != "") {
		return inv.misuse("refresh focus show --remote separately before deletion preview")
	}
	listing, err := service.List(inv.ctx, query, values["--remote"] != "")
	if err != nil {
		return inv.fail(err)
	}
	if action != "rm" {
		return printResourceEntities(inv, listing.Entities, listing.Meta)
	}
	if len(listing.Entities) != 1 {
		return inv.misuse("refresh this exact focus record before deleting it")
	}
	entity := listing.Entities[0]
	preview, err := service.Prepare(inv.ctx, store.EntityMutation{Ref: entity.Ref, ProjectKey: entity.ProjectKey, Action: "delete", Patch: json.RawMessage(`{}`)})
	if err != nil {
		return inv.fail(err)
	}
	return printFocusPreview(inv, preview.ID, preview)
}

func printFocusPreview(inv *invocation, id string, data any) int {
	if inv.jsonOutput {
		result := app.Result(inv.verb, data, app.ResultMeta{Source: "local"})
		result.Status = "preview"
		return inv.writeResult(result)
	}
	cli.WriteLines(inv.stdout, cli.Wrap("Preview "+fullReportAtom(id)+". Accept with the same action and --accept "+fullReportAtom(id), cli.Width))
	return exitOK
}
