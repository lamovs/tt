package app

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/taskdoc"
)

func TestIntervalDocumentReviewFailureKeepsDraft(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "overlap"}[conflict], func(t *testing.T) {
			a, st := actionStore(t)
			ctx := context.Background()
			i, err := dates.ParseInterval("2026-09-10 14:00 + 90min", "UTC", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			task, err := st.CreateTask(ctx, model.Task{Title: "editable", ProjectId: "work", Kind: "NOTE", StartDate: i.Start, DueDate: i.End, TimeZone: "UTC", Reminders: []string{"TRIGGER:-PT10M"}})
			if err != nil {
				t.Fatal(err)
			}
			task, err = st.Task(ctx, task.Id)
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				other := task
				other.Id = ""
				other.Title = "overlap"
				if _, err := st.CreateTask(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			before := dumpCache(t, st)
			doc := documentBytes(t, task)
			changed := []byte(strings.Replace(string(doc), taskdoc.FieldsOf(task).Due, "2026-09-11 14:00", 1))
			documentEditor(t, changed)
			result, err := a.Document(ctx, DocumentRequest{Original: &task, Seed: task}, strings.NewReader("yes\n"), io.Discard, io.Discard)
			if conflict {
				if err == nil || result.DraftPath == "" || result.Outcome.Changed || before != dumpCache(t, st) {
					t.Fatalf("unsafe refusal=%+v %v", result, err)
				}
				if _, err := os.Stat(result.DraftPath); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil || !result.Outcome.Changed || result.DraftPath != "" {
					t.Fatalf("save=%+v %v", result, err)
				}
				got := result.Outcome.Task
				if !got.StartDate.Equal(task.StartDate.Time) || got.TimeZone != "UTC" || got.IsAllDay || got.Reminders[0] != task.Reminders[0] || got.Kind != "NOTE" {
					t.Fatalf("lost fields=%+v", got)
				}
			}
		})
	}
}
