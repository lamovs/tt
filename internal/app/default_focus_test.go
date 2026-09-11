package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func focusFixture(t *testing.T) (*store.Store, model.Task) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Work"}, {Id: "p2", Name: "Closed", Closed: true}}); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Original", Kind: "TEXT"})
	if err != nil {
		t.Fatal(err)
	}
	return st, task
}

func TestDefaultFocusChoicesNeverSelectAnotherList(t *testing.T) {
	st, task := focusFixture(t)
	for _, setting := range []string{"id:p1", "Work", "id:missing"} {
		choices, err := DefaultFocusChoices(context.Background(), st, setting)
		if err != nil || len(choices.Projects) != 1 || len(choices.Tasks) != 1 || choices.Tasks[0].Id != task.Id {
			t.Fatalf("%+v %v", choices, err)
		}
		want := "p1"
		if setting == "id:missing" {
			want = ""
			if choices.Notice == "" {
				t.Fatal("missing warning")
			}
		}
		if choices.InitialProjectID != want {
			t.Fatal("substituted initial list")
		}
	}
}

func TestDefaultFocusStartRechecksAvailability(t *testing.T) {
	for _, mutation := range []string{"UPDATE tasks SET status=2", "UPDATE projects SET closed=1 WHERE id='p1'", "DELETE FROM tasks", "UPDATE tasks SET title='Renamed'"} {
		t.Run(mutation, func(t *testing.T) {
			st, task := focusFixture(t)
			ctx := context.Background()
			data, _ := st.ReadTimer(ctx, time.Now())
			guard, err := BindDefaultFocus(ctx, st, data.Guard, config.FocusReference{TaskID: task.Id})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ControlTimer(ctx, "start", store.TimerStartOptions{TaskID: guard.TaskID, FocusType: 1}, &guard, time.Now()); err == nil {
				t.Fatal("stale default started")
			}
			data, _ = st.ReadTimer(ctx, time.Now())
			if data.State != nil {
				t.Fatal("failed default mutated timer")
			}
		})
	}
}

func TestDefaultFocusDoesNotRetargetSessionsOrFrozenUploads(t *testing.T) {
	for _, mode := range []int{0, 1} {
		t.Run(model.Duration(time.Duration(mode)*time.Second).String(), func(t *testing.T) {
			st, task := focusFixture(t)
			ctx := context.Background()
			now := time.Now()
			data, _ := st.ReadTimer(ctx, now)
			guard, err := BindDefaultFocus(ctx, st, data.Guard, config.FocusReference{TaskID: task.Id})
			if err != nil {
				t.Fatal(err)
			}
			options := store.TimerStartOptions{TaskID: guard.TaskID, FocusType: mode}
			if mode == 0 {
				options.Planned = time.Minute
			}
			started, err := st.ControlTimer(ctx, "start", options, &guard, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec("UPDATE tasks SET title='Renamed'"); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.DefaultFocus = config.FocusReference{TaskID: "different"}
			timers := NewTimers(st, cfg, nil, nil, nil)
			read, err := timers.Read(ctx, now.Add(time.Second))
			if err != nil || read.Active.State.TaskID != task.Id {
				t.Fatalf("changed active target: %+v %v", read, err)
			}
			if _, err := st.DB().Exec("DELETE FROM tasks"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.StopTimer(ctx, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			const request = `{"taskId":"original","note":"frozen"}`
			_, err = st.DB().Exec(`INSERT INTO focus_uploads(session_id,phase,request,prior_ids) VALUES (?,'armed',?,'[]')`, started.State.SessionID, request)
			if err != nil {
				t.Fatal(err)
			}
			read, err = timers.Read(ctx, now.Add(3*time.Second))
			if err != nil || len(read.History) != 1 || read.History[0].Session.TaskID != task.Id || string(read.History[0].Upload.Request) != request {
				t.Fatalf("retargeted history/upload: %+v %v", read, err)
			}
		})
	}
}
