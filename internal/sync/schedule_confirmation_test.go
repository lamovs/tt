package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestScheduleClearRequiresReadbackBeforeSettlement(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		for _, ignored := range []bool{false, true} {
			name := "plain"
			if versioned {
				name = "with repeat"
			}
			if ignored {
				name += "/ignored"
			} else {
				name += "/cleared"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				st := testStore(t)
				prior := model.NewTime(time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC))
				seedProject(t, st, model.Project{Id: "p1", Name: "Test"})
				seedTasks(t, st, "p1", model.Task{Id: "t1", ProjectId: "p1", Title: "Clear date", Kind: "TEXT", StartDate: prior, DueDate: prior, IsAllDay: true})
				edit := model.TaskEdit{StartDate: model.NewEditTime(model.Time{}), DueDate: model.NewEditTime(model.Time{}), IsAllDay: model.Ptr(false)}
				if versioned {
					edit.RepeatFlag = model.Ptr("RRULE:FREQ=DAILY;INTERVAL=1")
				}
				if _, err := st.UpdateTask(ctx, "t1", edit); err != nil {
					t.Fatal(err)
				}
				var sent map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							t.Error(err)
						}
						w.WriteHeader(http.StatusNoContent)
						return
					}
					body := map[string]any{"id": "t1", "projectId": "p1", "kind": "TEXT", "title": "Clear date", "isAllDay": false}
					if versioned {
						body["repeatFlag"] = *edit.RepeatFlag
					}
					if ignored {
						body["dueDate"] = prior.String()
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				result, err := testSyncer(t, st, server).Push(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if sent["startDate"] != "1970-01-01T00:00:00.000+0000" || sent["dueDate"] != "1970-01-01T00:00:00.000+0000" || sent["isAllDay"] != false {
					t.Fatalf("clear wire payload = %#v", sent)
				}
				if ignored {
					if result.Pushed != 0 || stage1COutboxCount(t, st) != 1 || len(result.Errors) != 1 {
						t.Fatalf("ignored edit lost: %+v", result)
					}
				} else if result.Pushed != 1 || stage1COutboxCount(t, st) != 0 || len(result.Errors) != 0 {
					t.Fatalf("confirmed clear = %+v", result)
				}
			})
		}
	}
}
