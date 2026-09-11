package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/tui"
)

func inspectFocusCache(ctx context.Context) (*store.Inspection, error) {
	inspection, err := store.Inspect(ctx, "")
	if err != nil {
		return nil, err
	}
	version, meta, row, err := inspection.Version(ctx)
	if err == nil && (!meta || !row || version != inspection.SchemaLatest) {
		err = errors.New("cache schema is incompatible; use a compatible tt and sync before choosing a focus task")
	}
	if err != nil {
		inspection.Close()
		return nil, err
	}
	return inspection, nil
}

func cmdDefaultFocus(inv *invocation) int {
	args := inv.args[1:]
	if len(args) > 1 {
		return inv.misuse("usage: tt config default-focus [task:ID|none]")
	}
	var ref config.FocusReference
	var err error
	if len(args) == 1 {
		ref, err = config.ParseFocusReference(args[0])
		if err != nil {
			return inv.misuse("expected task:ID or none; omit it for the cached picker")
		}
	} else if !canAsk(inv) {
		return inv.misuse("the focus picker needs terminal stdin and stderr; use tt config default-focus task:ID or none")
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	prepared, err := readDefaultProjectConfig()
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	var st *store.Store
	if len(args) == 0 || ref.TaskID != "" {
		inspection, err := inspectFocusCache(inv.ctx)
		if err != nil {
			return defaultProjectFailure(inv, err)
		}
		defer inspection.Close()
		st = inspection.Store
	}
	if len(args) == 0 {
		choices, err := app.DefaultFocusChoices(inv.ctx, st, prepared.config.DefaultProject)
		if err != nil {
			return defaultProjectFailure(inv, err)
		}
		var selected bool
		ref, selected, err = pickDefaultFocus(inv, choices)
		if inv.interrupted() {
			return exitInterrupted
		}
		if err != nil {
			return defaultProjectFailure(inv, err)
		}
		if !selected {
			fmt.Fprintln(inv.stdout, "Canceled; config unchanged.")
			return exitOK
		}
	}
	preview, err := prepareDefaultFocus(inv.ctx, prepared, st, ref)
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	result, err := preview.Apply(inv.ctx)
	if inv.interrupted() {
		return exitInterrupted
	}
	if err != nil {
		return defaultProjectFailure(inv, err)
	}
	fmt.Fprintln(inv.stdout, result)
	return exitOK
}

func pickDefaultFocus(inv *invocation, choices app.FocusChoices) (config.FocusReference, bool, error) {
	reader := bufio.NewReader(inv.stdin)
	var outputErr error
	print := func(format string, args ...any) {
		if outputErr == nil {
			_, outputErr = fmt.Fprintf(inv.stderr, format, args...)
		}
	}
	read := func() (string, error) {
		if outputErr != nil {
			return "", outputErr
		}
		line, err := readDefaultProjectAnswer(inv.ctx, reader)
		if errors.Is(err, io.EOF) {
			return "q", nil
		}
		return strings.TrimSpace(line), err
	}
	if choices.Notice != "" {
		print("%s\n", choices.Notice)
	}
	print("Cached focus lists (0: no destination; q: cancel):\n")
	for i, p := range choices.Projects {
		mark := ""
		if p.Id == choices.InitialProjectID {
			mark = " (default)"
		}
		print("%d. %s [id:%s]%s\n", i+1, cli.Foreign(p.Name, max(4, cli.Width-len(strconv.Itoa(i+1))-len(p.Id)-len(mark)-8)), p.Id, mark)
	}
	print("List number (Enter uses default_project): ")
	answer, err := read()
	print("\n")
	if err != nil || answer == "q" {
		return config.FocusReference{}, false, err
	}
	if answer == "0" {
		return config.FocusReference{}, true, nil
	}
	id := choices.InitialProjectID
	if answer != "" {
		n, err := strconv.Atoi(answer)
		if err != nil || n < 1 || n > len(choices.Projects) {
			return config.FocusReference{}, false, errors.New("choose a displayed list number")
		}
		id = choices.Projects[n-1].Id
	}
	if id == "" {
		return config.FocusReference{}, false, errors.New("no usable default_project; choose a list explicitly")
	}
	var tasks []model.Task
	for _, task := range choices.Tasks {
		if task.ProjectId == id {
			tasks = append(tasks, task)
		}
	}
	print("Cached open tasks (0: no destination; empty/q: cancel):\n")
	for i, task := range tasks {
		print("%d. %s [task:%s]\n", i+1, cli.Foreign(task.Title, max(4, cli.Width-len(strconv.Itoa(i+1))-len(task.Id)-10)), task.Id)
	}
	print("Task number: ")
	answer, err = read()
	if err != nil || answer == "" || answer == "q" {
		return config.FocusReference{}, false, err
	}
	if answer == "0" {
		return config.FocusReference{}, true, nil
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(tasks) {
		return config.FocusReference{}, false, errors.New("choose a displayed task number")
	}
	return config.FocusReference{TaskID: tasks[n-1].Id}, true, nil
}

func prepareDefaultFocus(ctx context.Context, prepared defaultProjectConfig, st *store.Store, ref config.FocusReference) (tui.SystemPreview, error) {
	p := tui.SystemPreview{Action: "default focus"}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	var task model.Task
	var err error
	if ref.TaskID != "" {
		task, err = st.DefaultFocusTask(ctx, ref.TaskID)
		if err != nil {
			return p, err
		}
	}
	candidate, err := config.SetDefaultFocus(prepared.source, ref)
	if err != nil {
		return p, err
	}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := prepared.verify(); err != nil {
			return err
		}
		if ref.TaskID != "" {
			current, err := st.DefaultFocusTask(ctx, ref.TaskID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, task) {
				return errors.New("focus task changed; choose it again")
			}
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return p, err
	}
	p.Lines = []string{"Set default_focus: " + ref.String(), "Task: " + task.Title, "Config: " + prepared.target,
		"Applies to future starts using the default. Active sessions, history and queued uploads keep their destinations.",
		"The cache is not a server availability check. Existing config is backed up before a change."}
	p.Apply = func(ctx context.Context) (string, error) {
		var err error
		if err := check(ctx); err != nil {
			return "", err
		}
		if prepared.config.DefaultFocus == ref {
			return "Default focus unchanged.", nil
		}
		if err := os.MkdirAll(filepath.Dir(prepared.target), 0o700); err != nil {
			return "", err
		}
		backup := ""
		if prepared.exists {
			backup, err = nextBackupPath(prepared.target)
			if err == nil {
				err = writeBackupFile(backup, prepared.source, prepared.mode)
			}
			if err != nil {
				return "", err
			}
		}
		verify := func() error { return check(ctx) }
		if prepared.exists {
			err = writeConfigFileChecked(prepared.target, candidate, prepared.mode, verify)
		} else {
			err = writeNewDefaultConfig(prepared.target, candidate, verify)
		}
		if err != nil {
			return "", fmt.Errorf("%w%s", err, backupNote(backup))
		}
		result := "Default focus: " + ref.String()
		if backup != "" {
			result += "\nBackup: " + fullReportAtom(backup)
		}
		return result, nil
	}
	return p, nil
}
