package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func projectNamesOf(ps []model.Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestReplaceProjects(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s,
		model.Project{Id: "p1", Name: "Личное", Kind: "TASK", SortOrder: 2},
		model.Project{Id: "p2", Name: "Работа", Kind: "TASK", SortOrder: 1, Closed: true},
	)
	got, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Id != "p2" || got[1].Id != "p1" {
		t.Fatalf("projects %+v, want sort order", got)
	}
	if !got[0].Closed || got[0].Kind != "TASK" {
		t.Errorf("fields lost: %+v", got[0])
	}

	one, err := s.Project(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if one.Name != "Личное" {
		t.Errorf("got %+v", one)
	}
	if _, err := s.Project(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing project: %v", err)
	}

	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})
	got, err = s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Id != "p1" {
		t.Fatalf("projects after the replace: %v", projectNamesOf(got))
	}
}

func TestProjectNamesTheMissingListWithAnArticle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	_, err := s.Project(ctx, "nope")
	if err == nil {
		t.Fatal("Project on an uncached id returned nil, want ErrNotFound")
	}
	const want = "the list nope: not in the local cache"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestReplaceProjectsDropsTasksOfAVanishedProject(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"},
	)
	orphan := openTask("t1", "p2", "Отчёт за квартал")
	orphan.Items = []model.Item{{Id: "i1", Title: "цифры"}}
	if _, err := s.SyncProject(ctx, "p2", fromServer(orphan, openTask("t2", "p2", "Позвонить в банк"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t3", "p1", "Забрать посылку"))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateTask(ctx, "t2", model.TaskEdit{Title: model.Ptr("Позвонить в банк утром")}); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateTask(ctx, model.Task{ProjectId: "p2", Title: "Собрать документы"})
	if err != nil {
		t.Fatal(err)
	}

	seedProjects(t, s, model.Project{Id: "p1", Name: "Личное"})

	left, err := s.Tasks(ctx, TaskFilter{Status: StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, task := range left {
		ids[task.Id] = true
	}
	if len(ids) != 3 || ids["t1"] || !ids["t2"] || !ids["t3"] || !ids[created.Id] {
		t.Fatalf("left %v, want t2, t3 and the offline task", taskTitles(left))
	}
	var items int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM items WHERE task_id = 't1'`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Errorf("%d checklist rows outlived their task", items)
	}
}

func TestFindProjects(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s,
		model.Project{Id: "p1", Name: "Личное"},
		model.Project{Id: "p2", Name: "Работа"},
		model.Project{Id: "p3", Name: "Ремонт квартиры"},
	)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"личное", 1},
		{"ЛИЧНОЕ", 1},
		{"Р", 2},
		{"квартир", 1},
		{"", 3},
		{"молоко", 0},
	} {
		got, err := s.FindProjects(ctx, tc.query)
		if err != nil {
			t.Fatalf("find %q: %v", tc.query, err)
		}
		if len(got) != tc.want {
			t.Errorf("find %q found %v, want %d", tc.query, projectNamesOf(got), tc.want)
		}
	}
}

func TestMatchNamesAgreesWithFindProjects(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, crossCheckProjects()...)

	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := projectNamesOf(all)

	for _, query := range crossCheckQueries {
		found, err := s.FindProjects(ctx, query)
		if err != nil {
			t.Fatalf("find %q: %v", query, err)
		}
		want := projectNamesOf(found)
		got := MatchNames(names, query)
		if !slices.Equal(got, want) {
			t.Errorf("MatchNames(%q) = %v, FindProjects(%q) = %v", query, got, query, want)
		}
	}

	behind := "the name standing behind the ones passed in"
	room := make([]string, len(names)+1)
	copy(room, names)
	room[len(names)] = behind
	everything := MatchNames(room[:len(names)], "")
	if !slices.Equal(everything, names) {
		t.Fatalf("MatchNames on an empty query = %v, want every name: %v", everything, names)
	}
	everything = append(everything, "what a caller appends to what it was given")
	if room[len(names)] != behind {
		t.Errorf("appending %q to the answer to an empty query wrote it into the caller's own array",
			everything[len(everything)-1])
	}
}

func crossCheckProjects() []model.Project {
	return []model.Project{
		{Id: "p1", Name: "Office Hub", Kind: "TASK", SortOrder: 1},
		{Id: "p2", Name: "Home", Kind: "TASK", SortOrder: 2},
		{Id: "p3", Name: "Homework", Kind: "TASK", SortOrder: 3},

		{Id: "p4", Name: "Work ", Kind: "TASK", SortOrder: 4},
		{Id: "p5", Name: "Workshop", Kind: "TASK", SortOrder: 5},
		{Id: "p6", Name: "Личное", Kind: "TASK", SortOrder: 6},
		{Id: "p7", Name: "личное", Kind: "TASK", SortOrder: 7},
		{Id: "p8", Name: "Ремонт квартиры", Kind: "TASK", SortOrder: 8, Closed: true},
		{Id: "p9", Name: "Заметки", Kind: "NOTE", SortOrder: 9},
		{Id: "p10", Name: "Дубль", Kind: "TASK", SortOrder: 10},
		{Id: "p11", Name: "Дубль", Kind: "TASK", SortOrder: 11},
	}
}

var crossCheckQueries = []string{
	"",
	"   ",
	"Office Hub",
	"office hub",
	"office",
	"Home",
	"home",
	"Work",
	"  Work  ",
	"shop",
	"Личное",
	"личн",
	"квартир",
	"Заметки",
	"Дубль",
	"о",
	"молоко",
}
