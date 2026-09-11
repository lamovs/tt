package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestScheduledNoteCreateCarriesAndConfirmsKind(t *testing.T) {
	for _, mode := range []string{"plain-note", "repeat-note", "reminder-note", "repeat-text"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p", Name: "Project"}}); err != nil {
				t.Fatal(err)
			}
			input := model.Task{ProjectId: "p", Title: "Created", Kind: "NOTE", Content: "  exact\r\n"}
			switch mode {
			case "repeat-note", "repeat-text":
				input.RepeatFlag = "RRULE:FREQ=WEEKLY;INTERVAL=1"
			case "reminder-note":
				input.Reminders = []string{"TRIGGER:-PT10M", "TRIGGER:PT0S"}
			}
			if mode == "repeat-text" {
				input.Kind = "TEXT"
			}
			if _, err := st.CreateTask(ctx, input); err != nil {
				t.Fatal(err)
			}
			entries, _, err := st.Claim(ctx, 1, time.Minute)
			if err != nil || len(entries) != 1 {
				t.Fatalf("claim %v %v", entries, err)
			}
			task, metadata, err := DecodeTaskPayload(entries[0].Payload)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "plain-note" {
				if metadata != nil || task.Kind != "NOTE" {
					t.Fatal("legacy plain NOTE contract changed")
				}
				return
			}
			if metadata == nil {
				t.Fatal("scheduled create not versioned")
			}
			if mode == "repeat-text" {
				if metadata.Fields.Kind || task.Kind != "" {
					t.Fatal("existing TEXT field mask changed")
				}
				return
			}
			if !metadata.Fields.Kind || task.Kind != "NOTE" {
				t.Fatalf("NOTE kind dropped: %+v %+v", task, metadata)
			}
			send, versioned, post, err := st.PrepareFeatureSend(ctx, entries[0])
			if err != nil || !versioned || !post || send.Metadata.Snapshot.Kind == nil || *send.Metadata.Snapshot.Kind != "NOTE" {
				t.Fatalf("NOTE snapshot not frozen: %+v %v", send, err)
			}
			for _, kind := range []string{"", "TEXT", "NOTE"} {
				response := map[string]any{"id": "remote-note", "projectId": "p", "repeatFlag": input.RepeatFlag, "reminders": input.Reminders}
				if kind != "" {
					response["kind"] = kind
				}
				raw, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				_, err = ConfirmFeatureResponse(send, "remote-note", "p", raw)
				if (err == nil) != (kind == "NOTE") {
					t.Fatalf("kind %q confirmation: %v", kind, err)
				}
			}
		})
	}
}
