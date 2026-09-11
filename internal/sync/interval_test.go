package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

func TestIntervalCreateReadbackRecoveryAndUpdate(t *testing.T) {
	for _, kind := range []string{"NOTE", "CHECKLIST"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "cache.db")
			st := stage1COpenStore(t, path)
			t.Cleanup(func() { st.Close() })
			seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
			fake := newStage1CWireFake(t)
			fake.SeedProject("p1", "P1")
			i, err := dates.ParseInterval("2026-09-10 14:00 + 90min", "Europe/Moscow", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			input := model.Task{ProjectId: "p1", Title: "interval", Kind: kind, StartDate: i.Start, DueDate: i.End, TimeZone: i.Zone, Reminders: []string{"TRIGGER:-PT10M"}}
			if kind == "CHECKLIST" {
				input.Items = []model.Item{{Title: "one"}, {Title: "two"}}
			}
			if _, err = st.CreateTask(ctx, input); err != nil {
				t.Fatal(err)
			}
			if m := stage1CBoundaryFeature(t, st); m.Version != 3 || !m.Fields.Interval {
				t.Fatalf("metadata=%+v", m)
			}
			fake.SetResponseHook(func(req stage1CWireRequest, res stage1CWireResponse) stage1CWireResponse {
				if req.Method == http.MethodGet {
					var body map[string]any
					if err := json.Unmarshal(res.Body, &body); err != nil {
						t.Fatal(err)
					}
					delete(body, "startDate")
					res.Body, _ = json.Marshal(body)
				}
				return res
			})
			first, _ := testSyncer(t, st, fake.Server()).Push(ctx)
			if first.Pushed != 0 || fake.Count(http.MethodPost, "/open/v1/task") != 1 {
				t.Fatalf("partial readback: %+v", first)
			}
			if m := stage1CBoundaryFeature(t, st); m.AcceptedTaskID == nil || *m.AcceptedTaskID != "remote-task-1" {
				t.Fatalf("accepted id not durable: %+v", m)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st = stage1COpenStore(t, path)
			fake.SetResponseHook(nil)
			stage1CBoundaryRetryIfFailed(t, st)
			second, err := testSyncer(t, st, fake.Server()).Push(ctx)
			if err != nil || second.Pushed != 1 || fake.Count(http.MethodPost, "/open/v1/task") != 1 {
				t.Fatalf("recovery: %+v %v", second, err)
			}
			current := stage1CTask(t, st, "remote-task-1")
			if kind == "CHECKLIST" && (len(current.Items) != 2 || current.Items[0].Id == "" || current.Items[0].Key == "") {
				t.Fatalf("interval checklist recovery=%+v", current.Items)
			}
			if !current.StartDate.Equal(i.Start.Time) || !current.DueDate.Equal(i.End.Time) || current.IsAllDay || current.TimeZone != i.Zone || current.Kind != kind {
				t.Fatalf("recovered=%+v", current)
			}
			i.End = model.NewTime(i.End.Add(time.Hour))
			edit, err := schedule.IntervalEdit(current, i)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.UpdateTaskIfUnchanged(ctx, current, edit); err != nil {
				t.Fatal(err)
			}
			if result := stage1CPush(t, st, fake); result.Pushed != 1 {
				t.Fatalf("update=%+v", result)
			}
			current = stage1CTask(t, st, current.Id)
			if !current.DueDate.Equal(i.End.Time) || current.Reminders[0] != "TRIGGER:-PT10M" {
				t.Fatalf("update=%+v", current)
			}
			for _, req := range stage1CPostRequests(fake) {
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(req.Body, &raw); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"startDate", "dueDate", "isAllDay", "timeZone"} {
					if _, ok := raw[key]; !ok {
						t.Fatalf("missing %s in %s", key, req.Body)
					}
				}
				if string(raw["isAllDay"]) != "false" {
					t.Fatalf("all-day not explicit: %s", req.Body)
				}
			}
		})
	}
}
