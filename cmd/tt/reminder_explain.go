package main

import (
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/schedule"
)

func cmdExplainReminders(inv *invocation) int {
	if len(inv.refinements) != 1 || len(inv.data) == 0 {
		return inv.misuse("remind --explain needs one task reference")
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	id, code, ok := inv.oneFeatureTask(st, strings.Join(inv.data, " "))
	if !ok {
		return code
	}
	task, err := st.Task(inv.ctx, id)
	if err != nil {
		return inv.fail(err)
	}
	rows := schedule.InterpretReminders(task)
	if inv.jsonOutput {
		return inv.output(map[string]any{"task_id": id, "reminders": rows, "legacy_fallback": "unchanged; plain triggers use the accepted due-date allowlist"}, app.ResultMeta{Source: "local"})
	}
	for _, row := range rows {
		fmt.Fprintln(inv.stdout, cli.ReportLine("Trigger: ", row.Raw, "", nil))
		if row.Computable {
			fmt.Fprintln(inv.stdout, "Interpreted time: "+row.At.String())
		}
		fmt.Fprintln(inv.stdout, cli.ReportLine("Delivery: ", row.Delivery, "", nil))
		fmt.Fprintln(inv.stdout, cli.ReportLine("Reason: ", row.Reason, "", nil))
	}
	cli.WriteLines(inv.stdout, cli.Wrap("Existing local fallback delivery is unchanged. Interpretation does not prove a device notification.", cli.Width))
	return exitOK
}
