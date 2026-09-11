package main

import (
	"bytes"
	"context"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/cli"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/model"
)

func TestCmdSchedulePreviewConsentAndDueProtection(t *testing.T) {
	isolate(t)
	seedProjects(t, model.Project{Id: "p1", Name: "Work", Kind: "TASK"})
	runArgs := func(want int, args ...string) string {
		t.Helper()
		var out, diag bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &diag)
		if code != want {
			t.Fatalf("%v: code=%d out=%s err=%s", args, code, out.String(), diag.String())
		}
		return out.String() + diag.String()
	}
	runArgs(exitOK, "add", "alpha", "-P", "Work", "--schedule", "2026-09-10 14:00 + 90min", "--zone", "UTC", "--preview")
	if len(cachedTasks(t)) != 0 {
		t.Fatal("preview created a task")
	}
	runArgs(exitOK, "add", "alpha", "-P", "Work", "--schedule", "2026-09-10 14:00 + 90min", "--zone", "UTC")
	runArgs(exitError, "add", "beta", "-P", "Work", "--schedule", "2026-09-10 13:00 + 4h", "--zone", "UTC")
	if len(cachedTasks(t)) != 1 {
		t.Fatal("unconfirmed overlap saved")
	}
	runArgs(exitOK, "add", "beta", "-P", "Work", "--schedule", "2026-09-10 13:00 + 4h", "--zone", "UTC", "--allow-overlap")
	if report := runArgs(exitOK, "schedule", "alpha", "2026-09-10 14:00 + 90min", "--zone", "UTC", "--preview"); !strings.Contains(report, "1 overlaps") {
		t.Fatal(report)
	}
	runArgs(exitError, "due", "alpha", "2026-09-10")
	before := loadDueTaskByTitle(t, "alpha")
	runArgs(exitError, "due", "alpha", "2026-09-09 14:00")

	runArgs(exitOK, "due", "alpha", "2026-09-11 14:00", "--allow-overlap")
	after := loadDueTaskByTitle(t, "alpha")
	if !after.StartDate.Equal(before.StartDate.Time) || after.TimeZone != "UTC" || after.IsAllDay || !after.DueDate.After(before.DueDate.Time) {
		t.Fatalf("end edit=%+v", after)
	}
	runArgs(exitOK, "due", "alpha", "none")
	after = loadDueTaskByTitle(t, "alpha")
	if !after.StartDate.IsZero() || !after.DueDate.IsZero() {
		t.Fatal("clear was not coupled")
	}
	runArgs(exitOK, "schedule", "alpha", "2026-11-01T01:30:00-05:00 + 90min", "--zone", "America/New_York")
	after = loadDueTaskByTitle(t, "alpha")
	if after.DueDate.Sub(after.StartDate.Time) != 90*time.Minute {
		t.Fatal("fold duration changed")
	}
}

func TestAddIntervalIncompatibleOptions(t *testing.T) {
	for _, tail := range [][]string{{"-d", "tmr"}, {"--at", "14:00"}, {"--in", "2h"}, {"--repeat", "weekly"}, {"-e", "markdown"}} {
		if _, err := parseAddOptions(append([]string{"--schedule", "14:00 + 1h"}, tail...)); err == nil {
			t.Fatalf("accepted %v", tail)
		}
	}
	if _, err := parseAddOptions([]string{"--zone", "UTC"}); err == nil {
		t.Fatal("zone without schedule")
	}
}

func TestIntervalInputErrorsFitAndEscape(t *testing.T) {
	for _, arg := range []string{"--" + strings.Repeat("long", 100), "--\x1b]52;c;secret\x07"} {
		var out, diag bytes.Buffer
		code := run(context.Background(), []string{"schedule", "task", "14:00 + 1h", arg}, strings.NewReader(""), &out, &diag)
		if code != exitUsage || strings.ContainsAny(diag.String(), "\x1b\x07") {
			t.Fatalf("unsafe refusal: %q", diag.String())
		}
		for _, line := range strings.Split(diag.String(), "\n") {
			if ansi.StringWidth(line) > cli.Width {
				t.Fatalf("wide refusal: %q", line)
			}
		}
	}
}
