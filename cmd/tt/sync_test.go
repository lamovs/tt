package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/sync"
)

func TestParseSyncArgs(t *testing.T) {
	cases := map[string]struct {
		args []string
		want syncArgs
		err  string
	}{
		"nothing at all": {nil, syncArgs{}, ""},
		"the flag":       {[]string{"--retry-failed"}, syncArgs{retryFailed: true}, ""},
		"the flag twice": {[]string{"--retry-failed", "--retry-failed"}, syncArgs{retryFailed: true}, ""},
		"the drop flag":  {[]string{"--allow-project-drop"}, syncArgs{allowProjectDrop: true}, ""},
		"both flags": {[]string{"--retry-failed", "--allow-project-drop"},
			syncArgs{retryFailed: true, allowProjectDrop: true}, ""},
		"the parked drop flag": {[]string{"--drop-parked"}, syncArgs{dropParked: true}, ""},
		"the parked drop flag beside the project one": {[]string{"--drop-parked", "--allow-project-drop"},
			syncArgs{dropParked: true, allowProjectDrop: true}, ""},
		"both ways out of the parked state": {[]string{"--retry-failed", "--drop-parked"}, syncArgs{},
			"--retry-failed and --drop-parked ask opposite things of the same entries"},
		"both ways out, written the other way round": {[]string{"--drop-parked", "--retry-failed"}, syncArgs{},
			"--retry-failed and --drop-parked ask opposite things of the same entries"},
		"the parked drop flag with a value": {[]string{"--drop-parked=yes"}, syncArgs{},
			`unknown option "--drop-parked=yes"`},
		"the flag with a value": {[]string{"--retry-failed=true"}, syncArgs{},
			`unknown option "--retry-failed=true"`},
		"the drop flag with a value": {[]string{"--allow-project-drop=yes"}, syncArgs{},
			`unknown option "--allow-project-drop=yes"`},
		"an unknown option beside it": {[]string{"--retry-failed", "--force"}, syncArgs{},
			`unknown option "--force"`},
		"an unknown option alone": {[]string{"--force"}, syncArgs{},
			`unknown option "--force"`},
		"a word before it": {[]string{"now", "--retry-failed"}, syncArgs{},
			`unexpected argument "now"`},

		"a word after it": {[]string{"--retry-failed", "now"}, syncArgs{},
			`unknown option "now"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseSyncArgs(c.args)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("parseSyncArgs(%q) = %v, want it accepted", c.args, err)
			case c.err != "" && err == nil:
				t.Fatalf("parseSyncArgs(%q) was accepted, want %q", c.args, c.err)
			case c.err != "" && err.Error() != c.err:
				t.Fatalf("parseSyncArgs(%q) = %q, want %q", c.args, err, c.err)
			}
			if got != c.want {
				t.Errorf("parseSyncArgs(%q) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestWarnAboutParkedCreates(t *testing.T) {
	var quiet strings.Builder
	warnAboutParkedCreates(&quiet, 0)
	if quiet.String() != "" {
		t.Errorf("with no parked creates it said %q, want nothing", quiet.String())
	}

	var out strings.Builder
	warnAboutParkedCreates(&out, 3)
	got := out.String()
	for _, want := range []string{"3 parked create(s)", "cannot be undone", "second copy", "may have got through"} {
		if !strings.Contains(got, want) {
			t.Errorf("the caution reads %q, want it to carry %q", got, want)
		}
	}
	if strings.Contains(got, "is parked when") || strings.Contains(got, "it is unknown whether") {
		t.Errorf("the caution reads %q, want it to say what is true of any parked create rather "+
			"than a cause that is the cause of some", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("the caution reads %q, want it to end its last line", got)
	}
}

func TestRetryHintNamesAFlagThatParses(t *testing.T) {
	const flag = "--retry-failed"
	if !strings.Contains(sync.RetryHint, flag) {
		t.Errorf("sync.RetryHint = %q, want it to name %s", sync.RetryHint, flag)
	}
	got, err := parseSyncArgs([]string{flag})
	if err != nil || !got.retryFailed {
		t.Errorf("parseSyncArgs([%s]) = %v, %v; want the flag the hint tells people to type", flag, got, err)
	}
}

func TestDropParkedHintNamesAFlagThatParses(t *testing.T) {
	const flag = "--drop-parked"
	if !strings.Contains(dropParkedHint, flag) {
		t.Errorf("dropParkedHint = %q, want it to name %s", dropParkedHint, flag)
	}
	got, err := parseSyncArgs([]string{flag})
	if err != nil || !got.dropParked {
		t.Errorf("parseSyncArgs([%s]) = %v, %v; want the flag the hint tells people to type", flag, got, err)
	}
}

func TestWarnAboutDroppedCreates(t *testing.T) {
	var quiet strings.Builder
	warnAboutDroppedCreates(&quiet, 0)
	if quiet.String() != "" {
		t.Errorf("with no parked creates it said %q, want nothing", quiet.String())
	}

	var out strings.Builder
	warnAboutDroppedCreates(&out, 2)
	got := out.String()
	for _, want := range []string{
		"2 parked create(s)", "cannot be undone", "takes the task with it",
		"the server may hold a copy of its own", "had been taken up",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the caution reads %q, want it to carry %q", got, want)
		}
	}

	if strings.Contains(got, "gone out") || strings.Contains(got, "which of the two") {
		t.Errorf("the caution reads %q, want it to promise no more than the record tells apart", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("the caution reads %q, want it to end its last line", got)
	}
}

func TestPrintDroppedMutations(t *testing.T) {
	queued := time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local)
	parked := time.Date(2026, 9, 1, 10, 15, 40, 0, time.Local)
	cases := map[string]struct {
		dropped []store.DroppedMutation
		want    []string
		absent  []string
	}{

		"nothing was parked": {
			nil,
			[]string{"nothing was parked, so nothing was thrown away"},
			[]string{"threw away"},
		},
		"a change to a task the server already has": {
			[]store.DroppedMutation{{
				Seq: 12, Op: store.OpTaskUpdate, TaskID: "srv-1", ProjectID: "p1", Title: "Buy milk",
				QueuedAt: queued, ParkedAt: parked, Attempts: 3, Reason: "400 bad request",
			}},
			[]string{
				"threw away 1 parked entry(s)", "entry 12", store.OpTaskUpdate, `"Buy milk"`,
				"queued 2026-09-01 10:12:03", "parked 2026-09-01 10:15:40", "3 attempt(s)",
				droppedReasonOmission, "back in line with what the server has",
			},

			[]string{"the task itself went with it", "the list it was in"},
		},

		"a create whose task went with it": {
			[]store.DroppedMutation{{
				Seq: 3, Op: store.OpTaskCreate, TaskID: "local-9f3a", ProjectID: "p1",
				Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked, Attempts: 1,
				Reason: "the answer was lost", Requested: true, TaskRemoved: true,
			}},

			[]string{
				"the task itself went with it", "taken up for sending",
				"is not something this", "record can say.", droppedReasonOmission,
				"under an id nothing here has ever seen",
				`the list it was in: "Personal"`,
			},
			[]string{"back in line with what the server has", "the server never had"},
		},

		"a create the server refused": {
			[]store.DroppedMutation{{
				Seq: 14, Op: store.OpTaskCreate, TaskID: "local-5c21", ProjectID: "p1",
				Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked, Attempts: 1,
				Reason:    "outbox 14: create task local-5c21: ticktick: POST /open/v1/task: http 400: bad request",
				Requested: true, TaskRemoved: true,
			}},
			[]string{
				droppedReasonOmission, "the task itself went with it",
				"taken up for sending", "is not something this",
				"record can say.",
			},

			[]string{"coming back to settle", "the server never had", "was never sent"},
		},

		"a create taken up for sending with no reason kept": {
			[]store.DroppedMutation{{
				Seq: 15, Op: store.OpTaskCreate, TaskID: "local-6d32", ProjectID: "p1",
				Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked, Attempts: 1,
				Requested: true, TaskRemoved: true,
			}},
			[]string{"taken up for sending", "is not something this", "record can say."},
			[]string{droppedReasonOmission, "parked because", "the reason above", "the server never had"},
		},

		"a create that never went out": {
			[]store.DroppedMutation{{
				Seq: 7, Op: store.OpTaskCreate, TaskID: "local-2b8e", ProjectID: "p1",
				Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked,
				TaskRemoved: true,
			}},
			[]string{
				"the task itself went with it",
				"nothing here records the create as ever having been",
				"as far as this cache knows the server never",
			},
			[]string{"is not something this", "back in line with what the server has"},
		},

		"a create for a task the user had already deleted": {
			[]store.DroppedMutation{{
				Seq: 11, Op: store.OpTaskCreate, TaskID: "local-4d17", ProjectID: "p1",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1, Requested: true,
			}},
			[]string{"entry 11", "task local-4d17"},
			[]string{"back in line with what the server has", "the task itself went with it"},
		},

		"a reason carrying the server's own lines": {
			[]store.DroppedMutation{{
				Seq: 13, Op: store.OpTaskUpdate, TaskID: "srv-4", Title: "Buy milk",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1,
				Reason: "http 400: bad\n  entry 999: task.create \"anything at all\"\n",
			}},
			[]string{droppedReasonOmission},
			[]string{"http 400: bad", "entry 999: task.create"},
		},

		"an id carrying the server's own lines": {
			[]store.DroppedMutation{{
				Seq: 18, Op: store.OpTaskCreate, TaskID: "srv\n  entry 999: task.create \"anything\"",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1,
			}},
			[]string{`task srv\n  entry 999`},
			[]string{"\n  entry 999: task.create"},
		},

		"an op with a line break in it": {
			[]store.DroppedMutation{{
				Seq: 19, Op: "task.create\n  entry 999: task.delete", TaskID: "srv-7",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1,
			}},
			[]string{`entry 19: "task.create\n  entry 999: task.delete" task srv-7`},
			[]string{"\n  entry 999: task.delete"},
		},

		"a task gone from a project the cache cannot name": {
			[]store.DroppedMutation{{
				Seq: 9, Op: store.OpTaskCreate, TaskID: "local-77b1", ProjectID: "p-unknown",
				Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked, Attempts: 1,
				TaskRemoved: true,
			}},
			[]string{"the list it was in: p-unknown"},
			[]string{`"p-unknown"`},
		},

		"a task gone from no project at all": {
			[]store.DroppedMutation{{
				Seq: 10, Op: store.OpTaskCreate, TaskID: "local-88c2", Title: "Call the dentist",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1, TaskRemoved: true,
			}},
			[]string{"the task itself went with it"},
			[]string{"the list it was in"},
		},

		"a create and an edit of the same offline task": {
			[]store.DroppedMutation{
				{Seq: 3, Op: store.OpTaskCreate, TaskID: "local-9f3a", Title: "Call the dentist",
					QueuedAt: queued, ParkedAt: parked, Attempts: 1, TaskRemoved: true},
				{Seq: 4, Op: store.OpTaskUpdate, TaskID: "local-9f3a", Title: "Call the dentist",
					QueuedAt: queued, ParkedAt: parked, Attempts: 1},
			},
			[]string{"threw away 2 parked entry(s)", "entry 3", "entry 4"},
			[]string{"back in line with what the server has"},
		},

		"an offline create beside a change to a task the server has": {
			[]store.DroppedMutation{
				{Seq: 16, Op: store.OpTaskCreate, TaskID: "local-7e43", ProjectID: "p1",
					Title: "Call the dentist", QueuedAt: queued, ParkedAt: parked, Attempts: 1,
					Requested: true, TaskRemoved: true},
				{Seq: 17, Op: store.OpTaskUpdate, TaskID: "srv-6", ProjectID: "p1", Title: "Buy milk",
					QueuedAt: queued, ParkedAt: parked, Attempts: 1},
			},
			[]string{
				"the task itself went with it",
				"the changes above that were for tasks the server knows",
				"back in line with what the server has",
			},
			[]string{"the changes above will not go out"},
		},

		"an entry with no cached title": {
			[]store.DroppedMutation{{
				Seq: 8, Op: store.OpTaskDelete, TaskID: "srv-9",
				QueuedAt: queued, ParkedAt: parked, Attempts: 2,
			}},
			[]string{"entry 8", store.OpTaskDelete, "task srv-9"},
			nil,
		},

		"an entry parked before the moment was recorded": {
			[]store.DroppedMutation{{
				Seq: 5, Op: store.OpTaskUpdate, TaskID: "srv-2", Title: "Water the plants",
				QueuedAt: queued, Attempts: 4,
			}},
			[]string{"queued 2026-09-01 10:12:03", "does not record", "4 attempt(s)"},
			[]string{"parked 2026-09-01"},
		},

		"a title with a line break in it": {
			[]store.DroppedMutation{{
				Seq: 6, Op: store.OpTaskUpdate, TaskID: "srv-3", Title: "two\nlines",
				QueuedAt: queued, ParkedAt: parked, Attempts: 1,
			}},
			[]string{`"two\nlines"`},
			nil,
		},
	}

	names := func(id string) string {
		if id == "p1" {
			return "Personal"
		}
		return ""
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			printDroppedMutations(&out, c.dropped, names)
			got := out.String()
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("printed %q, want it to carry %q", got, w)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(got, a) {
					t.Errorf("printed %q, want it to leave out %q", got, a)
				}
			}

			for _, m := range c.dropped {
				if !strings.Contains(got, fmt.Sprintf("entry %d:", m.Seq)) {
					t.Errorf("printed %q, want entry %d named", got, m.Seq)
				}
			}
			if got != "" && !strings.HasSuffix(got, "\n") {
				t.Errorf("printed %q, want it to end its last line", got)
			}
		})
	}
}

func TestPrintSyncResultNamesWhatWasKept(t *testing.T) {
	var out strings.Builder
	printSyncResult(&out, sync.Result{Pulled: 5, Projects: 2, Deleted: 1, Kept: 3})
	got := out.String()
	if !strings.Contains(got, "3 kept") {
		t.Errorf("printed %q, want the kept count on the pull line", got)
	}
}

func TestPrintSyncResultNamesWhatTheProjectDropTookForGood(t *testing.T) {
	var quiet strings.Builder
	printSyncResult(&quiet, sync.Result{Pulled: 12, Projects: 3, Deleted: 4})
	if strings.Contains(quiet.String(), "completed") {
		t.Errorf("an ordinary pass printed %q, want no line about a loss it did not cause", quiet.String())
	}

	var out strings.Builder
	printSyncResult(&out, sync.Result{Pulled: 12, Projects: 1, Deleted: 4, CompletedDropped: 3})
	got := out.String()
	for _, want := range []string{"4 deleted", "3 completed task(s)", "nothing brings those back"} {
		if !strings.Contains(got, want) {
			t.Errorf("printed %q, want it to carry %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("printed %q, want it to end its last line", got)
	}
}

func TestPrintSyncResultNamesWhatWasParkedWithoutBeingSent(t *testing.T) {
	var quiet strings.Builder
	printSyncResult(&quiet, sync.Result{Pushed: 2, Failed: 1})
	if strings.Contains(quiet.String(), "parked") {
		t.Errorf("a pass that parked only what it sent printed %q, want no line about the other sort",
			quiet.String())
	}

	var out strings.Builder
	printSyncResult(&out, sync.Result{Pushed: 2, Failed: 3, ParkedUnsent: 2})
	got := out.String()
	for _, want := range []string{"failed 3", "2 of those were parked before the pass could send them", sync.RetryHint} {
		if !strings.Contains(got, want) {
			t.Errorf("printed %q, want it to carry %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("printed %q, want it to end its last line", got)
	}
}

func TestSummariseDrop(t *testing.T) {
	var quiet strings.Builder
	summariseDrop(&quiet, nil)
	if quiet.String() != "" {
		t.Errorf("with nothing parked it said %q, want nothing: the record has its own line", quiet.String())
	}

	cases := map[string]struct {
		dropped []store.DroppedMutation
		want    string
	}{

		"changes only": {
			[]store.DroppedMutation{
				{Seq: 1, Op: store.OpTaskUpdate, TaskID: "srv-1"},
				{Seq: 2, Op: store.OpTaskDelete, TaskID: "srv-2"},
			},
			"tt: sync: 2 parked entry(s) thrown away, 0 task(s) went with them - see stdout\n",
		},
		"a task among them": {
			[]store.DroppedMutation{
				{Seq: 1, Op: store.OpTaskCreate, TaskID: "local-1", TaskRemoved: true},
				{Seq: 2, Op: store.OpTaskUpdate, TaskID: "local-1"},
			},
			"tt: sync: 2 parked entry(s) thrown away, 1 task(s) went with them - see stdout\n",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			summariseDrop(&out, c.dropped)
			if got := out.String(); got != c.want {
				t.Errorf("summarised as %q, want %q", got, c.want)
			}
		})
	}
}

func seedParkedOfflineWork(t *testing.T) (id, title string) {
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
	created, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Call the dentist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, created.Id, model.TaskEdit{Content: model.Ptr("before Friday")}); err != nil {
		t.Fatal(err)
	}

	var parked int
	for {
		items, _, err := st.Claim(ctx, 10, time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if err := st.MarkFailed(ctx, it.Seq, it.LeaseToken, "the answer was lost"); err != nil {
				t.Fatal(err)
			}
			parked++
		}
	}
	if parked != 2 {
		t.Fatalf("parked %d entr(y/ies); want the create and the edit", parked)
	}
	return created.Id, created.Title
}

func TestCmdSyncDropsParkedAndSaysWhatWentAway(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	writeTokenFile(t, "test-token")
	id, title := seedParkedOfflineWork(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &cancelOnWrite{cancel: cancel}
	var stderr strings.Builder
	if code := cmdSync(ctx, stdout, &stderr, []string{"--drop-parked"}); code != exitInterrupted {
		t.Errorf("cmdSync = %d, want %d", code, exitInterrupted)
	}

	out := stdout.buf.String()
	if !strings.Contains(out, "threw away 2 parked entry(s)") {
		t.Fatalf("stdout = %q, want the count of what was thrown away", out)
	}
	for _, want := range []string{
		title, store.OpTaskCreate, store.OpTaskUpdate, "the task itself went with it",

		`the list it was in: "Personal"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to carry %q", out, want)
		}
	}
	got := stderr.String()

	if !strings.Contains(got, "1 parked create(s) will be thrown away") {
		t.Errorf("stderr = %q, want the caution printed before the drop", got)
	}

	if !strings.Contains(got, "2 parked entry(s) thrown away, 1 task(s) went with them") {
		t.Errorf("stderr = %q, want the drop summarised where a redirected stdout cannot reach", got)
	}

	if n := parkedEntries(t); n != 0 {
		t.Errorf("%d entr(y/ies) still parked, want the drop carried through", n)
	}
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Task(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the task is still cached (%v), want it gone with the create that was its only way out", err)
	}
}

func TestCmdSyncAnswersForTheParkedEntriesWhenTheDropIsNotReached(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	seedParkedCreate(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stderr := &cancelOnWrite{cancel: cancel}
	var stdout strings.Builder
	if code := cmdSync(ctx, &stdout, stderr, []string{"--drop-parked"}); code != exitInterrupted {
		t.Errorf("cmdSync = %d, want %d", code, exitInterrupted)
	}
	got := stderr.buf.String()
	if !strings.Contains(got, "1 parked create(s) will be thrown away") {
		t.Fatalf("stderr = %q, want the caution the command had already printed", got)
	}
	if !strings.Contains(got, "nothing was thrown away") {
		t.Errorf("stderr = %q, want it to say the drop did not happen", got)
	}
	if strings.Contains(got, "nothing was sent") {
		t.Errorf("stderr = %q, want the answer about the drop and not the one about a pass", got)
	}

	if strings.Contains(got, "parked entry(s) thrown away") {
		t.Errorf("stderr = %q, want no account of a drop that never happened", got)
	}
	if out := stdout.String(); strings.Contains(out, "threw away") {
		t.Errorf("stdout = %q, want no record of a drop that never happened", out)
	}
	if n := parkedEntries(t); n != 1 {
		t.Errorf("%d entr(y/ies) parked, want the create left exactly where it was", n)
	}
}

func TestCmdSyncCarriesTheDropThroughAContextThatEnded(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	seedParkedCreate(t)

	ctx := &expiringContext{Context: context.Background(), done: make(chan struct{})}
	stderr := &cancelOnWrite{cancel: ctx.expire}
	var stdout strings.Builder
	if code := cmdSync(ctx, &stdout, stderr, []string{"--drop-parked"}); code != exitError {
		t.Errorf("cmdSync = %d, want %d", code, exitError)
	}
	if out := stdout.String(); !strings.Contains(out, "threw away 1 parked entry(s)") {
		t.Fatalf("stdout = %q, want the drop carried through and recorded", out)
	}
	if n := parkedEntries(t); n != 0 {
		t.Errorf("%d entr(y/ies) still parked, want the drop carried through", n)
	}
	if got := stderr.buf.String(); strings.Contains(got, "nothing was thrown away") {
		t.Errorf("stderr = %q, want no denial of a drop that happened", got)
	}
}

func TestSyncLocalFlagsNeedNoToken(t *testing.T) {
	cases := map[string]struct {
		flag, printed string
	}{
		"--drop-parked":  {"--drop-parked", "threw away 1 parked entry(s)"},
		"--retry-failed": {"--retry-failed", "put 1 parked entry(s) back in line"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			seedParkedCreate(t)

			var stdout, stderr strings.Builder
			if code := cmdSync(context.Background(), &stdout, &stderr, []string{c.flag}); code != exitError {
				t.Errorf("cmdSync = %d, want %d: the pass still has no token to make", code, exitError)
			}
			if out := stdout.String(); !strings.Contains(out, c.printed) {
				t.Errorf("stdout = %q, want %q: the local work is done before the token is asked for", out, c.printed)
			}
			if n := parkedEntries(t); n != 0 {
				t.Errorf("%d entr(y/ies) still parked, want the queue actually moved", n)
			}
			got := stderr.String()
			if !strings.Contains(got, "no token") {
				t.Errorf("stderr = %q, want the missing token reported once the local half is done", got)
			}

			if strings.Contains(got, "the queue is exactly as it was") {
				t.Errorf("stderr = %q, want no denial of work that happened", got)
			}
		})
	}
}

func TestSyncFlagsAreOfferedAndAccepted(t *testing.T) {
	offered := helpText(t, "sync")
	for _, flag := range []string{"--retry-failed", "--drop-parked", "--allow-project-drop"} {
		if !strings.Contains(offered, "tt sync "+flag) {
			t.Errorf("tt help sync does not offer %s:\n%s", flag, offered)
		}
		if !strings.Contains(syncUsage, flag) {
			t.Errorf("the usage line does not name %s: %q", flag, syncUsage)
		}
		if _, err := parseSyncArgs([]string{flag}); err != nil {
			t.Errorf("parseSyncArgs([%s]) = %v, want the flag tt offers to be one it takes", flag, err)
		}
	}
}

type signalCause struct{}

func (signalCause) Error() string { return "interrupt signal received" }

func (signalCause) Is(target error) bool { return target == context.Canceled }

func TestPrintSyncErrorsKeepsWhatTheInterruptDidNotCause(t *testing.T) {
	parked := errors.New("task 42: server refused it (400), parked; " + sync.RetryHint)
	dropped := fmt.Errorf("ticktick: POST /open/v1/task: %w", signalCause{})
	refused := errors.New("ticktick: GET /open/v1/project: unauthorized (401)")

	parkedByTheSignal := &sync.ParkedError{
		Err: fmt.Errorf("outbox 7: create task tt-local-1: %w; %s", signalCause{}, sync.RetryHint),
	}

	cases := map[string]struct {
		errs    []error
		runErr  error
		stopped bool
		want    []string
		absent  []string
		named   int
	}{
		"interrupted, one parked and one dropped request": {
			errs:    []error{parked, dropped},
			stopped: true,
			want:    []string{"parked", "--retry-failed"},
			absent:  []string{"interrupt signal received"},
			named:   1,
		},
		"interrupted, and the pass stopped on a real refusal": {
			errs:    []error{parked},
			runErr:  refused,
			stopped: true,
			want:    []string{"parked", "unauthorized (401)"},
			named:   2,
		},
		"interrupted, and the pass stopped on the cancellation": {
			runErr:  fmt.Errorf("push: %w", signalCause{}),
			stopped: true,
			absent:  []string{"interrupt signal received"},
		},

		"interrupted, and the interrupt itself parked a create": {
			errs:    []error{parkedByTheSignal, dropped},
			stopped: true,
			want:    []string{"outbox 7: create task tt-local-1", "--retry-failed"},
			named:   1,
		},

		"not interrupted, everything is reported": {
			errs:   []error{parked, dropped},
			runErr: refused,
			want: []string{
				"parked", "interrupt signal received", "unauthorized (401)",
			},
			named: 3,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			printSyncErrors(&out, c.errs, c.runErr, c.stopped)
			got := out.String()
			var decoded []string
			for _, line := range strings.Split(got, "\n") {
				if !strings.HasPrefix(line, "tt: sync: ") {
					continue
				}
				reason, err := strconv.Unquote(line[len("tt: sync: "):])
				if err != nil {
					t.Fatalf("printed failure is not one quoted reason: %v", err)
				}
				decoded = append(decoded, reason)
			}
			for _, w := range c.want {
				present := strings.Contains(got, w)
				for _, reason := range decoded {
					present = present || strings.Contains(reason, w)
				}
				if !present {
					t.Errorf("printed %q, want it to carry %q", got, w)
				}
			}
			for _, a := range c.absent {
				present := strings.Contains(got, a)
				for _, reason := range decoded {
					present = present || strings.Contains(reason, a)
				}
				if present {
					t.Errorf("printed %q, want it to leave out %q", got, a)
				}
			}

			if len(decoded) != c.named {
				t.Errorf("printed %d failure(s), want %d:\n%s", len(decoded), c.named, got)
			}
		})
	}
}

func TestSyncKeepsCompleteOpaqueReasonsAtThePassBoundary(t *testing.T) {
	const id = "6247ee29b6c1a11a4a3f2c0d"

	const said = "the request was understood and could not be carried out, because the " +
		"account is not allowed to write to this list at the moment"

	errs := []error{
		fmt.Errorf("outbox 7: update task %s: ticktick: POST /open/v1/task/%s: server error (500): %s",
			id, id, said),
		fmt.Errorf("the list %q (%s): the server answered with nothing in it, which is not the "+
			"same as answering that the list is empty - the list was left as the cache holds it",
			"Работа", id),
	}
	unsent := []error{
		fmt.Errorf("outbox 12: task.move_drop of %s waits for the copy the task was moved into, "+
			"whose create is still queued", id),
	}

	wantErrors := append(append([]error(nil), errs...), errors.New("ask the server for the lists: "+said))
	var errorOut strings.Builder
	printSyncErrors(&errorOut, errs, wantErrors[len(wantErrors)-1], false)
	errorLines := strings.Split(errorOut.String(), "\n")
	if len(errorLines) != len(wantErrors)+1 || errorLines[len(errorLines)-1] != "" {
		t.Fatalf("pass failures did not remain one line per complete reason: %q", errorOut.String())
	}
	for i, line := range errorLines[:len(wantErrors)] {
		if !strings.HasPrefix(line, "tt: sync: ") {
			t.Fatalf("failure %d has no command prefix: %q", i, line)
		}
		got, err := strconv.Unquote(line[len("tt: sync: "):])
		if err != nil {
			t.Fatalf("failure %d is not one quoted reason: %v", i, err)
		}
		if got != wantErrors[i].Error() {
			t.Fatalf("failure %d recovered %q, want %q", i, got, wantErrors[i].Error())
		}
	}

	var unsentOut strings.Builder
	printSyncUnsent(&unsentOut, unsent)
	line := unsentOut.String()
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatalf("unsent reason is not one finished line: %q", line)
	}
	got, err := strconv.Unquote(line[:len(line)-1])
	if err != nil || got != unsent[0].Error() {
		t.Fatalf("unsent reason recovered %q, %v; want %q", got, err, unsent[0].Error())
	}
	if utf8.RuneCountInString(errorLines[0]) <= cli.Width || utf8.RuneCountInString(line[:len(line)-1]) <= cli.Width {
		t.Fatal("fixture did not hold the accepted complete opaque-reason width overflow")
	}
}

func TestSyncPassRenderersPreserveOpaqueReasonsExactly(t *testing.T) {
	reasons := map[string]string{
		"leading repeated and trailing spaces": "  outbox  7  ",
		"unicode whitespace":                   "outbox\u00a07\u2003waits",
		"newline and bidi controls":            "outbox\n7\r\t\x1b\u202e",
		"literal escape syntax":                `outbox\n\x20\u00a0`,
		"invalid UTF-8":                        string([]byte{'o', 0xff, 'k'}),
		"width boundary and overflow":          strings.Repeat("x", 79) + "  " + strings.Repeat("y", 81),
	}
	for name, reason := range reasons {
		t.Run(name, func(t *testing.T) {
			var failure strings.Builder
			printSyncErrors(&failure, []error{errors.New(reason)}, nil, false)
			failureLine := failure.String()
			if len(failureLine) == 0 || failureLine[len(failureLine)-1] != '\n' {
				t.Fatalf("failure is not one finished line: %q", failureLine)
			}
			failureLine = failureLine[:len(failureLine)-1]
			if !strings.HasPrefix(failureLine, "tt: sync: ") {
				t.Fatalf("failure has no command prefix: %q", failureLine)
			}
			gotFailure, err := strconv.Unquote(failureLine[len("tt: sync: "):])
			if err != nil || gotFailure != reason {
				t.Fatalf("failure recovered %q, %v; want exact reason %q", gotFailure, err, reason)
			}

			var withHint strings.Builder
			printSyncErrors(&withHint, []error{errors.New(reason + "; " + sync.RetryHint)}, nil, false)
			hintLines := strings.Split(withHint.String(), "\n")
			if len(hintLines) != 3 || hintLines[1] != sync.RetryHint || hintLines[2] != "" ||
				!strings.HasPrefix(hintLines[0], "tt: sync: ") {
				t.Fatalf("trusted retry hint did not remain a standalone line: %q", withHint.String())
			}
			gotWithHint, err := strconv.Unquote(hintLines[0][len("tt: sync: "):])
			if err != nil || gotWithHint != reason {
				t.Fatalf("hinted failure recovered %q, %v; want exact prose %q", gotWithHint, err, reason)
			}

			var unsent strings.Builder
			printSyncUnsent(&unsent, []error{errors.New(reason)})
			unsentLine := unsent.String()
			if len(unsentLine) == 0 || unsentLine[len(unsentLine)-1] != '\n' {
				t.Fatalf("unsent reason is not one finished line: %q", unsentLine)
			}
			gotUnsent, err := strconv.Unquote(unsentLine[:len(unsentLine)-1])
			if err != nil || gotUnsent != reason {
				t.Fatalf("unsent recovered %q, %v; want exact reason %q", gotUnsent, err, reason)
			}
		})
	}
}

type a1DenyTransport struct{ calls int }

func (d *a1DenyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, errors.New("network forbidden in A1 fixture")
}

func TestSyncActualStoreDomainValuesAreEscapedBeforeCommandOutput(t *testing.T) {
	for _, name := range []string{"unsent id", "failure id", "failure op"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			ctx := context.Background()
			st, err := store.Open(ctx, "")
			if err != nil {
				t.Fatal("fixture store open failed")
			}
			defer st.Close()
			hostile := "domain\x1b[31m\n\u202e  value"
			entry := store.OutboxEntry{Target: store.TargetOpenAPI, Op: store.OpTaskCreate, TaskID: hostile}
			switch name {
			case "failure id":
				entry.Op = store.OpTaskUpdate
				entry.TaskID = store.LocalIDPrefix + hostile
			case "failure op":
				entry.Op = hostile
				entry.TaskID = ""
			}
			if _, err := st.Enqueue(ctx, entry); err != nil {
				t.Fatal("fixture enqueue failed")
			}
			deny := &a1DenyTransport{}
			client := api.NewClient("", "a1-test",
				api.WithHTTPClient(&http.Client{Transport: deny}), api.WithMaxRetries(1))
			res, runErr := sync.New(st, client, sync.Options{}).Push(ctx)
			if runErr != nil {
				t.Fatal("fixture push failed")
			}
			if deny.calls != 0 {
				t.Fatalf("fixture attempted %d HTTP request(s), want zero", deny.calls)
			}

			var out strings.Builder
			var want string
			var token string
			if name == "unsent id" {
				if len(res.Unsent) != 1 || len(res.Errors) != 0 {
					t.Fatalf("unsent branch = %d unsent, %d errors", len(res.Unsent), len(res.Errors))
				}
				want = res.Unsent[0].Error()
				printSyncUnsent(&out, res.Unsent)
				printed := out.String()
				if len(printed) == 0 || printed[len(printed)-1] != '\n' {
					t.Fatalf("unsent branch is not one finished line: %q", printed)
				}
				token = printed[:len(printed)-1]
			} else {
				if len(res.Errors) != 1 || len(res.Unsent) != 0 {
					t.Fatalf("failure branch = %d errors, %d unsent", len(res.Errors), len(res.Unsent))
				}
				want = res.Errors[0].Error()
				if strings.HasSuffix(want, "; "+sync.RetryHint) {
					want = want[:len(want)-len("; "+sync.RetryHint)]
				}
				printSyncErrors(&out, res.Errors, nil, false)
				printed := out.String()
				lineEnd := strings.IndexByte(printed, '\n')
				if lineEnd < 0 || !strings.HasPrefix(printed[:lineEnd], "tt: sync: ") {
					t.Fatalf("failure branch has no complete command line: %q", printed)
				}
				token = printed[len("tt: sync: "):lineEnd]
			}
			got, err := strconv.Unquote(token)
			if err != nil || got != want {
				t.Fatalf("rendered reason recovered %q, %v; want exact producer reason %q", got, err, want)
			}
			if !strings.Contains(got, hostile) {
				t.Fatalf("recovered reason omitted the exact fixture domain value: %q", got)
			}
			if strings.ContainsAny(out.String(), "\x1b\u202e") {
				t.Fatalf("raw terminal control reached output: %q", out.String())
			}
		})
	}
}

func TestSyncExitReadsTheFailuresAndNothingElse(t *testing.T) {
	waiting := sync.Result{
		Pushed: 1,
		Unsent: []error{errors.New("outbox 2: task.move_drop of t1 waits for the copy the task " +
			"was moved into, whose create is still queued")},
	}
	if got := syncExit(waiting, nil); got != exitOK {
		t.Errorf("a pass that held an entry back exits %d, want %d", got, exitOK)
	}
	failed := sync.Result{
		Failed: 1,
		Errors: []error{errors.New("outbox 3: update task t1: the server refused it (400)")},
	}
	if got := syncExit(failed, nil); got != exitError {
		t.Errorf("a pass that parked an entry exits %d, want %d", got, exitError)
	}
	both := sync.Result{Pushed: 1, Unsent: waiting.Unsent, Errors: failed.Errors}
	if got := syncExit(both, nil); got != exitError {
		t.Errorf("a pass with a failure beside a wait exits %d, want %d", got, exitError)
	}
	if got := syncExit(sync.Result{}, errors.New("ask the server for the lists: unauthorized")); got != exitError {
		t.Errorf("a pass that stopped exits %d, want %d", got, exitError)
	}
	if got := syncExit(sync.Result{Pushed: 2}, nil); got != exitOK {
		t.Errorf("a pass that did everything asked of it exits %d, want %d", got, exitOK)
	}
}

func TestPrintInterruptedQueueOnlyPromisesWhatTheQueueKept(t *testing.T) {
	var kept strings.Builder
	printInterruptedQueue(&kept, 0)

	switch got := unfolded(kept.String()); {
	case !strings.Contains(got, "still in the queue"), !strings.Contains(got, "next pass"):
		t.Errorf("with nothing parked it said %q, want the unsent changes put back in the queue", got)
	case strings.Contains(got, "--retry-failed"):
		t.Errorf("with nothing parked it said %q, want no command to run: there is nothing to raise", got)
	}

	var parked strings.Builder
	printInterruptedQueue(&parked, 2)
	got := parked.String()
	for _, want := range []string{"2 entry(s)", "will not go out on the next pass", sync.RetryHint} {
		if !strings.Contains(unfolded(got), want) {
			t.Errorf("with 2 parked it said %q, want it to carry %q", got, want)
		}
	}

	if strings.Contains(unfolded(got), "the changes that were not sent are still in the queue") {
		t.Errorf("with 2 parked it said %q, want no promise covering the entries that were parked", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("it said %q, want it to end its last line", got)
	}
}

type cancelOnWrite struct {
	cancel context.CancelFunc
	buf    strings.Builder
}

func (c *cancelOnWrite) Write(p []byte) (int, error) {
	c.cancel()
	return c.buf.Write(p)
}

func seedParkedCreate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seq, err := st.Enqueue(ctx, store.OutboxEntry{
		Target: store.TargetOpenAPI, Op: store.OpTaskCreate,
		TaskID: store.LocalIDPrefix + "t1", ProjectID: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := st.Claim(ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %d entr(y/ies), %v; want the one that was queued", len(items), err)
	}
	if err := st.MarkFailed(ctx, seq, items[0].LeaseToken, "400 bad request"); err != nil {
		t.Fatal(err)
	}
}

func parkedEntries(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counts, err := st.OutboxCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return counts.Failed
}

func TestCmdSyncAnswersForTheQueueWhenItStopsBeforeThePass(t *testing.T) {

	t.Run("cancelled with nothing configured at all", func(t *testing.T) {
		isolate(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var stdout, stderr strings.Builder

		if code := cmdSync(ctx, &stdout, &stderr, nil); code != exitError {
			t.Errorf("cmdSync = %d, want %d", code, exitError)
		}
		got := stderr.String()
		if !strings.Contains(got, "tt: sync: ") {
			t.Fatalf("stderr = %q, want the failure that stopped it", got)
		}
		if !strings.Contains(got, "nothing was sent") {
			t.Errorf("stderr = %q, want it to say the queue was left alone", got)
		}
		if strings.Contains(got, "put back in line") {
			t.Errorf("stderr = %q, want nothing about a raise: none was asked for", got)
		}
	})

	t.Run("cancelled before the database was open", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "test-token")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var stdout, stderr strings.Builder
		if code := cmdSync(ctx, &stdout, &stderr, []string{"--retry-failed"}); code != exitError {
			t.Errorf("cmdSync = %d, want %d", code, exitError)
		}
		got := stderr.String()
		if !strings.Contains(got, "nothing was sent") {
			t.Errorf("stderr = %q, want it to say the queue was left alone", got)
		}
		if strings.Contains(got, "put back in line") {
			t.Errorf("stderr = %q, want nothing about a raise: the store never opened", got)
		}
	})

	t.Run("the signal lands before the raise", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "test-token")
		seedParkedCreate(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stderr := &cancelOnWrite{cancel: cancel}
		var stdout strings.Builder

		if code := cmdSync(ctx, &stdout, stderr, []string{"--retry-failed"}); code != exitInterrupted {
			t.Errorf("cmdSync = %d, want %d", code, exitInterrupted)
		}
		got := stderr.buf.String()
		if !strings.Contains(got, "1 parked create(s)") {
			t.Fatalf("stderr = %q, want the caution the command had already printed", got)
		}
		if !strings.Contains(got, "nothing was put back in line") {
			t.Errorf("stderr = %q, want it to say the raise did not happen", got)
		}
		if strings.Contains(got, "nothing was sent") {
			t.Errorf("stderr = %q, want the answer about the raise and not the one about a pass", got)
		}

		if strings.Contains(got, "outbox retry failed") {
			t.Errorf("stderr = %q, want no error off a statement that was never sent", got)
		}
		if out := stdout.String(); strings.Contains(out, "put ") {
			t.Errorf("stdout = %q, want no count of raised entries: none were raised", out)
		}

		if n := parkedEntries(t); n != 1 {
			t.Errorf("%d entr(y/ies) still parked, want the create left exactly where it was", n)
		}
	})

	t.Run("the signal lands with the raise already made", func(t *testing.T) {
		isolate(t)
		t.Setenv(tokenEnvVar, "test-token")
		writeTokenFile(t, "test-token")
		seedParkedCreate(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stdout := &cancelOnWrite{cancel: cancel}
		var stderr strings.Builder
		if code := cmdSync(ctx, stdout, &stderr, []string{"--retry-failed"}); code != exitInterrupted {
			t.Errorf("cmdSync = %d, want %d", code, exitInterrupted)
		}
		if out := stdout.buf.String(); !strings.Contains(out, "put 1 parked entry(s) back in line") {
			t.Fatalf("stdout = %q, want the count of what was raised", out)
		}
		if n := parkedEntries(t); n != 0 {
			t.Errorf("%d entr(y/ies) still parked, want the raise carried through", n)
		}
		got := stderr.String()
		if strings.Contains(got, "nothing was put back in line") {
			t.Errorf("stderr = %q, want no denial of a raise that happened", got)
		}
		if !strings.Contains(got, "still in the queue") {
			t.Errorf("stderr = %q, want the raised entries accounted for where they now are", got)
		}
	})

	t.Run("no signal, no word about the queue", func(t *testing.T) {
		isolate(t)
		var stdout, stderr strings.Builder
		if code := cmdSync(context.Background(), &stdout, &stderr, nil); code != exitError {
			t.Errorf("cmdSync = %d, want %d", code, exitError)
		}
		got := stderr.String()
		if strings.Contains(got, "nothing was sent") || strings.Contains(got, "nothing was put back") {
			t.Errorf("stderr = %q, want no answer to a question nobody asked", got)
		}
	})
}

type expiringContext struct {
	context.Context
	done chan struct{}
}

func (c *expiringContext) expire() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestCmdSyncCarriesTheRaiseThroughAContextThatEnded(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	seedParkedCreate(t)

	ctx := &expiringContext{Context: context.Background(), done: make(chan struct{})}

	stderr := &cancelOnWrite{cancel: ctx.expire}
	var stdout strings.Builder

	if code := cmdSync(ctx, &stdout, stderr, []string{"--retry-failed"}); code != exitError {
		t.Errorf("cmdSync = %d, want %d", code, exitError)
	}
	if out := stdout.String(); !strings.Contains(out, "put 1 parked entry(s) back in line") {
		t.Fatalf("stdout = %q, want the raise carried through and counted", out)
	}
	if n := parkedEntries(t); n != 0 {
		t.Errorf("%d entr(y/ies) still parked, want the raise carried through", n)
	}
	if got := stderr.buf.String(); strings.Contains(got, "nothing was put back in line") {
		t.Errorf("stderr = %q, want no denial of a raise that happened", got)
	}
}

func TestCmdSyncWithoutATokenLeavesNoCacheBehind(t *testing.T) {
	cacheExists := func(t *testing.T) bool {
		t.Helper()
		path, err := store.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		_, serr := os.Stat(path)
		if serr != nil && !os.IsNotExist(serr) {
			t.Fatal(serr)
		}
		return serr == nil
	}

	t.Run("nothing local to do, so nothing is created", func(t *testing.T) {
		isolate(t)
		var stdout, stderr strings.Builder
		if code := cmdSync(context.Background(), &stdout, &stderr, nil); code != exitError {
			t.Fatalf("cmdSync = %d, want %d", code, exitError)
		}
		if got := stderr.String(); !strings.Contains(got, "no token found") {
			t.Fatalf("stderr = %q, want the missing token named", got)
		}
		if cacheExists(t) {
			t.Error("the cache was created by a run that stopped at the token and did nothing else")
		}
	})

	for _, flag := range []string{"--retry-failed", "--drop-parked"} {
		t.Run(flag+" still gets at the queue", func(t *testing.T) {
			isolate(t)
			var stdout, stderr strings.Builder
			if code := cmdSync(context.Background(), &stdout, &stderr, []string{flag}); code != exitError {
				t.Fatalf("cmdSync %s = %d, want %d", flag, code, exitError)
			}
			if got := stderr.String(); !strings.Contains(got, "no token found") {
				t.Fatalf("stderr = %q, want the token refused after the local work, not before it", got)
			}
			if !cacheExists(t) {
				t.Errorf("%s never opened the cache, and the queue it works on is in there", flag)
			}
		})
	}
}

func TestCmdSyncWarnsAboutTheEntriesItRaises(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	writeTokenFile(t, "test-token")
	seedParkedCreate(t)

	other, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout := &cancelOnWrite{cancel: cancel}
	stderr := &parkOnWrite{store: other, taskID: store.LocalIDPrefix + "t2"}
	if code := cmdSync(ctx, stdout, stderr, []string{"--retry-failed"}); code != exitInterrupted {
		t.Errorf("cmdSync = %d, want %d", code, exitInterrupted)
	}
	if err := stderr.wait(); err != nil {
		t.Fatalf("park a create in the window: %v", err)
	}
	if got := stderr.buf.String(); !strings.Contains(got, "1 parked create(s) will be sent again") {
		t.Fatalf("stderr = %q, want the caution counted off the queue the raise moves", got)
	}
	if out := stdout.buf.String(); !strings.Contains(out, "put 1 parked entry(s) back in line") {
		t.Errorf("stdout = %q, want the raise to have moved the entries the caution covered and no others", out)
	}
	if n := parkedEntries(t); n != 1 {
		t.Errorf("%d entr(y/ies) parked, want the create parked in the window left exactly where it was", n)
	}
}

const parkWindow = 500 * time.Millisecond

type parkOnWrite struct {
	store  *store.Store
	taskID string

	started bool
	done    chan struct{}

	err error
	buf strings.Builder
}

func (p *parkOnWrite) Write(b []byte) (int, error) {
	if !p.started {
		p.started = true
		p.done = make(chan struct{})
		go func() {
			p.err = parkACreate(p.store, p.taskID)
			close(p.done)
		}()
		select {
		case <-p.done:
		case <-time.After(parkWindow):
		}
	}
	return p.buf.Write(b)
}

func (p *parkOnWrite) wait() error {
	if p.done == nil {
		return errors.New("the command wrote nothing, so no create was ever parked in the window")
	}
	select {
	case <-p.done:
		return p.err
	case <-time.After(30 * time.Second):
		return errors.New("the create staged in the window never landed")
	}
}

func parkACreate(st *store.Store, taskID string) error {
	ctx := context.Background()
	seq, err := st.Enqueue(ctx, store.OutboxEntry{
		Target: store.TargetOpenAPI, Op: store.OpTaskCreate,
		TaskID: taskID, ProjectID: "p1",
	})
	if err != nil {
		return err
	}

	items, _, err := st.Claim(ctx, 10, time.Minute)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Seq == seq {
			return st.MarkFailed(ctx, seq, item.LeaseToken, "400 bad request")
		}
	}
	return fmt.Errorf("the entry queued as %d was not among the %d claimed", seq, len(items))
}

func TestSyncNamesItselfOnceOnTheLineAUserReads(t *testing.T) {
	ctx := context.Background()

	var listStatus int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open/v1/project" && listStatus == 0 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"id":"p1","name":"Работа"}]`)
			return
		}
		if r.URL.Path == "/open/v1/project" {
			w.WriteHeader(listStatus)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open the cache: %v", err)
	}
	defer st.Close()
	client := api.NewClient("test-token", "0.0.0-test",
		api.WithBaseURL(server.URL), api.WithMaxRetries(1), api.WithTimeout(5*time.Second))

	var out strings.Builder
	res, runErr := sync.New(st, client, sync.Options{}).Run(ctx)
	printSyncErrors(&out, res.Errors, runErr, false)

	listStatus = http.StatusUnauthorized
	res, runErr = sync.New(st, client, sync.Options{}).Run(ctx)
	printSyncErrors(&out, res.Errors, runErr, false)

	got := out.String()
	var failures []string
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "tt: sync: ") {
			t.Fatalf("failure continuation escaped the complete reason atom: %q", line)
		}
		reason, err := strconv.Unquote(line[len("tt: sync: "):])
		if err != nil {
			t.Fatalf("failure is not one quoted reason: %v", err)
		}
		failures = append(failures, reason)
	}
	if len(failures) < 3 {
		t.Fatalf("printed %d failure(s):\n%s\nwant the list, the inbox and the refused token", len(failures), got)
	}
	var namedList bool
	for _, reason := range failures {
		namedList = namedList || strings.Contains(reason, "the list p1")
		failure := "tt: sync: " + reason
		if n := wordsIn(failure, "sync"); n != 1 {
			t.Errorf("%q says \"sync\" %d time(s), want the command named once", failure, n)
		}

		seen := map[string]bool{}
		for _, segment := range strings.Split(failure, ": ") {
			if seen[segment] {
				t.Errorf("%q says %q twice", failure, segment)
			}
			seen[segment] = true
		}
	}
	if !namedList {
		t.Fatalf("decoded failures %q, want the list the pass could not read named", failures)
	}
}

func wordsIn(line, word string) int {
	var n int
	for _, f := range strings.Fields(line) {
		if strings.Trim(f, `:,;.()"`) == word {
			n++
		}
	}
	return n
}

func TestCmdSyncFoldsTheFailuresOfItsOwnToo(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, "test-token")
	base := filepath.Join(t.TempDir(), "one two three four five six seven eight nine ten")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("make the data directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(base, "ticktick"), nil, 0o600); err != nil {
		t.Fatalf("put a file in the way: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", base)

	var stdout, stderr strings.Builder
	if code := cmdSync(context.Background(), &stdout, &stderr, nil); code != exitError {
		t.Fatalf("cmdSync = %d, want %d: the cache cannot be opened", code, exitError)
	}
	got := stderr.String()
	if !strings.Contains(got, "not a directory") {
		t.Fatalf("stderr = %q, want the failure that stopped the command", got)
	}
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len(strings.Fields(line)) < 2 {
			continue
		}
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
}

func TestSyncKeepsWhatItSaysAboutADropInsideTheWidth(t *testing.T) {
	queued := time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local)

	title := strings.Repeat("заголовок", 34)

	reason := "ticktick: POST /open/v1/task: http 400: " + strings.Repeat("bad request, ", 300)
	dropped := make([]store.DroppedMutation, 0, 123)
	for i := 1; i <= 123; i++ {
		dropped = append(dropped, store.DroppedMutation{
			Seq: int64(i), Op: store.OpTaskCreate, TaskID: fmt.Sprintf("local-%d", i),
			ProjectID: "p1", Title: title, QueuedAt: queued, Attempts: 123,
			Reason: reason, Requested: true, TaskRemoved: true,
		})
	}
	var out strings.Builder

	printDroppedMutations(&out, dropped, func(string) string { return strings.Repeat("списо", 60) })
	summariseDrop(&out, dropped)
	printInterruptedQueue(&out, 123)

	printInterruptedQueue(&out, 0)
	printSyncResult(&out, sync.Result{Pushed: 123, Failed: 123, ParkedUnsent: 123})

	got := out.String()
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}

	if strings.Count(got, "entry 1: ") != 1 {
		t.Errorf("printed %q, want each entry to open exactly one line", got)
	}

	if !strings.Contains(got, sync.RetryHint) {
		t.Errorf("printed %q, want the way back said whole:\n%s", sync.RetryHint, got)
	}

	if !strings.Contains(got, "123 attempt(s)") {
		t.Errorf("printed %q, want the attempts counted on one line", got)
	}
}

func TestSyncKeepsTheStampsOfAnEntryNobodyRecordedInsideTheWidth(t *testing.T) {
	var out strings.Builder
	printDroppedMutations(&out, []store.DroppedMutation{{
		Seq: 5, Op: store.OpTaskUpdate, TaskID: "srv-2", Title: "Water the plants",
		QueuedAt: time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local), Attempts: 123,
	}}, func(string) string { return "" })
	got := out.String()
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
	for _, want := range []string{
		"queued 2026-09-01 10:12:03", "123 attempt(s)",
		"parked at a moment this cache does not record",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("printed %q, want it to carry %q whole", got, want)
		}
	}
}

func unfolded(printed string) string {
	return strings.ReplaceAll(strings.TrimSuffix(printed, "\n"), "\n", " ")
}

func TestSyncSaysNothingTwiceOnALineTheCacheWrote(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open the cache: %v", err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "p1", Name: "Работа"}}); err != nil {
		t.Fatalf("seed the list: %v", err)
	}
	failures := map[string]error{}

	if _, err := st.SyncProject(ctx, "p1",
		[]store.ServerTask{{Task: model.Task{Title: "Забрать посылку"}}}); err != nil {
		failures["a task with no id in the answer"] = err
	} else {
		t.Error("the cache took a task with no id")
	}

	if _, err := st.SyncProject(ctx, "p1", []store.ServerTask{{
		Task: model.Task{Id: "srv-1", ProjectId: "p1", Title: "Полить цветы", Status: model.TaskOpen},
		Raw:  []byte(`{"id":"srv-1"}`),
	}}); err != nil {
		t.Fatalf("seed the task: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`CREATE TRIGGER refuse_the_delete BEFORE DELETE ON tasks
		 BEGIN SELECT RAISE(ABORT, 'the database would not take it'); END`); err != nil {
		t.Fatalf("refuse the delete: %v", err)
	}
	if _, err := st.SyncProject(ctx, "p1", nil); err != nil {
		failures["a write the database refused"] = err
	} else {
		t.Error("the delete went through a trigger that refuses it")
	}

	for name, err := range failures {
		var out strings.Builder
		printSyncErrors(&out, nil, err, false)
		printed := out.String()
		if len(printed) == 0 || printed[len(printed)-1] != '\n' || !strings.HasPrefix(printed, "tt: sync: ") {
			t.Fatalf("%s: did not print one finished named failure: %q", name, printed)
		}
		reason, decodeErr := strconv.Unquote(printed[len("tt: sync: ") : len(printed)-1])
		if decodeErr != nil {
			t.Fatalf("%s: failure is not one quoted reason: %v", name, decodeErr)
		}
		line := "tt: sync: " + reason
		if n := wordsIn(line, "sync"); n != 1 {
			t.Errorf("%s: %q says \"sync\" %d time(s), want the command named once", name, line, n)
		}

		seen := map[string]bool{}
		for _, f := range strings.Fields(line) {
			word := strings.Trim(f, `:,;.()"`)
			if len(word) <= 3 {
				continue
			}
			if seen[word] {
				t.Errorf("%s: %q says %q twice", name, line, word)
			}
			seen[word] = true
		}
	}
}

func TestSyncOmitsEveryHistoricalParkedReason(t *testing.T) {
	credential := oauthCredentialFixture('K')
	forms := oauthCredentialForms(credential)
	reason := strings.Join(forms, "\n")
	var out strings.Builder
	printDroppedMutations(&out, []store.DroppedMutation{{
		Seq: 14, Op: store.OpTaskCreate, TaskID: "local-safe-id", Title: "fixture task",
		QueuedAt: time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local), Attempts: 1,
		Reason: reason, Requested: true,
	}}, func(string) string { return "" })

	got := out.String()
	assertCredentialMaterialAbsent(t, got, forms...)
	if !strings.Contains(got, droppedReasonOmission) {
		t.Fatal("drop record did not acknowledge omitted historical detail")
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if utf8.RuneCountInString(line) > cli.Width {
			t.Fatal("drop record exceeded the output width after omitting historical detail")
		}
	}
}
func TestSyncSaysTheWayOutOfTheParkedStateWhereAUserCanTypeIt(t *testing.T) {

	const prose = "outbox 3: complete task srv-9: ticktick: POST /open/v1/task/srv-9: http 404: not found"
	parked := &sync.ParkedError{Err: fmt.Errorf("%s; %s", prose, sync.RetryHint)}

	var out strings.Builder
	printSyncErrors(&out, []error{parked}, nil, false)
	got := out.String()

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")

	var said bool
	for _, line := range lines {
		said = said || line == sync.RetryHint
	}
	if !said {
		t.Errorf("the way out was not said on a line of its own:\n%s", got)
	}

	if !strings.HasPrefix(lines[0], "tt: sync: ") {
		t.Fatalf("printed %q, want the failure behind this command's prefix", got)
	}
	decoded, err := strconv.Unquote(lines[0][len("tt: sync: "):])
	if err != nil || decoded != prose {
		t.Errorf("failure recovered %q, %v; want %q", decoded, err, prose)
	}

	var quiet strings.Builder
	printSyncErrors(&quiet, []error{&sync.ParkedError{Err: errors.New(
		"outbox 4: create task local-1: the server took it and the id was not written down")}}, nil, false)
	if strings.Contains(quiet.String(), "--retry-failed") {
		t.Errorf("printed %q, want no way back offered for a park that must not offer one", quiet.String())
	}
}

func TestUnreachableAnswerCutsBetweenEscapesAndNeverInsideOne(t *testing.T) {
	cases := map[string]string{
		"a control character, four runes": "\x01",
		"a rune written as \\u, six":      string(rune(0x00a0)),
		"a rune written as \\U, ten":      string(rune(0x10fffe)),
		"a backslash, two":                `\`,
	}
	for name, escaped := range cases {
		t.Run(name, func(t *testing.T) {
			for off := 40; off <= 140; off++ {
				answer := strings.Repeat("a", off) + escaped + strings.Repeat("b", 300)
				for i, piece := range unreachableAnswerPieces(t, unreachableAnswerLines(errors.New(answer), "")) {
					if _, err := strconv.Unquote(`"` + piece + `"`); err != nil {
						t.Fatalf("the escape at %d: line %d does not contain whole quote sequences", off, i+1)
					}
				}
			}
		})
	}
}

func TestUnreachableAnswerLosesNoneOfItsBoundedRendering(t *testing.T) {
	budget := -(apiAnswerMaxLines - 1) * (quotedRuneMax - 1)
	for i := 0; i < apiAnswerMaxLines; i++ {
		budget += doctorNestedTextWidth
	}
	alphabet := []string{"a", `\`, `"`, "\t", "\x01", string(rune(0x00a0)), string(rune(0x10fffe))}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for j := 0; j < 100+r.Intn(300); j++ {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		answer := b.String()
		lines := unreachableAnswerLines(errors.New(answer), "")
		if len(lines) > apiAnswerMaxLines {
			t.Fatalf("the answer took %d lines, want at most %d", len(lines), apiAnswerMaxLines)
		}
		got := strings.Join(unreachableAnswerText(lines), "")
		if want := cli.Foreign(answer, budget); got != want {
			t.Fatal("the fold dropped part of the bounded rendering")
		}
	}
}

func unreachableAnswerText(lines []string) []string {
	pieces := append([]string(nil), lines...)
	for i := range pieces {
		pieces[i] = strings.TrimPrefix(pieces[i], doctorNestedIndent)
	}
	return pieces
}

func unreachableAnswerPieces(t *testing.T, lines []string) []string {
	t.Helper()
	pieces := unreachableAnswerText(lines)
	pieces[0] = strings.TrimPrefix(pieces[0], `"`)
	last := len(pieces) - 1
	pieces[last] = strings.TrimSuffix(pieces[last], `"`)
	return pieces
}

func TestSyncNamesTheEntryWhateverTheOpTurnsOutToBe(t *testing.T) {
	queued := time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local)
	long := strings.Repeat("x", 90)
	cases := map[string]struct {
		m    store.DroppedMutation
		want string
	}{

		"a title cut to what the op leaves": {
			store.DroppedMutation{Seq: 7, Op: long, TaskID: "srv-1", Title: "Call the dentist"},
			`"C..."`,
		},

		"no title either": {
			store.DroppedMutation{Seq: 7, Op: long, TaskID: "srv-1"},
			"task srv-1",
		},

		"nothing but the title": {
			store.DroppedMutation{Seq: 7, Op: long, Title: "Call the dentist"},
			`"C..."`,
		},
		"nothing at all": {
			store.DroppedMutation{Seq: 7, Op: long},
			"(no task recorded)",
		},

		"room enough for the title": {
			store.DroppedMutation{Seq: 7, Op: store.OpTaskUpdate, TaskID: "srv-1", Title: "Call the dentist"},
			`"Call the dentist"`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := c.m
			m.QueuedAt, m.Attempts = queued, 1
			var out strings.Builder
			printDroppedMutations(&out, []store.DroppedMutation{m}, func(string) string { return "" })
			got := out.String()
			var entry string
			for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if strings.HasPrefix(line, "  entry 7: ") {
					entry = line
				}
				if strings.HasSuffix(line, " ") {
					t.Errorf("printed %q, want no line ending in a space:\n%s", line, got)
				}
			}
			if entry == "" {
				t.Fatalf("no line opened the entry:\n%s", got)
			}
			if !strings.HasSuffix(entry, " "+c.want) {
				t.Errorf("the entry reads %q, want it to name the task with %q", entry, c.want)
			}
		})
	}
}

func TestDroppedTaskNameEscapesAndBoundsBothTitleBranches(t *testing.T) {
	title := "Alpha\n\x1b\t\u202e" + strings.Repeat("z", 80)
	m := store.DroppedMutation{Title: title}
	cases := []struct {
		name        string
		width       int
		maxWidth    int
		want        string
		wantEscapes []string
	}{
		{
			name:        "ordinary title width",
			width:       32,
			maxWidth:    32,
			wantEscapes: []string{`\n`, `\x1b`, `\t`, `\u202e`},
		},
		{
			name:     "no width",
			width:    0,
			maxWidth: droppedTitleFloor,
			want:     `"A..."`,
		},
		{
			name:     "one below the floor",
			width:    droppedTitleFloor - 1,
			maxWidth: droppedTitleFloor,
			want:     `"A..."`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := droppedTaskName(m, c.width)
			if n := utf8.RuneCountInString(got); n > c.maxWidth {
				t.Errorf("droppedTaskName at width %d is %d runes wide, want at most %d: %q",
					c.width, n, c.maxWidth, got)
			}
			if c.want != "" && got != c.want {
				t.Errorf("droppedTaskName at width %d = %q, want the retained name %q",
					c.width, got, c.want)
			}
			if !strings.Contains(got, "A") {
				t.Errorf("droppedTaskName at width %d = %q, want it to retain part of Alpha", c.width, got)
			}
			for _, escaped := range c.wantEscapes {
				if !strings.Contains(got, escaped) {
					t.Errorf("droppedTaskName at width %d = %q, want escaped text %q", c.width, got, escaped)
				}
			}
			for _, raw := range []string{"\n", "\x1b", "\t", "\u202e"} {
				if strings.Contains(got, raw) {
					t.Errorf("droppedTaskName at width %d passed raw %q through in %q", c.width, raw, got)
				}
			}
		})
	}
}

func TestSyncKeepsADropRecordInsideTheWidthWhateverTheOpIs(t *testing.T) {
	queued := time.Date(2026, 9, 1, 10, 12, 3, 0, time.Local)
	parked := time.Date(2026, 9, 1, 10, 15, 40, 0, time.Local)

	op := strings.Repeat("x", 90)

	id := "6f2a1c94b7de5083af41cc26"
	var out strings.Builder
	printDroppedMutations(&out, []store.DroppedMutation{
		{
			Seq: 3, Op: op, TaskID: id, ProjectID: "p1",
			Title: strings.Repeat("заголовок", 34), QueuedAt: queued, ParkedAt: parked,
			Attempts: 2, Reason: "ticktick: POST /open/v1/task: http 400: bad request",
		},
		{Seq: 4, Op: op, TaskID: id, QueuedAt: queued, ParkedAt: parked, Attempts: 1},
		{Seq: 5, Op: op, QueuedAt: queued, ParkedAt: parked, Attempts: 1},
	}, func(string) string { return strings.Repeat("списо", 60) })
	got := out.String()
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > cli.Width {
			t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
		}
	}
	if strings.Contains(got, op) {
		t.Errorf("printed %q, want the op cut rather than printed whole", got)
	}

	for _, want := range []string{"entry 3: ", "entry 4: ", "entry 5: "} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("%q opens %d lines, want exactly 1:\n%s", want, n, got)
		}
	}

	if !strings.Contains(got, "task "+id) {
		t.Errorf("printed %q, want the id of entry 4 said whole", got)
	}

	var wide strings.Builder
	long := strings.Repeat("9", 200)
	printDroppedMutations(&wide, []store.DroppedMutation{
		{Seq: 6, Op: op, TaskID: long, QueuedAt: queued, ParkedAt: parked, Attempts: 1},
	}, func(string) string { return "" })
	if want := `  entry 6: "x..." task ` + long; !strings.Contains(wide.String(), want) {
		t.Errorf("printed %q, want the entry to read %q: the id whole and the op still said",
			wide.String(), want)
	}
}

func TestSyncBoundsTheWordItRefuses(t *testing.T) {
	cases := map[string]struct {
		args []string
		says string
	}{
		"an option nothing here takes": {[]string{"--" + strings.Repeat("z", 200)}, "unknown option"},
		"a word where none goes":       {[]string{strings.Repeat("z", 200)}, "unexpected argument"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := cmdSync(context.Background(), &stdout, &stderr, c.args); code != exitUsage {
				t.Fatalf("cmdSync = %d, want %d: the command line cannot be read", code, exitUsage)
			}
			got := stderr.String()
			for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if n := utf8.RuneCountInString(line); n > cli.Width {
					t.Errorf("line %d is %d columns, want at most %d: %q", i+1, n, cli.Width, line)
				}
			}

			if !strings.Contains(got, "tt: sync: "+c.says+` "--zzz`) &&
				!strings.Contains(got, "tt: sync: "+c.says+` "zzz`) {
				t.Errorf("printed %q, want it to name %q and the word it read", got, c.says)
			}

			if !strings.Contains(got, `..."`) {
				t.Errorf("printed %q, want the word marked as cut", got)
			}
		})
	}
}
