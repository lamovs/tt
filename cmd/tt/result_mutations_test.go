package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/store"
)

func TestJSONTaskFieldsNoopDoesNotClaimQueuedWork(t *testing.T) {
	for _, field := range [][]string{{"--sort-order", "0"}, {"--estimated-duration", "0s"}, {"--estimated-pomo", "0"}} {
		t.Run(field[0], func(t *testing.T) {
			isolate(t)
			mvSeed(t, mvOpen(mvParcel, "p1", "Unchanged"))
			preview, code := resourceJSONRun(t, append([]string{"edit", mvParcel}, field...)...)
			if code != exitOK {
				t.Fatalf("preview: %d %s", code, preview)
			}
			id := previewIdentity(t, preview)
			result, code := runMachine(t, context.Background(), "edit", "--accept", id)
			var outcome store.TaskMutationOutcome
			if err := json.Unmarshal(result.Data, &outcome); err != nil {
				t.Fatal(err)
			}
			if code != exitOK || result.Status != "ok" || result.Meta.Pending != 0 || outcome.Changed {
				t.Fatalf("no-op: %d %+v %s", code, result, result.Data)
			}
			st, err := store.Open(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			counts, err := st.OutboxCounts(context.Background())
			if err != nil || counts.Pending != 0 {
				t.Fatalf("no-op queued work: %+v %v", counts, err)
			}
			var stdout, stderr bytes.Buffer
			code = run(context.Background(), []string{"edit", "--accept", id}, strings.NewReader(""), &stdout, &stderr)
			if code != exitOK || strings.Contains(stdout.String(), "queued") || !strings.Contains(stdout.String(), "No task changes.") {
				t.Fatalf("human no-op: %d %s %s", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestJSONTaskFieldsChangedReportsQueuedWork(t *testing.T) {
	isolate(t)
	mvSeed(t, mvOpen(mvParcel, "p1", "Changed"))
	preview, code := resourceJSONRun(t, "edit", mvParcel, "--sort-order", "1")
	if code != exitOK {
		t.Fatalf("preview: %d %s", code, preview)
	}
	result, code := runMachine(t, context.Background(), "edit", "--accept", previewIdentity(t, preview))
	var outcome store.TaskMutationOutcome
	if err := json.Unmarshal(result.Data, &outcome); err != nil {
		t.Fatal(err)
	}
	if code != exitOK || result.Status != "queued" || result.Meta.Pending != 1 || !outcome.Changed {
		t.Fatalf("changed: %d %+v %s", code, result, result.Data)
	}
}

func TestJSONFocusAcceptanceRecordedWithoutArtificialQueueCount(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	preview, code := resourceJSONRun(t, "timer", "focus", "add", "--type", "1", "--from", "2026-09-11T10:00:00Z", "--to", "2026-09-11T10:30:00Z")
	if code != exitOK {
		t.Fatalf("preview: %d %s", code, preview)
	}
	id := previewIdentity(t, preview)
	var sessionID string
	for attempt := 0; attempt < 3; attempt++ {
		result, code := runMachine(t, ctx, "timer", "focus", "add", "--accept", id)
		var session struct {
			ID       string     `json:"session_id"`
			SyncedAt *time.Time `json:"synced_at"`
		}
		if err := json.Unmarshal(result.Data, &session); err != nil {
			t.Fatal(err)
		}
		if code != exitOK || result.Status != "recorded" || result.Meta.Pending != 0 || session.ID == "" {
			t.Fatalf("recorded: %d %+v %s", code, result, result.Data)
		}
		if sessionID != "" && session.ID != sessionID {
			t.Fatal("acceptance duplicated the session")
		}
		sessionID = session.ID
		if attempt == 2 && session.SyncedAt == nil {
			t.Fatal("replay lost confirmed upload state")
		}
		if attempt == 1 {
			st, err := store.Open(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			row, send, err := st.ArmFocusUpload(ctx, sessionID, "", json.RawMessage(`{}`), []string{})
			if err != nil || !send {
				st.Close()
				t.Fatalf("arm synthetic upload: %v %v", send, err)
			}
			if err := st.ConfirmFocusUpload(ctx, row, "remote-focus", json.RawMessage(`{}`), time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)); err != nil {
				st.Close()
				t.Fatal(err)
			}
			st.Close()
		}
	}
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM focus_sessions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("sessions: %d %v", count, err)
	}
	ids, err := st.PendingFocusSessionIDs(ctx, false)
	if err != nil || len(ids) != 0 {
		t.Fatalf("confirmed replay queued upload: %v %v", ids, err)
	}
}

func TestJSONExactFocusActionsRefuseDateRangesBeforeFetch(t *testing.T) {
	isolate(t)
	for _, action := range []string{"show", "rm"} {
		for _, flag := range []string{"--from", "--to"} {
			for _, value := range []string{"2026-09-11T10:00:00Z", ""} {
				result, code := runMachine(t, context.Background(), "timer", "focus", action, "remote-id", "--type", "1", "--remote", flag, value)
				if code != exitUsage || result.Error == nil || !strings.Contains(result.Error.Message, "date range") {
					t.Fatalf("%s %s %q: %d %+v", action, flag, value, code, result)
				}
			}
		}
	}
}
