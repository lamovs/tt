package taskdoc

import (
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
)

func TestIntervalDocumentEndProtectionAndUnchangedRaw(t *testing.T) {
	i, err := dates.ParseInterval("2026-09-10 14:00 + 90min", "UTC", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	original := model.Task{Id: "task", ProjectId: "p1", Title: "before", Kind: "TEXT", StartDate: i.Start, DueDate: i.End, TimeZone: i.Zone}
	doc, err := Encode(original, model.HintsFull)
	if err != nil {
		t.Fatal(err)
	}
	next, edit, err := Apply(original, doc, false)
	if err != nil || edit.DueDate != nil || next.TimeZone != "UTC" || !next.StartDate.Equal(original.StartDate.Time) {
		t.Fatalf("unchanged=%+v %+v %v", next, edit, err)
	}
	before := FieldsOf(original).Due
	for _, value := range []string{"2026-09-10", "2026-09-09 14:00"} {
		changed := []byte(strings.Replace(string(doc), before, value, 1))
		if _, _, err := Apply(original, changed, false); err == nil {
			t.Fatalf("accepted end %s", value)
		}
	}
	changed := []byte(strings.Replace(string(doc), before, "2026-09-11 14:00", 1))
	next, edit, err = Apply(original, changed, false)
	if err != nil || edit.StartDate == nil || edit.TimeZone == nil || *edit.TimeZone != "UTC" || next.IsAllDay || !next.StartDate.Equal(original.StartDate.Time) {
		t.Fatalf("protected=%+v %+v %v", next, edit, err)
	}
}
