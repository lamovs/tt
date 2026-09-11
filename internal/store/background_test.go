package store

import (
	"context"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestDueReminderOccurrencesAndClaims(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Work"})
	now := time.Date(2026, 9, 11, 12, 0, 20, 0, time.UTC)
	due := model.NewTime(time.Date(2026, 9, 11, 12, 10, 0, 0, time.UTC))
	clean := model.Task{Id: "server", ProjectId: "p1", Title: "Clean", Kind: "TEXT", DueDate: due, Reminders: []string{"TRIGGER:-PT10M"}}
	if _, err := st.SyncProject(ctx, "p1", []ServerTask{{Task: clean}}); err != nil {
		t.Fatal(err)
	}
	local, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Offline", Kind: "TEXT", DueDate: due, Reminders: []string{"TRIGGER:-PT10M", "TRIGGER:vendor"}})
	if err != nil {
		t.Fatal(err)
	}
	occurrences, unsupported, err := st.DueReminderOccurrences(ctx, now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(occurrences) != 2 || unsupported != 1 {
		t.Fatalf("occurrences=%+v unsupported=%d", occurrences, unsupported)
	}
	found := map[string]ReminderOccurrence{}
	for _, occurrence := range occurrences {
		found[occurrence.TaskID] = occurrence
	}
	if !found["server"].ServerConfirmed || found[local.Id].ServerConfirmed {
		t.Fatalf("confirmation decisions: %+v", found)
	}
	claimed, err := st.ClaimReminderOccurrence(ctx, found[local.Id], "local", now)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	claimed, err = st.ClaimReminderOccurrence(ctx, found[local.Id], "local", now)
	if err != nil || claimed {
		t.Fatalf("second claim=%v err=%v", claimed, err)
	}
}

func TestReminderClaimFollowsLocalIDReplacement(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Work"})
	due := model.NewTime(time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC))
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Offline", Kind: "TEXT", DueDate: due, Reminders: []string{"TRIGGER:PT0S"}})
	if err != nil {
		t.Fatal(err)
	}
	occurrence := ReminderOccurrence{TaskID: task.Id, Due: due, Trigger: "TRIGGER:PT0S"}
	if claimed, err := st.ClaimReminderOccurrence(ctx, occurrence, "local", due.Time); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := st.ReplaceLocalID(ctx, task.Id, "server-id"); err != nil {
		t.Fatal(err)
	}
	occurrence.TaskID = "server-id"
	if claimed, err := st.ClaimReminderOccurrence(ctx, occurrence, "provider", due.Time); err != nil || claimed {
		t.Fatalf("claim after id replacement=%v err=%v", claimed, err)
	}
}

func TestAutomaticTaskWorkIgnoresParkedEntries(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.Enqueue(ctx, OutboxEntry{Target: TargetOpenAPI, Op: "task.update", TaskID: "t"}); err != nil {
		t.Fatal(err)
	}
	pending, _, err := st.AutomaticTaskWork(ctx)
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE outbox SET state='failed'`); err != nil {
		t.Fatal(err)
	}
	pending, _, err = st.AutomaticTaskWork(ctx)
	if err != nil || pending {
		t.Fatalf("parked pending=%v err=%v", pending, err)
	}
}
