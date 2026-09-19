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

// localParentFixture caches a server child and a local parent whose only
// queued change is the create that will give the parent a server id.
func localParentFixture(t *testing.T) (*Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"}, model.Project{Id: "p2", Name: "Q"})
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: model.Task{Id: "child", ProjectId: "p1", Title: "Child"}}}); err != nil {
		t.Fatal(err)
	}
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	return st, parent
}

func TestTaskExtensionsAttachOnlyToALocalParentAwaitingItsOwnCreate(t *testing.T) {
	ctx := context.Background()
	st, parent := localParentFixture(t)
	if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("a parent waiting for nothing but its own create was refused: %v", err)
	}
	child, err := st.Task(ctx, "child")
	if err != nil || child.ParentId != parent.Id {
		t.Fatalf("child = %+v %v, want it under the local parent", child, err)
	}

	for _, c := range []struct {
		name  string
		stage func(t *testing.T, st *Store, parent model.Task)
	}{
		{"inflight create", func(t *testing.T, st *Store, _ model.Task) {
			if claimed, _, err := st.Claim(ctx, 1, time.Minute); err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
		}},
		{"failed create", func(t *testing.T, st *Store, _ model.Task) {
			claimed, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			if err := st.MarkFailed(ctx, claimed[0].Seq, claimed[0].LeaseToken, "parked"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a second queued change", func(t *testing.T, st *Store, parent model.Task) {
			if _, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskUpdate,
				TaskID: parent.Id, ProjectID: "p1", Payload: []byte(`{"title":"Parent"}`)}); err != nil {
				t.Fatal(err)
			}
		}},
		{"no queued create at all", func(t *testing.T, st *Store, parent model.Task) {
			if _, err := st.DB().ExecContext(ctx, `DELETE FROM outbox WHERE task_id = ?`, parent.Id); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, parent := localParentFixture(t)
			c.stage(t, st, parent)
			_, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr(parent.Id)})
			if err == nil || !strings.Contains(err.Error(), "sync the parent task before attaching a child") {
				t.Fatalf("error = %v, want the unresolved parent refused", err)
			}
			if child, err := st.Task(ctx, "child"); err != nil || child.ParentId != "" {
				t.Fatalf("child = %+v %v, want it left alone", child, err)
			}
		})
	}
}

func TestTaskExtensionsCheckALocalParentLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		want string
		edit func(t *testing.T, st *Store, parent model.Task) (string, model.TaskEdit)
	}{
		{"another list", "same project", func(t *testing.T, st *Store, _ model.Task) (string, model.TaskEdit) {
			elsewhere, err := st.CreateTask(ctx, model.Task{ProjectId: "p2", Title: "Elsewhere"})
			if err != nil {
				t.Fatal(err)
			}
			return "child", model.TaskEdit{ParentId: model.Ptr(elsewhere.Id)}
		}},
		{"completed parent", "parent task must be open", func(t *testing.T, st *Store, _ model.Task) (string, model.TaskEdit) {
			done, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Done", Status: model.TaskDone})
			if err != nil {
				t.Fatal(err)
			}
			return "child", model.TaskEdit{ParentId: model.Ptr(done.Id)}
		}},
		{"its own parent", "task cycle", func(t *testing.T, st *Store, parent model.Task) (string, model.TaskEdit) {
			return parent.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}
		}},
		{"uncached parent", "must be cached", func(t *testing.T, st *Store, _ model.Task) (string, model.TaskEdit) {
			return "child", model.TaskEdit{ParentId: model.Ptr(LocalIDPrefix + "0123456789abcdef")}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, parent := localParentFixture(t)
			id, edit := c.edit(t, st, parent)
			_, err := st.UpdateTask(ctx, id, edit)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// TestPrepareFeatureSendRearmsARejectedLocalParentWithoutRevalidating records
// what the second arming of a rejected operation does with the parent it
// froze: it replays the frozen snapshot and never reaches
// freezeFeatureSnapshot, so validateTaskExtensions does not run again. The
// state below cannot arise from a push - sync checks the parent chain of a
// prepared as well as of a rejected payload before it prepares the send, and
// a local parent is parked there - so it is built by hand here.
func TestPrepareFeatureSendRearmsARejectedLocalParentWithoutRevalidating(t *testing.T) {
	ctx := context.Background()
	st, parent := localParentFixture(t)
	if _, err := st.UpdateTask(ctx, "child", model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.Claim(ctx, 2, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	create, update := claimed[0], claimed[1]
	if create.Op != OpTaskCreate || update.Op != OpTaskUpdate {
		t.Fatalf("claimed %s then %s, want the create of the parent first", create.Op, update.Op)
	}
	// The freeze validates the parent chain, so the create of the parent has
	// to be waiting in the queue the way it does when the update is reached.
	if err := st.Unclaim(ctx, create.Seq, create.LeaseToken, "waiting"); err != nil {
		t.Fatal(err)
	}
	send, present, post, err := st.PrepareFeatureSend(ctx, update)
	if err != nil || !present || !post {
		t.Fatalf("arm: %t %t %v", present, post, err)
	}
	if send.Metadata.Snapshot == nil || send.Metadata.Snapshot.Extensions == nil ||
		send.Metadata.Snapshot.Extensions.ParentId == nil || *send.Metadata.Snapshot.Extensions.ParentId != parent.Id {
		t.Fatalf("frozen snapshot = %+v, want the local parent of the queued create", send.Metadata.Snapshot)
	}
	if err := st.RejectFeature(ctx, update); err != nil {
		t.Fatal(err)
	}
	// Claiming the create takes away what the relaxed guard accepts, so a
	// second validation would refuse this parent.
	if again, _, err := st.Claim(ctx, 1, time.Minute); err != nil || len(again) != 1 || again[0].Seq != create.Seq {
		t.Fatalf("reclaim the create: %+v %v", again, err)
	}

	send, present, post, err = st.PrepareFeatureSend(ctx, update)
	if err != nil || !present || !post {
		t.Fatalf("rearm of the rejected operation = %t %t %v, want it replayed", present, post, err)
	}
	if send.Metadata.Phase != FeatureArmed {
		t.Fatalf("phase = %q, want the frozen request armed again", send.Metadata.Phase)
	}
	if send.Metadata.Snapshot.Extensions.ParentId == nil || *send.Metadata.Snapshot.Extensions.ParentId != parent.Id {
		t.Fatalf("frozen parent = %+v, want the snapshot replayed unchanged", send.Metadata.Snapshot.Extensions)
	}
}

// queuedChildFixture queues what a task written under another one queues:
// the create of a parent, the create of a child, and the update that hangs
// the child under the local id the parent still has.
func queuedChildFixture(t *testing.T) (*Store, model.Task, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("queue the link to a parent waiting for its own create: %v", err)
	}
	return st, parent, child
}

// claimChildLink settles the creates of queuedChildFixture and claims the
// relationship alone: an entry is claimable only once every older entry of
// its task is settled, so the link of a child is reached one round after the
// create of that child.
func claimChildLink(t *testing.T, st *Store) OutboxItem {
	t.Helper()
	ctx := context.Background()
	for _, item := range claimAll(t, st) {
		if err := st.MarkDone(ctx, item.Seq, item.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	items := claimAll(t, st)
	if len(items) != 1 || items[0].Op != OpTaskUpdate {
		t.Fatalf("claimed %+v, want the queued relationship alone", items)
	}
	return items[0]
}

// The ids a push gives the two tasks of queuedChildFixture once their creates
// are confirmed: the relationship of a child is only ever sent under them.
const (
	sentParentID = "srv-parent"
	sentChildID  = "srv-child"
)

// armChildLink carries queuedChildFixture to the state a push leaves behind
// halfway through: the creates are confirmed, both tasks go by a server id,
// and the relationship has been sent with its outcome still unknown.
func armChildLink(t *testing.T, st *Store, parent, child model.Task) (OutboxItem, FeatureSend) {
	t.Helper()
	ctx := context.Background()
	for _, item := range claimAll(t, st) {
		if err := st.MarkDone(ctx, item.Seq, item.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	for _, swap := range []struct{ local, server string }{{parent.Id, sentParentID}, {child.Id, sentChildID}} {
		if err := st.ReplaceLocalID(ctx, swap.local, swap.server); err != nil {
			t.Fatal(err)
		}
	}
	items := claimAll(t, st)
	if len(items) != 1 || items[0].Op != OpTaskUpdate {
		t.Fatalf("claimed %+v, want the queued relationship alone", items)
	}
	send, present, post, err := st.PrepareFeatureSend(ctx, items[0])
	if err != nil || !present || !post || send.Metadata.Phase != FeatureArmed {
		t.Fatalf("arm the relationship = %q %t %t %v, want the request sent", send.Metadata.Phase, present, post, err)
	}
	return items[0], send
}

// recordChildLinkEvidence writes back what the server answered an armed
// relationship with, the way the push records it before it settles.
func recordChildLinkEvidence(t *testing.T, st *Store, item OutboxItem, send FeatureSend, phase FeaturePhase) {
	t.Helper()
	raw := []byte(`{"id":"` + sentChildID + `","projectId":"p1","parentId":"` + sentParentID + `"}`)
	confirmation, err := ConfirmFeatureResponse(send, sentChildID, "p1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFeatureEvidence(context.Background(), item, phase, confirmation); err != nil {
		t.Fatal(err)
	}
}

// drainQueue settles the whole queue the way a push that answers every
// request does, round after round.
func drainQueue(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	for range 10 {
		items := claimAll(t, st)
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			if err := st.MarkDone(ctx, item.Seq, item.LeaseToken); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("the queue did not drain")
}

// A closed parent is what the push and the server both refuse to hang a child
// under, so a parent whose child is still queued may not be closed either:
// the relationship would be parked on the push, parked again on every retry,
// and dropped by the next pull. Every unsent state of that relationship
// counts, a parked one included - tt sync --retry-failed raises it back - and
// every phase of it, an armed one included: the store cannot tell whether a
// sent request reached the server, and tt sync is what settles it either way.
func TestCompletionRefusesAParentOfAnUnsentChildLink(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// stage leaves the relationship in the state under test and
		// answers with the id the parent goes by afterwards.
		stage func(t *testing.T, st *Store, parent, child model.Task) string
	}{
		{"queued link", func(_ *testing.T, _ *Store, parent, _ model.Task) string { return parent.Id }},
		{"inflight link", func(t *testing.T, st *Store, parent, _ model.Task) string {
			claimChildLink(t, st)
			return parent.Id
		}},
		{"parked link", func(t *testing.T, st *Store, parent, _ model.Task) string {
			item := claimChildLink(t, st)
			if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "parked"); err != nil {
				t.Fatal(err)
			}
			return parent.Id
		}},
		{"armed link", func(t *testing.T, st *Store, parent, child model.Task) string {
			armChildLink(t, st, parent, child)
			return sentParentID
		}},
		{"accepted link", func(t *testing.T, st *Store, parent, child model.Task) string {
			item, send := armChildLink(t, st, parent, child)
			recordChildLinkEvidence(t, st, item, send, FeatureAccepted)
			return sentParentID
		}},
		{"mismatched link", func(t *testing.T, st *Store, parent, child model.Task) string {
			item, send := armChildLink(t, st, parent, child)
			recordChildLinkEvidence(t, st, item, send, FeatureMismatch)
			return sentParentID
		}},
		{"rejected link", func(t *testing.T, st *Store, parent, child model.Task) string {
			item, _ := armChildLink(t, st, parent, child)
			if err := st.RejectFeature(ctx, item); err != nil {
				t.Fatal(err)
			}
			return sentParentID
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, parent, child := queuedChildFixture(t)
			parentID := c.stage(t, st, parent, child)
			if _, err := st.CompleteTask(ctx, parentID, CompleteOptions{}); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("tt done = %v, want the unsent child relationship refused", err)
			}
			stored, err := st.Task(ctx, parentID)
			if err != nil || stored.Status != model.TaskOpen {
				t.Fatalf("parent = %+v %v, want it left open", stored, err)
			}
			// The browser, an applied tt ai plan and a batch close a task
			// through the guarded entry point, and it reads the same rule.
			if _, err := st.CompleteTaskIfUnchanged(ctx, stored, CompleteOptions{}); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("the guarded completion = %v, want the same refusal", err)
			}
		})
	}
}

func TestCompletionTakesAParentWhoseChildLinkIsSent(t *testing.T) {
	ctx := context.Background()
	st, parent, _ := queuedChildFixture(t)
	drainQueue(t, st)
	if counts, err := st.OutboxCounts(ctx); err != nil || counts != (OutboxCounts{}) {
		t.Fatalf("outbox = %+v %v, want nothing left of the batch", counts, err)
	}
	done, err := st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if err != nil || !done.Status.Done() {
		t.Fatalf("complete = %+v %v, want the parent closed once its child is on the server", done, err)
	}
}

// The guard reads the relationship, never the mention of an id: closing the
// child of that same queued link, or a task nothing hangs under, is what
// tt done does all day.
func TestCompletionTakesATaskNothingHangsUnder(t *testing.T) {
	ctx := context.Background()
	st, _, child := queuedChildFixture(t)
	done, err := st.CompleteTask(ctx, child.Id, CompleteOptions{})
	if err != nil || !done.Status.Done() {
		t.Fatalf("the child of the queued link = %+v %v, want it closed", done, err)
	}
	alone, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Alone"})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := st.CompleteTask(ctx, alone.Id, CompleteOptions{}); err != nil || !done.Status.Done() {
		t.Fatalf("a task of its own = %+v %v, want it closed", done, err)
	}
}

// A relationship written by the create of the child, the way the undo of a
// deleted child writes it back, is the same relationship and is read out of
// the create payload.
func TestCompletionRefusesAParentNamedByAQueuedCreate(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child", ParentId: parent.Id})
	if err != nil {
		t.Fatalf("create a child that names its parent: %v", err)
	}
	if _, err := st.CompleteTask(ctx, parent.Id, CompleteOptions{}); !errors.Is(err, ErrChildLinkUnsent) {
		t.Fatalf("complete the parent = %v, want the relationship of the create refused", err)
	}
	// The create of the child holds the id of the child as well as the id of
	// its parent, and only one of the two hangs anything under the other.
	if done, err := st.CompleteTask(ctx, child.Id, CompleteOptions{}); err != nil || !done.Status.Done() {
		t.Fatalf("complete the child = %+v %v, want it closed", done, err)
	}
}

// ancestorChainFixture stores the chain a pull returns - great-grandparent,
// grandparent, parent - beside a task of its own, and hangs a child under the
// bottom of it the way tt edit --parent does, leaving that relationship
// queued.
func ancestorChainFixture(t *testing.T) (*Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	top := openTask("top", "p1", "Great-grandparent")
	upper := openTask("upper", "p1", "Grandparent")
	upper.ParentId = top.Id
	parent := openTask("parent", "p1", "Parent")
	parent.ParentId = upper.Id
	child := openTask("child", "p1", "Child")
	aside := openTask("aside", "p1", "Aside")
	if _, err := st.SyncProject(ctx, "p1", fromServer(top, upper, parent, child, aside)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []model.Task{upper, parent} {
		stored, err := st.Task(ctx, want.Id)
		if err != nil || stored.ParentId != want.ParentId {
			t.Fatalf("cached %s = %+v %v, want the pulled chain in the cache", want.Id, stored, err)
		}
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatalf("queue the relationship: %v", err)
	}
	return st, child
}

// The attach walks the whole chain of ancestors and wants every one of them
// open, so closing any of them strands the queued relationship the same way:
// the push reads the chain out of the cache, the server reads it too, and the
// relationship parks over a closed ancestor however far above it sits.
func TestCompletionRefusesEveryAncestorOfAnUnsentChildLink(t *testing.T) {
	ctx := context.Background()
	for _, ancestor := range []string{"parent", "upper", "top"} {
		t.Run(ancestor, func(t *testing.T) {
			st, child := ancestorChainFixture(t)
			_, err := st.CompleteTask(ctx, ancestor, CompleteOptions{})
			if !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("tt done %s = %v, want the unsent relationship refused", ancestor, err)
			}
			if !strings.Contains(err.Error(), child.Id) {
				t.Fatalf("tt done %s = %q, want the queued child named", ancestor, err)
			}
			stored, err := st.Task(ctx, ancestor)
			if err != nil || stored.Status != model.TaskOpen {
				t.Fatalf("%s = %+v %v, want it left open", ancestor, stored, err)
			}
			// The guarded entry point the browser and a batch close
			// through reads the same chain.
			if _, err := st.CompleteTaskIfUnchanged(ctx, stored, CompleteOptions{}); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("the guarded completion of %s = %v, want the same refusal", ancestor, err)
			}
		})
	}
}

// Nothing hangs under the child of the relationship or beside the chain, and
// a task off the chain is what tt done closes all day.
func TestCompletionTakesATaskOffTheChainOfAnUnsentChildLink(t *testing.T) {
	ctx := context.Background()
	st, child := ancestorChainFixture(t)
	for _, id := range []string{"aside", child.Id} {
		done, err := st.CompleteTask(ctx, id, CompleteOptions{})
		if err != nil || !done.Status.Done() {
			t.Fatalf("tt done %s = %+v %v, want it closed", id, done, err)
		}
	}
}

// Once the chain is closed off, the refusal has to name a way out that moves
// the entry it is refusing over, and tt sync is that way only while the entry
// is still in line for it. A parked entry is passed over by tt sync, comes
// back with --retry-failed and goes away with --drop-parked - and without
// that last one a relationship the server rejects every time would close the
// task above it off for good.
func TestClosureRefusalNamesTheCommandThatMovesTheParkedEntry(t *testing.T) {
	ctx := context.Background()
	st, parent, _ := queuedChildFixture(t)
	_, err := st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if !errors.Is(err, ErrChildLinkUnsent) || errors.Is(err, ErrChildLinkParked) {
		t.Fatalf("the queued relationship = %v, want it refused as unsent and not as parked", err)
	}
	if !strings.Contains(err.Error(), "tt sync first") || strings.Contains(err.Error(), "--retry-failed") {
		t.Fatalf("the queued relationship = %q, want tt sync named and the parked commands left out", err)
	}

	item := claimChildLink(t, st)
	if err := st.MarkFailed(ctx, item.Seq, item.LeaseToken, "rejected"); err != nil {
		t.Fatal(err)
	}
	_, err = st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if !errors.Is(err, ErrChildLinkParked) || !errors.Is(err, ErrChildLinkUnsent) {
		t.Fatalf("the parked relationship = %v, want it refused as parked", err)
	}
	for _, command := range []string{"tt sync --retry-failed", "tt sync --drop-parked"} {
		if !strings.Contains(err.Error(), command) {
			t.Fatalf("the parked relationship = %q, want %q named", err, command)
		}
	}

	// The last way out the refusal names is a way out: what it throws away
	// is the parked relationship, and the task closes afterwards.
	if _, err := st.DropParked(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if err != nil || !done.Status.Done() {
		t.Fatalf("tt done after the drop = %+v %v, want the task closed", done, err)
	}
}

// The relationship the refusal names is the one the queue holds, not the one
// the cache shows: an undo takes the relationship back out of the cache and
// leaves the entry that sends it in the queue, and the reader is told which
// task that entry is about.
func TestClosureRefusalNamesTheQueuedChildAnUndoTookOutOfTheCache(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
		t.Fatal(err)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyUndo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Task(ctx, child.Id)
	if err != nil || stored.ParentId != "" {
		t.Fatalf("the child after the undo = %+v %v, want the relationship out of the cache", stored, err)
	}
	_, err = st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if !errors.Is(err, ErrChildLinkUnsent) {
		t.Fatalf("tt done = %v, want the queued relationship still refused", err)
	}
	if !strings.Contains(err.Error(), child.Id) {
		t.Fatalf("tt done = %q, want the queued child named", err)
	}
}

// The guard reads relationships, and a queued change is not one because it
// holds the id somewhere: a title that spells the id out, an estimate of the
// child, the create of the task being closed - none of them hangs anything
// under anything.
func TestCompletionTakesATaskOnlyMentionedByTheQueue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "P"})
	parent := openTask("parent", "p1", "Parent")
	child := openTask("child", "p1", "Child")
	if _, err := st.SyncProject(ctx, "p1", fromServer(parent, child)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{Title: model.Ptr(parent.Id)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{EstimatedPomo: model.Ptr(2)}); err != nil {
		t.Fatal(err)
	}
	done, err := st.CompleteTask(ctx, parent.Id, CompleteOptions{})
	if err != nil || !done.Status.Done() {
		t.Fatalf("tt done = %+v %v, want a task nothing hangs under closed", done, err)
	}
}
