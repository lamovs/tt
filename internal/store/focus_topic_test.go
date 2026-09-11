package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFocusTopicSnapshotResolutionAndReplacement(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	topics := []FocusTopic{
		{ID: "study-1", Name: "Study", Status: 0, SortOrder: 2, Raw: json.RawMessage(`{"id":"study-1","name":"Study"}`)},
		{ID: "work-1", Name: "Work", Status: 0, SortOrder: 1, Raw: json.RawMessage(`{"id":"work-1","name":"Work"}`)},
	}
	if err := st.ReplaceFocusTopics(ctx, topics, "credential-a", now); err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveFocusTopic(ctx, "study")
	if err != nil || got.ID != "study-1" || got.CredentialFingerprint != "credential-a" || !got.RefreshedAt.Equal(now) {
		t.Fatalf("resolved %+v, %v", got, err)
	}
	if err := st.ReplaceFocusTopics(ctx, []FocusTopic{{ID: "reading-1", Name: "Reading", Raw: json.RawMessage(`{"id":"reading-1","name":"Reading"}`)}}, "credential-b", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.FocusTopics(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "reading-1" || rows[0].CredentialFingerprint != "credential-b" {
		t.Fatalf("replacement %+v, %v", rows, err)
	}
}

func TestFocusTopicResolutionRefusesAmbiguousName(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if err := st.ReplaceFocusTopics(ctx, []FocusTopic{
		{ID: "one", Name: "Work", Raw: json.RawMessage(`{"id":"one","name":"Work"}`)},
		{ID: "two", Name: "work", Raw: json.RawMessage(`{"id":"two","name":"work"}`)},
	}, "credential", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveFocusTopic(ctx, "WORK"); err == nil || !strings.Contains(err.Error(), "one, two") {
		t.Fatalf("ambiguity error = %v", err)
	}
	got, err := st.ResolveFocusTopic(ctx, "two")
	if err != nil || got.ID != "two" {
		t.Fatalf("exact ID %+v, %v", got, err)
	}
}

func TestTimerTopicDestinationIsFrozenInStateAndHistory(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := timerTestNow()
	options := TimerStartOptions{FocusType: 1, TopicID: "work-id", TopicName: "Work", TopicCredential: "credential-a"}
	started, err := st.StartTimer(ctx, options, now)
	if err != nil || started.State.TopicID != options.TopicID || started.State.TopicName != options.TopicName || started.State.TopicCredential != options.TopicCredential {
		t.Fatalf("start %+v, %v", started, err)
	}
	stopped, err := st.StopTimer(ctx, now.Add(2*time.Second))
	if err != nil || stopped.Completed.TopicID != options.TopicID || stopped.Completed.TopicName != options.TopicName || stopped.Completed.TopicCredential != options.TopicCredential {
		t.Fatalf("stop %+v, %v", stopped, err)
	}
	if _, err := st.StartTimer(ctx, TimerStartOptions{FocusType: 1, TaskID: "task", TopicID: "work-id", TopicName: "Work", TopicCredential: "credential-a"}, now); err == nil {
		t.Fatal("accepted task and Timer topic together")
	}
}
