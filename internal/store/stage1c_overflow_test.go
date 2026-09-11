package store

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestStage1COverflowAddUndoRestoresExactRanks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProjects(t, st, model.Project{Id: "p1", Name: "Personal"})
	created, err := st.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "overflow", Items: []model.Item{{Title: "a"}, {Title: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]model.Item(nil), created.Items...)
	before[0].SortOrder = 17
	before[1].SortOrder = math.MaxInt64
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Items: model.NewEditList(before)}); err != nil {
		t.Fatal(err)
	}
	added, err := st.AddTaskItem(ctx, created.Id, "c")
	if err != nil {
		t.Fatal(err)
	}
	if got := itemRanks(added.Items); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("overflow add ranks = %v", got)
	}
	undo, err := st.LastUndo(ctx)
	if err != nil || undo.Feature == nil || !undo.Feature.Fields.Items {
		t.Fatalf("overflow add undo = %+v, %v", undo, err)
	}
	restored, err := st.ApplyUndo(ctx, undo)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemRanks(restored.Items); !reflect.DeepEqual(got, []int64{17, math.MaxInt64}) {
		t.Fatalf("overflow add undo ranks = %v", got)
	}
	var payload []byte
	if err := st.DB().QueryRowContext(ctx, `SELECT payload FROM outbox ORDER BY seq DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	edit, metadata, err := DecodeTaskEditPayload(payload)
	if err != nil || metadata == nil || edit.Items == nil {
		t.Fatalf("queued overflow undo = %+v, %+v, %v", edit, metadata, err)
	}
	if got := itemRanks([]model.Item(*edit.Items)); !reflect.DeepEqual(got, []int64{17, math.MaxInt64}) {
		t.Fatalf("queued overflow undo ranks = %v", got)
	}
}

func itemRanks(items []model.Item) []int64 {
	ranks := make([]int64, len(items))
	for i := range items {
		ranks[i] = items[i].SortOrder
	}
	return ranks
}
