package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func TestIntervalCompleteAddressedReadback(t *testing.T) {
	task := intervalTask(t, "interval", "p1", "14:00 + 90min")
	want := dates.Interval{Start: task.StartDate, End: task.DueDate, Zone: task.TimeZone}
	send := FeatureSend{Metadata: FeaturePayloadMetadata{Snapshot: &FeatureSnapshot{Interval: &want}}}
	base := map[string]any{"id": "t1", "projectId": "p1", "startDate": task.StartDate.String(), "dueDate": task.DueDate.String(), "isAllDay": false, "timeZone": "UTC"}
	check := func(t *testing.T, raw map[string]any, bad bool) {
		t.Helper()
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ConfirmFeatureResponse(send, "t1", "p1", data)
		if (err != nil) != bad {
			t.Fatalf("bad=%v error=%v body=%s", bad, err, data)
		}
	}
	check(t, base, false)
	for _, field := range []string{"id", "projectId", "startDate", "dueDate", "isAllDay", "timeZone"} {
		for _, mode := range []string{"missing", "null", "mismatch"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				raw := map[string]any{}
				for k, v := range base {
					raw[k] = v
				}
				switch mode {
				case "missing":
					delete(raw, field)
				case "null":
					raw[field] = nil
				default:
					raw[field] = "wrong"
				}
				check(t, raw, true)
			})
		}
	}
	base["startDate"] = "2026-09-10T17:00:00.000+0300"
	base["dueDate"] = "2026-09-10T18:30:00.000+0300"
	check(t, base, false)
}

func TestIntervalCodecAndLegacyMasksRemainDistinct(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedProjects(t, s, model.Project{Id: "p1", Name: "P1"})
	input := intervalTask(t, "new", "p1", "14:00 + 90min")
	task, err := s.CreateTask(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = s.DB().QueryRow(`SELECT payload FROM outbox WHERE task_id=?`, task.Id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	root, meta, err := DecodeTaskPayload(raw)
	if err != nil || meta == nil || meta.Version != 3 || !meta.Fields.Interval {
		t.Fatalf("meta=%+v error=%v", meta, err)
	}
	for _, version := range []int{1, 2} {
		legacy := *meta
		legacy.Version = version
		legacy.Fields.Interval = false
		legacy.Fields.Reminders = true
		root.Reminders = []string{"TRIGGER:PT0S"}
		if version == 2 {
			legacy.Fields.Items = true
			root.Items = []model.Item{{Title: "item"}}
			legacy.ItemKeys = []string{"key"}
		}
		data, err := EncodeTaskPayload(root, legacy)
		if err != nil {
			t.Fatal(err)
		}
		_, got, err := DecodeTaskPayload(data)
		if err != nil || got.Version != version || got.Fields.Interval {
			t.Fatalf("reclassified legacy: %+v %v", got, err)
		}
	}
}
