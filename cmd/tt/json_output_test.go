package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type machineResult struct {
	SchemaVersion int              `json:"schema_version"`
	Command       string           `json:"command"`
	Status        string           `json:"status"`
	Data          json.RawMessage  `json:"data"`
	Meta          app.ResultMeta   `json:"meta"`
	Warnings      []string         `json:"warnings"`
	Error         *app.ResultError `json:"error"`
	Raw           json.RawMessage  `json:"raw"`
}

func runMachine(t *testing.T, ctx context.Context, args ...string) (machineResult, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append([]string{"--json"}, args...)
	// The output flag is global and may precede the command.
	code := run(ctx, args, strings.NewReader(""), &stdout, &stderr)
	decoder := json.NewDecoder(&stdout)
	var result machineResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("%v: not JSON: %v; stdout=%s stderr=%s", args, err, stdout.String(), stderr.String())
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("%v: extra stdout after envelope: %v", args, err)
	}
	if result.SchemaVersion != 1 || result.Command == "" || result.Warnings == nil {
		t.Fatalf("bad envelope: %+v", result)
	}
	if code == exitOK && result.Error != nil {
		t.Fatalf("successful code with error: %+v", result)
	}
	return result, code
}

func machineTasks(t *testing.T, result machineResult) []taskCommandOutcome {
	t.Helper()
	var data struct {
		Tasks []taskCommandOutcome `json:"tasks"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Tasks
}

func TestJSONTaskMutationsAndLocalQueries(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})
	ctx := context.Background()
	created, code := runMachine(t, ctx, "add", "machine task")
	if code != exitOK {
		t.Fatalf("add: %+v", created)
	}
	tasks := machineTasks(t, created)
	if len(tasks) != 1 || !tasks[0].Changed || tasks[0].Task.Id == "" || created.Meta.Pending != 1 {
		t.Fatalf("create: %+v %s", created, string(created.Data))
	}
	id := tasks[0].Task.Id
	for _, args := range [][]string{
		{"pri", id, "high"}, {"pri", id, "high"}, {"due", id, "tmr"},
		{"item", id, "add", "step"}, {"item", id, "done", "1"},
		{"repeat", id, "daily"}, {"remind", id, "at"}, {"done", id}, {"done", id},
	} {
		result, code := runMachine(t, ctx, args...)
		if code != exitOK || len(machineTasks(t, result)) != 1 {
			t.Fatalf("%v: %d %s", args, code, result.Data)
		}
	}
	result, code := runMachine(t, ctx, "s", "machine", "-a")
	var listed []model.Task
	if json.Unmarshal(result.Data, &listed) != nil || code != exitOK || len(listed) != 1 || !listed[0].Status.Done() {
		t.Fatalf("search all: %d %s", code, result.Data)
	}
	result, code = runMachine(t, ctx, "undo")
	if code != exitOK || !bytes.Contains(result.Data, []byte(`"changed":true`)) {
		t.Fatalf("undo: %d %s", code, result.Data)
	}
	result, code = runMachine(t, ctx, "rm", id)
	if code != exitOK || !bytes.Contains(result.Data, []byte(`"preview":true`)) {
		t.Fatalf("preview: %d %s", code, result.Data)
	}
	result, code = runMachine(t, ctx, "rm", id, "-y")
	if code != exitOK || !machineTasks(t, result)[0].Deleted {
		t.Fatalf("delete: %d %s", code, result.Data)
	}
}

func TestJSONPreviewAndInteractiveRefusalHaveNoMutation(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})
	ctx := context.Background()
	result, code := runMachine(t, ctx, "add", "meeting", "--schedule", "2026-09-12 14:00 + 1h", "--zone", "UTC", "--preview")
	if code != exitOK || !bytes.Contains(result.Data, []byte(`"preview":true`)) {
		t.Fatalf("interval preview: %d %s", code, result.Data)
	}
	for _, args := range [][]string{{"login"}, {"ui"}, {"add", "draft", "-e", "markdown"}, {"edit", "something"}, {"timer", "_watch", "sid"}, {"add", "raw", "--raw"}} {
		result, code = runMachine(t, ctx, args...)
		if code != exitUsage || result.Error == nil {
			t.Fatalf("%v did not refuse: %d %+v", args, code, result)
		}
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counts, err := st.OutboxCounts(ctx)
	if err != nil || counts.Pending != 0 {
		t.Fatalf("refusal/preview queued work: %+v %v", counts, err)
	}
}

func TestJSONLocalTimerAndIndicator(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	for _, args := range [][]string{{"timer", "status"}, {"timer", "start", "--none"}, {"timer", "pause"}, {"timer", "resume"}, {"timer", "indicator"}, {"timer", "cancel"}, {"timer", "history"}, {"timer", "sync"}} {
		result, code := runMachine(t, ctx, args...)
		if code != exitOK || len(result.Data) == 0 || bytes.Equal(result.Data, []byte("null")) {
			t.Fatalf("%v: %d %+v", args, code, result)
		}
	}
}

func TestJSONErrorsCancellationAndLiteralFlags(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, code := runMachine(t, ctx, "add", "cancelled")
	if code != exitInterrupted || result.Status != "cancelled" {
		t.Fatalf("cancelled: %d %+v", code, result)
	}
	result, code = runMachine(t, context.Background(), "add", "--", "--json")
	if code != exitOK || machineTasks(t, result)[0].Task.Title != "--json" {
		t.Fatalf("literal: %d %+v", code, result)
	}
	result, code = runMachine(t, context.Background(), "s", "--json", "--color=never")
	if code != exitUsage || result.Error == nil {
		t.Fatalf("duplicate: %d %+v", code, result)
	}
	result, code = runMachine(t, context.Background(), "sync")
	if code != exitError || result.Error == nil {
		t.Fatalf("missing-token sync: %d %+v", code, result)
	}
}

func TestJSONPartialMutationRetainsTypedOutcome(t *testing.T) {
	var output bytes.Buffer
	inv := &invocation{ctx: context.Background(), verb: "done", jsonOutput: true, resultWriter: &output, stdout: io.Discard, stderr: io.Discard}
	inv.recordTask(model.Task{Id: "t1", Title: "done"}, true, false)
	if inv.fail(errors.New("another task changed")) != exitError {
		t.Fatal("missing error")
	}
	var result machineResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Status != "partial" || len(machineTasks(t, result)) != 1 {
		t.Fatalf("partial: %s", output.String())
	}
}

func TestJSONReadsKeepNumberedListingAndRawFields(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	seedProjects(t, model.Project{Id: "p1", Name: config.Default().DefaultProject, Kind: "TASK"})
	created, code := runMachine(t, ctx, "add", "readable")
	if code != exitOK {
		t.Fatal(created.Error)
	}
	id := machineTasks(t, created)[0].Task.Id
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetListing(ctx, []string{"preserved-listing-id"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE tasks SET raw=? WHERE id=?`, `{"unmodeled":{"number":9007199254740993}}`, id); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"ls"}, {"s", "readable"}, {"today"}, {"show", id, "--raw"}} {
		result, code := runMachine(t, ctx, args...)
		if code != exitOK {
			t.Fatalf("%v: %+v", args, result.Error)
		}
		if args[0] == "show" && !bytes.Contains(result.Raw, []byte("9007199254740993")) {
			t.Fatalf("raw loss: %s", result.Raw)
		}
	}
	listing, err := st.Listing(ctx)
	if err != nil || len(listing) != 1 || listing[0] != "preserved-listing-id" {
		t.Fatalf("JSON changed numeric refs: %v %v", listing, err)
	}
}

func TestJSONNotifyUsesExecutionData(t *testing.T) {
	isolate(t)
	writeOnEndConfig(t, `printf notice; printf detail >&2`)
	result, code := runMachine(t, context.Background(), "notify", "test")
	if code != exitOK {
		t.Fatal(result.Error)
	}
	var data struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if json.Unmarshal(result.Data, &data) != nil || data.Stdout != "notice" || data.Stderr != "detail" || data.ExitCode != 0 {
		t.Fatalf("notify result: %s", result.Data)
	}
}

func TestJSONResourceUndoUsesTypedIdentity(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	preview, err := st.PreviewEntityMutation(ctx, store.EntityMutation{Ref: store.EntityRef{Kind: "project"}, Action: "create", Patch: json.RawMessage(`{"name":"local project"}`)})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := st.ApplyEntityMutation(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	result, code := runMachine(t, ctx, "undo")
	if code != exitOK || !bytes.Contains(result.Data, []byte(`"cancelled_unsent":true`)) || bytes.Contains(result.Data, []byte(`"task":`)) {
		t.Fatalf("resource undo: %d %s", code, result.Data)
	}
	if _, err := st.Entity(ctx, outcome.Entity.Ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resource remains: %v", err)
	}
}

func TestJSONWrittenErrorCannotReturnSuccess(t *testing.T) {
	var output bytes.Buffer
	inv := &invocation{ctx: context.Background(), verb: "timer", jsonOutput: true, resultWriter: &output, stdout: io.Discard, stderr: io.Discard}
	code := inv.runStructured(command{run: func(current *invocation) int { current.fail(errors.New("notification failed")); return exitOK }})
	if code != exitError {
		t.Fatal("structured error returned success")
	}
	var result machineResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Error == nil {
		t.Fatalf("invalid error output: %s", output.String())
	}
}
