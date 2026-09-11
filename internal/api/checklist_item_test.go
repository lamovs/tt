package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestChecklistItemTimeWireEncodings(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"milliseconds", `1788955513123`, "2026-09-09T12:05:13.123+0000"},
		{"live start milliseconds", `1789130695741`, "2026-09-11T12:44:55.741+0000"},
		{"string keeps offset", `"2026-09-09T15:05:13.123+0300"`, "2026-09-09T15:05:13.123+0300"},
		{"epoch is a date", `0`, "1970-01-01T00:00:00.000+0000"},
		{"before epoch", `-1`, "1969-12-31T23:59:59.999+0000"},
		{"empty", `""`, ""},
	} {
		for _, field := range []string{"startDate", "completedTime"} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				var got ChecklistItem
				if err := json.Unmarshal([]byte(`{"id":"i1","title":"same","sortOrder":-7,"timeZone":"Europe/Moscow","`+field+`":`+tc.value+`}`), &got); err != nil {
					t.Fatal(err)
				}
				value, other := got.CompletedTime, got.StartDate
				if field == "startDate" {
					value, other = got.StartDate, got.CompletedTime
				}
				if value != tc.want || other != "" || got.ID != "i1" || got.Title != "same" || got.SortOrder != -7 || got.TimeZone != "Europe/Moscow" {
					t.Fatalf("decoded = %+v, want %s %q with other fields unchanged", got, field, tc.want)
				}
			})
		}
	}
	var missing ChecklistItem
	if err := json.Unmarshal([]byte(`{"id":"i1","title":"open"}`), &missing); err != nil || missing.CompletedTime != "" || missing.StartDate != "" {
		t.Fatalf("omitted item dates = %+v, %v", missing, err)
	}
}

func TestChecklistItemTimeRejectsUnsupportedWireValues(t *testing.T) {
	for _, value := range []string{
		`null`, `true`, `{}`, `[]`, `1.5`, `1.0`, `1e3`,
		`9223372036854775808`, `-9223372036854775809`,
		`9223372036854775807`, `-9223372036854775808`,
		`253402300800000`, `-62135596800000`,
	} {
		for _, field := range []string{"startDate", "completedTime"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				item := ChecklistItem{ID: "unchanged"}
				if err := json.Unmarshal([]byte(`{"`+field+`":`+value+`}`), &item); err == nil {
					t.Fatalf("unsupported %s decoded successfully", field)
				}
				if item.ID != "unchanged" {
					t.Fatalf("failed decode changed destination: %+v", item)
				}
			})
		}
	}

	for _, raw := range []string{
		`{"completedTime":1788955513000}`,
		`{"startDate":1789130695741}`,
		`{"dueDate":1789130695741}`,
		`{"items":[{"status":"1"}]}`,
	} {
		var task Task
		if err := json.Unmarshal([]byte(raw), &task); err == nil {
			t.Errorf("unrelated wire field was relaxed: %s", raw)
		}
	}
}

func TestGetTaskRawWithNumericItemDatesPreservesProvenance(t *testing.T) {
	body := []byte(`{"id":"t1","projectId":"p1","title":"checklist","startDate":"2026-09-11T15:44:55.741+0300","completedTime":"2026-09-09T15:05:13.123+0300","items":[{"id":"i1","title":"done","status":1,"startDate":1789130695741,"completedTime":1788955513123,"future":{"keep":true}}],"futureTask":"keep"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/open/v1/project/p1/task/t1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	got, err := testClient(t, server).GetTaskRaw(context.Background(), "p1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Raw, body) {
		t.Fatalf("wire provenance changed: %s", got.Raw)
	}
	task, warnings := TaskToModel(got.Task)
	if len(warnings) != 0 || len(task.Items) != 1 || task.Items[0].Status != model.ItemDone || task.Items[0].CompletedTime.UnixMilli() != 1788955513123 || task.Items[0].StartDate.UnixMilli() != 1789130695741 {
		t.Fatalf("task=%+v warnings=%+v", task, warnings)
	}
	if got.Task.CompletedTime != "2026-09-09T15:05:13.123+0300" {
		t.Fatalf("task timestamp changed: %q", got.Task.CompletedTime)
	}
	if got.Task.StartDate != "2026-09-11T15:44:55.741+0300" || got.Task.Items[0].StartDate != "2026-09-11T12:44:55.741+0000" {
		t.Fatalf("task startDate=%q item startDate=%q", got.Task.StartDate, got.Task.Items[0].StartDate)
	}
}
