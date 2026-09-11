package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func resourceJSONRun(t *testing.T, args ...string) (map[string]json.RawMessage, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append(args, "--json"), strings.NewReader(""), &stdout, &stderr)
	var result map[string]json.RawMessage
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("JSON %v: %v; stderr=%s", args, err, stderr.String())
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("multiple output documents: %v", err)
	}
	return result, code
}

func previewIdentity(t *testing.T, result map[string]json.RawMessage) string {
	t.Helper()
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result["data"], &value); err != nil || len(value.ID) != 64 {
		t.Fatalf("preview: %s %v", result["data"], err)
	}
	return value.ID
}

func TestResourceCommandPreviewAcceptanceAndCapabilities(t *testing.T) {
	isolate(t)
	result, code := resourceJSONRun(t, "folder", "add", "Work", "--preview")
	if code != exitOK || string(result["status"]) != `"preview"` {
		t.Fatalf("%d %s", code, result)
	}
	id := previewIdentity(t, result)
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counts, _ := st.OutboxCounts(context.Background())
	if counts.Pending != 0 {
		t.Fatal("preview queued mutation")
	}
	result, code = resourceJSONRun(t, "folder", "add", "--accept", id)
	if code != exitOK || string(result["status"]) != `"queued"` {
		t.Fatalf("%d %s", code, result)
	}
	if _, code = resourceJSONRun(t, "folder", "add", "--accept", id); code == exitOK {
		t.Fatal("preview replay duplicated creation")
	}
	if _, code = resourceJSONRun(t, "folder", "rm", "Work", "--cascade", "--preview"); code == exitOK {
		t.Fatal("unverified cascade accepted")
	}
}

func TestNativeMoveCLIUsesExactPreviewAndPreservesID(t *testing.T) {
	isolate(t)
	mvSeed(t, mvOpen(mvParcel, "p1", "Move me"))
	result, code := resourceJSONRun(t, "mv", mvParcel, "id:p2", "--preview")
	if code != exitOK {
		t.Fatalf("%d %s", code, result)
	}
	id := previewIdentity(t, result)
	if tasks := mvTasksIn(t, "p1"); len(tasks) != 1 {
		t.Fatal("preview moved task")
	}
	result, code = resourceJSONRun(t, "mv", "--accept", id)
	if code != exitOK {
		t.Fatalf("%d %s", code, result)
	}
	tasks := mvTasksIn(t, "p2")
	if len(tasks) != 1 || tasks[0].Id != mvParcel {
		t.Fatal("move recreated ID")
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var op string
	if err := st.DB().QueryRow(`SELECT op FROM outbox WHERE task_id=?`, mvParcel).Scan(&op); err != nil || op != store.OpTaskMoveNative {
		t.Fatalf("%s %v", op, err)
	}
}

func TestResourceColorDoesNotConflictWithTerminalColor(t *testing.T) {
	isolate(t)
	result, code := resourceJSONRun(t, "project", "add", "Work", "--resource-color", "#123456", "--color", "never", "--preview")
	if code != exitOK {
		t.Fatalf("%d %s", code, result)
	}
	var preview app.ResourcePreview
	if err := json.Unmarshal(result["data"], &preview); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(preview.Preview.Mutation.Patch, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["color"] != "#123456" {
		t.Fatalf("resource color lost: %s", preview.Preview.Mutation.Patch)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"project", "add", "Work", "--resource-color", "#abcdef", "--color", "never", "--preview"}, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("text color: %d %s", code, stderr.String())
	}
}

func TestResourceUnsupportedReadOptionsRefuseBeforeFetch(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"project", "show", "id:p1", "--tasks", "--remote", "--raw"},
		{"timer", "focus", "ls", "--type", "1", "--remote", "--preview"},
		{"habit", "history", "habit1", "--date", "2026-09-11", "--remote"},
		{"habit", "checkin", "habit1", "--from", "2026-09-11", "--date", "2026-09-11", "--value", "1", "--preview"},
	} {
		if result, code := resourceJSONRun(t, args...); code != exitUsage {
			t.Fatalf("%v: %d %s", args, code, result)
		}
	}
}

func TestResourceHumanOutputEscapesAndBoundsEveryField(t *testing.T) {
	var stdout bytes.Buffer
	inv := &invocation{stdout: &stdout}
	entity := store.ResourceEntity{Ref: store.EntityRef{Kind: "folder", Key: strings.Repeat("x", 150) + "\x1b[31m"}, Data: json.RawMessage(`{"name":"hello\u001b[31m\nworld"}`), Dirty: true}
	printResourceEntities(inv, []store.ResourceEntity{entity}, app.ResultMeta{Source: "local", Completeness: "unknown"})
	if strings.Contains(stdout.String(), "\x1b") {
		t.Fatal("raw terminal escape")
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if cli.DisplayWidth(line) > cli.Width {
			t.Fatalf("line too wide: %s", line)
		}
	}
}

func TestResourceCLIRecoveryIsExplicitAndRevisionBound(t *testing.T) {
	isolate(t)
	result, code := resourceJSONRun(t, "folder", "add", "Test", "--preview")
	if code != exitOK {
		t.Fatal(result)
	}
	id := previewIdentity(t, result)
	if result, code = resourceJSONRun(t, "folder", "add", "--accept", id); code != exitOK {
		t.Fatal(result)
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ops, err := st.EntityOperationSummaries(context.Background())
	if err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	seq := strconv.FormatInt(ops[0].Item.Seq, 10)
	result, code = resourceJSONRun(t, "sync", "cancel", seq, "--preview")
	if code != exitOK {
		t.Fatal(result)
	}
	id = previewIdentity(t, result)
	if result, code = resourceJSONRun(t, "sync", "cancel", "--accept", id); code != exitOK {
		t.Fatal(result)
	}
	ops, err = st.EntityOperationSummaries(context.Background())
	if err != nil || len(ops) != 0 {
		t.Fatal(ops, err)
	}
	if _, code = resourceJSONRun(t, "sync", "cancel", "--accept", id); code == exitOK {
		t.Fatal("recovery preview replayed")
	}
}

func TestMaintenanceJSONReadsAndInteractiveRefusal(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"config"}, {"auto", "status"}} {
		result, code := resourceJSONRun(t, args...)
		if code != exitOK || string(result["data"]) == "null" {
			t.Fatalf("%v: %d %s", args, code, result)
		}
	}
	if _, code := resourceJSONRun(t, "doctor", "--fix"); code != exitUsage {
		t.Fatal("interactive doctor fix allowed")
	}
}
