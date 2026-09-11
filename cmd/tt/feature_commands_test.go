package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func seedFeatureCommandTask(t *testing.T, task model.Task) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Personal", Kind: "TASK"}}); err != nil {
		t.Fatal(err)
	}
	task.ProjectId = "p1"
	created, err := st.CreateTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetListing(ctx, []string{created.Id}); err != nil {
		t.Fatal(err)
	}
	return created.Id
}

func runFeatureCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readFeatureTask(t *testing.T, id string) model.Task {
	t.Helper()
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestFeatureCommandsApplyEveryChecklistActionAndUndoAfterReopen(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Trip plan"})
	steps := [][]string{
		{"item", "1", "add", "book", "hotel"},
		{"item", "Trip plan", "add", "book hotel"},
		{"item", id, "add", "--", "-call bank"},
		{"item", "1", "rename", "2", "book", "train"},
		{"item", "1", "done", "2"},
		{"item", "1", "undone", "2"},
		{"item", "1", "move", "3", "1"},
		{"item", "1", "remove", "2"},
	}
	for _, args := range steps {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("run(%v) = %d: %s", args, code, stderr)
		}
	}
	task := readFeatureTask(t, id)
	if task.Kind != "CHECKLIST" {
		t.Fatalf("kind = %q", task.Kind)
	}
	if got := []string{task.Items[0].Title, task.Items[1].Title}; !slices.Equal(got, []string{"-call bank", "book train"}) {
		t.Fatalf("items after actions = %q", got)
	}
	if !task.Items[1].Status.Done() {

	} else {
		t.Fatalf("reopened item is still done: %+v", task.Items[1])
	}
	if code, _, stderr := runFeatureCommand(t, "undo"); code != exitOK {
		t.Fatalf("undo = %d: %s", code, stderr)
	}
	task = readFeatureTask(t, id)
	if got := []string{task.Items[0].Title, task.Items[1].Title, task.Items[2].Title}; !slices.Equal(got, []string{"-call bank", "book hotel", "book train"}) {
		t.Fatalf("items after undo = %q", got)
	}
	for i, item := range task.Items {
		if item.Key == "" || item.SortOrder != int64(i+1) {
			t.Fatalf("item %d lost key/rank after reopen and undo: %+v", i+1, item)
		}
	}
}

func TestFeatureCommandsSetClearAndPreserveScheduleLists(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Schedule me"})
	for _, args := range [][]string{
		{"repeat", "1", "weekly"},
		{"remind", "Schedule me", "TRIGGER:-PT10M", "TRIGGER:-PT1H", "TRIGGER:PT0S"},
	} {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("run(%v) = %d: %s", args, code, stderr)
		}
	}
	task := readFeatureTask(t, id)
	if task.RepeatFlag != "RRULE:FREQ=WEEKLY;INTERVAL=1" {
		t.Fatalf("repeat = %q", task.RepeatFlag)
	}
	wantReminders := []string{"TRIGGER:-PT10M", "TRIGGER:-PT1H", "TRIGGER:PT0S"}
	if !slices.Equal(task.Reminders, wantReminders) {
		t.Fatalf("reminders = %q", task.Reminders)
	}
	if code, _, stderr := runFeatureCommand(t, "repeat", id, "RRULE:FREQ=MONTHLY;INTERVAL=2"); code != exitOK {
		t.Fatalf("raw repeat = %d: %s", code, stderr)
	}
	if code, _, stderr := runFeatureCommand(t, "remind", "1", "none"); code != exitOK {
		t.Fatalf("clear reminders = %d: %s", code, stderr)
	}
	if code, _, stderr := runFeatureCommand(t, "repeat", "1", "none"); code != exitOK {
		t.Fatalf("clear repeat = %d: %s", code, stderr)
	}
	task = readFeatureTask(t, id)
	if task.RepeatFlag != "" || len(task.Reminders) != 0 {
		t.Fatalf("schedule was not cleared: repeat=%q reminders=%q", task.RepeatFlag, task.Reminders)
	}
}

func TestParseRepeatRuleAliases(t *testing.T) {
	aliases := map[string]string{
		"daily":   "RRULE:FREQ=DAILY;INTERVAL=1",
		"weekly":  "RRULE:FREQ=WEEKLY;INTERVAL=1",
		"monthly": "RRULE:FREQ=MONTHLY;INTERVAL=1",
		"yearly":  "RRULE:FREQ=YEARLY;INTERVAL=1",
		"none":    "",
	}
	for input, want := range aliases {
		got, err := parseRepeatRule(input)
		if err != nil || got != want {
			t.Errorf("parseRepeatRule(%q) = %q, %v; want %q, nil", input, got, err, want)
		}
	}
}

func TestShowPrintsOneBasedPositionsIncludingCompletedItems(t *testing.T) {
	isolate(t)
	seedFeatureCommandTask(t, model.Task{
		Title: "Visible positions",
		Items: []model.Item{{Title: "open"}, {Title: "done", Status: model.ItemDone}},
	})
	code, stdout, stderr := runFeatureCommand(t, "show", "1")
	if code != exitOK || stderr != "" {
		t.Fatalf("show = %d, stderr=%q", code, stderr)
	}
	for _, want := range []string{"    1. [ ] open", "    2. [x] done"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show misses %q:\n%s", want, stdout)
		}
	}
}

func TestFeatureCLIFlowsThroughInterceptedSyntheticSync(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, stage1CCommandToken)
	remote := newStage1CCommandRemote(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = oauthRoundTripFunc(remote.RoundTrip)
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	if code, stdout, stderr := stage1CCommandSync(t); code != exitOK {
		t.Fatalf("initial synthetic sync = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	const taskRef = "recovery command"
	for _, args := range [][]string{
		{"item", taskRef, "add", "from CLI"},
		{"item", taskRef, "done", "1"},
		{"repeat", taskRef, "weekly"},
		{"remind", taskRef, "TRIGGER:PT0S", "TRIGGER:-PT10M"},
	} {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("run(%v) = %d: %s", args, code, stderr)
		}
	}
	if code, stdout, stderr := stage1CCommandSync(t); code != exitOK {
		t.Fatalf("feature synthetic sync = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	st := stage1CCommandOpenStore(t)
	local := stage1CCommandTask(t, st)
	counts, err := st.OutboxCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if counts != (store.OutboxCounts{}) || len(local.Items) != 2 || !local.Items[0].Status.Done() || local.Items[1].Id == "" {
		t.Fatalf("settled local feature state = %+v, counts=%+v", local, counts)
	}
	if local.RepeatFlag != "RRULE:FREQ=WEEKLY;INTERVAL=1" || !slices.Equal(local.Reminders, []string{"TRIGGER:PT0S", "TRIGGER:-PT10M"}) {
		t.Fatalf("settled local schedule = %q %q", local.RepeatFlag, local.Reminders)
	}
	remote.mu.Lock()
	remoteTask := stage1CCommandClone(remote.task).(map[string]any)
	remote.mu.Unlock()
	if remoteTask["repeatFlag"] != local.RepeatFlag || !reflect.DeepEqual(remoteTask["reminders"], []any{"TRIGGER:PT0S", "TRIGGER:-PT10M"}) {
		t.Fatalf("intercepted server state = %#v", remoteTask)
	}
	posts, idless, denied, _ := remote.counts()

	if posts != 4 || idless != 4 || denied != 0 {
		t.Fatalf("intercepted calls: posts=%d idless=%d denied=%d", posts, idless, denied)
	}

	for _, args := range [][]string{
		{"repeat", taskRef, "none"},
		{"remind", taskRef, "none"},
		{"item", taskRef, "undone", "1"},
		{"item", taskRef, "remove", "2"},
	} {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("clear run(%v) = %d: %s", args, code, stderr)
		}
	}
	if code, stdout, stderr := stage1CCommandSync(t); code != exitOK {
		t.Fatalf("clear synthetic sync = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	local = readFeatureTask(t, stage1CCommandTaskID)
	if local.RepeatFlag != "" || len(local.Reminders) != 0 || len(local.Items) != 1 || local.Items[0].Status.Done() {
		t.Fatalf("settled clear state = %+v", local)
	}
}

func TestAddCreatesCombinedFeatureTaskAndDateStopsAtNewFlags(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: "Personal", Kind: "TASK"})
	args := []string{"add", "Trip", "-P", "Personal", "-d", "fri", "18:00", "--item", "passport", "--item", "passport", "--repeat", "daily", "--remind", "TRIGGER:PT0S", "--remind", "TRIGGER:-PT10M"}
	if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
		t.Fatalf("combined add = %d: %s", code, stderr)
	}
	tasks := cachedTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	task := readFeatureTask(t, tasks[0].Id)
	if task.IsAllDay || task.DueDate.IsZero() || task.Kind != "CHECKLIST" {
		t.Fatalf("date/kind = %+v", task)
	}
	if task.RepeatFlag != "RRULE:FREQ=DAILY;INTERVAL=1" || !slices.Equal(task.Reminders, []string{"TRIGGER:PT0S", "TRIGGER:-PT10M"}) {
		t.Fatalf("schedule = %q %q", task.RepeatFlag, task.Reminders)
	}
	if len(task.Items) != 2 || task.Items[0].Title != "passport" || task.Items[1].Title != "passport" || task.Items[0].Key == task.Items[1].Key || task.Items[0].SortOrder != 1 || task.Items[1].SortOrder != 2 {
		t.Fatalf("items = %+v", task.Items)
	}
}

type commandState struct {
	Task   model.Task
	Tables map[string][][]string
}

func featureCommandState(t *testing.T, id string) commandState {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := commandState{Tables: make(map[string][][]string)}
	if state.Task, err = st.Task(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"projects", "tasks", "items", "outbox", "events", "undo_log",
		"item_identities", "meta", "listing",
	} {
		rows, err := st.DB().QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			record := make([]string, len(values))
			for i, value := range values {
				record[i] = fmt.Sprintf("%T:%v", value, value)
			}
			state.Tables[table] = append(state.Tables[table], record)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestFeatureCommandsReportExactMutationResults(t *testing.T) {
	completed := model.NewTime(time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		name string
		task model.Task
		args []string
		want string
	}{
		{"item add", model.Task{Title: "Stable"}, []string{"item", "1", "add", "new"}, "\"Stable\": checklist add applied\n"},
		{"item rename", model.Task{Title: "Stable", Items: []model.Item{{Title: "old"}}}, []string{"item", "1", "rename", "1", "new"}, "\"Stable\": checklist rename applied\n"},
		{"item done", model.Task{Title: "Stable", Items: []model.Item{{Title: "one"}}}, []string{"item", "1", "done", "1"}, "\"Stable\": checklist done applied\n"},
		{"item undone", model.Task{Title: "Stable", Items: []model.Item{{Title: "one", Status: model.ItemDone, CompletedTime: completed}}}, []string{"item", "1", "undone", "1"}, "\"Stable\": checklist undone applied\n"},
		{"item remove", model.Task{Title: "Stable", Items: []model.Item{{Title: "one"}}}, []string{"item", "1", "remove", "1"}, "\"Stable\": checklist remove applied\n"},
		{"item move", model.Task{Title: "Stable", Items: []model.Item{{Title: "one"}, {Title: "two"}}}, []string{"item", "1", "move", "1", "2"}, "\"Stable\": checklist move applied\n"},
		{"repeat set", model.Task{Title: "Stable"}, []string{"repeat", "1", "daily"}, "\"Stable\": repeat set\n"},
		{"repeat clear", model.Task{Title: "Stable", RepeatFlag: "RRULE:FREQ=DAILY;INTERVAL=1"}, []string{"repeat", "1", "none"}, "\"Stable\": repeat cleared\n"},
		{"reminders set", model.Task{Title: "Stable"}, []string{"remind", "1", "TRIGGER:PT0S"}, "\"Stable\": 1 reminders set\n"},
		{"reminders clear", model.Task{Title: "Stable", Reminders: []string{"TRIGGER:PT0S"}}, []string{"remind", "1", "none"}, "\"Stable\": reminders cleared\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedFeatureCommandTask(t, tc.task)
			code, stdout, stderr := runFeatureCommand(t, tc.args...)
			if code != exitOK || stderr != "" || stdout != tc.want {
				t.Fatalf("run(%v) = %d, stdout=%q, stderr=%q; want %q", tc.args, code, stdout, stderr, tc.want)
			}
		})
	}
}

func TestFeatureCommandInitialAndRepeatedNoopsReportUnchanged(t *testing.T) {
	completed := model.NewTime(time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		name string
		task model.Task
		args []string
		want string
	}{
		{"rename", model.Task{Title: "Stable", Items: []model.Item{{Title: "same"}}}, []string{"item", "1", "rename", "1", "same"}, "\"Stable\": checklist rename unchanged\n"},
		{"done", model.Task{Title: "Stable", Items: []model.Item{{Title: "same", Status: model.ItemDone, CompletedTime: completed}}}, []string{"item", "1", "done", "1"}, "\"Stable\": checklist done unchanged\n"},
		{"undone", model.Task{Title: "Stable", Items: []model.Item{{Title: "same"}}}, []string{"item", "1", "undone", "1"}, "\"Stable\": checklist undone unchanged\n"},
		{"move", model.Task{Title: "Stable", Items: []model.Item{{Title: "same"}}}, []string{"item", "1", "move", "1", "1"}, "\"Stable\": checklist move unchanged\n"},
		{"repeat set", model.Task{Title: "Stable", RepeatFlag: "RRULE:FREQ=DAILY;INTERVAL=1"}, []string{"repeat", "1", "daily"}, "\"Stable\": repeat unchanged\n"},
		{"repeat clear", model.Task{Title: "Stable"}, []string{"repeat", "1", "none"}, "\"Stable\": repeat unchanged\n"},
		{"reminders set", model.Task{Title: "Stable", Reminders: []string{"TRIGGER:PT0S"}}, []string{"remind", "1", "TRIGGER:PT0S"}, "\"Stable\": reminders unchanged\n"},
		{"reminders clear", model.Task{Title: "Stable"}, []string{"remind", "1", "none"}, "\"Stable\": reminders unchanged\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			id := seedFeatureCommandTask(t, tc.task)
			before := featureCommandState(t, id)
			for run := 1; run <= 2; run++ {
				code, stdout, stderr := runFeatureCommand(t, tc.args...)
				if code != exitOK || stderr != "" || stdout != tc.want {
					t.Fatalf("run %d of %v = %d, stdout=%q, stderr=%q; want %q", run, tc.args, code, stdout, stderr, tc.want)
				}
				after := featureCommandState(t, id)
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("no-op run %d changed complete state:\nbefore=%+v\nafter=%+v", run, before, after)
				}
			}
		})
	}
}

func TestFeatureCommandFailuresAndCancellationPrintNoSuccess(t *testing.T) {
	t.Run("transaction failure", func(t *testing.T) {
		isolate(t)
		seedFeatureCommandTask(t, model.Task{Title: "Stable", Items: []model.Item{{Title: "one"}}})
		code, stdout, stderr := runFeatureCommand(t, "item", "1", "done", "2")
		if code != exitError || stdout != "" || stderr == "" {
			t.Fatalf("failed command = %d, stdout=%q, stderr=%q", code, stdout, stderr)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		isolate(t)
		seedFeatureCommandTask(t, model.Task{Title: "Stable", Items: []model.Item{{Title: "one"}}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{"item", "1", "done", "1"}, strings.NewReader(""), &stdout, &stderr)
		if code != exitInterrupted || stdout.String() != "" {
			t.Fatalf("cancelled command = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestFeatureCommandNoopsWriteNothing(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Stable", Items: []model.Item{{Title: "same"}}})
	for _, args := range [][]string{
		{"item", "1", "rename", "1", "same"},
		{"item", "1", "undone", "1"},
		{"item", "1", "move", "1", "1"},
		{"repeat", "1", "none"},
		{"remind", "1", "none"},
	} {
		before := featureCommandState(t, id)
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("run(%v) = %d: %s", args, code, stderr)
		}
		after := featureCommandState(t, id)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("no-op %v changed state:\nbefore=%+v\nafter=%+v", args, before, after)
		}
	}
	for _, args := range [][]string{
		{"item", "1", "done", "1"},
		{"repeat", "1", "daily"},
		{"remind", "1", "TRIGGER:PT0S", "TRIGGER:-PT10M"},
	} {
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("initial run(%v) = %d: %s", args, code, stderr)
		}
		before := featureCommandState(t, id)
		if code, _, stderr := runFeatureCommand(t, args...); code != exitOK {
			t.Fatalf("repeated run(%v) = %d: %s", args, code, stderr)
		}
		after := featureCommandState(t, id)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("repeated no-op %v changed state:\nbefore=%+v\nafter=%+v", args, before, after)
		}
	}
}

func TestFeatureCommandMisuseAndPositionsDoNotMutate(t *testing.T) {
	cases := [][]string{
		{"item"}, {"item", "1"}, {"item", "1", "bogus"},
		{"item", "1", "add"}, {"item", "1", "rename", "1"},
		{"item", "1", "done", "0"}, {"item", "1", "done", "--", "-1"},
		{"item", "1", "done", "x"}, {"item", "1", "done", "1", "extra"},
		{"item", "1", "move", "1"}, {"item", "1", "move", "1", "2", "extra"},
		{"item", "1", "done", "1", "--bogus"},
		{"repeat"}, {"repeat", "1"}, {"repeat", "1", "daily", "extra"},
		{"repeat", "1", "RRULE:"}, {"repeat", "1", "RRULE:FREQ=DAILY\nBAD"},
		{"repeat", "1", "daily", "--bogus"},
		{"remind"}, {"remind", "1"}, {"remind", "1", "none", "TRIGGER:PT0S"},
		{"remind", "1", "TRIGGER:"}, {"remind", "1", "TRIGGER:PT0S\tbad"},
		{"remind", "1", "TRIGGER:PT0S", "--bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			id := seedFeatureCommandTask(t, model.Task{Title: "One", Items: []model.Item{{Title: "first"}}})
			before := featureCommandState(t, id)
			if code, _, stderr := runFeatureCommand(t, args...); code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d: %s", args, code, exitUsage, stderr)
			}
			if after := featureCommandState(t, id); !reflect.DeepEqual(after, before) {
				t.Fatalf("misuse changed state: %+v -> %+v", before, after)
			}
		})
	}
	for _, args := range [][]string{{"item", "1", "done", "2"}, {"item", "1", "move", "1", "2"}} {
		t.Run("out of range "+strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			id := seedFeatureCommandTask(t, model.Task{Title: "One", Items: []model.Item{{Title: "first"}}})
			before := featureCommandState(t, id)
			if code, _, stderr := runFeatureCommand(t, args...); code != exitError {
				t.Fatalf("run(%v) = %d, want %d: %s", args, code, exitError, stderr)
			}
			if after := featureCommandState(t, id); !reflect.DeepEqual(after, before) {
				t.Fatalf("out-of-range command changed state: %+v -> %+v", before, after)
			}
		})
	}
}

func TestFeatureCommandsRefuseAmbiguityAndNoteItems(t *testing.T) {
	t.Run("ambiguous title", func(t *testing.T) {
		isolate(t)
		seedFeatureCommandTask(t, model.Task{Title: "alpha one"})
		ctx := context.Background()
		st, err := store.Open(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "alpha two"}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if code, _, _ := runFeatureCommand(t, "item", "alpha", "add", "x"); code != exitError {
			t.Fatalf("ambiguous item exit = %d", code)
		}
	})
	t.Run("NOTE refusal", func(t *testing.T) {
		isolate(t)
		id := seedFeatureCommandTask(t, model.Task{Title: "A note", Kind: "NOTE"})
		before := featureCommandState(t, id)
		code, _, stderr := runFeatureCommand(t, "item", "1", "add", "x")
		if code != exitError || !strings.Contains(stderr, "not an editable checklist") {
			t.Fatalf("NOTE item = %d, %q", code, stderr)
		}
		if after := featureCommandState(t, id); !reflect.DeepEqual(after, before) {
			t.Fatalf("NOTE refusal changed state: %+v -> %+v", before, after)
		}
	})
}

func TestAddFeatureFlagsRejectMalformedAndDuplicateValues(t *testing.T) {
	cases := [][]string{
		{"add", "x", "--item"}, {"add", "x", "--item", "   "},
		{"add", "x", "--repeat"}, {"add", "x", "--repeat", "daily", "--repeat", "weekly"},
		{"add", "x", "--repeat", "RRULE:"}, {"add", "x", "--repeat", "RRULE:FREQ=DAILY\nBAD"},
		{"add", "x", "--remind"}, {"add", "x", "--remind", "none"},
		{"add", "x", "--remind", "TRIGGER:"}, {"add", "x", "--remind", "TRIGGER:PT0S\tbad"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolate(t)
			seedProjects(t, model.Project{Id: "p1", Name: "Personal"})
			if code, _, stderr := runFeatureCommand(t, args...); code != exitUsage {
				t.Fatalf("run(%v) = %d, want %d: %s", args, code, exitUsage, stderr)
			}
			if len(cachedTasks(t)) != 0 {
				t.Fatal("malformed add wrote a task")
			}
		})
	}
}

func TestFeatureHelpAndIndexDescribeTheGrammar(t *testing.T) {
	for verb, words := range map[string][]string{
		"item":   {"1-based", "including completed", "NOTE", "--"},
		"repeat": {"RRULE:", "semicolons", "server validates", "repeatFrom"},
		"remind": {"TRIGGER:PT0S", "replaces", "none must stand alone"},
		"add":    {"--item", "--repeat", "--remind"},
	} {
		text := strings.Join(strings.Fields(helpText(t, verb)), " ")
		for _, word := range words {
			if !strings.Contains(text, word) {
				t.Errorf("tt help %s misses %q:\n%s", verb, word, text)
			}
		}
		if strings.Contains(text, "not here yet") {
			t.Errorf("tt help %s retains obsolete unavailable wording", verb)
		}
	}
	isolate(t)
	code, stdout, stderr := runFeatureCommand(t)
	if code != exitOK || stderr != "" {
		t.Fatalf("index = %d, stderr=%q", code, stderr)
	}
	for _, verb := range []string{"item", "repeat", "remind"} {
		if !strings.Contains(stdout, verb) {
			t.Errorf("index misses %s:\n%s", verb, stdout)
		}
	}
}
