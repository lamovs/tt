package store

import "testing"

func TestReminderClearAcceptsOnlyKnownOmittedEmptyShape(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
		pass      bool
	}{
		{"cleared", `{"id":"t","projectId":"p","kind":"TEXT"}`, []string{}, true},
		{"explicit empty", `{"id":"t","projectId":"p","reminders":[]}`, []string{}, true},
		{"nonempty missing", `{"id":"t","projectId":"p","kind":"TEXT"}`, []string{"TRIGGER:PT0S"}, false},
		{"null", `{"id":"t","projectId":"p","kind":"TEXT","reminders":null}`, []string{}, false},
		{"unknown kind", `{"id":"t","projectId":"p","kind":"FUTURE"}`, []string{}, false},
		{"partial object", `{"id":"t","projectId":"p"}`, []string{}, false},
		{"wrong address", `{"id":"other","projectId":"p","kind":"TEXT"}`, []string{}, false},
		{"nonempty remains", `{"id":"t","projectId":"p","kind":"TEXT","reminders":["TRIGGER:PT0S"]}`, []string{}, false},
		{"duplicates unchanged", `{"id":"t","projectId":"p","kind":"TEXT","reminders":["TRIGGER:PT0S"]}`, []string{"TRIGGER:PT0S", "TRIGGER:PT0S"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send := FeatureSend{Metadata: FeaturePayloadMetadata{Version: 1, Fields: FeatureFields{Reminders: true}, Snapshot: &FeatureSnapshot{Reminders: &tc.want}}}
			_, err := ConfirmFeatureResponse(send, "t", "p", []byte(tc.raw))
			if (err == nil) != tc.pass {
				t.Fatalf("confirmation %v", err)
			}
		})
	}
}
