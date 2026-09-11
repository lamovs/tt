package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestGuardedEditRejectsStaleSnapshotWithoutAnyWrite(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.DB().Exec("CREATE TABLE outcome_commit_guard (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	seedProjects(t, st, model.Project{Id: "p", Name: "Project"})
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "original", Items: []model.Item{{Title: "item"}}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := st.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RenameTaskItem(ctx, created.Id, 1, "concurrent item"); err != nil {
		t.Fatal(err)
	}
	before := outcomeStoreState(t, st)
	outcome, err := st.UpdateTaskIfUnchanged(ctx, original, model.TaskEdit{Title: model.Ptr("editor title")})
	if !errors.Is(err, ErrTaskChanged) || outcome.Changed {
		t.Fatalf("outcome %+v, err %v", outcome, err)
	}
	if !reflect.DeepEqual(before, outcomeStoreState(t, st)) {
		t.Fatal("conflict changed persistent state")
	}
}

func TestGuardedEditNoopAndAtomicMutation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.DB().Exec("CREATE TABLE outcome_commit_guard (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	seedProjects(t, st, model.Project{Id: "p", Name: "Project"})
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p", Title: "original", Content: "old", Items: []model.Item{{Title: "item"}}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := st.Task(ctx, created.Id)
	if err != nil {
		t.Fatal(err)
	}
	before := outcomeStoreState(t, st)
	outcome, err := st.UpdateTaskIfUnchanged(ctx, original, model.TaskEdit{Title: model.Ptr(original.Title)})
	if err != nil || outcome.Changed || !reflect.DeepEqual(before, outcomeStoreState(t, st)) {
		t.Fatal("noop wrote a mutation")
	}
	outcome, err = st.UpdateTaskIfUnchanged(ctx, original, model.TaskEdit{Content: model.Ptr("\n# new\n\n")})
	if err != nil || !outcome.Changed || !reflect.DeepEqual(outcome.Task.Items, original.Items) || outcome.Task.Content != "\n# new\n\n" {
		t.Fatalf("mutation %+v, %v", outcome, err)
	}
}
