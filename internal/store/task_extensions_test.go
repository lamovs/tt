package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestTaskExtensionsRoundtripUndoAndRawPreservation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	project := model.Project{Id: "p1", Name: "Project", Color: "#aabbcc", GroupId: "folder", ViewMode: "kanban", Permission: "write"}
	seedProjects(t, st, project)
	gotProject, err := st.Project(ctx, "p1")
	if err != nil || gotProject != project {
		t.Fatalf("project metadata: %+v %v", gotProject, err)
	}
	task := model.Task{Id: "t1", ProjectId: "p1", Title: "Task", ParentId: "parent", ChildIds: []string{"child"}, ColumnId: "column", ColumnName: "Doing", EstimatedDuration: 120, EstimatedPomo: 2, FocusSummaries: json.RawMessage(`[{"estimatedDuration":120,"estimatedPomo":2,"custom":[1,2]}]`)}
	raw := json.RawMessage(`{"id":"t1","projectId":"p1","unknown":{"kept":true},"focusSummaries":[{"estimatedDuration":120,"estimatedPomo":2,"custom":[1,2]}]}`)
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: task, Raw: raw}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !samePulledTask(got, task) {
		t.Fatalf("task fields lost: %+v", got)
	}
	preview, err := st.PreviewTaskExtensions(ctx, got, model.TaskEdit{EstimatedDuration: model.Ptr(int64(0)), EstimatedPomo: model.Ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := st.ApplyTaskExtensions(ctx, preview)
	if err != nil || !out.Changed {
		t.Fatalf("apply: %+v %v", out, err)
	}
	if _, err := st.ApplyTaskExtensions(ctx, preview); !errors.Is(err, ErrTaskChanged) {
		t.Fatalf("stale preview: %v", err)
	}
	var storedRaw string
	if err := st.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id='t1'`).Scan(&storedRaw); err != nil || storedRaw != string(raw) {
		t.Fatalf("raw changed: %s %v", storedRaw, err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Feature == nil || undo.Feature.Version != FeatureExtensionsPayloadVersion {
		t.Fatalf("unversioned undo: %+v", undo)
	}
	restored, err := st.ApplyUndo(ctx, undo)
	if err != nil {
		t.Fatal(err)
	}
	if restored.EstimatedDuration != 120 || restored.EstimatedPomo != 2 || restored.ParentId != "parent" || restored.ColumnId != "column" {
		t.Fatalf("undo lost fields: %+v", restored)
	}
}

func TestTaskExtensionsVersionedSnapshotAndExactConfirmation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Estimate", EstimatedDuration: 90, EstimatedPomo: 3}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	send, present, post, err := st.PrepareFeatureSend(ctx, claimed[0])
	if err != nil || !present || !post || send.Metadata.Version != 4 {
		t.Fatalf("prepare: %+v %t %t %v", send, present, post, err)
	}
	if _, err := ConfirmFeatureResponse(send, "server", "p1", []byte(`{"id":"server","projectId":"p1","focusSummaries":[{"estimatedDuration":90}]}`)); err == nil {
		t.Fatal("missing estimate accepted")
	}
	if _, err := ConfirmFeatureResponse(send, "server", "p1", []byte(`{"id":"server","projectId":"p1","focusSummaries":[{"estimatedDuration":90,"estimatedPomo":3}]}`)); err != nil {
		t.Fatal(err)
	}
	_, present, post, err = st.PrepareFeatureSend(ctx, claimed[0])
	if err != nil || !present || post {
		t.Fatalf("armed operation was resent: %t %t %v", present, post, err)
	}
	metadata := send.Metadata
	metadata.Version = 3
	if _, err := EncodeTaskPayload(*send.Task, metadata); err == nil {
		t.Fatal("new fields reinterpreted as old protocol")
	}
}

func TestTaskExtensionsValidationAndParentCycle(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: model.Task{Id: "parent", ProjectId: "p1", Title: "Parent"}}, {Task: model.Task{Id: "child", ProjectId: "p1", Title: "Child", ParentId: "parent"}}}); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []model.TaskEdit{{EstimatedDuration: model.Ptr(int64(-1))}, {EstimatedPomo: model.Ptr(61)}, {ParentId: model.Ptr("child")}, {ColumnId: model.Ptr("unknown")}} {
		if _, err := st.UpdateTask(ctx, "parent", edit); err == nil {
			t.Fatalf("invalid edit accepted: %+v", edit)
		}
	}
	if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr("")}); err != nil {
		t.Fatal(err)
	}
	if counts, err := st.OutboxCounts(ctx); err != nil || counts.Pending != 1 {
		t.Fatalf("invalid edits left queue rows: %+v %v", counts, err)
	}
}

func TestTaskExtensionsMigrationBackfillsRaw(t *testing.T) {
	ctx := context.Background()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := referenceSchema(ctx, migrations, 12)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw := `{"parentId":"parent","childIds":["child"],"columnId":"col","columnName":"Doing","focusSummaries":[{"estimatedDuration":42,"estimatedPomo":2,"seconds":10}],"unknown":true}`
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks(id,raw)VALUES('task',?)`, raw); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version == 13 {
			if _, err := db.ExecContext(ctx, migration.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	var parent, children, column, focus, stored string
	var seconds, pomo int
	if err := db.QueryRowContext(ctx, `SELECT parent_id,child_ids,column_id,estimated_duration,estimated_pomo,focus_summaries,raw FROM tasks WHERE id='task'`).Scan(&parent, &children, &column, &seconds, &pomo, &focus, &stored); err != nil {
		t.Fatal(err)
	}
	if parent != "parent" || column != "col" || seconds != 42 || pomo != 2 || !strings.Contains(children, "child") || !strings.Contains(focus, "seconds") || stored != raw {
		t.Fatalf("bad backfill: %q %q %q %d %d %s", parent, children, column, seconds, pomo, focus)
	}
}

func TestTaskExtensionsBaselineRefusesConflict(t *testing.T) {
	baseline := model.TaskEdit{EstimatedDuration: model.Ptr(int64(0)), ParentId: model.Ptr("")}
	if err := CheckTaskExtensionBaseline([]byte(`{"id":"t","projectId":"p"}`), "t", "p", baseline); err != nil {
		t.Fatal(err)
	}
	if err := CheckTaskExtensionBaseline([]byte(`{"id":"t","projectId":"p","focusSummaries":[{"estimatedDuration":10}]}`), "t", "p", baseline); err == nil {
		t.Fatal("remote conflict ignored")
	}
}

func TestTaskEstimatesRefuseAmbiguousSummaryEdits(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p", Name: "P"})
	task := model.Task{Id: "t", ProjectId: "p", Title: "Task", FocusSummaries: json.RawMessage(`[{"estimatedPomo":1},{"estimatedPomo":2}]`)}
	if _, err := st.SyncProject(ctx, "p", []ServerTask{{Task: task}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PreviewTaskExtensions(ctx, task, model.TaskEdit{EstimatedPomo: model.Ptr(3)}); err == nil {
		t.Fatal("ambiguous summary accepted")
	}
	if _, err := st.UpdateTask(ctx, task.Id, model.TaskEdit{Title: model.Ptr("unrelated edit")}); err != nil {
		t.Fatalf("ambiguous read data blocked unrelated edit: %v", err)
	}
}
