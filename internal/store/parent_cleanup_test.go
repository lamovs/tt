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

func TestDroppedParentDetachesQueuedChildCreation(t *testing.T) {
	for _, features := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "checklist and estimates"}[features], func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			task := model.Task{ProjectId: "p1", Title: "Child to create", ParentId: parent.Id}
			if features {
				task.EstimatedPomo = 3
				task.Items = []model.Item{{Title: "Keep this item"}}
			}
			child, err := st.CreateTask(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			parkLocalParent(t, st, parent)
			if _, err := st.DropParked(ctx); err != nil {
				t.Fatal(err)
			}
			claimed, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 1 || claimed[0].TaskID != child.Id || claimed[0].Op != OpTaskCreate {
				t.Fatalf("child creation lost: %+v %v", claimed, err)
			}
			queued, metadata, err := DecodeTaskPayload(claimed[0].Payload)
			if err != nil || queued.ParentId != "" || queued.Title != child.Title || queued.EstimatedPomo != child.EstimatedPomo {
				t.Fatalf("queued child: %+v %v", queued, err)
			}
			if features && (metadata == nil || len(queued.Items) != 1 || queued.Items[0].Key != child.Items[0].Key) {
				t.Fatalf("checklist identity lost: %+v", queued)
			}
			if !features && metadata != nil {
				t.Fatal("parent-only metadata was retained")
			}
			if _, _, _, err := st.PrepareFeatureSend(ctx, claimed[0]); err != nil {
				t.Fatalf("child cannot be sent: %v", err)
			}
			cached, err := st.Task(ctx, child.Id)
			if err != nil || cached.ParentId != "" {
				t.Fatalf("cached child: %+v %v", cached, err)
			}
		})
	}
}

func TestDroppedParentRepairsTheNextRelationshipBaseline(t *testing.T) {
	ctx := context.Background()
	st, parent := localParentFixture(t)
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "New child", ParentId: parent.Id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr("child")}); err != nil {
		t.Fatal(err)
	}
	parkLocalParent(t, st, parent)
	if _, err := st.DropParked(ctx); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := st.DB().QueryRowContext(ctx, `SELECT payload FROM outbox WHERE task_id = ? AND op = ?`, child.Id, OpTaskUpdate).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	edit, metadata, err := DecodeTaskEditPayload(payload)
	if err != nil || metadata == nil || metadata.ExtensionBaseline == nil || metadata.ExtensionBaseline.ParentId == nil {
		t.Fatalf("relationship metadata: %+v %v", metadata, err)
	}
	if edit.ParentId == nil || *edit.ParentId != "child" || *metadata.ExtensionBaseline.ParentId != "" {
		t.Fatalf("desired=%+v baseline=%+v", edit, metadata.ExtensionBaseline)
	}
	if err := CheckTaskExtensionBaseline([]byte(`{"id":"server-child","projectId":"p1"}`), "server-child", "p1", *metadata.ExtensionBaseline); err != nil {
		t.Fatalf("detached creation cannot satisfy later baseline: %v", err)
	}
}

func TestDroppedParentRefusesInflightAndFrozenChildCreation(t *testing.T) {
	for _, phase := range []string{"inflight", "armed", "rejected"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			child, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Child", ParentId: parent.Id})
			if err != nil {
				t.Fatal(err)
			}
			claimed, _, err := st.Claim(ctx, 2, time.Minute)
			if err != nil || len(claimed) != 2 {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			if err := st.Unclaim(ctx, claimed[0].Seq, claimed[0].LeaseToken, "waiting"); err != nil {
				t.Fatal(err)
			}
			if phase != "inflight" {
				if _, _, _, err := st.PrepareFeatureSend(ctx, claimed[1]); err != nil {
					t.Fatal(err)
				}
				if phase == "rejected" {
					if err := st.RejectFeature(ctx, claimed[1]); err != nil {
						t.Fatal(err)
					}
				}
				if err := st.Unclaim(ctx, claimed[1].Seq, claimed[1].LeaseToken, "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			parkLocalParent(t, st, parent)
			before, err := rowVersion(ctx, st.DB(), `SELECT * FROM outbox ORDER BY seq`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DropParked(ctx); err == nil || !strings.Contains(err.Error(), "recover that operation first") {
				t.Fatalf("drop: %v", err)
			}
			after, err := rowVersion(ctx, st.DB(), `SELECT * FROM outbox ORDER BY seq`)
			if err != nil || before != after {
				t.Fatal("refusal changed the queue")
			}
			if _, err := st.Task(ctx, parent.Id); err != nil {
				t.Fatal(err)
			}
			got, err := st.Task(ctx, child.Id)
			if err != nil || got.ParentId != parent.Id {
				t.Fatalf("child changed: %+v %v", got, err)
			}
		})
	}
}

func TestParentRemovalAndMoveRefuseUnsentDescendants(t *testing.T) {
	for _, operation := range []string{"delete", "recreate", "delete preview", "move preview", "native preview"} {
		for _, state := range []string{"pending", "inflight", "failed"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				st, child := ancestorChainFixture(t)
				seedProjects(t, st, model.Project{Id: "p1", Name: "P"}, model.Project{Id: "p2", Name: "Q"})
				if state != "pending" {
					items, _, err := st.Claim(ctx, 1, time.Minute)
					if err != nil || len(items) != 1 {
						t.Fatalf("claim: %+v %v", items, err)
					}
					if state == "failed" {
						if err := st.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "rejected"); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, err := rowVersion(ctx, st.DB(), `SELECT * FROM tasks ORDER BY id`)
				if err != nil {
					t.Fatal(err)
				}
				top, err := st.Task(ctx, "top")
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "delete":
					err = st.DeleteTask(ctx, top.Id)
				case "recreate":
					_, err = st.MoveTask(ctx, top.Id, "p2", MoveOptions{ByRecreate: true})
				case "delete preview":
					_, err = st.PreviewTaskOperation(ctx, top, "")
				case "move preview":
					_, err = st.PreviewTaskOperation(ctx, top, "p2")
				case "native preview":
					_, err = st.PreviewNativeMove(ctx, top, "p2")
				}
				if !errors.Is(err, ErrChildLinkUnsent) || !strings.Contains(err.Error(), child.Id) {
					t.Fatalf("refusal: %v", err)
				}
				after, err := rowVersion(ctx, st.DB(), `SELECT * FROM tasks ORDER BY id`)
				if err != nil || before != after {
					t.Fatal("refusal changed tasks")
				}
			})
		}
	}
}

func TestParentOperationRechecksLinksAtAcceptance(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "native move"}[native], func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			parent := nativeMoveFixture(t, st)
			var accept func() error
			if native {
				preview, err := st.PreviewNativeMove(ctx, parent, "target")
				if err != nil {
					t.Fatal(err)
				}
				accept = func() error { _, err := st.ApplyNativeMove(ctx, preview); return err }
			} else {
				preview, err := st.PreviewTaskOperation(ctx, parent, "")
				if err != nil {
					t.Fatal(err)
				}
				accept = func() error { _, err := st.ApplyTaskOperation(ctx, preview); return err }
			}
			if _, err := st.CreateTask(ctx, model.Task{ProjectId: "source", Title: "Child", ParentId: parent.Id}); err != nil {
				t.Fatal(err)
			}
			if err := accept(); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("accept: %v", err)
			}
		})
	}
}

func TestUncachedParentsIncludesQueuedLinksWithoutChangingCache(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO tasks(id, project_id, parent_id) VALUES ('child', 'p', 'absent'), ('parent', 'p', ''), ('healthy', 'p', 'parent')`); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(model.TaskEdit{ParentId: model.Ptr("queue-parent")})
	seq, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: "child", ProjectID: "p", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: OpTaskUpdate, TaskID: "healthy", Payload: []byte(`broken`)}); err != nil {
		t.Fatal(err)
	}
	refs, err := st.UncachedTaskParents(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("references: %+v %v", refs, err)
	}
	if refs[0] != (TaskParentReference{TaskID: "child", ParentID: "absent"}) || refs[1] != (TaskParentReference{TaskID: "child", ParentID: "queue-parent", QueueSeq: seq}) {
		t.Fatalf("references: %+v", refs)
	}
	var parent string
	if err := st.DB().QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id='child'`).Scan(&parent); err != nil || parent != "absent" {
		t.Fatalf("parent changed: %q %v", parent, err)
	}
}

func TestDroppedParentRetainsPreviousServerRelationship(t *testing.T) {
	for _, reparent := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore previous", true: "rebase next"}[reparent], func(t *testing.T) {
			ctx := context.Background()
			st, parent := localParentFixture(t)
			child := openTask("child", "p1", "Child")
			child.ParentId = "previous"
			if _, err := st.SyncProject(ctx, "p1", fromServer(child, openTask("previous", "p1", "Previous"), openTask("next", "p1", "Next"))); err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(parent.Id)}); err != nil {
				t.Fatal(err)
			}
			if reparent {
				if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr("next")}); err != nil {
					t.Fatal(err)
				}
			}
			parkLocalParent(t, st, parent)
			if _, err := st.DropParked(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := st.Task(ctx, child.Id)
			want := "previous"
			if reparent {
				want = "next"
			}
			if err != nil || got.ParentId != want {
				t.Fatalf("child parent=%q want=%q %v", got.ParentId, want, err)
			}
			if reparent {
				var payload []byte
				if err := st.DB().QueryRowContext(ctx, `SELECT payload FROM outbox WHERE task_id='child'`).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				_, metadata, err := DecodeTaskEditPayload(payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := CheckTaskExtensionBaseline([]byte(`{"id":"child","projectId":"p1","parentId":"previous"}`), "child", "p1", *metadata.ExtensionBaseline); err != nil {
					t.Fatalf("baseline lost original parent: %v", err)
				}
			}
		})
	}
}

func TestParentRemovalWaitsForQueuedDetachOrReparent(t *testing.T) {
	for _, desired := range []string{"", "next"} {
		t.Run("parent="+desired, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProjects(t, st, model.Project{Id: "p1", Name: "P"}, model.Project{Id: "p2", Name: "Q"})
			child := openTask("child", "p1", "Child")
			child.ParentId = "previous"
			if _, err := st.SyncProject(ctx, "p1", fromServer(child, openTask("previous", "p1", "Previous"), openTask("next", "p1", "Next"))); err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpdateTask(ctx, child.Id, model.TaskEdit{ParentId: model.Ptr(desired)}); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteTask(ctx, "previous"); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("delete old parent: %v", err)
			}
			if _, err := st.MoveTask(ctx, "previous", "p2", MoveOptions{ByRecreate: true}); !errors.Is(err, ErrChildLinkUnsent) {
				t.Fatalf("move old parent: %v", err)
			}
		})
	}
}
