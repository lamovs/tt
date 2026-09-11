package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestTaskExtensionsWireRoundtripAndClear(t *testing.T) {
	var task Task
	if err := json.Unmarshal([]byte(`{"id":"t","projectId":"p","parentId":"parent","childIds":["child"],"columnId":"column","columnName":"Doing","focusSummaries":[{"estimatedDuration":1800,"estimatedPomo":3,"pomoCount":4,"future":true}]}`), &task); err != nil {
		t.Fatal(err)
	}
	value, warnings := TaskToModel(task)
	if len(warnings) != 0 || value.ParentId != "parent" || value.ChildIds[0] != "child" || value.ColumnId != "column" || value.ColumnName != "Doing" || value.EstimatedDuration != 1800 || value.EstimatedPomo != 3 || !strings.Contains(string(value.FocusSummaries), "future") {
		t.Fatalf("conversion lost fields: %+v %v", value, warnings)
	}
	create, err := json.Marshal(TaskCreateFrom(value))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(create), "childIds") || strings.Contains(string(create), "future") || strings.Contains(string(create), "pomoCount") || strings.Contains(string(create), "columnId") {
		t.Fatalf("read-only fields leaked: %s", create)
	}
	update := TaskUpdateFrom("t", "p", model.TaskEdit{ParentId: model.Ptr(""), ColumnId: model.Ptr(""), EstimatedDuration: model.Ptr(int64(0)), EstimatedPomo: model.Ptr(0)})
	raw, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"columnId", "parentId", "focusSummaries"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("clear field omitted: %s", raw)
		}
	}
	for _, key := range []string{"estimatedDuration", "estimatedPomo"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unverified top-level mutation: %s", raw)
		}
	}
	if string(fields["focusSummaries"]) != `[{"estimatedDuration":0,"estimatedPomo":0}]` {
		t.Fatalf("nested explicit clear lost: %s", raw)
	}
}

func TestColumnPatchWritesOnlyItsAddressAndColumn(t *testing.T) {
	raw, err := json.Marshal(TaskUpdateFrom("t", "p", model.TaskEdit{ColumnId: model.Ptr("done")}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || string(fields["id"]) != `"t"` || string(fields["projectId"]) != `"p"` || string(fields["columnId"]) != `"done"` {
		t.Fatalf("column patch contains unrelated writes: %s", raw)
	}
}

func TestTaskEstimatePatchDoesNotWriteReadOnlySummaryCounters(t *testing.T) {
	raw, err := json.Marshal(TaskUpdateFrom("t", "p", model.TaskEdit{EstimatedDuration: model.Ptr(int64(12))}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"focusSummaries":[{"estimatedDuration":12}],"id":"t","projectId":"p"}` {
		t.Fatalf("patch did not preserve omitted fields: %s", raw)
	}
}
