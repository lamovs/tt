package taskdoc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestTaskDocumentExtensionsAndLegacyDraft(t *testing.T) {
	task := model.Task{Id: "t", ProjectId: "p", Title: "Task", Kind: "TEXT", ParentId: "parent", ColumnId: "column", EstimatedDuration: 90, EstimatedPomo: 2, SortOrder: 42}
	document, err := Encode(task, model.HintsNone)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(document, []byte("estimated_duration = 90"), []byte("estimated_duration = 0"), 1)
	if bytes.Equal(changed, document) {
		t.Fatalf("estimate field not encoded: %s", document)
	}
	next, edit, err := Apply(task, changed, false)
	if err != nil || next.EstimatedDuration != 0 || edit.EstimatedDuration == nil || *edit.EstimatedDuration != 0 {
		t.Fatalf("clear estimate: %+v %+v %v", next, edit, err)
	}
	var legacy []string
	for _, line := range strings.Split(string(document), "\n") {
		if strings.HasPrefix(line, "parent_id =") || strings.HasPrefix(line, "column_id =") || strings.HasPrefix(line, "estimated_duration =") || strings.HasPrefix(line, "estimated_pomo =") || strings.HasPrefix(line, "sort_order =") {
			continue
		}
		legacy = append(legacy, line)
	}
	next, edit, err = Apply(task, []byte(strings.Join(legacy, "\n")), false)
	if err != nil || !edit.IsEmpty() || next.ParentId != "parent" || next.EstimatedDuration != 90 {
		t.Fatalf("legacy draft changed extensions: %+v %+v %v", next, edit, err)
	}
}
