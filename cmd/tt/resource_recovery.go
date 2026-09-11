package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

type resourceRecoveryPreview struct {
	Action    string                `json:"action"`
	Operation store.EntityOperation `json:"operation"`
}

func isResourceRecovery(inv *invocation) bool {
	if len(inv.data) == 0 {
		return false
	}
	switch inv.data[0] {
	case "queue", "recover", "cancel":
		return true
	}
	return false
}

func cmdResourceRecovery(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("queue recovery has no provider raw output")
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	service := inv.resourceService(st)
	action := inv.data[0]
	operations, err := service.Queue(inv.ctx)
	if err != nil {
		return inv.fail(err)
	}
	if action == "queue" {
		if len(inv.data) != 1 || len(inv.refinements) != 0 {
			return inv.misuse("sync queue takes no additional arguments")
		}
		queue, err := st.RecoveryQueue(inv.ctx)
		if err != nil {
			return inv.fail(err)
		}
		tasks := []store.QueueEntry{}
		for _, entry := range queue.Tasks {
			if !strings.HasPrefix(entry.Item.Op, "entity.") {
				tasks = append(tasks, entry)
			}
		}
		if inv.jsonOutput {
			return inv.output(map[string]any{"tasks": tasks, "resources": operations, "focus": queue.Focus}, app.ResultMeta{Source: "local"})
		}
		for _, op := range operations {
			fmt.Fprintf(inv.stdout, "Resource operation %d; revision %d\n", op.Item.Seq, op.Revision)
			fmt.Fprintln(inv.stdout, cli.ReportLine("  ", op.Mutation.Ref.Kind+" "+op.Mutation.Ref.Key+" "+op.Phase, "", nil))
		}
		fmt.Fprintf(inv.stdout, "Task operations: %d; focus uploads: %d\n", len(tasks), len(queue.Focus))
		fmt.Fprintln(inv.stdout, "Use sync recover/cancel SEQ --preview for resources. TUI Q handles tasks.")
		return exitOK
	}
	if len(inv.refinements) == 2 && inv.refinements[0] == "--accept" {
		if len(inv.data) != 1 {
			return inv.misuse("accept uses only the saved recovery preview")
		}
		id := inv.refinements[1]
		if len(id) != 64 {
			return inv.misuse("invalid preview ID")
		}
		text, exists, err := st.Meta(inv.ctx, "resource_recovery_preview_"+id)
		if err != nil {
			return inv.fail(err)
		}
		if !exists {
			return inv.misuse("recovery preview is unavailable")
		}
		sum := sha256.Sum256([]byte(text))
		if hex.EncodeToString(sum[:]) != id {
			return inv.misuse("recovery preview failed validation")
		}
		var preview resourceRecoveryPreview
		if err := json.Unmarshal([]byte(text), &preview); err != nil {
			return inv.fail(err)
		}
		if preview.Action != action {
			return inv.misuse("preview belongs to a different recovery action")
		}
		matched := false
		for _, current := range operations {
			if current.Item.Seq != preview.Operation.Item.Seq {
				continue
			}
			raw, _ := json.Marshal(resourceRecoveryPreview{Action: action, Operation: current})
			matched = string(raw) == text
		}
		if !matched {
			return inv.fail(store.ErrEntityChanged)
		}
		op := preview.Operation
		if action == "cancel" {
			err = service.Cancel(inv.ctx, op.Item.Seq, op.Revision)
		} else {
			err = service.Recover(inv.ctx, op.Item.Seq, op.Revision)
		}
		if err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			return inv.output(map[string]any{"operation_seq": op.Item.Seq, "action": action, "network_sent": false}, app.ResultMeta{Source: "local"})
		}
		fmt.Fprintln(inv.stdout, "Recovery applied locally. Sync remains a separate action.")
		return exitOK
	}
	if len(inv.data) != 2 || len(inv.refinements) != 1 || inv.refinements[0] != "--preview" {
		return inv.misuse("use sync recover/cancel SEQ --preview, then the same action --accept ID")
	}
	seq, err := strconv.ParseInt(inv.data[1], 10, 64)
	if err != nil || seq <= 0 {
		return inv.misuse("operation sequence must be a positive integer")
	}
	for _, op := range operations {
		if op.Item.Seq != seq {
			continue
		}
		preview := resourceRecoveryPreview{Action: action, Operation: op}
		raw, err := json.Marshal(preview)
		if err != nil {
			return inv.fail(err)
		}
		sum := sha256.Sum256(raw)
		id := hex.EncodeToString(sum[:])
		if err := st.SetMeta(inv.ctx, "resource_recovery_preview_"+id, string(raw)); err != nil {
			return inv.fail(err)
		}
		return printFocusPreview(inv, id, map[string]any{"id": id, "preview": preview, "warning": "uncertain writes are read-back only; cancellation requires proven unsent state"})
	}
	return inv.misuse("resource operation not found; use sync queue")
}
