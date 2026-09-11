package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestReminderShortcutsAndFriendlyCLIValues(t *testing.T) {
	for _, flag := range []string{"--in", "-d"} {
		for _, expr := range []string{"in 2h", "+2h"} {
			t.Run(flag+expr, func(t *testing.T) {
				isolate(t)
				seedProjects(t, model.Project{Id: "p1", Name: "Personal"})
				args := []string{"add", "Call", "-P", "Personal", flag, expr}
				if flag == "-d" {
					args = append(args, "--remind", "at")
				}
				before := time.Now()
				code, _, stderr := runFeatureCommand(t, args...)
				if code != exitOK {
					t.Fatalf("%v: %d %s", args, code, stderr)
				}
				tasks := cachedTasks(t)
				if len(tasks) != 1 || tasks[0].IsAllDay || tasks[0].DueDate.Before(before.Add(2*time.Hour-time.Second)) || tasks[0].DueDate.After(time.Now().Add(2*time.Hour)) || !slices.Equal(tasks[0].Reminders, []string{"TRIGGER:PT0S"}) {
					t.Fatalf("unexpected result: %+v", tasks)
				}
				id := tasks[0].Id
				if code, _, stderr = runFeatureCommand(t, "remind", id, "-10min", "at", "-1h"); code != exitOK {
					t.Fatalf("friendly remind: %d %s", code, stderr)
				}
				if got := readFeatureTask(t, id).Reminders; !slices.Equal(got, []string{"TRIGGER:-PT10M", "TRIGGER:PT0S", "TRIGGER:-PT1H"}) {
					t.Fatalf("order: %q", got)
				}
				if code, _, stderr = runFeatureCommand(t, "due", id, "in", "3h"); code != exitOK {
					t.Fatalf("due in: %d %s", code, stderr)
				}
			})
		}
	}
}

func TestReminderShortcutClockAndMisuseAtomicity(t *testing.T) {
	isolate(t)
	id := seedFeatureCommandTask(t, model.Task{Title: "Existing"})
	for _, expr := range []string{"18:00", "tmr 09:00"} {
		if code, _, stderr := runFeatureCommand(t, "add", "Timed", "-P", "Personal", "--at", expr); code != exitOK {
			t.Fatalf("--at %s: %d %s", expr, code, stderr)
		}
	}
	for _, args := range [][]string{
		{"add", "bad", "--in", "0h"}, {"add", "bad", "--in", "2m"}, {"add", "bad", "--at", "tmr"},
		{"add", "bad", "--in", "2h", "-d", "tmr"}, {"add", "bad", "-d", "tmr", "--in", "2h"},
		{"add", "bad", "--in", "2h", "--at", "18:00"}, {"add", "bad", "--in", "2h", "--remind", "at"},
		{"remind", id, "at", "TRIGGER:PT0S"}, {"remind", id, "-1h", "-60min"},
	} {
		before := featureCommandState(t, id)
		if code, stdout, _ := runFeatureCommand(t, args...); code != exitUsage || stdout != "" {
			t.Errorf("accepted %v: %d %s", args, code, stdout)
		}
		if !reflect.DeepEqual(featureCommandState(t, id), before) {
			t.Fatalf("invalid input wrote data: %v", args)
		}
	}
	if code, stdout, stderr := runFeatureCommand(t, "add", "Plain", "-P", "Personal", "-d", "+2h"); code != exitOK || !strings.Contains(stdout, "Plain") {
		t.Fatalf("plain due: %d %s", code, stderr)
	}
	for _, task := range cachedTasks(t) {
		if task.Title == "Plain" && len(task.Reminders) != 0 {
			t.Fatal("ordinary -d invented a reminder")
		}
	}
}

func TestScheduleHelpExplainsShortcutsAndAvailableUI(t *testing.T) {
	for verb, words := range map[string][]string{
		"add":    {"--in 2h", "--at tmr 09:00", "-d +2h --remind at", "minutes use min", "local until tt sync"},
		"due":    {"in 2h", "+2h", "+30min", "one calendar month", "not reminders"},
		"remind": {"remind 3 at", "remind 3 -10min", "not delays from now", "preserves order"},
		"ui":     {"d opens dates/repeat/reminders", "Remind at", "Alt+up/down reorders"},
	} {
		text := strings.Join(strings.Fields(helpText(t, verb)), " ")
		for _, word := range words {
			if !strings.Contains(text, word) {
				t.Errorf("help %s misses %q", verb, word)
			}
		}
	}
	isolate(t)
	code, out, err := runFeatureCommand(t, "help")
	if code != exitOK || err != "" || strings.Contains(out, "not written") || !strings.Contains(strings.Join(strings.Fields(out), " "), "tt add Call --in 2h") {
		t.Fatalf("index: %d %s %s", code, out, err)
	}
}
