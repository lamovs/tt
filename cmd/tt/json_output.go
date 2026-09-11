package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	clean, machine, raw, err := outputArguments(args)
	if !machine {
		if err != nil {
			fmt.Fprintln(stderr, "tt: "+err.Error())
			return exitUsage
		}
		return runText(ctx, clean, stdin, stdout, stderr)
	}
	inv := &invocation{ctx: ctx, stdin: stdin, stdout: io.Discard, stderr: stderr, resultWriter: stdout, jsonOutput: true, rawOutput: raw, color: config.ColorNever, colorKnown: true}
	if len(clean) > 0 {
		inv.verb, inv.args = clean[0], clean[1:]
	}
	if err != nil {
		return inv.jsonFailure("usage", err.Error(), exitUsage)
	}
	if inv.verb == "" || inv.verb == "--help" || inv.verb == "-h" {
		inv.verb = "help"
	}
	if inv.verb == "--version" {
		inv.verb = "version"
	}
	cleanArgs, _, _, _, colorErr := takeColor(inv.args)
	if colorErr != nil {
		return inv.misuse("invalid --color option")
	}
	inv.args = cleanArgs
	inv.data, inv.refinements = splitArgs(inv.args)
	selected, exists := commands[inv.verb]
	if !exists {
		return inv.jsonFailure("usage", "unknown command", exitUsage)
	}
	if wantsHelp(inv.refinements) {
		return inv.output(selected.help, app.ResultMeta{Source: "local"})
	}
	switch inv.verb {
	case "help":
		if len(inv.refinements) != 0 || len(inv.data) > 1 {
			return inv.misuse("help takes at most one command")
		}
		if len(inv.data) == 1 {
			item, ok := commands[inv.data[0]]
			if !ok {
				return inv.misuse("unknown help command")
			}
			return inv.output(item.help, app.ResultMeta{Source: "local"})
		}
		return inv.output(indexGroups(), app.ResultMeta{Source: "local"})
	case "version":
		if len(inv.args) != 0 {
			return inv.misuse("version takes no arguments")
		}
		return inv.output(map[string]string{"version": version}, app.ResultMeta{Source: "local"})
	case "ui":
		return inv.jsonFailure("unsupported_mode", "ui requires an interactive terminal; use CLI queries with --json", exitUsage)
	case "show":
		return inv.runStructured(command{run: func(current *invocation) int { return current.jsonShow() }})
	case "ls", "today", "s":
		if hasServerTaskQuery(inv) {
			return inv.serverTaskQuery()
		}
		if raw {
			return inv.misuse("--raw is available for show and resource reads")
		}
		return inv.runStructured(selected)
	case "project", "folder", "tag", "habit", "comment", "countdown", "auth":
		return inv.runStructured(selected)
	case "config", "doctor", "auto":
		return inv.jsonMaintenance()
	case "notify":
		return inv.jsonNotify()
	case "setup":
		return inv.jsonFailure("unsupported_mode", "setup requires text output; use tt help setup --json for its options", exitUsage)
	case "login":
		return inv.jsonFailure("unsupported_mode", "login is interactive; inspect authorization with tt auth status --json", exitUsage)
	case "add":
		for _, arg := range inv.refinements {
			if arg == "-e" || arg == "--editor" {
				return inv.jsonFailure("unsupported_mode", "an external editor requires interactive output", exitUsage)
			}
		}
	case "edit":
		if !hasTaskFieldOptions(inv) {
			return inv.jsonFailure("unsupported_mode", "an external editor requires interactive output", exitUsage)
		}
	case "timer":
		if len(inv.data) > 0 && inv.data[0] == "focus" {
			return inv.runStructured(selected)
		}
	}
	if raw {
		return inv.misuse("--raw is only available for reads that expose retained server data")
	}
	return inv.runStructured(selected)
}

type taskCommandOutcome struct {
	Task    model.Task `json:"task"`
	Changed bool       `json:"changed"`
	Deleted bool       `json:"deleted"`
}

func (inv *invocation) recordTask(task model.Task, changed, deleted bool) {
	if inv.jsonOutput {
		inv.mutationResults = append(inv.mutationResults, taskCommandOutcome{Task: task, Changed: changed, Deleted: deleted})
	}
}

func (inv *invocation) structuredData() any {
	if len(inv.mutationResults) != 0 {
		return map[string]any{"tasks": inv.mutationResults}
	}
	return inv.resultData
}

func (inv *invocation) runStructured(command command) int {
	code := command.run(inv)
	if inv.resultWritten {
		if inv.resultExit != exitOK {
			return inv.resultExit
		}
		return code
	}
	if code != exitOK {
		if code == exitInterrupted {
			return inv.jsonFailure("cancelled", "command was interrupted", code)
		}
		kind, message := "operation_failed", "command did not complete"
		if code == exitUsage {
			kind = "usage"
		}
		if inv.resultError != nil {
			message = inv.resultError.Error()
		}
		return inv.jsonFailure(kind, message, code)
	}
	data := inv.structuredData()
	if data == nil {
		return inv.jsonFailure("unsupported_mode", "structured result is unavailable for this operation", exitUsage)
	}
	return inv.output(data, inv.localResultMeta())
}

func (inv *invocation) localResultMeta() app.ResultMeta {
	meta := app.ResultMeta{Source: "local", Completeness: "complete"}
	if inv.resultSource != "" {
		meta.Source = inv.resultSource
	}
	inspection, err := store.Inspect(inv.ctx, "")
	if err == nil {
		defer inspection.Close()
		counts, err := inspection.Store.OutboxCounts(inv.ctx)
		if err == nil {
			meta.Pending = counts.Pending
		} else {
			meta.Completeness = "unknown"
		}
	} else if !errors.Is(err, store.ErrNoCache) {
		meta.Completeness = "unknown"
	}
	return meta
}

func outputArguments(args []string) ([]string, bool, bool, error) {
	out := make([]string, 0, len(args))
	machine, raw, literal := false, false, false
	var err error
	for _, arg := range args {
		if literal {
			out = append(out, arg)
			continue
		}
		if arg == "--" {
			literal = true
			out = append(out, arg)
			continue
		}
		switch arg {
		case "--json":
			if machine {
				err = errors.New("--json may be specified only once")
			}
			machine = true
		case "--raw":
			if raw {
				err = errors.New("--raw may be specified only once")
			}
			raw = true
		default:
			out = append(out, arg)
		}
	}
	if raw && !machine {
		err = errors.New("--raw requires --json")
	}
	return out, machine, raw, err
}

func (inv *invocation) writeResult(result app.CommandResult) int {
	result.Warnings = append(result.Warnings, inv.resultWarnings...)
	writer := inv.stdout
	if inv.resultWriter != nil {
		writer = inv.resultWriter
	}
	if inv.resultWritten {
		return exitError
	}
	inv.resultWritten = true
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		inv.resultExit = exitError
		return exitError
	}
	if result.Error != nil {
		inv.resultExit = exitError
		return exitError
	}
	return exitOK
}

func (inv *invocation) output(data any, meta app.ResultMeta) int {
	return inv.writeResult(app.Result(inv.verb, data, meta))
}

func (inv *invocation) jsonFailure(code, message string, exitCode int) int {
	result := app.Result(inv.verb, inv.structuredData(), inv.localResultMeta())
	result.Status = "error"
	if inv.interrupted() {
		result.Status, code, exitCode = "cancelled", "cancelled", exitInterrupted
	}
	if code == "cancelled" {
		result.Status = "cancelled"
	}
	changed := inv.resultChanged
	for _, item := range inv.mutationResults {
		changed = changed || item.Changed
	}
	if changed && result.Status != "cancelled" {
		result.Status = "partial"
	}
	result.Error = &app.ResultError{Code: code, Message: uiPrivateText(message)}
	if inv.writeResult(result) == exitError && result.Error == nil {
		return exitError
	}
	inv.resultExit = exitCode
	return exitCode
}

func (inv *invocation) jsonShow() int {
	if len(inv.refinements) != 0 {
		return inv.misuse("unknown browse option")
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	if len(inv.data) == 0 {
		return inv.misuse("show needs a task reference")
	}
	id, code, ok := inv.oneFeatureTask(st, strings.Join(inv.data, " "))
	if !ok {
		return code
	}
	task, err := st.Task(inv.ctx, id)
	if err != nil {
		return inv.fail(err)
	}
	result := app.Result(inv.verb, task, inv.localResultMeta())
	if inv.rawOutput {
		var raw string
		if err := st.DB().QueryRowContext(inv.ctx, `SELECT raw FROM tasks WHERE id=?`, id).Scan(&raw); err != nil {
			return inv.fail(err)
		}
		if !json.Valid([]byte(raw)) {
			return inv.fail(errors.New("retained task data is not valid JSON"))
		}
		result.Raw = json.RawMessage(raw)
	}
	return inv.writeResult(result)
}
