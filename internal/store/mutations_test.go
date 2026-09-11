package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func claimAll(t *testing.T, s *Store) []OutboxItem {
	t.Helper()
	items, _, err := s.Claim(context.Background(), 100, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return items
}

func TestCreateTaskOffline(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	created, err := s.CreateTask(ctx, model.Task{
		ProjectId: "p1",
		Title:     "Забрать посылку",
		Priority:  model.PriorityHigh,
		Items:     []model.Item{{Id: "i1", Title: "паспорт"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocalID(created.Id) {
		t.Fatalf("id %q is not a local one", created.Id)
	}
	if created.CreatedTime.IsZero() {
		t.Error("no creation time to sort by")
	}

	got, err := s.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку" || len(got.Items) != 1 {
		t.Fatalf("cached %+v", got)
	}

	var local, dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT local, dirty FROM tasks WHERE id = ?`, created.Id).
		Scan(&local, &dirty); err != nil {
		t.Fatal(err)
	}
	if local != 1 || dirty != 1 {
		t.Errorf("local %d dirty %d, want a local row at revision 1", local, dirty)
	}

	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskCreate || items[0].TaskID != created.Id {
		t.Fatalf("queued %+v", items)
	}
	var payload model.Task
	if err := json.Unmarshal(items[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Забрать посылку" || payload.ProjectId != "p1" ||
		payload.Priority != model.PriorityHigh || len(payload.Items) != 1 {
		t.Fatalf("payload %+v", payload)
	}

	events, err := s.EventsSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != TaskCreatedKind {
		t.Fatalf("journal %+v", events)
	}
	undo, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Op != OpTaskCreate || undo.Action.TaskID != created.Id {
		t.Fatalf("undo %+v", undo.Action)
	}
	if _, err := s.CreateTask(ctx, model.Task{Title: "без проекта"}); err == nil {
		t.Error("a task without a project was accepted")
	}
}

func TestMutationIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	if _, err := s.DB().ExecContext(ctx, `ALTER TABLE outbox RENAME TO outbox_hidden`); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err == nil {
		t.Fatal("create succeeded with no queue to write to")
	}
	if _, err := s.DB().ExecContext(ctx, `ALTER TABLE outbox_hidden RENAME TO outbox`); err != nil {
		t.Fatal(err)
	}

	tasks, err := s.Tasks(ctx, TaskFilter{Status: StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("the cache kept a task whose queue entry never landed: %v", taskTitles(tasks))
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{}) {
		t.Errorf("queue %+v", counts)
	}
	events, err := s.EventsSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("journal %+v", events)
	}
	if _, err := s.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Errorf("undo log: %v", err)
	}

	if _, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"}); err != nil {
		t.Fatal(err)
	}
	if tasks, _ = s.Tasks(ctx, TaskFilter{}); len(tasks) != 1 {
		t.Fatalf("after the retry: %v", taskTitles(tasks))
	}
	if counts, _ = s.OutboxCounts(ctx); counts.Pending != 1 {
		t.Fatalf("after the retry: %+v", counts)
	}
}

func TestUpdateTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	server := openTask("t1", "p1", "Забрать посылку")
	server.DueDate = mustTime(t, "2026-09-10T09:00:00.000+0000")
	if _, err := s.SyncProject(ctx, "p1", fromServer(server)); err != nil {
		t.Fatal(err)
	}

	got, err := s.UpdateTask(ctx, "t1", model.TaskEdit{
		Title:   model.Ptr("Забрать посылку на почте"),
		DueDate: model.NewEditTime(model.Time{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку на почте" || !got.DueDate.IsZero() {
		t.Fatalf("got %+v", got)
	}
	stored, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != got.Title || !stored.DueDate.IsZero() {
		t.Fatalf("stored %+v", stored)
	}

	if found, _ := s.Tasks(ctx, TaskFilter{Search: "почте"}); len(found) != 1 {
		t.Errorf("search after the edit found %v", taskTitles(found))
	}

	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskUpdate || items[0].ProjectID != "p1" {
		t.Fatalf("queued %+v", items)
	}
	var edit model.TaskEdit
	if err := json.Unmarshal(items[0].Payload, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Title == nil || *edit.Title != got.Title {
		t.Errorf("payload title %v", edit.Title)
	}
	if edit.DueDate == nil || !edit.DueDate.IsZero() {
		t.Errorf("payload does not clear the due date: %v", edit.DueDate)
	}

	undo, err := s.PopUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Before == nil || undo.Action.Before.Title == nil ||
		*undo.Action.Before.Title != "Забрать посылку" {
		t.Fatalf("undo %+v", undo.Action.Before)
	}
	if undo.Action.Before.DueDate == nil ||
		undo.Action.Before.DueDate.String() != "2026-09-10T09:00:00.000+0000" {
		t.Fatalf("undo lost the old due date: %+v", undo.Action.Before.DueDate)
	}
	if _, err := s.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Errorf("the stack still has %v", err)
	}
}

func TestUndoOfATagOnATaskWithoutTagsTakesItOff(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	tagged, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Tags: model.NewEditList([]string{"почта"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged.Tags) != 1 {
		t.Fatalf("the tag was not added: %v", tagged.Tags)
	}

	undo, err := s.PopUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Before == nil || undo.Action.Before.Tags == nil {
		t.Fatalf("the undo record leaves the tags alone instead of restoring them: %+v", undo.Action.Before)
	}
	if len(*undo.Action.Before.Tags) != 0 {
		t.Fatalf("the undo record restores %v, and the task had no tags", *undo.Action.Before.Tags)
	}
	if _, err := s.UpdateTask(ctx, "t1", *undo.Action.Before); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Tags) != 0 {
		t.Errorf("undo left the tag on: %v", stored.Tags)
	}
}

func TestUndoOfAChecklistOnATaskWithoutOneTakesItOff(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	server := openTask("t1", "p1", "Забрать посылку")
	server.Items = []model.Item{}
	if _, err := s.SyncProject(ctx, "p1", fromServer(server)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskItem(ctx, "t1", "паспорт"); err != nil {
		t.Fatal(err)
	}

	undo, err := s.PopUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Before == nil || undo.Action.Before.Items == nil {
		t.Fatalf("the undo record leaves the checklist alone instead of restoring it: %+v", undo.Action.Before)
	}
	if _, err := s.UpdateTask(ctx, "t1", *undo.Action.Before); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 0 {
		t.Errorf("undo left the checklist on: %+v", stored.Items)
	}
	var items int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM items WHERE task_id = 't1'`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Errorf("%d checklist rows left behind", items)
	}
}

func TestClearingTagsSurvivesTheOutbox(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Tags = []string{"дом", "почта"}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}

	cleared, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Tags: model.NewEditList[string](nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Tags) != 0 {
		t.Fatalf("the tags were not cleared: %v", cleared.Tags)
	}

	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskUpdate {
		t.Fatalf("queued %+v", items)
	}
	if !strings.Contains(string(items[0].Payload), `"tags":[]`) {
		t.Errorf("the queued payload does not clear the tags: %s", items[0].Payload)
	}
	var edit model.TaskEdit
	if err := json.Unmarshal(items[0].Payload, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Tags == nil {
		t.Fatalf("the syncer reads the payload as leaving the tags alone: %s", items[0].Payload)
	}
	if len(*edit.Tags) != 0 {
		t.Errorf("the payload sets the tags to %v", *edit.Tags)
	}
}

func TestCompleteTaskClosesItems(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Items = []model.Item{
		{Id: "i1", Title: "паспорт"},
		{Id: "i2", Title: "трек-номер", Status: model.ItemDone},
	}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}

	got, err := s.CompleteTask(ctx, "t1", CompleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Status.Done() || got.CompletedTime.IsZero() {
		t.Fatalf("task %+v", got)
	}
	stored, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range stored.Items {
		if !it.Status.Done() {
			t.Errorf("item %s left open", it.Id)
		}

		if it.Id == "i1" && it.CompletedTime.IsZero() {
			t.Errorf("item %s completed without a time", it.Id)
		}
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskComplete {
		t.Fatalf("queued %+v", items)
	}
	var edit model.TaskEdit
	if err := json.Unmarshal(items[0].Payload, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Status == nil || !edit.Status.Done() || edit.CompletedTime == nil {
		t.Fatalf("payload %+v", edit)
	}
	if edit.Items == nil || len(*edit.Items) != 2 {
		t.Fatalf("payload carries no items: %+v", edit.Items)
	}
	for _, it := range *edit.Items {
		if !it.Status.Done() {
			t.Errorf("payload item %s open", it.Id)
		}
	}
}

func TestCompleteTaskKeepingItems(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Items = []model.Item{{Id: "i1", Title: "паспорт"}}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompleteTask(ctx, "t1", CompleteOptions{KeepItems: true}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Status.Done() {
		t.Fatal("task not completed")
	}
	if len(stored.Items) != 1 || stored.Items[0].Status.Done() {
		t.Fatalf("items %+v, want the checklist left open", stored.Items)
	}
	items := claimAll(t, s)
	var edit model.TaskEdit
	if err := json.Unmarshal(items[0].Payload, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Items != nil {
		t.Errorf("payload touches the items: %+v", edit.Items)
	}
}

func TestReopenTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTask(ctx, "t1", CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReopenTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Done() || !got.CompletedTime.IsZero() {
		t.Fatalf("got %+v", got)
	}
	open, err := s.Tasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open tasks %v", taskTitles(open))
	}

	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskComplete {
		t.Fatalf("queued %+v", items)
	}
	if err := s.MarkDone(ctx, items[0].Seq, items[0].LeaseToken); err != nil {
		t.Fatal(err)
	}
	if items = claimAll(t, s); len(items) != 1 || items[0].Op != OpTaskUpdate {
		t.Fatalf("queued behind the completion %+v", items)
	}
}

func TestMoveTaskRecreatesTheTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Priority = model.PriorityHigh
	task.Tags = []string{"почта"}
	task.Items = []model.Item{{Id: "i1", Title: "паспорт"}}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}

	got, err := s.MoveTask(ctx, "t1", "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectId != "p2" || !IsLocalID(got.Id) {
		t.Fatalf("moved task %+v, want a local id in p2", got)
	}
	if got.Priority != model.PriorityHigh || len(got.Tags) != 1 || got.Tags[0] != "почта" {
		t.Errorf("the copy lost fields: %+v", got)
	}

	if len(got.Items) != 1 || got.Items[0].Title != "паспорт" || got.Items[0].Id != "" {
		t.Errorf("copied items %+v", got.Items)
	}
	if _, err := s.Task(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the original is still cached: %v", err)
	}
	if found, _ := s.Tasks(ctx, TaskFilter{Search: "работа"}); len(found) != 1 {
		t.Errorf("search by the new project name found %v", taskTitles(found))
	}

	items := claimAll(t, s)
	if len(items) != 2 {
		t.Fatalf("queued %+v, want a create and the delete behind it", items)
	}
	if items[0].Op != OpTaskCreate || items[0].TaskID != got.Id || items[0].ProjectID != "p2" {
		t.Errorf("first entry %+v, want the create of the copy in p2", items[0])
	}
	if items[1].Op != OpTaskMoveDrop || items[1].TaskID != "t1" || items[1].ProjectID != "p1" {
		t.Errorf("second entry %+v, want the delete of the original in p1", items[1])
	}
	var drop MoveDrop
	if err := json.Unmarshal(items[1].Payload, &drop); err != nil {
		t.Fatal(err)
	}
	if drop.CopyID != got.Id {
		t.Errorf("the delete names copy %q, want %q", drop.CopyID, got.Id)
	}

	if drop.CopyProjectID != "p2" {
		t.Errorf("the delete looks for the copy in %q, want p2", drop.CopyProjectID)
	}

	undo, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Op != OpTaskMoveRecreate || undo.Action.TaskID != got.Id {
		t.Fatalf("undo %+v", undo.Action)
	}
	if undo.Action.Before != nil || undo.Action.Task != nil {
		t.Errorf("undo %+v, want nothing to reverse the move with", undo.Action)
	}
}

func TestMoveTaskJournalsBothIDs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveTask(ctx, "t1", "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.EventsSince(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var moved []Event
	for _, e := range events {
		if e.Kind == TaskMovedKind {
			moved = append(moved, e)
		}
	}
	if len(moved) != 1 {
		t.Fatalf("journal %+v, want one move", events)
	}
	var ev taskEvent
	if err := json.Unmarshal(moved[0].Payload, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ID != got.Id || ev.FromID != "t1" || ev.ProjectID != "p2" {
		t.Errorf("journal entry %+v", ev)
	}
}

func TestMoveTaskRefusesToRecreateUnasked(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTask(ctx, "t1", "p2", MoveOptions{}); !errors.Is(err, ErrMoveNeedsRecreate) {
		t.Fatalf("err = %v, want ErrMoveNeedsRecreate", err)
	}
	cur, err := s.Task(ctx, "t1")
	if err != nil || cur.ProjectId != "p1" {
		t.Fatalf("task %+v, %v; want it untouched in p1", cur, err)
	}
	if items := claimAll(t, s); len(items) != 0 {
		t.Fatalf("queued %+v, want nothing", items)
	}
}

func TestMoveTaskRefusesACompletedTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	done := openTask("t1", "p1", "Забрать посылку")
	done.Status = model.TaskDone
	if _, err := s.SyncProject(ctx, "p1", fromServer(done)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.MoveTask(ctx, "t1", "p2", MoveOptions{ByRecreate: true}); !errors.Is(err, ErrMoveCompleted) {
		t.Fatalf("err = %v, want ErrMoveCompleted", err)
	}
	cur, err := s.Task(ctx, "t1")
	if err != nil || cur.ProjectId != "p1" {
		t.Fatalf("task %+v, %v; want it untouched in p1", cur, err)
	}
	if items := claimAll(t, s); len(items) != 0 {
		t.Fatalf("queued %+v, want nothing", items)
	}
	if _, err := s.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Errorf("undo = %v, want nothing recorded for a move that was not made", err)
	}
}

func TestMoveTaskRefusesToMoveTheCopyOfAnUnfinishedMove(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"},
		model.Project{Id: "p3", Name: "Учёба"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	first, err := s.MoveTask(ctx, "t1", "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatalf("first move: %v", err)
	}

	if _, err := s.MoveTask(ctx, first.Id, "p3", MoveOptions{ByRecreate: true}); !errors.Is(err, ErrMoveChained) {
		t.Fatalf("err = %v, want ErrMoveChained", err)
	}

	cur, err := s.Task(ctx, first.Id)
	if err != nil || cur.ProjectId != "p2" {
		t.Fatalf("task %+v, %v; want the copy untouched in p2", cur, err)
	}
	items := claimAll(t, s)
	if len(items) != 2 {
		t.Fatalf("queued %+v, want the create and the delete of the first move and nothing else", items)
	}
	if items[0].Op != OpTaskCreate || items[1].Op != OpTaskMoveDrop {
		t.Errorf("queued %s and %s, want the create and the delete of the first move", items[0].Op, items[1].Op)
	}
}

func TestMoveTaskToTheSameProjectDoesNothing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}

	got, err := s.MoveTask(ctx, "t1", "p1", MoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Id != "t1" {
		t.Errorf("id = %q, want the task untouched", got.Id)
	}
	if items := claimAll(t, s); len(items) != 0 {
		t.Fatalf("queued %+v, want nothing", items)
	}
}

func TestMoveOfflineTaskDropsItsQueuedCreate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveTask(ctx, created.Id, "p2", MoveOptions{ByRecreate: true})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskCreate || items[0].TaskID != got.Id {
		t.Fatalf("queued %+v, want only the create of the copy", items)
	}
	if items[0].ProjectID != "p2" {
		t.Errorf("the create goes to %q, want p2", items[0].ProjectID)
	}
	if _, err := s.Task(ctx, created.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the original row is still there: %v", err)
	}
}

func TestDeleteTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Items = []model.Item{{Id: "i1", Title: "паспорт"}}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task still there: %v", err)
	}
	var items int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM items WHERE task_id = 't1'`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Errorf("%d checklist rows left behind", items)
	}
	queued := claimAll(t, s)
	if len(queued) != 1 || queued[0].Op != OpTaskDelete || queued[0].TaskID != "t1" {
		t.Fatalf("queued %+v", queued)
	}

	undo, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Task == nil || undo.Action.Task.Title != "Забрать посылку" ||
		len(undo.Action.Task.Items) != 1 {
		t.Fatalf("undo %+v", undo.Action.Task)
	}
}

func TestDeleteOfflineTaskDropsItsQueuedCreate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{}) {
		t.Fatalf("queue %+v, want nothing left to send", counts)
	}
}

func TestDeleteTaskDropsAParkedQueueEntry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 {
		t.Fatalf("claimed %d", len(items))
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "500 server error"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Pending: 1}) {
		t.Fatalf("queue %+v, want the parked create gone and a delete in its place", counts)
	}
	var op, state string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT op, state FROM outbox WHERE task_id = ?`, created.Id).Scan(&op, &state); err != nil {
		t.Fatal(err)
	}
	if op != OpTaskDelete || state != string(OutboxPending) {
		t.Fatalf("queued %s in state %s, want a pending %s", op, state, OpTaskDelete)
	}
}

func TestDeleteAfterRetryFailedKeepsTheRecordOfTheDoubt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 {
		t.Fatalf("claimed %d", len(items))
	}

	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RetryFailed(ctx); err != nil || n != 1 {
		t.Fatalf("retry failed: %v %d", err, n)
	}
	var state string
	var attempts int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT state, attempts FROM outbox WHERE task_id = ?`, created.Id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != string(OutboxPending) || attempts != 1 {
		t.Fatalf("the raised create is %s with %d attempt(s), want a pending entry that has been sent once", state, attempts)
	}

	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Pending: 1}) {
		t.Fatalf("queue %+v, want the raised create gone and a delete in its place", counts)
	}
	var op string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT op, state FROM outbox WHERE task_id = ?`, created.Id).Scan(&op, &state); err != nil {
		t.Fatal(err)
	}
	if op != OpTaskDelete || state != string(OutboxPending) {
		t.Fatalf("queued %s in state %s, want a pending %s", op, state, OpTaskDelete)
	}
}

func TestCreateTaskMintsAnIDTheAnswerCanBeWrittenTo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	undo, err := s.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Action.Task == nil {
		t.Fatalf("undo %+v carries no task to put back", undo.Action)
	}

	restored, err := s.CreateTask(ctx, *undo.Action.Task)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocalID(restored.Id) {
		t.Fatalf("the restored task was queued for creation under %q, an id the server assigned", restored.Id)
	}
	if _, err := s.Task(ctx, restored.Id); err != nil {
		t.Fatal(err)
	}

	items, _, err := s.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var create OutboxItem
	for _, it := range items {
		if it.Op == OpTaskCreate {
			create = it
		}
	}
	if create.Seq == 0 || create.TaskID != restored.Id {
		t.Fatalf("queued %+v, want a create of %s", items, restored.Id)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, create.Seq, create.LeaseToken); err != nil {
			return err
		}
		if _, err := MarkPushedTx(ctx, tx, create.TaskID, create.Rev); err != nil {
			return err
		}
		return ReplaceLocalIDTx(ctx, tx, create.TaskID, "t9")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t9", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	got, err := s.Tasks(ctx, TaskFilter{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Id != "t9" {
		t.Fatalf("cached %+v, want the restored task under the id the server answered with", got)
	}
}

func TestPopUndoJoinsATransaction(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := PopUndoTx(ctx, tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := s.LastUndo(ctx); err != nil {
		t.Fatalf("the record went with the rollback: %v", err)
	}

	var popped UndoEntry
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		popped, err = PopUndoTx(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if popped.Action.Op != OpTaskDelete || popped.Action.Task == nil {
		t.Fatalf("popped %+v", popped.Action)
	}
	if _, err := s.LastUndo(ctx); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("the stack still has %v", err)
	}
}

func TestDeleteOfflineTaskInFlight(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Забрать посылку"})
	if err != nil {
		t.Fatal(err)
	}
	if items := claimAll(t, s); len(items) != 1 {
		t.Fatalf("claimed %d", len(items))
	}
	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 || counts.Inflight != 1 {
		t.Fatalf("queue %+v, want the delete queued behind the create", counts)
	}
	var rev sql.NullInt64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT rev FROM outbox WHERE task_id = ? AND state = ?`,
		created.Id, string(OutboxInflight)).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev.Valid {
		t.Errorf("the create still in flight kept revision %d of a row that is gone", rev.Int64)
	}
}

func TestDeleteTaskTakesBackWhatIsStillInFlight(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}

	inflight, _, err := s.Claim(ctx, 1, time.Minute)
	if err != nil || len(inflight) != 1 || inflight[0].Rev != 1 {
		t.Fatalf("claim the edit: %v %+v", err, inflight)
	}

	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE seq = ?`, inflight[0].Seq).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Error("the edit of a task that is gone is still queued, revision and all")
	}

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку до пятницы")}); err != nil {
		t.Fatal(err)
	}

	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, inflight[0].Seq, inflight[0].LeaseToken); err != nil {
			return err
		}
		_, err := MarkPushedTx(ctx, tx, inflight[0].TaskID, inflight[0].Rev)
		return err
	})

	if !errors.Is(err, ErrNoOutboxRow) {
		t.Errorf("the worker's commit = %v, want %v", err, ErrNoOutboxRow)
	}

	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = ?`, "t1").Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	switch {
	case dirty == 0:
		t.Error("a revision of the deleted row cleared the mark of the one the pull brought back")
	case dirty != inflight[0].Rev:
		t.Errorf("the row is at revision %d while the stale entry carries %d: the two no longer collide, "+
			"and the test has stopped being about anything", dirty, inflight[0].Rev)
	}

	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку до пятницы" {
		t.Errorf("the pull overwrote an unpushed edit: %q", got.Title)
	}
}

func TestDeleteOfAnOfflineTaskLeavesNoOrphan(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 {
		t.Fatalf("claimed %+v", items)
	}
	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, created.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the task is still cached: %v", err)
	}
	orphans, err := s.OrphanedLocalTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("left behind %+v", orphans)
	}

	var op, state string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT op, state FROM outbox WHERE task_id = ?`, created.Id).Scan(&op, &state); err != nil {
		t.Fatal(err)
	}
	if op != OpTaskDelete || state != string(OutboxPending) {
		t.Fatalf("queued %s in state %s", op, state)
	}
}

func TestDroppingAParkedCreateKeepsTheDeleteQueuedBehindIt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Zabrat posylku"})
	if err != nil {
		t.Fatal(err)
	}
	inflight := claimAll(t, s)
	if len(inflight) != 1 {
		t.Fatalf("claimed %+v", inflight)
	}
	if err := s.DeleteTask(ctx, created.Id); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, inflight[0].Seq, inflight[0].LeaseToken, "connection reset"); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.DropParked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Op != OpTaskCreate {
		t.Fatalf("dropped %+v, want the parked create", dropped)
	}
	if dropped[0].TaskRemoved {
		t.Error("the line says a task was removed; there was none left to remove")
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Pending: 1}) {
		t.Fatalf("queue %+v, want the delete that records the doubt still there", counts)
	}
}
