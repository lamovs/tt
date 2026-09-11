package store

import (
	"context"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestReplaceProjectsPassesOverARepeatedId(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "Lichnoe", Kind: "TASK", SortOrder: 2})
	if _, err := s.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Zabrat posylku"))); err != nil {
		t.Fatal(err)
	}

	err := s.ReplaceProjects(ctx, []model.Project{
		{Id: "p1", Name: "Lichnoe", Kind: "TASK", SortOrder: 2},
		{Id: "p1", Name: "Inbox", Kind: "TASK", SortOrder: -9},
	})
	if err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	got, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("projects = %+v, want the one project the two rows name", got)
	}
	if got[0].Name != "Lichnoe" || got[0].SortOrder != 2 {
		t.Errorf("project = %+v, want the first row of the two", got[0])
	}

	if _, err := s.Task(ctx, "t1"); err != nil {
		t.Errorf("the task of a project the list names twice is gone: %v", err)
	}
}
