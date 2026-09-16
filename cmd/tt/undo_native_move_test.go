package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func undoNativeMove(t *testing.T, st *store.Store, ctx context.Context) model.Task {
	t.Helper()
	synced := model.Task{Id: "server-task", ProjectId: "p1", Title: "Забрать посылку"}
	raw := json.RawMessage(`{"id":"server-task","projectId":"p1","title":"Забрать посылку"}`)
	if _, err := st.SyncProject(context.Background(), "p1", []store.ServerTask{{Task: synced, Raw: raw}}); err != nil {
		t.Fatalf("sync fixture: %v", err)
	}
	task := undoTask(t, st, synced.Id)
	preview, err := st.PreviewNativeMove(ctx, task, "p2")
	if err != nil {
		t.Fatalf("preview move: %v", err)
	}
	if _, err := st.ApplyNativeMove(ctx, preview); err != nil {
		t.Fatalf("move: %v", err)
	}
	return task
}

func TestUndoReversesANativeMove(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	task := undoNativeMove(t, st, ctx)

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" {
		t.Fatalf("tt undo = %d (stderr: %s)", code, stderr)
	}
	if want := "undid the move of \"Забрать посылку\": it is back in its old list\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if got := undoTask(t, st, task.Id); got.ProjectId != "p1" {
		t.Errorf("the task is in %s, want the list it was moved out of", got.ProjectId)
	}
	if counts, err := st.OutboxCounts(ctx); err != nil || counts != (store.OutboxCounts{}) {
		t.Errorf("queue after cancelling an unsent move = %+v, %v; want it empty", counts, err)
	}
	if _, ok := undoTop(t, st); ok {
		t.Error("the move's record is still on the undo stack")
	}
}

func TestUndoReversesANativeMoveInsideAGroup(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	grouped := store.WithUndoGroup(ctx, "group-cli-move")
	added := undoGroupAdd(t, st, grouped, "Sample added")
	task := undoNativeMove(t, st, grouped)

	code, stdout, stderr := undoRun(t)
	if code != exitOK || stderr != "" {
		t.Fatalf("tt undo = %d (stderr: %s)", code, stderr)
	}
	want := []string{
		"undid 2 changes made together, newest first:",
		`undid the move of "Забрать посылку": it is back in its old list`,
		`undid adding "Sample added": the task is gone again`,
	}
	if lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n"); !reflect.DeepEqual(lines, want) {
		t.Errorf("stdout lines:\n got %q\nwant %q", lines, want)
	}
	if got := undoTask(t, st, task.Id); got.ProjectId != "p1" {
		t.Errorf("the task is in %s, want p1", got.ProjectId)
	}
	if _, err := st.Task(ctx, added.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the add in the group survived: %v", err)
	}
}

func TestUndoRefusesANativeMoveThatMayHaveReachedTheServer(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st := undoCache(t)
	undoProjects(t, st)
	undoNativeMove(t, st, ctx)
	claimed, _, err := st.Claim(ctx, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := st.RecordNativeMovePhase(ctx, claimed[0], "armed", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFailed(ctx, claimed[0].Seq, claimed[0].LeaseToken, "lost response"); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := undoRun(t)
	if code != exitError || stdout != "" {
		t.Fatalf("tt undo = %d, stdout %q", code, stdout)
	}
	for _, want := range []string{"may already have reached the server", "run tt sync", undoSkipOption} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	undoGroupLinesFit(t, stderr)
	if top, ok := undoTop(t, st); !ok || top.Action.Op != store.OpTaskMoveNative {
		t.Errorf("the stack is at %+v, want the move kept", top.Action)
	}
}
