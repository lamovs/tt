package app

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestBrowserViewsSearchDetailsAndReadOnlyBoundary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "\u0414\u043e\u043c"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	tomorrow := time.Date(2026, 9, 10, 0, 0, 0, 0, time.Local)
	seed := []model.Task{
		{ProjectId: "work", Title: "Overdue", DueDate: model.NewTime(now.Add(-48 * time.Hour)), Tags: []string{"\u0412\u0410\u0416\u041d\u041e"}},
		{ProjectId: "work", Title: "Today", Content: "body phrase", DueDate: model.NewTime(tomorrow.Add(-time.Nanosecond))},
		{ProjectId: "work", Title: "Tomorrow", DueDate: model.NewTime(tomorrow)},
		{ProjectId: "home", Title: "\u0417\u0410\u041c\u0415\u0422\u041a\u0410", Kind: "NOTE", Content: "Whole note\nlast line"},
		{ProjectId: "work", Title: "Completed", DueDate: model.NewTime(now.Add(-time.Hour))},
	}
	var tasks []model.Task
	for _, task := range seed {
		created, err := st.CreateTask(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}
	if _, err := st.CompleteTask(ctx, tasks[4].Id, store.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskItem(ctx, tasks[0].Id, "First item"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskItem(ctx, tasks[0].Id, "Second item"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetListing(ctx, []string{tasks[3].Id, tasks[1].Id}); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	browser := NewBrowser(st)
	for _, tc := range []struct {
		name    string
		query   BrowseQuery
		indexes []int
	}{
		{"today and overdue", BrowseQuery{View: TodayView}, []int{0, 1}},
		{"all open", BrowseQuery{View: OpenView}, []int{0, 1, 2, 3}},
		{"completed", BrowseQuery{View: CompletedView}, []int{4}},
		{"project", BrowseQuery{View: ProjectView, ProjectID: "home"}, []int{3}},
		{"title Unicode", BrowseQuery{View: OpenView, Search: "\u0437\u0430\u043c\u0435\u0442\u043a\u0430"}, []int{3}},
		{"body", BrowseQuery{View: OpenView, Search: "BODY PHRASE"}, []int{1}},
		{"tag", BrowseQuery{View: OpenView, Search: "\u0432\u0430\u0436\u043d\u043e"}, []int{0}},
		{"list", BrowseQuery{View: OpenView, Search: "\u0434\u043e\u043c"}, []int{3}},
		{"scoped search", BrowseQuery{View: ProjectView, ProjectID: "work", Search: "\u0434\u043e\u043c"}, nil},
		{"phrase order", BrowseQuery{View: OpenView, Search: "phrase body"}, nil},
		{"completed search", BrowseQuery{View: CompletedView, Search: "Completed"}, []int{4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := browser.Tasks(ctx, tc.query, now)
			if err != nil {
				t.Fatal(err)
			}
			var ids, want []string
			for _, task := range got {
				ids = append(ids, task.Id)
			}
			for _, i := range tc.indexes {
				want = append(want, tasks[i].Id)
			}
			slices.Sort(ids)
			slices.Sort(want)
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("IDs = %v, want %v", ids, want)
			}
		})
	}
	detail, err := browser.Task(ctx, tasks[0].Id)
	if err != nil || len(detail.Items) != 2 || detail.Items[1].Title != "Second item" {
		t.Fatalf("full checklist: %+v, %v", detail.Items, err)
	}
	note, err := browser.Task(ctx, tasks[3].Id)
	if err != nil || note.Kind != "NOTE" || note.Content != seed[3].Content {
		t.Fatal("note content changed")
	}
	if dumpCache(t, st) != before {
		t.Fatal("read views or details mutated database rows")
	}
	for _, query := range []BrowseQuery{{View: ProjectView}, {View: BrowseView(99)}} {
		if _, err := browser.Tasks(ctx, query, now); err == nil {
			t.Fatal("invalid view accepted")
		}
	}
	if _, err := browser.Task(ctx, ""); err == nil {
		t.Fatal("empty ID accepted")
	}
	if _, err := browser.Task(ctx, "missing"); err == nil {
		t.Fatal("missing task was an empty success")
	}
}

type recordingCache struct{ filter store.TaskFilter }

func (c *recordingCache) Projects(context.Context) ([]model.Project, error) { return nil, nil }
func (c *recordingCache) Tasks(_ context.Context, f store.TaskFilter) ([]model.Task, error) {
	c.filter = f
	return nil, nil
}
func (c *recordingCache) Task(context.Context, string) (model.Task, error) { return model.Task{}, nil }

func TestTodayUsesCivilMidnightAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	for _, day := range []struct {
		month time.Month
		day   int
		hours int
	}{{time.March, 29, 23}, {time.October, 25, 25}} {
		start := time.Date(2026, day.month, day.day, 0, 0, 0, 0, zone)
		cache := &recordingCache{}
		_, err := NewBrowser(cache).Tasks(context.Background(), BrowseQuery{View: TodayView}, start.Add(12*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := start.AddDate(0, 0, 1)
		if !cache.filter.DueTo.Equal(want) || cache.filter.DueTo.Sub(start) != time.Duration(day.hours)*time.Hour || !cache.filter.DueFrom.IsZero() || cache.filter.Status != store.StatusOpen {
			t.Fatalf("wrong civil-day filter: %+v", cache.filter)
		}
	}
}
