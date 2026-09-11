package store

import (
	"context"
	"errors"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestStage1CLegacyTaskRechecksCurrentAbsenceGuards(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *Store)
		wantExists bool
		wantKept   int
		check      func(*testing.T, *Store, model.Task)
	}{
		{
			name: "dirty_edit",
			mutate: func(t *testing.T, st *Store) {
				if _, err := st.UpdateTask(context.Background(), "t1", model.TaskEdit{Title: model.Ptr("New local title")}); err != nil {
					t.Fatal(err)
				}
			},
			wantExists: true, wantKept: 1,
			check: func(t *testing.T, _ *Store, task model.Task) {
				if task.Title != "New local title" {
					t.Fatalf("title=%q, want newer local edit", task.Title)
				}
			},
		},
		{
			name: "completed",
			mutate: func(t *testing.T, st *Store) {
				if _, err := st.DB().ExecContext(context.Background(), `UPDATE tasks SET status=2 WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			},
			wantExists: true, wantKept: 1,
			check: func(t *testing.T, _ *Store, task model.Task) {
				if !task.Status.Done() {
					t.Fatalf("status=%s, want completed row preserved", task.Status)
				}
			},
		},
		{
			name: "moved",
			mutate: func(t *testing.T, st *Store) {
				if _, err := st.DB().ExecContext(context.Background(), `UPDATE tasks SET project_id='p2' WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			},
			wantExists: true, wantKept: 1,
			check: func(t *testing.T, _ *Store, task model.Task) {
				if task.ProjectId != "p2" {
					t.Fatalf("project=%q, want concurrently moved row preserved", task.ProjectId)
				}
			},
		},
		{
			name: "local",
			mutate: func(t *testing.T, st *Store) {
				if _, err := st.DB().ExecContext(context.Background(), `UPDATE tasks SET local=1 WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			},
			wantExists: true, wantKept: 1,
			check: func(t *testing.T, st *Store, _ model.Task) {
				var local bool
				if err := st.DB().QueryRowContext(context.Background(), `SELECT local FROM tasks WHERE id='t1'`).Scan(&local); err != nil || !local {
					t.Fatalf("local=%v err=%v, want local row preserved", local, err)
				}
			},
		},
		{
			name: "already_deleted",
			mutate: func(t *testing.T, st *Store) {
				if _, err := st.DB().ExecContext(context.Background(), `DELETE FROM tasks WHERE id='t1'`); err != nil {
					t.Fatal(err)
				}
			},
			wantExists: false, wantKept: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProjects(t, st, model.Project{Id: "p1", Name: "Synthetic"}, model.Project{Id: "p2", Name: "Other"})
			if _, err := st.SyncProject(ctx, "p1", fromServer(openTask("t1", "p1", "Remote"))); err != nil {
				t.Fatal(err)
			}
			fence, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, st)
			current, err := st.CapturePullFence(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			if current.Epoch != fence.Epoch {
				t.Fatalf("legacy transition advanced identity epoch: before=%+v after=%+v", fence, current)
			}
			result, err := st.SyncProjectFenced(ctx, "p1", nil, fence)
			if err != nil {
				t.Fatal(err)
			}
			got, taskErr := st.Task(ctx, "t1")
			if tc.wantExists {
				if taskErr != nil {
					t.Fatalf("guarded legacy row was deleted: %v, result=%+v", taskErr, result)
				}
				if tc.check != nil {
					tc.check(t, st, got)
				}
			} else if !errors.Is(taskErr, ErrNotFound) {
				t.Fatalf("already-deleted row returned task=%+v err=%v", got, taskErr)
			}
			if result.Deleted != 0 || result.Kept != tc.wantKept {
				t.Fatalf("result=%+v, want deleted=0 kept=%d", result, tc.wantKept)
			}
		})
	}
}
