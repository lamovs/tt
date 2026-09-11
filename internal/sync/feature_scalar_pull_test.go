package sync

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestRegisteredScalarPullReopensNextRecurringOccurrence(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: stage1CProjectID, Name: "Recurrence"})
	fake := newStage1CWireFake(t)
	fake.SeedProject(stage1CProjectID, "Recurrence")
	fake.SeedProject("inbox", "Inbox")
	sy := testSyncer(t, st, fake.Server())
	due := model.NewTime(time.Date(2026, 9, 9, 12, 10, 0, 0, time.UTC))
	if _, err := st.CreateTask(ctx, model.Task{
		ProjectId: stage1CProjectID, Title: "repeat without checklist", DueDate: due,
		RepeatFlag: "RRULE:FREQ=DAILY;INTERVAL=1",
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := sy.Push(ctx); err != nil || result.Pushed != 1 {
		t.Fatalf("create push = %+v, %v", result, err)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("created tasks = %+v, %v", tasks, err)
	}
	taskID := tasks[0].Id
	if store.IsLocalID(taskID) || stage1CControl(t, st, taskID) != string(store.RecoveryIdle) {
		t.Fatalf("schedule create has no settled address/control: %s", taskID)
	}
	if _, err := st.CompleteTask(ctx, taskID, store.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if result, err := sy.Push(ctx); err != nil || result.Pushed != 1 {
		t.Fatalf("completion push = %+v, %v", result, err)
	}
	if completed := stage1CTask(t, st, taskID); !completed.Status.Done() || !completed.DueDate.Equal(due.Time) {
		t.Fatalf("local completed occurrence = %+v", completed)
	}
	nextDue := "2026-09-10T12:10:00.000+0000"
	fake.MutateTask(stage1CProjectID, taskID, func(task map[string]any) {
		task["kind"] = "TEXT"
		task["status"] = 0
		task["dueDate"] = nextDue
		task["startDate"] = nextDue
		task["timeZone"] = "Europe/Moscow"
		task["modifiedTime"] = "2026-09-09T11:55:14.535+0000"
		delete(task, "completedTime")
		delete(task, "items")
	})
	requestsBefore := len(fake.Requests())
	for pass := 0; pass < 2; pass++ {
		if result, err := sy.Pull(ctx); err != nil || result.Pulled != 1 || result.Skipped != 0 || len(result.Errors) != 0 {
			t.Fatalf("recurrence pull %d = %+v, %v", pass, result, err)
		}
		got := stage1CTask(t, st, taskID)
		if got.Status != model.TaskOpen || got.DueDate.String() != nextDue || got.StartDate.String() != nextDue ||
			!got.CompletedTime.IsZero() || got.Kind != "TEXT" || len(got.Items) != 0 || got.RepeatFlag != "RRULE:FREQ=DAILY;INTERVAL=1" {
			t.Fatalf("next occurrence after pull %d = %+v", pass, got)
		}
		open, err := st.Tasks(ctx, store.TaskFilter{Search: "repeat without checklist"})
		if err != nil || len(open) != 1 || open[0].Id != taskID {
			t.Fatalf("open search after pull %d = %+v, %v", pass, open, err)
		}
	}
	if stage1COutboxCount(t, st) != 0 || len(stage1CRegistry(t, st, taskID)) != 1 {
		t.Fatal("recurrence pull changed the outbox or adopted item identities")
	}
	for _, request := range fake.Requests()[requestsBefore:] {
		if request.Method != http.MethodGet {
			t.Fatalf("recurrence pull sent another mutation: %+v", request)
		}
	}

	fake.MutateTask(stage1CProjectID, taskID, func(task map[string]any) { task["title"] = "older response" })
	release, pending := stage1CHolderHoldPull(t, sy, fake, http.MethodGet, stage1CProjectDataPath)
	fake.MutateTask(stage1CProjectID, taskID, func(task map[string]any) { task["title"] = "newer response" })
	if _, err := sy.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	before := stage1CHolderProtectedState(t, st)
	stage1CHolderFinishPull(t, release, pending)
	if after := stage1CHolderProtectedState(t, st); after != before {
		t.Fatalf("stale scalar response changed newer state\nbefore=%s\nafter=%s", before, after)
	}
}
