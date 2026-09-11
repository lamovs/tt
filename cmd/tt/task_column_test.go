package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedColumnCommand(t *testing.T) (*store.Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Project", Permission: "write"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeEntities(ctx, "column", "p1", []store.ResourceEntity{
		{ServerID: "todo", Data: json.RawMessage(`{"id":"todo","projectId":"p1","name":"Todo","sortOrder":0}`)},
		{ServerID: "done", Data: json.RawMessage(`{"id":"done","projectId":"p1","name":"Done","sortOrder":1}`)},
	}, false); err != nil {
		t.Fatal(err)
	}
	task := model.Task{Id: mvParcel, ProjectId: "p1", Title: "Move a card", ColumnId: "todo", ColumnName: "Todo", Kind: "TEXT", Status: model.TaskOpen, SortOrder: 4, Content: "Keep body", Items: []model.Item{{Id: "item", Title: "Still open"}}}
	raw := json.RawMessage(`{"id":"` + mvParcel + `","projectId":"p1","title":"Move a card","columnId":"todo","columnName":"Todo","kind":"TEXT","status":0,"sortOrder":4,"content":"Keep body","items":[{"id":"item","title":"Still open","status":0}]}`)
	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	task, err = st.Task(ctx, mvParcel)
	if err != nil {
		t.Fatal(err)
	}
	return st, task
}

func columnCommandCounts(t *testing.T, st *store.Store) store.OutboxCounts {
	t.Helper()
	counts, err := st.OutboxCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestColumnCommandPreviewThenAcceptOnlyChangesColumn(t *testing.T) {
	isolate(t)
	st, original := seedColumnCommand(t)
	preview, code := resourceJSONRun(t, "edit", original.Id, "--column", "done", "--preview")
	if code != exitOK || string(preview["status"]) != `"preview"` || !strings.Contains(string(preview["data"]), taskColumnOrigin) {
		t.Fatalf("preview: %d %s", code, preview)
	}
	id := previewIdentity(t, preview)
	current, err := st.Task(context.Background(), original.Id)
	if err != nil || !reflect.DeepEqual(current, original) || columnCommandCounts(t, st).Pending != 0 {
		t.Fatalf("preview changed a task or queue: %v", err)
	}
	result, code := runMachine(t, context.Background(), "edit", "--accept", id)
	if code != exitOK || result.Status != "queued" || result.Meta.Pending != 1 {
		t.Fatalf("accept: code=%d status=%q pending=%d error=%+v data=%s", code, result.Status, result.Meta.Pending, result.Error, result.Data)
	}
	current, err = st.Task(context.Background(), original.Id)
	if err != nil || current.ColumnId != "done" || current.ProjectId != original.ProjectId || current.Status != original.Status || current.SortOrder != original.SortOrder || current.Content != original.Content || !reflect.DeepEqual(current.Items, original.Items) {
		t.Fatalf("assignment changed unrelated fields: %+v %v", current, err)
	}
	before := columnCommandCounts(t, st)
	_, _ = runMachine(t, context.Background(), "edit", "--accept", id)
	if after := columnCommandCounts(t, st); after != before {
		t.Fatalf("preview replay queued another operation: %+v -> %+v", before, after)
	}
}

func TestColumnCommandNoopDoesNotClaimQueuedWork(t *testing.T) {
	isolate(t)
	st, original := seedColumnCommand(t)
	preview, code := resourceJSONRun(t, "edit", original.Id, "--column", "todo", "--preview")
	if code != exitOK {
		t.Fatalf("preview: %d %s", code, preview)
	}
	id := previewIdentity(t, preview)
	result, code := runMachine(t, context.Background(), "edit", "--accept", id)
	if code != exitOK || result.Status != "ok" || result.Meta.Pending != 0 || columnCommandCounts(t, st).Pending != 0 {
		t.Fatalf("no-op: code=%d status=%q pending=%d error=%+v data=%s", code, result.Status, result.Meta.Pending, result.Error, result.Data)
	}
	code, stdout, stderr := runFeatureCommand(t, "edit", "--accept", id)
	if code != exitOK || !strings.Contains(stdout, "No task changes.") || strings.Contains(stdout, "queued") {
		t.Fatalf("human no-op: %d %s %s", code, stdout, stderr)
	}
}

func TestColumnCommandRefusesMixedMissingAndForeignTargets(t *testing.T) {
	isolate(t)
	st, original := seedColumnCommand(t)
	for _, flags := range [][]string{
		{"--column", "done", "--parent", "none"},
		{"--column", "done", "--estimated-pomo", "1"},
		{"--column", "done", "--estimated-duration", "1m"},
		{"--column", "done", "--sort-order", "9"},
		{"--column", ""},
		{"--column", "missing"},
		{"--column", "done", "--column", "todo"},
	} {
		result, code := runMachine(t, context.Background(), append([]string{"edit", original.Id}, flags...)...)
		if code == exitOK {
			t.Fatalf("accepted invalid assignment %v: %+v", flags, result)
		}
	}
	if err := st.MergeEntities(context.Background(), "column", "p2", []store.ResourceEntity{{ServerID: "foreign", Data: json.RawMessage(`{"id":"foreign","projectId":"p2","name":"Foreign"}`)}}, false); err != nil {
		t.Fatal(err)
	}
	if _, code := runMachine(t, context.Background(), "edit", original.Id, "--column", "foreign", "--preview"); code == exitOK {
		t.Fatal("accepted a column from another project")
	}
	if columnCommandCounts(t, st).Pending != 0 {
		t.Fatal("invalid column arguments queued work")
	}
}

func TestColumnCommandRejectsTamperedWrongKindAndStalePreviews(t *testing.T) {
	for _, scenario := range []string{"tampered", "wrong kind", "task changed", "column changed"} {
		t.Run(scenario, func(t *testing.T) {
			isolate(t)
			st, original := seedColumnCommand(t)
			ctx := context.Background()
			result, code := resourceJSONRun(t, "edit", original.Id, "--column", "done", "--preview")
			if code != exitOK {
				t.Fatalf("preview: %d %s", code, result)
			}
			id := previewIdentity(t, result)
			switch scenario {
			case "tampered", "wrong kind":
				raw, exists, err := st.Meta(ctx, "task_column_preview_"+id)
				if err != nil || !exists {
					t.Fatalf("read preview: %v %v", exists, err)
				}
				if scenario == "tampered" {
					raw += " "
				} else {
					var envelope taskColumnEnvelope
					if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
						t.Fatal(err)
					}
					envelope.Kind = "task_fields"
					encoded, err := json.Marshal(envelope)
					if err != nil {
						t.Fatal(err)
					}
					raw = string(encoded)
					sum := sha256.Sum256(encoded)
					id = hex.EncodeToString(sum[:])
				}
				if err := st.SetMeta(ctx, "task_column_preview_"+id, raw); err != nil {
					t.Fatal(err)
				}
			case "task changed":
				if _, err := st.UpdateTask(ctx, original.Id, model.TaskEdit{Content: model.Ptr("Changed elsewhere")}); err != nil {
					t.Fatal(err)
				}
			case "column changed":
				if err := st.MergeEntities(ctx, "column", "p1", []store.ResourceEntity{{ServerID: "done", Data: json.RawMessage(`{"id":"done","projectId":"p1","name":"Renamed","sortOrder":1}`)}}, false); err != nil {
					t.Fatal(err)
				}
			}
			before := columnCommandCounts(t, st)
			if _, code := runMachine(t, ctx, "edit", "--accept", id); code == exitOK {
				t.Fatal("accepted an invalid or stale column preview")
			}
			if after := columnCommandCounts(t, st); after != before {
				t.Fatal("invalid preview added queued work")
			}
			current, err := st.Task(ctx, original.Id)
			if err != nil || current.ColumnId != "todo" {
				t.Fatalf("invalid preview moved the task: %+v %v", current, err)
			}
		})
	}
}

func TestColumnCommandHumanPreviewEscapesAndWrapsCompleteIdentity(t *testing.T) {
	isolate(t)
	st, original := seedColumnCommand(t)
	raw, err := json.Marshal(map[string]any{"id": "done", "projectId": "p1", "name": "Done\x1b]52;c;x\a " + strings.Repeat("long destination ", 12), "sortOrder": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MergeEntities(context.Background(), "column", "p1", []store.ResourceEntity{{ServerID: "done", Data: raw}}, false); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runFeatureCommand(t, "edit", original.Id, "--column", "done", "--preview")
	if code != exitOK || strings.Contains(stdout, "\x1b]52") || strings.ContainsRune(stdout, '\a') {
		t.Fatalf("unsafe human preview: %d %q %s", code, stdout, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if ansi.StringWidth(line) > cli.Width {
			t.Fatalf("preview line exceeds width: %q", line)
		}
	}
	for _, want := range []string{original.Id, "Source column:", "Destination column:", "Official Open API", "--accept"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("human preview misses %q: %s", want, stdout)
		}
	}
}
