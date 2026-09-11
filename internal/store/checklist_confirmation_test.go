package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestRawItemsCompletedTimeCompatibility(t *testing.T) {
	for _, stamp := range []string{`1788955513123`, `"2026-09-09T15:05:13.123+0300"`} {
		raw := []byte(`{"items":[{"id":"i1","title":"done","status":1,"completedTime":` + stamp + `}]}`)
		items, present, err := decodeRawItems(raw)
		if err != nil || !present || len(items) != 1 || items[0].CompletedTime.UnixMilli() != 1788955513123 {
			t.Fatalf("stamp %s: items=%+v present=%v err=%v", stamp, items, present, err)
		}
	}
	for _, stamp := range []string{`null`, `1.5`, `1.0`, `1e3`, `true`, `[]`, `{}`, `"invalid"`, `9223372036854775808`, `-9223372036854775809`, `9223372036854775807`, `-9223372036854775808`, `253402300800000`, `-62135596800000`} {
		raw := []byte(`{"items":[{"id":"i1","title":"done","status":1,"completedTime":` + stamp + `}]}`)
		if _, _, err := decodeRawItems(raw); err == nil {
			t.Errorf("unsupported completion accepted: %s", stamp)
		}
	}
	for _, raw := range []string{
		`{"items":[{"id":"i1","title":"done","status":1,"completedTime":1788955513000,"unknown":1}]}`,
		`{"items":[{"id":"i1","title":"same","status":0},{"id":"i1","title":"same","status":0}]}`,
		`{"items":[{"id":"i1","title":"done","status":1,"startDate":1788955513000.5}]}`,
	} {
		if _, _, err := decodeRawItems([]byte(raw)); err == nil {
			t.Errorf("unsafe retained items accepted: %s", raw)
		}
	}
}

func TestChecklistConfirmationNumericStartAndClearedCompletion(t *testing.T) {
	want := []FeatureItemSnapshot{{Key: "private", ID: "i1", Title: "open", SortOrder: 1, StartDate: "2026-09-09T15:05:13.123+0300"}}
	send := FeatureSend{Metadata: FeaturePayloadMetadata{Snapshot: &FeatureSnapshot{Items: &want}}}
	for _, stamp := range []string{`1788955513123`, `"2026-09-09T12:05:13.123+0000"`} {
		raw := []byte(`{"id":"t1","projectId":"p1","items":[{"id":"i1","title":"open","status":0,"sortOrder":1,"isAllDay":false,"timeZone":"","startDate":` + stamp + `}]}`)
		confirmation, err := ConfirmFeatureResponse(send, "t1", "p1", raw)
		if err != nil || !bytes.Equal(confirmation.Raw, raw) {
			t.Fatalf("start %s with cleared completion: %+v, %v", stamp, confirmation, err)
		}
	}
	for _, stamp := range []string{`1788955513124`, `1788955513123.5`, `true`, `null`} {
		raw := []byte(`{"id":"t1","projectId":"p1","items":[{"id":"i1","title":"open","status":0,"sortOrder":1,"isAllDay":false,"timeZone":"","startDate":` + stamp + `}]}`)
		if _, err := ConfirmFeatureResponse(send, "t1", "p1", raw); err == nil {
			t.Fatalf("inexact or invalid start accepted: %s", stamp)
		}
	}
}

func TestChecklistConfirmationOptionalTimesAndParentTimeZone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantZone   string
		parentZone any
		gotZone    string
		wantError  bool
	}{
		{"parent default", "", "Europe/Moscow", "Europe/Moscow", false},
		{"UTC default", "", "UTC", "UTC", false},
		{"no default needed", "", nil, "", false},
		{"explicit exact", "Europe/Moscow", nil, "Europe/Moscow", false},
		{"different parent", "", "Europe/Moscow", "UTC", true},
		{"missing parent", "", nil, "Europe/Moscow", true},
		{"wrong parent type", "", 1, "Europe/Moscow", true},
		{"invalid parent", "", "invalid/zone", "invalid/zone", true},
		{"local alias rejected", "", "Local", "Local", true},
		{"explicit changed to parent", "UTC", "Europe/Moscow", "Europe/Moscow", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := []FeatureItemSnapshot{{Key: "private", ID: "i1", Title: "same", SortOrder: 7, TimeZone: tc.wantZone}}
			send := FeatureSend{Metadata: FeaturePayloadMetadata{Snapshot: &FeatureSnapshot{Items: &want}}}
			record := map[string]any{"id": "i1", "title": "same", "status": 0, "sortOrder": 7, "isAllDay": false, "timeZone": tc.gotZone}
			body := map[string]any{"id": "t1", "projectId": "p1", "items": []any{record}}
			if tc.parentZone != nil {
				body["timeZone"] = tc.parentZone
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(send)
			confirmation, err := ConfirmFeatureResponse(send, "t1", "p1", raw)
			if (err != nil) != tc.wantError {
				t.Fatalf("confirmation=%+v err=%v wantError=%v", confirmation, err, tc.wantError)
			}
			after, _ := json.Marshal(send)
			if !bytes.Equal(before, after) || !bytes.Equal(raw, confirmation.Raw) {
				t.Fatal("confirmation changed frozen intent or raw provenance")
			}
			if !tc.wantError && !reflect.DeepEqual(confirmation.Bindings, []FeatureItemBinding{{Key: "private", ID: "i1"}}) {
				t.Fatalf("bindings=%+v", confirmation.Bindings)
			}
		})
	}
}

func TestChecklistConfirmationDatesAndCoreFieldsRemainExact(t *testing.T) {
	base := map[string]any{"id": "i1", "title": "same", "status": 1, "sortOrder": 7, "isAllDay": false, "timeZone": "Europe/Moscow", "startDate": "2026-09-09T15:00:00.000+0300", "completedTime": int64(1788955513123)}
	want := []FeatureItemSnapshot{{Key: "private", ID: "i1", Title: "same", Status: 1, SortOrder: 7, StartDate: "2026-09-09T12:00:00.000+0000", CompletedTime: "2026-09-09T15:05:13.123+0300"}}
	send := FeatureSend{Metadata: FeaturePayloadMetadata{Snapshot: &FeatureSnapshot{Items: &want}}}
	confirm := func(t *testing.T, item map[string]any, wantError bool) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"id": "t1", "projectId": "p1", "timeZone": "Europe/Moscow", "items": []any{item}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ConfirmFeatureResponse(send, "t1", "p1", raw); (err != nil) != wantError {
			t.Fatalf("confirmation err=%v wantError=%v raw=%s", err, wantError, raw)
		}
	}
	confirm(t, base, false)
	for _, field := range []string{"id", "title", "status", "sortOrder", "isAllDay", "timeZone", "startDate", "completedTime"} {
		for _, mutation := range []string{"omitted", "null"} {
			t.Run(field+" "+mutation, func(t *testing.T) {
				item := make(map[string]any, len(base))
				for name, value := range base {
					item[name] = value
				}
				if mutation == "omitted" {
					delete(item, field)
				} else {
					item[field] = nil
				}
				confirm(t, item, true)
			})
		}
	}
	for field, value := range map[string]any{"id": "other-id", "title": "changed", "status": 0, "sortOrder": 8, "isAllDay": true, "timeZone": "UTC", "startDate": "", "completedTime": int64(1788955513124)} {
		t.Run(field+" mismatch", func(t *testing.T) {
			item := make(map[string]any, len(base))
			for name, original := range base {
				item[name] = original
			}
			item[field] = value
			confirm(t, item, true)
		})
	}
}

func TestChecklistConfirmationEmptyDatesStillRejectExplicitNull(t *testing.T) {
	want := []FeatureItemSnapshot{{Key: "private", ID: "i1", Title: "open", SortOrder: 1}}
	send := FeatureSend{Metadata: FeaturePayloadMetadata{Snapshot: &FeatureSnapshot{Items: &want}}}
	for _, field := range []string{"startDate", "completedTime"} {
		raw := []byte(`{"id":"t1","projectId":"p1","items":[{"id":"i1","title":"open","status":0,"sortOrder":1,"isAllDay":false,"timeZone":"","` + field + `":null}]}`)
		if _, err := ConfirmFeatureResponse(send, "t1", "p1", raw); err == nil {
			t.Errorf("explicit null %s confirmed an empty desired date", field)
		}
	}
}
