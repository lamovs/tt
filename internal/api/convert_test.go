package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestTaskToModel(t *testing.T) {
	in := Task{
		ID:            "t1",
		ProjectID:     "p1",
		Title:         "Забрать посылку",
		Content:       "отделение на Ленина",
		Desc:          "not the description",
		Status:        2,
		Priority:      3,
		DueDate:       "2026-09-10T09:00:00.000+0000",
		StartDate:     "2026-09-10T08:00:00.000+0000",
		CreatedTime:   "2026-09-01T10:00:00.000+0000",
		ModifiedTime:  "2026-09-02T10:00:00.000+0000",
		CompletedTime: "2026-09-03T10:00:00.000+0000",
		IsAllDay:      true,
		TimeZone:      "Europe/Moscow",
		RepeatFlag:    "RRULE:FREQ=WEEKLY;INTERVAL=1",
		Reminders:     []string{"TRIGGER:PT0S"},
		Tags:          []string{"дом"},
		Kind:          "CHECKLIST",
		SortOrder:     -42,
		Items: []ChecklistItem{
			{ID: "i1", Title: "паспорт", Status: 1, SortOrder: 1, CompletedTime: "2026-09-03T09:00:00.000+0000"},
			{ID: "i2", Title: "трек-номер", Status: 0},
		},
	}
	got, warnings := TaskToModel(in)
	if len(warnings) != 0 {
		t.Fatalf("warnings on a clean task: %v", warnings)
	}
	if got.Id != "t1" || got.ProjectId != "p1" || got.Title != "Забрать посылку" {
		t.Errorf("identity: %+v", got)
	}

	if got.Content != "отделение на Ленина" {
		t.Errorf("content = %q", got.Content)
	}
	if strings.Contains(got.Content+got.Title, "not the description") {
		t.Error("desc leaked into the domain task")
	}
	if got.Status != model.TaskDone || got.Priority != model.PriorityMedium {
		t.Errorf("status %v priority %v", got.Status, got.Priority)
	}
	if got.DueDate.String() != "2026-09-10T09:00:00.000+0000" {
		t.Errorf("due date = %q", got.DueDate.String())
	}
	if !got.IsAllDay || got.TimeZone != "Europe/Moscow" || got.Kind != "CHECKLIST" || got.SortOrder != -42 {
		t.Errorf("flags: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "дом" || len(got.Reminders) != 1 {
		t.Errorf("tags %v reminders %v", got.Tags, got.Reminders)
	}
	if len(got.Items) != 2 || got.Items[0].Status != model.ItemDone || got.Items[1].Status != model.ItemOpen {
		t.Fatalf("items: %+v", got.Items)
	}
	if got.Items[0].CompletedTime.IsZero() {
		t.Error("item completion time lost")
	}
}

func TestTaskToModelDegrades(t *testing.T) {
	in := Task{
		ID:        "t1",
		ProjectID: "p1",
		Title:     "Забрать посылку",
		Priority:  4,
		Status:    7,
		DueDate:   "10.09.2026",
		Items:     []ChecklistItem{{ID: "i1", Title: "паспорт", Status: 9}},
	}
	got, warnings := TaskToModel(in)

	if got.Priority != model.PriorityNone {
		t.Errorf("priority = %v, want none", got.Priority)
	}
	if got.Status != model.TaskDone {
		t.Errorf("status = %v, want done", got.Status)
	}
	if !got.DueDate.IsZero() {
		t.Errorf("due date = %v, want zero", got.DueDate)
	}
	if len(got.Items) != 1 || got.Items[0].Status != model.ItemDone {
		t.Fatalf("items: %+v", got.Items)
	}

	if got.Id != "t1" || got.Title != "Забрать посылку" {
		t.Errorf("good fields lost: %+v", got)
	}

	want := map[string]bool{"priority": false, "status": false, "dueDate": false}
	items := 0
	for _, w := range warnings {
		if w.TaskID != "t1" {
			t.Errorf("warning without a task id: %+v", w)
		}
		if w.Detail == "" || w.Value == "" {
			t.Errorf("warning without detail: %+v", w)
		}
		if w.ItemID != "" {
			items++
			continue
		}
		if _, ok := want[w.Field]; !ok {
			t.Errorf("unexpected warning: %s", w)
			continue
		}
		want[w.Field] = true
	}
	for field, seen := range want {
		if !seen {
			t.Errorf("no warning about %s", field)
		}
	}
	if items != 1 {
		t.Errorf("item warnings = %d, want 1", items)
	}
	if s := warnings[0].String(); !strings.Contains(s, "t1") {
		t.Errorf("warning renders as %q", s)
	}
}

func TestTasksToModelKeepsEveryTask(t *testing.T) {
	got, warnings := TasksToModel([]Task{
		{ID: "t1", Title: "ok"},
		{ID: "t2", Title: "битая", Priority: 4},
		{ID: "t3", Title: "ok"},
	})
	if len(got) != 3 {
		t.Fatalf("converted %d tasks, want 3", len(got))
	}
	if len(warnings) != 1 || warnings[0].TaskID != "t2" {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestProjectToModel(t *testing.T) {
	got := ProjectToModel(Project{ID: "p1", Name: "Личное", Kind: "TASK", SortOrder: 7, Closed: true, Color: "#fff"})
	want := model.Project{Id: "p1", Name: "Личное", Kind: "TASK", SortOrder: 7, Closed: true, Color: "#fff"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestTaskCreateFromDropsTheLocalID(t *testing.T) {
	got := TaskCreateFrom(model.Task{
		Id:        "local-deadbeef",
		ProjectId: "p1",
		Title:     "Забрать посылку",
		Priority:  model.PriorityHigh,
		DueDate:   mustTime(t, "2026-09-10T09:00:00.000+0000"),
		Tags:      []string{"дом"},
		Items:     []model.Item{{Id: "i1", Title: "паспорт", Status: model.ItemDone}},
	})
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	if _, ok := object["id"]; ok {
		t.Errorf("create body carries a task id: %s", payload)
	}
	if got.ProjectID != "p1" || got.Title != "Забрать посылку" || got.Priority != 5 {
		t.Errorf("got %+v", got)
	}
	if got.DueDate != "2026-09-10T09:00:00.000+0000" {
		t.Errorf("due date = %q", got.DueDate)
	}
	if len(got.Items) != 1 || got.Items[0].Status != 1 {
		t.Errorf("items: %+v", got.Items)
	}
}

func TestTaskUpdateFrom(t *testing.T) {
	e := model.TaskEdit{
		Title:    model.Ptr("Забрать посылку"),
		Priority: model.Ptr(model.PriorityHigh),
		Status:   model.Ptr(model.TaskDone),
		DueDate:  model.NewEditTime(model.Time{}),
	}
	u := TaskUpdateFrom("t1", "p1", e)
	if u.ID != "t1" || u.ProjectID != "p1" {
		t.Fatalf("addressing: %+v", u)
	}
	if u.Title == nil || *u.Title != "Забрать посылку" {
		t.Errorf("title = %v", u.Title)
	}
	if u.Priority == nil || *u.Priority != 5 || u.Status == nil || *u.Status != 2 {
		t.Errorf("priority %v status %v", u.Priority, u.Status)
	}

	if u.DueDate == nil || *u.DueDate != "1970-01-01T00:00:00.000+0000" {
		t.Errorf("due date = %v, want the epoch clear sentinel", u.DueDate)
	}
	if u.Content != nil || u.StartDate != nil || u.Items != nil || u.Desc != nil {
		t.Errorf("untouched fields present: %+v", u)
	}

	moved := TaskUpdateFrom("t1", "p1", model.TaskEdit{
		ProjectId: model.Ptr("p2"),
		Title:     model.Ptr("Забрать посылку"),
	})
	if moved.ProjectID != "p1" {
		t.Errorf("addressed at project %q, want the one the task is in", moved.ProjectID)
	}
	if moved.Title == nil || *moved.Title != "Забрать посылку" {
		t.Errorf("title = %v, want the rest of the edit kept", moved.Title)
	}
}

func TestItemReplacementEmitsEveryWritableZero(t *testing.T) {
	item := model.Item{Key: "local-private", Title: "milk"}
	edit := model.TaskEdit{Items: model.NewEditList([]model.Item{item})}
	payload, err := json.Marshal(TaskUpdateFrom("t1", "p1", edit))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(root["items"], &items); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "status", "sortOrder", "startDate", "isAllDay", "timeZone", "completedTime"} {
		if _, ok := items[0][field]; !ok {
			t.Errorf("item body %s omitted %s", root["items"], field)
		}
	}
	if bytes.Contains(payload, []byte("local-private")) || bytes.Contains(payload, []byte(`"key"`)) {
		t.Fatalf("private item key reached HTTP DTO JSON: %s", payload)
	}

	created, err := json.Marshal(TaskCreateFrom(model.Task{Title: "shopping", Items: []model.Item{item}}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(created, []byte("local-private")) {
		t.Fatalf("private item key reached create JSON: %s", created)
	}
	for _, fragment := range []string{`"status":0`, `"sortOrder":0`, `"isAllDay":false`, `"completedTime":""`} {
		if !bytes.Contains(created, []byte(fragment)) {
			t.Errorf("create body %s omitted %s", created, fragment)
		}
	}
}

func TestTaskUpdateFromTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit model.TaskEdit
		want string
	}{
		{"set", model.TaskEdit{Tags: model.NewEditList([]string{"дом", "почта"})}, `"tags":["дом","почта"]`},
		{"cleared", model.TaskEdit{Tags: model.NewEditList([]string{})}, `"tags":[]`},
		{"reminders cleared", model.TaskEdit{Reminders: model.NewEditList([]string{})}, `"reminders":[]`},
	} {
		b, err := json.Marshal(TaskUpdateFrom("t1", "p1", tc.edit))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("%s: body %s, want %s in it", tc.name, b, tc.want)
		}
	}

	b, err := json.Marshal(TaskUpdateFrom("t1", "p1", model.TaskEdit{Title: model.Ptr("x")}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tags") {
		t.Errorf("an edit that does not touch tags sent them: %s", b)
	}
}

func TestTaskEditSurvivesJSON(t *testing.T) {
	e := model.TaskEdit{
		DueDate:       model.NewEditTime(model.Time{}),
		CompletedTime: model.NewEditTime(mustTime(t, "2026-09-10T09:00:00.000+0000")),
		Content:       model.Ptr(""),
		Tags:          model.NewEditList([]string{}),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back model.TaskEdit
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%v (payload %s)", err, b)
	}
	if back.DueDate == nil || !back.DueDate.IsZero() {
		t.Errorf("cleared due date came back as %v (payload %s)", back.DueDate, b)
	}
	if back.CompletedTime == nil || back.CompletedTime.String() != "2026-09-10T09:00:00.000+0000" {
		t.Errorf("completed time came back as %v", back.CompletedTime)
	}
	if back.Content == nil || *back.Content != "" {
		t.Errorf("cleared content came back as %v", back.Content)
	}
	if back.Tags == nil || len(*back.Tags) != 0 {
		t.Errorf("cleared tags came back as %v", back.Tags)
	}
	if back.Title != nil || back.StartDate != nil {
		t.Errorf("untouched fields appeared: %+v", back)
	}
	u := TaskUpdateFrom("t1", "p1", back)
	if u.DueDate == nil || *u.DueDate != "1970-01-01T00:00:00.000+0000" {
		t.Errorf("patch after the round trip does not clear the date: %v", u.DueDate)
	}
}

func mustTime(t *testing.T, s string) model.Time {
	t.Helper()
	v, err := model.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
