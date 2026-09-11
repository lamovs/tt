package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func duplicateReminderCommands() map[string][]string {
	return map[string][]string{
		"remind": {"remind", "1", "TRIGGER:PT0S", "TRIGGER:-PT10M", "TRIGGER:PT0S"},
		"add": {"add", "duplicate reminders", "-P", "Personal", "--item", "same", "--item", "same",
			"--remind", "TRIGGER:PT0S", "--remind", "TRIGGER:-PT10M", "--remind", "TRIGGER:PT0S"},
	}
}

func TestDuplicateReminderCommandsPreserveCompleteState(t *testing.T) {
	for name, args := range duplicateReminderCommands() {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			id := seedFeatureCommandTask(t, model.Task{Title: "Stable", Reminders: []string{"TRIGGER:-PT1H"}})
			before := featureCommandState(t, id)
			code, stdout, stderr := runFeatureCommand(t, args...)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, "duplicate reminder trigger at position 3") {
				t.Fatalf("duplicate command = %d, stdout=%q, stderr=%q", code, stdout, stderr)
			}
			if after := featureCommandState(t, id); !reflect.DeepEqual(after, before) {
				t.Fatalf("duplicate command changed complete state\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

func TestDuplicateReminderCommandsDoNotCreateCache(t *testing.T) {
	for name, args := range duplicateReminderCommands() {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			path, err := store.DefaultPath()
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runFeatureCommand(t, args...)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, "duplicate reminder trigger at position 3") {
				t.Fatalf("duplicate without cache = %d, stdout=%q, stderr=%q", code, stdout, stderr)
			}
			if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("duplicate validation opened the cache directory: %v", err)
			}
		})
	}
}

func TestReminderListValidationPreservesDistinctValues(t *testing.T) {
	values := []string{"TRIGGER:-PT1H", "TRIGGER:PT0S", "TRIGGER:-PT10M"}
	before := slices.Clone(values)
	if err := validateReminderList(values); err != nil || !slices.Equal(values, before) {
		t.Fatalf("distinct validation = %v, values=%q", err, values)
	}
	for _, values := range [][]string{
		{"TRIGGER:PT0S", "TRIGGER:"},
		{"TRIGGER:PT0S", "TRIGGER:-PT10M\n"},
		{"TRIGGER:PT0S", "TRIGGER:PT0S"},
	} {
		if err := validateReminderList(values); err == nil {
			t.Fatalf("invalid list accepted: %q", values)
		}
	}
}

func TestUnrelatedCommandsPreserveImportedDuplicateReminders(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal", Kind: "TASK"}}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	values := []string{"TRIGGER:PT0S", "TRIGGER:-PT10M", "TRIGGER:PT0S"}
	task := model.Task{Id: "remote-task", ProjectId: "p1", Title: "Imported", Kind: "TEXT", Reminders: values}
	raw := json.RawMessage(`{"id":"remote-task","projectId":"p1","title":"Imported","kind":"TEXT","status":0,"reminders":["TRIGGER:PT0S","TRIGGER:-PT10M","TRIGGER:PT0S"]}`)
	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{Task: task, Raw: raw}}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.SetListing(ctx, []string{task.Id}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"pri", "1", "high"}, {"due", "1", "tmr"}, {"repeat", "1", "daily"}} {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("unrelated command %v = %d, %q", args, code, stderr)
		}
		if got := readFeatureTask(t, task.Id).Reminders; !slices.Equal(got, values) {
			t.Fatalf("unrelated command %v changed imported reminders: %q", args, got)
		}
	}
}
