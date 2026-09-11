package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func outcomeStoreState(t *testing.T, st *Store) map[string][][]string {
	t.Helper()
	ctx := context.Background()
	state := make(map[string][][]string)
	for _, table := range []string{
		"projects", "tasks", "items", "outbox", "events", "undo_log",
		"item_identities", "meta", "listing", "outcome_commit_guard",
	} {
		rows, err := st.DB().QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			record := make([]string, len(values))
			for i, value := range values {
				record[i] = fmt.Sprintf("%T:%v", value, value)
			}
			state[table] = append(state[table], record)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestTaskMutationOutcomeIsPublishedOnlyAfterSuccessfulTransaction(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	created, err := st.CreateTask(ctx, model.Task{
		ProjectId: "p1",
		Title:     "Stable",
		Items:     []model.Item{{Title: "same"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	unchanged, err := st.RenameTaskItemOutcome(ctx, created.Id, 1, "same")
	if err != nil || unchanged.Changed || unchanged.Task.Id != created.Id {
		t.Fatalf("no-op outcome = %+v, %v", unchanged, err)
	}
	changed, err := st.RenameTaskItemOutcome(ctx, created.Id, 1, "different")
	if err != nil || !changed.Changed || changed.Task.Items[0].Title != "different" {
		t.Fatalf("mutation outcome = %+v, %v", changed, err)
	}

	t.Run("late rollback", func(t *testing.T) {
		if _, err := st.DB().ExecContext(ctx, `
			CREATE TRIGGER fail_feature_outcome_undo
			BEFORE INSERT ON undo_log
			BEGIN
				SELECT RAISE(ABORT, 'forced undo failure');
			END
		`); err != nil {
			t.Fatal(err)
		}
		before := stage1BStateOf(t, st, created.Id)
		outcome, err := st.SetTaskRepeatOutcome(ctx, created.Id, "RRULE:FREQ=DAILY;INTERVAL=1")
		if err == nil || !strings.Contains(err.Error(), "forced undo failure") {
			t.Fatalf("late failure = %+v, %v", outcome, err)
		}
		if !reflect.DeepEqual(outcome, TaskMutationOutcome{}) {
			t.Fatalf("late failure returned outcome %+v", outcome)
		}
		after := stage1BStateOf(t, st, created.Id)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("late failure was not rolled back:\nbefore=%+v\nafter=%+v", before, after)
		}
		if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER fail_feature_outcome_undo`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("commit failure", func(t *testing.T) {
		if _, err := st.DB().ExecContext(ctx, `
			CREATE TABLE outcome_commit_guard (
				project_id TEXT REFERENCES projects(id) DEFERRABLE INITIALLY DEFERRED
			)
		`); err != nil {
			t.Fatal(err)
		}
		before := outcomeStoreState(t, st)
		outcome, err := st.editTaskWithTxOutcome(ctx, created.Id, OpTaskUpdate, TaskUpdatedKind,
			func(ctx context.Context, tx *sql.Tx, _ model.Task) (model.TaskEdit, []string, error) {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO outcome_commit_guard (project_id) VALUES ('missing-project')`); err != nil {
					return model.TaskEdit{}, nil, err
				}
				return model.TaskEdit{RepeatFlag: model.Ptr("RRULE:FREQ=DAILY;INTERVAL=1")}, nil, nil
			})
		if err == nil || !strings.HasPrefix(err.Error(), "commit transaction: ") {
			t.Fatalf("commit failure = %+v, %v", outcome, err)
		}
		if !reflect.DeepEqual(outcome, TaskMutationOutcome{}) {
			t.Fatalf("commit failure returned outcome %+v", outcome)
		}
		after := outcomeStoreState(t, st)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("commit failure was not fully rolled back:\nbefore=%+v\nafter=%+v", before, after)
		}
		var fixtureRows int
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM outcome_commit_guard`).Scan(&fixtureRows); err != nil {
			t.Fatal(err)
		}
		if fixtureRows != 0 {
			t.Fatalf("commit failure left %d fixture rows", fixtureRows)
		}
	})

	t.Run("cancelled begin", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		outcome, err := st.SetTaskRepeatOutcome(cancelled, created.Id, "")
		if err == nil || !reflect.DeepEqual(outcome, TaskMutationOutcome{}) {
			t.Fatalf("cancelled outcome = %+v, %v", outcome, err)
		}
	})
}

func TestEqualFeatureOutcomeWorkersSerializeBehindHeldWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	holder, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	peer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	seedProjects(t, holder, model.Project{Id: "p1", Name: "Personal"})
	created, err := holder.CreateTask(ctx, model.Task{
		ProjectId: "p1",
		Title:     "Concurrent",
		Items:     []model.Item{{Title: "one"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := stage1BStateOf(t, holder, created.Id)

	locked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHolder)
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.Tx(ctx, func(*sql.Tx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	type workerResult struct {
		outcome TaskMutationOutcome
		err     error
	}
	results := make(chan workerResult, 2)
	started := make(chan struct{}, 2)
	for _, st := range []*Store{holder, peer} {
		go func(st *Store) {
			started <- struct{}{}
			outcome, err := st.SetTaskItemDoneOutcome(ctx, created.Id, 1, true)
			results <- workerResult{outcome: outcome, err: err}
		}(st)
	}
	<-started
	<-started
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			t.Fatalf("worker passed held BEGIN IMMEDIATE transaction: %+v", result)
		case <-time.After(40 * time.Millisecond):
		}
	}

	releaseHolder()
	select {
	case err := <-holderDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held writer did not finish after release")
	}
	var changed, unchanged int
	for i := 0; i < 2; i++ {
		var result workerResult
		select {
		case result = <-results:
		case <-time.After(5 * time.Second):
			t.Fatal("feature outcome worker did not finish after release")
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.outcome.Task.Id != created.Id {
			t.Fatalf("worker outcome task = %+v", result.outcome.Task)
		}
		if result.outcome.Changed {
			changed++
		} else {
			unchanged++
		}
	}
	if changed != 1 || unchanged != 1 {
		t.Fatalf("worker outcomes: changed=%d unchanged=%d", changed, unchanged)
	}

	after := stage1BStateOf(t, holder, created.Id)
	if !after.Task.Items[0].Status.Done() {
		t.Fatalf("final item is not done: %+v", after.Task.Items[0])
	}
	if after.Outbox != before.Outbox+1 || after.Events != before.Events+1 ||
		after.Undo != before.Undo+1 || after.Dirty != before.Dirty+1 {
		t.Fatalf("equal workers made more than one mutation:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestUpdateTaskOutcomeKeepsLegacyValidationSemantics(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Stable"})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("builder failed")
	outcome, err := st.editTaskWithTxOutcome(ctx, created.Id, OpTaskUpdate, TaskUpdatedKind,
		func(_ context.Context, _ *sql.Tx, _ model.Task) (model.TaskEdit, []string, error) {
			return model.TaskEdit{}, nil, sentinel
		})
	if !errors.Is(err, sentinel) || !reflect.DeepEqual(outcome, TaskMutationOutcome{}) {
		t.Fatalf("builder failure outcome = %+v, %v", outcome, err)
	}
}
