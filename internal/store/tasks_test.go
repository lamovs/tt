package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func seedProjects(t *testing.T, s *Store, ps ...model.Project) {
	t.Helper()
	if err := s.ReplaceProjects(context.Background(), ps); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
}

func openTask(id, projectID, title string) model.Task {
	return model.Task{Id: id, ProjectId: projectID, Title: title, Status: model.TaskOpen}
}

func fromServer(tasks ...model.Task) []ServerTask {
	out := make([]ServerTask, 0, len(tasks))
	for _, t := range tasks {
		body := map[string]any{"id": t.Id, "title": t.Title}
		if t.Items != nil {
			items := make([]map[string]any, len(t.Items))
			for i, item := range t.Items {
				items[i] = map[string]any{
					"id": item.Id, "title": item.Title, "status": item.Status.Wire(),
					"sortOrder": item.SortOrder, "startDate": item.StartDate.String(),
					"isAllDay": item.IsAllDay, "timeZone": item.TimeZone,
					"completedTime": item.CompletedTime.String(),
				}
			}
			body["items"] = items
		}
		raw, _ := json.Marshal(body)
		out = append(out, ServerTask{Task: t, Raw: raw})
	}
	return out
}

func mustTime(t *testing.T, s string) model.Time {
	t.Helper()
	v, err := model.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func taskTitles(tasks []model.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.Title)
	}
	return out
}

func TestSyncProjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	full := openTask("t1", "p1", "Забрать посылку")
	full.Content = "отделение на Ленина"
	full.Priority = model.PriorityHigh
	full.DueDate = mustTime(t, "2026-09-10T09:00:00.000+0000")
	full.Tags = []string{"дом"}
	full.Reminders = []string{"TRIGGER:PT0S"}
	full.TimeZone = "Europe/Moscow"
	full.Items = []model.Item{{Id: "i1", Title: "паспорт", Status: model.ItemOpen}}

	res, err := s.SyncProject(ctx, "p1", fromServer(full))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 || res.Deleted != 0 || res.Skipped != 0 {
		t.Fatalf("result %+v", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != full.Title || got.Content != full.Content || got.Priority != model.PriorityHigh {
		t.Errorf("got %+v", got)
	}
	if !got.DueDate.Equal(full.DueDate.Time) || got.TimeZone != "Europe/Moscow" {
		t.Errorf("dates: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "дом" || len(got.Reminders) != 1 {
		t.Errorf("tags %v reminders %v", got.Tags, got.Reminders)
	}
	if len(got.Items) != 1 || got.Items[0].Title != "паспорт" {
		t.Fatalf("items %+v", got.Items)
	}

	var raw string
	if err := s.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = 't1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "{}" {
		t.Error("raw payload not stored")
	}
}

func TestChecklistItemFieldsSurviveTheCache(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	task := openTask("t1", "p1", "Забрать посылку")
	task.Items = []model.Item{{
		Id:            "i1",
		Title:         "паспорт",
		Status:        model.ItemDone,
		SortOrder:     3,
		StartDate:     mustTime(t, "2026-09-10T09:00:00.000+0000"),
		IsAllDay:      true,
		TimeZone:      "Europe/Moscow",
		CompletedTime: mustTime(t, "2026-09-11T09:00:00.000+0000"),
	}}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items %+v", got.Items)
	}
	it := got.Items[0]
	if !it.IsAllDay || it.TimeZone != "Europe/Moscow" {
		t.Errorf("is_all_day %v time_zone %q", it.IsAllDay, it.TimeZone)
	}
	if !it.StartDate.Equal(task.Items[0].StartDate.Time) || !it.CompletedTime.Equal(task.Items[0].CompletedTime.Time) {
		t.Errorf("dates: %+v", it)
	}
	if it.SortOrder != 3 || !it.Status.Done() || it.Title != "паспорт" {
		t.Errorf("got %+v", it)
	}

	created, err := s.CreateTask(ctx, model.Task{
		ProjectId: "p1",
		Title:     "Собрать документы",
		Items:     []model.Item{{Id: "i1", Title: "снилс", IsAllDay: true, TimeZone: "Europe/Moscow"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	offline, err := s.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(offline.Items) != 1 || !offline.Items[0].IsAllDay || offline.Items[0].TimeZone != "Europe/Moscow" {
		t.Fatalf("offline item %+v", offline.Items)
	}
}

func TestChecklistItemsWithoutServerIDs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	good := openTask("t1", "p1", "Забрать посылку")
	unnumbered := openTask("t2", "p1", "Купить продукты")
	unnumbered.Items = []model.Item{{Title: "молоко"}, {Title: "хлеб"}}

	repeated := openTask("t3", "p1", "Собрать документы")
	repeated.Items = []model.Item{{Id: "i1", Title: "паспорт"}, {Id: "i1", Title: "снилс"}}

	res, err := s.SyncProject(ctx, "p1", fromServer(good, unnumbered, repeated))
	if err != nil {
		t.Fatalf("one task's checklist took the whole project down: %v", err)
	}
	if res.Upserted != 3 {
		t.Fatalf("sync %+v, want every task of the answer cached", res)
	}
	if _, err := s.Task(ctx, "t1"); err != nil {
		t.Fatalf("the well-formed task went with it: %v", err)
	}

	got, err := s.Task(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Title != "молоко" || got.Items[1].Title != "хлеб" {
		t.Fatalf("checklist %+v, want both items in the order they arrived", got.Items)
	}
	for _, it := range got.Items {
		if it.Id != "" {
			t.Errorf("item %q reads back under id %q, which the server never assigned", it.Title, it.Id)
		}
	}
	dup, err := s.Task(ctx, "t3")
	if err != nil {
		t.Fatal(err)
	}
	if len(dup.Items) != 2 || dup.Items[0].Id != "i1" || dup.Items[1].Id != "" {
		t.Fatalf("checklist %+v, want the first item to keep the id and the second to lose it", dup.Items)
	}

	created, err := s.CreateTask(ctx, model.Task{
		ProjectId: "p1",
		Title:     "Записаться к врачу",
		Items:     []model.Item{{Title: "полис"}, {Title: "снилс"}},
	})
	if err != nil {
		t.Fatalf("a checklist written offline: %v", err)
	}
	offline, err := s.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(offline.Items) != 2 || offline.Items[0].Title != "полис" || offline.Items[1].Title != "снилс" {
		t.Fatalf("offline checklist %+v", offline.Items)
	}
	for _, it := range offline.Items {
		if it.Id != "" {
			t.Errorf("offline item %q reads back under id %q", it.Title, it.Id)
		}
	}
}

func TestSyncProjectDeletesOnlyCleanOpenTasks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	_, err := s.SyncProject(ctx, "p1", fromServer(
		openTask("t1", "p1", "Забрать посылку"),
		openTask("t2", "p1", "Позвонить в банк"),
		openTask("t3", "p1", "Купить хлеб"),
		openTask("t4", "p1", "Записаться к врачу"),
	))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CompleteTask(ctx, "t2", CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET dirty = 0 WHERE id = 't2'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t4", model.TaskEdit{Title: model.Ptr("Записаться к стоматологу")}); err != nil {
		t.Fatal(err)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Kept != 2 || res.Upserted != 1 {
		t.Fatalf("result %+v, want one delete and two kept", res)
	}
	for _, id := range []string{"t1", "t2", "t4"} {
		if _, err := s.Task(ctx, id); err != nil {
			t.Errorf("task %s gone: %v", id, err)
		}
	}
	if _, err := s.Task(ctx, "t3"); err == nil {
		t.Error("t3 was open, clean and absent from the response: it should be gone")
	}
}

func TestSyncProjectKeepsUnpushedEdits(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	server := openTask("t1", "p1", "Забрать посылку")
	if _, err := s.SyncProject(ctx, "p1", fromServer(server)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{
		Title:    model.Ptr("Забрать посылку на почте"),
		Priority: model.Ptr(model.PriorityHigh),
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(server))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Upserted != 0 {
		t.Fatalf("result %+v, want the row skipped", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку на почте" || got.Priority != model.PriorityHigh {
		t.Fatalf("local edit lost: %+v", got)
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = 't1'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 1 {
		t.Errorf("dirty %d, want the revision the edit produced", dirty)
	}
}

func TestDirtyMarkClearsOnceThePushIsCommitted(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Rev != 1 {
		t.Fatalf("queued %+v, want the revision the edit produced", items)
	}

	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, items[0].Seq, items[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "t1", items[0].Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the dirty mark was not cleared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку на Ленина")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 || res.Skipped != 0 {
		t.Fatalf("result %+v, want the row updated", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Забрать посылку на Ленина" {
		t.Fatalf("the cache ignored the server: %+v", got)
	}
}

func TestDirtyMarkSurvivesAnEditMadeMidPush(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("Забрать посылку на почте")}); err != nil {
		t.Fatal(err)
	}
	sent := claimAll(t, s)
	if len(sent) != 1 {
		t.Fatalf("queued %+v", sent)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)}); err != nil {
		t.Fatal(err)
	}

	var cleared bool
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, sent[0].Seq, sent[0].LeaseToken); err != nil {
			return err
		}
		var err error
		cleared, err = MarkPushedTx(ctx, tx, "t1", sent[0].Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cleared {
		t.Fatal("the mark was cleared although the row had moved on")
	}
	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку на почте")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result %+v, want the second edit kept", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != model.PriorityHigh {
		t.Fatalf("the second edit was lost: %+v", got)
	}

	second := claimAll(t, s)
	if len(second) != 1 || second[0].Rev != 2 {
		t.Fatalf("queued %+v, want the second revision", second)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, second[0].Seq, second[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "t1", second[0].Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the dirty mark was not cleared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if res, err = s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку на Ленина"))); err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("result %+v, want the row updated", res)
	}
}

func TestMarkPushedWithoutARevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Op != OpTaskDelete || items[0].Rev != 0 {
		t.Fatalf("queued %+v", items)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		ok, err := MarkPushedTx(ctx, tx, items[0].TaskID, items[0].Rev)
		if err != nil {
			return err
		}
		if ok {
			return errors.New("cleared a mark that does not exist")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParkingReleasesTheRowToTheNextPull(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("parcel at the post office")}); err != nil {
		t.Fatal(err)
	}
	items := claimAll(t, s)
	if len(items) != 1 || items[0].Rev != 1 {
		t.Fatalf("queued %+v, want the revision the edit produced", items)
	}

	if err := s.MarkFailed(ctx, items[0].Seq, items[0].LeaseToken, "422 unprocessable"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (OutboxCounts{Failed: 1}) {
		t.Fatalf("queue %+v, want the entry parked", counts)
	}
	var dirty int64
	if err := s.DB().QueryRowContext(ctx, `SELECT dirty FROM tasks WHERE id = ?`, "t1").Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 0 {
		t.Errorf("the row is still at revision %d with nothing left to send it", dirty)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel on Lenina")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 || res.Skipped != 0 {
		t.Fatalf("result %+v, want the row updated", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "parcel on Lenina" {
		t.Fatalf("the pull went on skipping the parked row: %+v", got)
	}
}

func TestParkingLeavesTheMarkOfALaterEdit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Title: model.Ptr("parcel at the post office")}); err != nil {
		t.Fatal(err)
	}
	sent := claimAll(t, s)
	if len(sent) != 1 || sent[0].Rev != 1 {
		t.Fatalf("queued %+v", sent)
	}

	if _, err := s.UpdateTask(ctx, "t1", model.TaskEdit{Priority: model.Ptr(model.PriorityHigh)}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, sent[0].Seq, sent[0].LeaseToken, "422 unprocessable"); err != nil {
		t.Fatal(err)
	}

	res, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel on Lenina")))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 {
		t.Fatalf("result %+v, want the second edit kept", res)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != model.PriorityHigh {
		t.Fatalf("the pull wrote over an edit that is still queued: %+v", got)
	}

	second := claimAll(t, s)
	if len(second) != 1 || second[0].Rev != 2 {
		t.Fatalf("queued %+v, want the second revision", second)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := MarkDoneTx(ctx, tx, second[0].Seq, second[0].LeaseToken); err != nil {
			return err
		}
		ok, err := MarkPushedTx(ctx, tx, "t1", second[0].Rev)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("the dirty mark was not cleared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if res, err = s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "parcel on Lenina"))); err != nil {
		t.Fatal(err)
	}
	if res.Upserted != 1 {
		t.Fatalf("result %+v, want the row updated", res)
	}
}

func TestSearchIgnoresCaseInRussian(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})
	first := openTask("t1", "p1", "Забрать посылку")
	first.Content = "отделение на Ленина"
	first.Tags = []string{"Дом"}
	if _, err := s.SyncProject(ctx, "p1", fromServer(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncProject(ctx, "p2", fromServer(openTask("t2", "p2", "Отчёт за квартал"))); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"забрать", "ЗАБРАТЬ", "ЗаБрАтЬ", "посылк", "ленина", "дом", "личное"} {
		got, err := s.Tasks(ctx, TaskFilter{Search: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != 1 || got[0].Id != "t1" {
			t.Errorf("search %q found %v", q, taskTitles(got))
		}
	}
	if got, _ := s.Tasks(ctx, TaskFilter{Search: "ОТЧЁТ"}); len(got) != 1 || got[0].Id != "t2" {
		t.Errorf("search by another project's task found %v", taskTitles(got))
	}
	if got, _ := s.Tasks(ctx, TaskFilter{Search: "молоко"}); len(got) != 0 {
		t.Errorf("search for a missing word found %v", taskTitles(got))
	}

	var n int
	err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE lower(title) LIKE '%забрать%'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("lower() folded Cyrillic after all (%d rows)", n)
	}
}

func TestSearchFollowsProjectRename(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}
	seedProjects(t, s, model.Project{Id: "p1", Name: "Домашние Дела"})

	if got, _ := s.Tasks(ctx, TaskFilter{Search: "домашние"}); len(got) != 1 {
		t.Errorf("search by the new project name found %v", taskTitles(got))
	}
	if got, _ := s.Tasks(ctx, TaskFilter{Search: "личное"}); len(got) != 0 {
		t.Errorf("search by the old project name still finds %v", taskTitles(got))
	}
}

func TestTaskFilter(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"}, model.Project{Id: "p2", Name: "Работа"})

	overdue := openTask("t1", "p1", "Просроченная")
	overdue.DueDate = mustTime(t, "2026-09-01T09:00:00.000+0000")
	overdue.Priority = model.PriorityLow
	today := openTask("t2", "p1", "Сегодняшняя")
	today.DueDate = mustTime(t, "2026-09-03T09:00:00.000+0000")
	today.Priority = model.PriorityHigh
	someday := openTask("t3", "p1", "Без срока")
	other := openTask("t4", "p2", "Чужая")
	done := openTask("t5", "p1", "Сделанная")
	done.Status = model.TaskDone
	done.CompletedTime = mustTime(t, "2026-09-02T09:00:00.000+0000")

	if _, err := s.SyncProject(ctx, "p1", fromServer(overdue, today, someday, done)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncProject(ctx, "p2", fromServer(other)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		filter TaskFilter
		want   []string
	}{
		{"open by default", TaskFilter{ProjectID: "p1"}, []string{"t1", "t2", "t3"}},
		{"with done", TaskFilter{ProjectID: "p1", Status: StatusAll}, []string{"t1", "t2", "t3", "t5"}},
		{"only done", TaskFilter{ProjectID: "p1", Status: StatusDone}, []string{"t5"}},
		{"overdue", TaskFilter{DueTo: mustTime(t, "2026-09-03T00:00:00.000+0000")}, []string{"t1"}},
		{"a day", TaskFilter{
			DueFrom: mustTime(t, "2026-09-03T00:00:00.000+0000"),
			DueTo:   mustTime(t, "2026-09-04T00:00:00.000+0000"),
		}, []string{"t2"}},
		{"undated", TaskFilter{Due: DueNone}, []string{"t3", "t4"}},
		{"dated", TaskFilter{ProjectID: "p1", Due: DueSet}, []string{"t1", "t2"}},
		{"by priority", TaskFilter{ProjectID: "p1", Order: OrderPriority}, []string{"t2", "t1", "t3"}},
		{"limited", TaskFilter{ProjectID: "p1", Limit: 2}, []string{"t1", "t2"}},
		{"another project", TaskFilter{ProjectID: "p2"}, []string{"t4"}},
	} {
		got, err := s.Tasks(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var ids []string
		for _, task := range got {
			ids = append(ids, task.Id)
		}
		if len(ids) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, ids, tc.want)
			continue
		}
		for i := range ids {
			if ids[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.name, ids, tc.want)
				break
			}
		}
	}

	got, err := s.Tasks(ctx, TaskFilter{ProjectID: "p1", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Items != nil {
		t.Errorf("listing loaded items: %+v", got)
	}
	if _, err := s.Task(ctx, "nope"); err == nil {
		t.Error("reading a missing task succeeded")
	}
}

func TestTasksWithoutADueDateKeepTheOrderTheyWereAddedIn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	first := openTask(LocalIDPrefix+"ffffffffffffffff", "p1", "Первая")
	first.CreatedTime = mustTime(t, "2026-09-03T09:00:00.000+0000")
	second := openTask(LocalIDPrefix+"0000000000000000", "p1", "Вторая")
	second.CreatedTime = mustTime(t, "2026-09-03T09:01:00.000+0000")
	if first.Id < second.Id {
		t.Fatalf("the ids no longer contradict the order of creation, so this test proves nothing: %s, %s", first.Id, second.Id)
	}
	for _, task := range []model.Task{first, second} {
		if _, err := s.CreateTask(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.Title, err)
		}
	}

	got, err := s.Tasks(ctx, TaskFilter{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Id != first.Id || got[1].Id != second.Id {
		t.Fatalf("listing = %v, want the order they were added in: %v", taskTitles(got), []string{first.Title, second.Title})
	}
}

func TestTaskFilterByCompletionTime(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	stamps := map[string]string{
		"t1": "2026-09-02T23:59:59.000+0000",
		"t2": "2026-09-03T00:00:00.000+0000",
		"t3": "2026-09-03T18:00:00.000+0000",
		"t4": "2026-09-04T00:00:00.000+0000",
	}
	var tasks []model.Task
	for id, stamp := range stamps {
		task := openTask(id, "p1", "Задача "+id)
		task.Status = model.TaskDone
		task.CompletedTime = mustTime(t, stamp)
		tasks = append(tasks, task)
	}
	tasks = append(tasks, openTask("t5", "p1", "Ещё открытая"))
	if _, err := s.SyncProject(ctx, "p1", fromServer(tasks...)); err != nil {
		t.Fatal(err)
	}

	day := TaskFilter{
		Status:   StatusDone,
		DoneFrom: mustTime(t, "2026-09-03T00:00:00.000+0000"),
		DoneTo:   mustTime(t, "2026-09-04T00:00:00.000+0000"),
		Order:    OrderModified,
	}
	got, err := s.Tasks(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, task := range got {
		ids[task.Id] = true
	}
	if len(ids) != 2 || !ids["t2"] || !ids["t3"] {
		t.Fatalf("closed that day: %v, want t2 and t3", ids)
	}

	from := TaskFilter{Status: StatusAll, DoneFrom: mustTime(t, "2026-09-03T18:00:00.000+0000")}
	if got, err = s.Tasks(ctx, from); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("from the lower bound alone: %v", taskTitles(got))
	}
	to := TaskFilter{Status: StatusAll, DoneTo: mustTime(t, "2026-09-03T00:00:00.000+0000")}
	if got, err = s.Tasks(ctx, to); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Id != "t1" {
		t.Errorf("up to the upper bound alone: %v", taskTitles(got))
	}

	for _, task := range got {
		if task.Id == "t5" {
			t.Error("an open task matched a completion range")
		}
	}
}

func TestPurgeCompleted(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	old := openTask("t1", "p1", "Старая")
	old.Status = model.TaskDone
	old.CompletedTime = mustTime(t, "2026-06-01T09:00:00.000+0000")
	recent := openTask("t2", "p1", "Свежая")
	recent.Status = model.TaskDone
	recent.CompletedTime = mustTime(t, "2026-09-02T09:00:00.000+0000")
	open := openTask("t3", "p1", "Открытая")
	if _, err := s.SyncProject(ctx, "p1", fromServer(old, recent, open)); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	n, err := s.PurgeCompleted(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	left, err := s.Tasks(ctx, TaskFilter{Status: StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("left %v", taskTitles(left))
	}

	if _, err := s.CompleteTask(ctx, "t3", CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET completed_time = '2026-06-01T09:00:00.000Z' WHERE id = 't3'`); err != nil {
		t.Fatal(err)
	}
	if n, err = s.PurgeCompleted(ctx, cutoff); err != nil || n != 0 {
		t.Fatalf("purged %d unpushed rows (%v)", n, err)
	}
}

func TestChecklistKeepsTheOrderItArrivedIn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})

	task := openTask("t1", "p1", "packing")
	task.Items = []model.Item{
		{Id: "ffffffffffffffffffffffff", Title: "passport"},
		{Id: "000000000000000000000001", Title: "tickets"},
	}
	if _, err := s.SyncProject(ctx, "p1", fromServer(task)); err != nil {
		t.Fatal(err)
	}

	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("read %d items, want 2", len(got.Items))
	}
	if got.Items[0].Title != "passport" || got.Items[1].Title != "tickets" {
		t.Errorf("checklist read back as %q, %q; want the order it arrived in",
			got.Items[0].Title, got.Items[1].Title)
	}
}

func TestChecklistOfALocalTaskKeepsItsOrder(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Work"})

	task := openTask("", "p1", "packing")
	task.Items = []model.Item{{Title: "passport"}, {Title: "tickets"}, {Title: "charger"}}
	created, err := s.CreateTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range got.Items {
		titles = append(titles, it.Title)
	}
	want := []string{"passport", "tickets", "charger"}
	if len(titles) != len(want) {
		t.Fatalf("read %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("checklist read back as %v, want %v", titles, want)
		}
	}
}
