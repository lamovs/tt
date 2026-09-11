package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, projects []model.Project, tasks ...model.Task) []string {
	t.Helper()
	ctx := context.Background()
	if err := st.ReplaceProjects(ctx, projects); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		created, err := st.CreateTask(ctx, task)
		if err != nil {
			t.Fatalf("create task %q: %v", task.Title, err)
		}
		ids[i] = created.Id
	}
	return ids
}

var testProjects = []model.Project{
	{Id: "p1", Name: "Office Hub"},
	{Id: "p2", Name: "Личное"},
}

func newResolver(st *store.Store, answer string) (r Resolver, stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return Resolver{
		Store:   st,
		Stdin:   strings.NewReader(answer),
		Stdout:  stdout,
		Stderr:  stderr,
		Palette: PlainPalette(),
		Ask:     answer != "",
	}, stdout, stderr
}

func cutOffTaskChoices() []string {
	choices := make([]string, candidateLimit)
	for i := range choices {
		choices[i] = fmt.Sprintf("%d  [ ] a task", i+1)
	}
	return choices
}

func shortlistedListChoices() []string {
	names := make([]string, namesInAnError)
	for i := range names {
		names[i] = fmt.Sprintf("List number %d", i+1)
	}
	return names
}

func TestClassifyRef(t *testing.T) {
	local, err := store.NewLocalID()
	if err != nil {
		t.Fatal(err)
	}

	var (
		localBody  = strings.TrimPrefix(local, store.LocalIDPrefix)
		localShort = store.LocalIDPrefix + localBody[:len(localBody)-1]
		localLong  = store.LocalIDPrefix + localBody + "0"
	)

	var (
		maxPos = strconv.Itoa(math.MaxInt)

		pastMaxPos = maxPos + strings.Repeat("0", max(1, idMinLen-len(maxPos)))
	)
	cases := []struct {
		in      string
		want    refKind
		wantErr bool
	}{
		{"1", refNumber, false},
		{"12", refNumber, false},
		{"2-5", refNumber, false},
		{"6a4f2c1d9b8e7f0a1b2c3d4e", refID, false},

		{local, refID, false},

		{"local-dev", refNone, false},
		{"local-first", refNone, false},
		{"local-", refNone, false},
		{localShort, refNone, false},
		{localLong, refNone, false},
		{"remont", refNone, false},
		{"1a", refNone, false},
		{"-1", refNone, false},
		{"up-to-date", refNone, false},
		{"3-x", refNone, false},
		{"", refNone, false},

		{"faded", refNone, false},

		{"abcdef1234567890", refID, false},
		{"abcdef123456789", refNone, false},

		{"1234567890123456", refNumber, false},
		{"123456789012345678", refNumber, false},
		{maxPos, refNumber, false},

		{pastMaxPos, refID, false},
		{"123456789012345678901234", refID, false},

		{"1234567890123456-1234567890123457", refNumber, false},

		{"3-", refNumber, true},
		{"0", refNumber, true},
		{"5-2", refNumber, true},
	}
	for _, c := range cases {
		kind, err := classifyRef(c.in)
		if kind != c.want || (err != nil) != c.wantErr {
			t.Errorf("classifyRef(%q) = %v, %v; want %v, error: %v", c.in, kind, err, c.want, c.wantErr)
		}
	}
	if allReferences([]string{"1", "remont"}) {
		t.Error("a list with a word in it is a phrase, not a list of references")
	}
	if !allReferences([]string{"1", "3", "5"}) {
		t.Error("three numbers are three references")
	}
	if !allReferences([]string{"3-"}) {
		t.Error("an unfinished range is a reference somebody mistyped, not a phrase")
	}
}

func TestTasksRefusesABadReferenceInItsOwnWords(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ids := seed(t, st, testProjects, model.Task{Title: "Продлить домен", ProjectId: "p1"})
	if err := st.SetListing(ctx, ids); err != nil {
		t.Fatalf("set listing: %v", err)
	}
	r, _, _ := newResolver(st, "")

	cases := map[string]string{
		"3-":  "both ends",
		"0":   "the first task in a listing is 1",
		"5-2": "runs backwards",
	}
	for arg, want := range cases {
		_, err := r.Tasks(ctx, []string{arg}, store.StatusOpen)
		if err == nil {
			t.Errorf("Tasks([%s]) was accepted", arg)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Tasks([%s]) = %q, want it to say %q", arg, err, want)
		}
		if strings.Contains(err.Error(), "bad task range") {
			t.Errorf("Tasks([%s]) answered in the store's words: %q", arg, err)
		}
	}
}

func TestSplitValue(t *testing.T) {

	knownDates := map[string]bool{"fri": true, "18:00": true, "fri 18:00": true, "mon": true, "today": true}
	knownPriorities := map[string]bool{"high": true, "med": true, "none": true}
	knownProjects := map[string]bool{"Office Hub": true, "Личное": true, "Hub": true}
	accepts := func(known map[string]bool) ValueParser {
		return func(v string) error {
			if known[v] {
				return nil
			}
			return fmt.Errorf("%q is not one of these", v)
		}
	}

	cases := []struct {
		name     string
		args     []string
		accepts  ValueParser
		wantRef  []string
		wantVal  string
		wantErr  bool
		whatName string
	}{
		{"a number and a two-word date", []string{"1", "fri", "18:00"}, accepts(knownDates), []string{"1"}, "fri 18:00", false, "date"},
		{"a two-word title and a two-word date", []string{"забрать", "посылку", "fri", "18:00"}, accepts(knownDates), []string{"забрать", "посылку"}, "fri 18:00", false, "date"},
		{"a number and a priority", []string{"1", "high"}, accepts(knownPriorities), []string{"1"}, "high", false, "priority"},
		{"a number and a two-word list", []string{"1", "Office", "Hub"}, accepts(knownProjects), []string{"1"}, "Office Hub", false, "list"},

		{"the longest value wins", []string{"забрать", "Office", "Hub"}, accepts(knownProjects), []string{"забрать"}, "Office Hub", false, "list"},
		{"a range and a date", []string{"1-3", "mon"}, accepts(knownDates), []string{"1-3"}, "mon", false, "date"},
		{"nothing that reads as a value", []string{"1", "tomorrowish"}, accepts(knownDates), nil, "", true, "date"},
		{"a value and no task", []string{"fri"}, accepts(knownDates), nil, "", true, "date"},
		{"nothing at all", nil, accepts(knownDates), nil, "", true, "date"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, value, err := SplitValue(c.args, c.whatName, c.accepts)
			if (err != nil) != c.wantErr {
				t.Fatalf("SplitValue(%v) error = %v, want error: %v", c.args, err, c.wantErr)
			}
			if c.wantErr {
				if !strings.Contains(err.Error(), c.whatName) {
					t.Errorf("error = %q, want it to name the %s it could not find", err, c.whatName)
				}
				return
			}
			if strings.Join(ref, "|") != strings.Join(c.wantRef, "|") || value != c.wantVal {
				t.Errorf("SplitValue(%v) = %v, %q; want %v, %q", c.args, ref, value, c.wantRef, c.wantVal)
			}
		})
	}
}

func TestResolveTasksByNumber(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ids := seed(t, st, testProjects,
		model.Task{Title: "one", ProjectId: "p1"},
		model.Task{Title: "two", ProjectId: "p1"},
		model.Task{Title: "three", ProjectId: "p1"},
	)
	if err := st.SetListing(ctx, ids); err != nil {
		t.Fatalf("set listing: %v", err)
	}
	r, _, _ := newResolver(st, "")

	got, err := r.Tasks(ctx, []string{"2"}, store.StatusOpen)
	if err != nil || len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("Tasks([2]) = %v, %v; want the second task", got, err)
	}
	got, err = r.Tasks(ctx, []string{"1-2", "3"}, store.StatusOpen)
	if err != nil {
		t.Fatalf("Tasks([1-2 3]): %v", err)
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("Tasks([1-2 3]) = %v, want all three in order (%v)", got, ids)
	}
}

func TestResolveTasksWithNothingToGoOn(t *testing.T) {
	r, _, _ := newResolver(testStore(t), "")
	if _, err := r.Tasks(context.Background(), nil, store.StatusOpen); !errors.Is(err, ErrNoReference) {
		t.Errorf("Tasks(nil) = %v, want ErrNoReference", err)
	}
}

func TestResolveTasksByText(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ids := seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1"},
		model.Task{Title: "Забрать посылку", ProjectId: "p2"},
	)
	r, _, _ := newResolver(st, "")

	got, err := r.Tasks(ctx, []string{"продлить", "домен"}, store.StatusOpen)
	if err != nil || len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("Tasks([продлить домен]) = %v, %v; want the first task", got, err)
	}
}

func TestResolveTasksNoMatchSaysWhatWasSearched(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects, model.Task{Title: "Продлить домен", ProjectId: "p1"})
	r, _, _ := newResolver(st, "")

	_, err := r.Tasks(ctx, []string{"ничего"}, store.StatusOpen)
	var noMatch *NoMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("Tasks() = %v, want a NoMatchError", err)
	}
	if !strings.Contains(err.Error(), "ничего") || !strings.Contains(err.Error(), "open") {
		t.Errorf("error = %q, want it to name both what was typed and what was searched", err)
	}
}

func TestResolveTasksScopeNarrowsTheSearch(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	ids := seed(t, st, testProjects, model.Task{Title: "Продлить домен", ProjectId: "p1"})
	if _, err := st.CompleteTask(ctx, ids[0], store.CompleteOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	r, _, _ := newResolver(st, "")

	if _, err := r.Tasks(ctx, []string{"домен"}, store.StatusOpen); err == nil {
		t.Error("a completed task was found among the open ones")
	}
	got, err := r.Tasks(ctx, []string{"домен"}, store.StatusDone)
	if err != nil || len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("Tasks() among the completed = %v, %v; want the task", got, err)
	}
}

func TestResolveTasksAmbiguousWithNobodyToAsk(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1"},
		model.Task{Title: "Продлить страховку", ProjectId: "p2"},
	)
	r, stdout, stderr := newResolver(st, "")
	r.Ask = false

	_, err := r.Tasks(ctx, []string{"продлить"}, store.StatusOpen)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Tasks() = %v, want an AmbiguousError", err)
	}
	if len(ambiguous.Choices) != 2 {
		t.Errorf("the error offers %d choices, want both matches", len(ambiguous.Choices))
	}
	for _, want := range []string{"Продлить домен", "Продлить страховку"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("a question was put to a stream nobody is reading: %q / %q", stdout.String(), stderr.String())
	}
}

func TestResolveTasksAmbiguousAsks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	ids := seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1", DueDate: day(2026, 9, 7), IsAllDay: true},
		model.Task{Title: "Продлить страховку", ProjectId: "p2", DueDate: day(2026, 9, 8), IsAllDay: true},
	)
	r, stdout, stderr := newResolver(st, "2\n")

	got, err := r.Tasks(ctx, []string{"продлить"}, store.StatusOpen)
	if err != nil {
		t.Fatalf("Tasks(): %v", err)
	}
	if len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("Tasks() = %v, want the second candidate (%s)", got, ids[1])
	}
	asked := stderr.String()
	for _, want := range []string{"Продлить домен", "Продлить страховку", "which one?"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the question was %q, want %q in it", asked, want)
		}
	}

	if stdout.Len() != 0 {
		t.Errorf("the question went to stdout: %q", stdout.String())
	}
}

func TestResolveTasksCancelled(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1"},
		model.Task{Title: "Продлить страховку", ProjectId: "p2"},
	)
	for name, answer := range map[string]string{
		"an empty line":       "\n",
		"a blank one":         "   \n",
		"a stream that ended": "",
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newResolver(st, answer)
			r.Ask = true
			if _, err := r.Tasks(ctx, []string{"продлить"}, store.StatusOpen); !errors.Is(err, ErrCancelled) {
				t.Errorf("Tasks() = %v, want ErrCancelled", err)
			}
		})
	}
}

func TestResolveTasksRefusesANumberNobodyOffered(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1"},
		model.Task{Title: "Продлить страховку", ProjectId: "p2"},
	)
	r, _, _ := newResolver(st, "7\n")
	_, err := r.Tasks(ctx, []string{"продлить"}, store.StatusOpen)
	if err == nil || errors.Is(err, ErrCancelled) {
		t.Fatalf("Tasks() = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "1-2") {
		t.Errorf("error = %q, want it to say which numbers were offered", err)
	}
}

func TestResolveProject(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, []model.Project{
		{Id: "p1", Name: "Office Hub"},
		{Id: "p2", Name: "Личное"},
		{Id: "p3", Name: "Личное - архив"},
	})
	r, _, _ := newResolver(st, "")

	p, err := r.Project(ctx, "office")
	if err != nil || p.Id != "p1" {
		t.Fatalf("Project(office) = %v, %v; want p1", p, err)
	}

	p, err = r.Project(ctx, "Личное")
	if err != nil || p.Id != "p2" {
		t.Fatalf("Project(Личное) = %v, %v; want the project of that exact name", p, err)
	}

	_, err = r.Project(ctx, "личн")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Project(личн) = %v, want an AmbiguousError", err)
	}
	if len(ambiguous.Choices) != 2 {
		t.Errorf("the error names %d projects, want both", len(ambiguous.Choices))
	}

	_, err = r.Project(ctx, "nothing like it")
	var noMatch *NoMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("Project(nothing like it) = %v, want a NoMatchError", err)
	}
	if !strings.Contains(err.Error(), "Office Hub") {
		t.Errorf("error = %q, want it to say what projects there are", err)
	}
}

func TestResolveProjectNeedsAName(t *testing.T) {
	r, _, _ := newResolver(testStore(t), "")
	if _, err := r.Project(context.Background(), "  "); err == nil {
		t.Error("an empty query resolved to a project")
	}
}

func TestDefaultProjectSaysWhereTheNameCameFrom(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects)
	r, _, _ := newResolver(st, "")

	p, err := r.DefaultProject(ctx, "Личное")
	if err != nil || p.Id != "p2" {
		t.Fatalf("DefaultProject(Личное) = %v, %v; want p2", p, err)
	}

	_, err = r.DefaultProject(ctx, "Gone")
	if err == nil {
		t.Fatal("a default project that does not exist resolved anyway")
	}
	if !strings.Contains(err.Error(), "default_project") {
		t.Errorf("error = %q, want it to name the setting the name came from", err)
	}
}

func TestSplitValueAgainstTheRealParsers(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects)
	r, _, _ := newResolver(st, "")

	date := func(v string) error { _, err := dates.Parse(v, listNow); return err }
	priority := func(v string) error { _, err := model.ParsePriority(v); return err }
	project := func(v string) error { _, err := r.Project(ctx, v); return err }

	cases := []struct {
		line    string
		args    []string
		what    string
		accepts ValueParser
		wantRef []string
		wantVal string
	}{
		{"tt due 1 fri 18:00", []string{"1", "fri", "18:00"}, "date", date, []string{"1"}, "fri 18:00"},
		{"tt due забрать посылку fri 18:00", []string{"забрать", "посылку", "fri", "18:00"}, "date", date,
			[]string{"забрать", "посылку"}, "fri 18:00"},
		{"tt pri 1 high", []string{"1", "high"}, "priority", priority, []string{"1"}, "high"},
		{"tt mv 1 Office Hub", []string{"1", "Office", "Hub"}, "list", project, []string{"1"}, "Office Hub"},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			ref, value, err := SplitValue(c.args, c.what, c.accepts)
			if err != nil {
				t.Fatalf("SplitValue(%v): %v", c.args, err)
			}
			if strings.Join(ref, "|") != strings.Join(c.wantRef, "|") || value != c.wantVal {
				t.Errorf("SplitValue(%v) = %v, %q; want %v, %q", c.args, ref, value, c.wantRef, c.wantVal)
			}
		})
	}
}

func TestChooseFallsBackToStdoutWithoutAStderr(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects,
		model.Task{Title: "Продлить домен", ProjectId: "p1", DueDate: day(2026, 9, 7), IsAllDay: true},
		model.Task{Title: "Продлить страховку", ProjectId: "p2", DueDate: day(2026, 9, 8), IsAllDay: true},
	)
	var stdout bytes.Buffer
	r := Resolver{Store: st, Stdin: strings.NewReader("1\n"), Stdout: &stdout, Palette: PlainPalette(), Ask: true}
	if _, err := r.Tasks(ctx, []string{"продлить"}, store.StatusOpen); err != nil {
		t.Fatalf("Tasks(): %v", err)
	}
	if !strings.Contains(stdout.String(), "which one?") {
		t.Errorf("the question went nowhere: %q", stdout.String())
	}
}

func TestSplitValueKeepsTheParsersReason(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, testProjects)
	r, _, _ := newResolver(st, "")

	cases := []struct {
		line   string
		args   []string
		what   string
		parse  ValueParser
		blamed string
	}{
		{"tt mv 3 Хобби", []string{"3", "Хобби"}, "list",
			func(v string) error { _, err := r.Project(ctx, v); return err }, "Хобби"},
		{"tt due 1 позавчера", []string{"1", "позавчера"}, "date",
			func(v string) error { _, err := dates.Parse(v, listNow); return err }, "позавчера"},
		{"tt pri 1 срочно", []string{"1", "срочно"}, "priority",
			func(v string) error { _, err := model.ParsePriority(v); return err }, "срочно"},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			_, _, err := SplitValue(c.args, c.what, c.parse)
			if err == nil {
				t.Fatalf("SplitValue(%v) divided a line that has no %s in it", c.args, c.what)
			}

			if !strings.Contains(err.Error(), "could not read a "+c.what) {
				t.Errorf("error = %q, want it to say the line would not divide", err)
			}
			reason := c.parse(c.blamed)
			if reason == nil {
				t.Fatalf("the test's own parser accepts %q", c.blamed)
			}
			if !strings.Contains(err.Error(), reason.Error()) {
				t.Errorf("error = %q, want the parser's own reason in it: %q", err, reason)
			}
		})
	}

	_, _, err := SplitValue([]string{"1", "срочно"}, "priority",
		func(v string) error { _, err := model.ParsePriority(v); return err })
	for _, name := range model.PriorityNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %q, want it to list the priority %q tt accepts", err, name)
		}
	}
}

func TestSplitValueBlamesTheLikeliestCandidate(t *testing.T) {

	parse := ValueParser(func(v string) error { return fmt.Errorf("no such list %q", v) })

	byNumber, _, err := SplitValue([]string{"1", "Office", "Hub"}, "list", parse)
	if byNumber != nil || !strings.Contains(err.Error(), `no such list "Office Hub"`) {
		t.Errorf("with a numbered task the whole value is explained; got %q", err)
	}
	byTitle, _, err := SplitValue([]string{"забрать", "посылку", "позавчера"}, "date",
		ValueParser(func(v string) error { return fmt.Errorf("not a date: %q", v) }))
	if byTitle != nil || !strings.Contains(err.Error(), `not a date: "позавчера"`) {
		t.Errorf("with a title the last word is explained; got %q", err)
	}
}

func TestSplitValueTellsAMissingValueFromAnUnreadableOne(t *testing.T) {
	parse := ValueParser(func(v string) error {
		if v == "fri" {
			return nil
		}
		return fmt.Errorf("not a date: %q", v)
	})
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"nothing at all", nil, "name a task and a date"},
		{"a task and no date", []string{"1"}, "names a task and no date"},
		{"a title and no date", []string{"посылку"}, "name a task and a date"},
		{"a date that will not read", []string{"1", "позавчера"}, "could not read a date"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := SplitValue(c.args, "date", parse)
			if err == nil {
				t.Fatalf("SplitValue(%v) was accepted", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("SplitValue(%v) = %q, want %q in it", c.args, err, c.want)
			}
		})
	}
}

func TestResolveTasksAmbiguousQuestionIsOneTaskPerLine(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	ids := seed(t, st, testProjects,
		model.Task{Title: "pay\nthe rent", ProjectId: "p1", DueDate: day(2026, 9, 7), IsAllDay: true},
		model.Task{Title: "pay\tthe bill", ProjectId: "p2", DueDate: day(2026, 9, 8), IsAllDay: true},
	)
	r, _, stderr := newResolver(st, "2\n")

	got, err := r.Tasks(ctx, []string{"pay"}, store.StatusOpen)
	if err != nil {
		t.Fatalf("Tasks(): %v", err)
	}
	if len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("Tasks() = %v, want the second candidate (%s)", got, ids[1])
	}

	asked := stderr.String()
	lines := strings.Split(asked, "\n")
	if len(lines) != 4 {
		t.Fatalf("the question is %d lines for 2 candidates:\n%s", len(lines), asked)
	}
	if !strings.HasPrefix(lines[1], "1  ") || !strings.HasPrefix(lines[2], "2  ") {
		t.Errorf("the candidates are not one numbered line each:\n%s", asked)
	}
	for _, c := range asked {
		if unicode.IsControl(c) && c != '\n' {
			t.Errorf("the question carries %U, which the terminal would act on:\n%q", c, asked)
		}
	}
}

func TestRefusalsFitTheWidth(t *testing.T) {
	long := strings.Repeat("Z", 300)

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no match, with a scope", &NoMatchError{What: "task", Query: long, Scope: "the open tasks"}, "among the open tasks"},
		{"no match, no scope", &NoMatchError{What: "task", Query: long}, "no task matches"},
		{"no match, with a shortlist", &NoMatchError{What: "list", Query: long, Known: []string{"Work", "Personal"}}, "no list matches"},
		{"ambiguous", &AmbiguousError{What: "task", Query: long, Choices: []string{"a", "b"}}, "matches 2 tasks:"},
		{"ambiguous, cut off", &AmbiguousError{What: "task", Query: long, Choices: cutOffTaskChoices(), More: true},
			fmt.Sprintf("matches more than %d tasks:", candidateLimit)},
		{"ambiguous, with a shortlist", &AmbiguousError{What: "list", Query: long, Choices: shortlistedListChoices(), Omitted: 8},
			fmt.Sprintf("matches %d lists, the first %d:", namesInAnError+8, namesInAnError)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := strings.SplitN(c.err.Error(), "\n", 2)[0]
			if n := runeLen(refusalPrefix + first); n > Width {
				t.Errorf("the first line is %d columns wide once %q is on it, want at most %d:\n%q", n, refusalPrefix, Width, first)
			}
			if !strings.Contains(first, c.want) {
				t.Errorf("the refusal no longer says %q: %q", c.want, first)
			}
			if !strings.Contains(first, "ZZZ") {
				t.Errorf("the refusal no longer says what was typed: %q", first)
			}
		})
	}
}

func TestRefusalsKeepTheReferenceOnOneLine(t *testing.T) {
	cases := []struct {
		err   error
		lines int
	}{
		{&NoMatchError{What: "task", Query: "pay\nthe rent", Scope: "the open tasks"}, 1},
		{&AmbiguousError{What: "task", Query: "pay\nthe rent", Choices: cutOffTaskChoices(), More: true}, 1 + candidateLimit},
	}
	for _, c := range cases {
		got := c.err.Error()
		if n := len(strings.Split(got, "\n")); n != c.lines {
			t.Errorf("the refusal is %d lines and not the %d it is made of, so something in it has split in two: %q", n, c.lines, got)
		}
		first := strings.SplitN(got, "\n", 2)[0]
		if !strings.Contains(first, `pay\nthe rent`) {
			t.Errorf("the refusal does not write the reference out on the line that says what went wrong: %q", got)
		}
	}
}

func TestChooseAsksAtTheWidth(t *testing.T) {
	var stderr bytes.Buffer
	r := Resolver{Stdin: strings.NewReader("\n"), Stderr: &stderr, Palette: PlainPalette()}
	found := []model.Task{{Title: "Продлить домен"}, {Title: "Продлить страховку"}}
	if _, err := r.choose(strings.Repeat("Z", 300), found, []string{"1  one", "2  two"}, false); !errors.Is(err, ErrCancelled) {
		t.Fatalf("choose() = %v, want the empty answer to come back as ErrCancelled", err)
	}
	first := strings.SplitN(stderr.String(), "\n", 2)[0]
	if n := runeLen(first); n > Width {
		t.Errorf("the question is %d columns wide, want at most %d:\n%q", n, Width, first)
	}
	if !strings.HasSuffix(first, " matches 2 tasks:") {
		t.Errorf("the question no longer says how many matched: %q", first)
	}
	if !strings.Contains(first, "ZZZ") {
		t.Errorf("the question no longer says what was typed: %q", first)
	}
}

func TestSplitValueRefusalFitsTheWidth(t *testing.T) {
	cases := []struct {
		name   string
		reason error
	}{
		{"a short reason", errors.New(`no list is called "Work"`)},
		{"a long reason", errors.New(strings.Repeat("r", 200))},

		{"a reason of several lines", errors.New("no list matches \"Work\"\nthere is: Inbox, Work")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := SplitValue([]string{strings.Repeat("Z", 300), "Work"}, "list",
				func(string) error { return c.reason })
			if err == nil {
				t.Fatal("SplitValue divided a line that has no list in it")
			}
			tt, rest, found := strings.Cut(err.Error(), "\n")
			if !found {
				t.Fatalf("error = %q, want the parser's reason on a line of its own", err)
			}
			if n := runeLen(refusalPrefix + tt); n > Width {
				t.Errorf("tt's line is %d columns wide once %q is on it, want at most %d:\n%q", n, refusalPrefix, Width, tt)
			}
			if !strings.HasSuffix(tt, ":") || !strings.Contains(tt, "could not read a list at the end of") {
				t.Errorf("the refusal no longer says what it could not read, or lost the colon that introduces the reason: %q", tt)
			}

			if rest != c.reason.Error() {
				t.Errorf("the parser's reason came through as %q, want %q", rest, c.reason)
			}

			if !errors.Is(err, c.reason) {
				t.Errorf("error = %q does not unwrap to the parser's own error", err)
			}
		})
	}
}

func TestChooseRefusesAPastedAnswerAtTheWidth(t *testing.T) {
	var stderr bytes.Buffer
	pasted := strings.Repeat("9", 300)
	r := Resolver{Stdin: strings.NewReader(pasted + "\n"), Stderr: &stderr, Palette: PlainPalette()}
	found := []model.Task{{Title: "one"}, {Title: "two"}}
	_, err := r.choose("x", found, []string{"1  one", "2  two"}, false)
	if err == nil {
		t.Fatal("choose() accepted an answer that names no candidate")
	}
	if n := runeLen(refusalPrefix + err.Error()); n > Width {
		t.Errorf("the refusal is %d columns wide once %q is on it, want at most %d:\n%q", n, refusalPrefix, Width, err)
	}
	if !strings.HasSuffix(err.Error(), " is not one of the numbers offered (1-2)") {
		t.Errorf("the refusal no longer says what was offered: %q", err)
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("the refusal no longer says what was answered: %q", err)
	}
}

func TestKnownLineFitsTheWidth(t *testing.T) {
	names := make([]string, namesInAnError)
	for i := range names {
		names[i] = fmt.Sprintf("List number %d of the account", i+1)
	}

	lines := strings.Split((&NoMatchError{What: "list", Query: "x", Known: names}).Error(), "\n")
	if len(lines) != 2 {
		t.Fatalf("the refusal is %d lines, want the message and the shortlist:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	got := lines[1]
	if n := runeLen(got); n > Width {
		t.Errorf("the shortlist is %d columns wide, want at most %d:\n%q", n, Width, got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the shortlist is more than one line: %q", got)
	}
	shown := strings.Count(got, ", ") + 1
	if shown >= len(names) {
		t.Fatalf("the test does not reach the cut: %q", got)
	}
	if want := fmt.Sprintf(" and %d more", len(names)-shown); !strings.HasSuffix(got, want) {
		t.Errorf("the shortlist ends %q, want it to end %q - a list that stopped has to say so", got, want)
	}

	if got := knownLine([]string{"Work", "Personal"}, 0); got != "there is: Work, Personal" {
		t.Errorf("knownLine of two short names = %q, want them both and no tail", got)
	}

	if got, want := knownLine([]string{"Work\nHome", "Inbox"}, 0), `there is: Work\nHome, Inbox`; got != want {
		t.Errorf("knownLine of a name holding a newline = %q, want %q", got, want)
	}

	long := strings.Repeat("L", 5) + "\t" + strings.Repeat("L", 90)
	want := "there is: " + strings.Repeat("L", 5) + `\t` + strings.Repeat("L", nameInAnError-len(ellipsis)-5-len(`\t`)) + ellipsis
	if got := knownLine([]string{long}, 0); got != want {
		t.Errorf("knownLine of one long name = %q, want %q", got, want)
	}
}

func TestKnownLineCountsEveryNameItDoesNotShow(t *testing.T) {
	const lists = 20
	names := make([]string, lists)
	for i := range names {
		names[i] = fmt.Sprintf("Work%d", i+1)
	}

	got := knownLine(shortlist(names))
	if n := runeLen(got); n > Width {
		t.Errorf("the shortlist is %d columns wide, want at most %d:\n%q", n, Width, got)
	}

	body := strings.TrimPrefix(got, "there is: ")
	shown := strings.Count(body, ", ") + 1
	if shown >= lists {
		t.Fatalf("the test does not reach the cut: %q", got)
	}
	for _, piece := range strings.Split(strings.TrimSuffix(body, andMore(lists-shown)), ", ") {
		if strings.HasPrefix(piece, "and ") {
			t.Errorf("a sentence about the missing names is standing among the names: %q", got)
		}
	}
	if want := andMore(lists - shown); !strings.HasSuffix(got, want) {
		t.Errorf("the shortlist ends %q, want it to end %q - %d names were shown out of %d", got, want, shown, lists)
	}
}

func TestKnownLineShowsEveryNameThatFits(t *testing.T) {

	names := []string{"Inbox", "Work", "Personal", "Shopping", "Health", "Travel", "Home", "Ideas", "Bank"}
	want := "there is: " + strings.Join(names, ", ")
	if runeLen(want) > Width {
		t.Fatalf("the fixture no longer fits on one line at %d columns, so the test asks nothing: %q", runeLen(want), want)
	}
	if got := knownLine(names, 0); got != want {
		t.Errorf("knownLine of an account of nine lists = %q (%d columns), want %q (%d columns) - every one of them fitted",
			got, runeLen(got), want, runeLen(want))
	}
}

func TestKnownLineDoesNotHideANameOnALineThatStops(t *testing.T) {

	all := []string{
		"Personal stuff", "Work projects", "Home and yard", "Shopping list",
		"Health", "Travel", "Ideas", "Bank", "Errands", "Reading", "Garden",
		"Car", "Taxes",
	}
	want := "there is: Personal stuff, Work projects, Home and yard, Shopping list and 9 more"
	if runeLen(want) != Width {
		t.Fatalf("the answer this pins is %d columns and not the %d it was written at, so the fixture no longer asks anything: %q",
			runeLen(want), Width, want)
	}
	if got := knownLine(shortlist(all)); got != want {
		t.Errorf("knownLine of an account of %d lists = %q (%d columns), want %q (%d columns) - the fourth name fitted",
			len(all), got, runeLen(got), want, runeLen(want))
	}
}

func TestKnownLineNamesAsManyAsTheLineHasRoomFor(t *testing.T) {
	for count := 1; count <= namesInAnError; count++ {
		for head := 1; head <= nameInAnError; head++ {
			for width := 1; width <= nameInAnError; width++ {
				for _, omitted := range []int{0, 1, 8, 12} {
					names := make([]string, count)
					for i := range names {
						w := width
						if i == 0 {
							w = head
						}
						names[i] = strings.Repeat(string(rune('a'+i%26)), w)
					}

					lines := make([]string, count+1)
					for k := range lines {
						lines[k] = "there is: " + strings.Join(names[:k], ", ") + andMore(count-k+omitted)
					}

					want := 1
					for k := 1; k <= count; k++ {
						if runeLen(lines[k]) <= Width {
							want = k
						}
					}
					if got := knownLine(names, omitted); got != lines[want] {
						t.Errorf("knownLine of %d names, the first of %d columns and the rest of %d, %d held back = %q (%d columns), want %q (%d columns) - %d of them fit",
							count, head, width, omitted, got, runeLen(got), lines[want], runeLen(lines[want]), want)
					}
				}
			}
		}
	}
}

func TestAmbiguousErrorPutsItsCandidatesAtTheLeftEdge(t *testing.T) {
	rows := []Row{
		{Num: 1, Project: "Wo" + strings.Repeat("\t", 8) + "rk", Task: model.Task{Title: strings.Repeat("T", 90)}},
	}
	choices := ListLines(rows, PlainPalette(), listNow)
	if n := runeLen(choices[0]); n != Width {
		t.Fatalf("the fixture row is %d columns wide, want a row that fills the width exactly", n)
	}
	e := &AmbiguousError{What: "task", Query: "T", Choices: choices}
	for i, line := range strings.Split(e.Error(), "\n") {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d of the refusal is %d columns wide, want at most %d:\n%q", i+1, n, Width, line)
		}
	}
	if !strings.Contains(e.Error(), "\n"+choices[0]) {
		t.Errorf("a candidate is not at the left edge:\n%s", e.Error())
	}
}

func TestAmbiguousListErrorEscapesTheNamesItPrints(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, []model.Project{
		{Id: "p1", Name: "Work\nHome"},
		{Id: "p2", Name: "Work\x1b[31m\tred"},
		{Id: "p3", Name: "Work " + strings.Repeat("!", 120)},
	})
	r, _, _ := newResolver(st, "")

	_, err := r.Project(ctx, "work")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Project(work) = %v, want an AmbiguousError", err)
	}
	lines := strings.Split(err.Error(), "\n")

	if got, want := len(lines)-1, len(ambiguous.Choices); got != want {
		t.Fatalf("the refusal puts %d lines under its heading for %d candidates:\n%s", got, want, err)
	}
	if !strings.Contains(lines[0], "matches 3 lists:") {
		t.Errorf("the heading reads %q, want it to say it matched three lists", lines[0])
	}
	for i, line := range lines {
		if n := runeLen(line); n > Width {
			t.Errorf("line %d of the refusal is %d columns wide, want at most %d:\n%q", i+1, n, Width, line)
		}
		if strings.ContainsAny(line, "\x1b\t") {
			t.Errorf("line %d of the refusal reaches the terminal with a control character in it: %q", i+1, line)
		}
	}
}

func TestAmbiguousListErrorCountsEveryMatch(t *testing.T) {
	const lists = 20
	projects := make([]model.Project, lists)
	for i := range projects {
		projects[i] = model.Project{Id: fmt.Sprintf("p%d", i+1), Name: fmt.Sprintf("Work%d", i+1)}
	}
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, projects)
	r, _, _ := newResolver(st, "")

	_, err := r.Project(ctx, "work")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Project(work) = %v, want an AmbiguousError", err)
	}
	if got := len(ambiguous.Choices) + ambiguous.Omitted; got != lists {
		t.Fatalf("the error accounts for %d lists, want the %d that matched", got, lists)
	}
	lines := strings.Split(err.Error(), "\n")
	if want := fmt.Sprintf("matches %d lists, the first %d:", lists, len(ambiguous.Choices)); !strings.Contains(lines[0], want) {
		t.Errorf("the heading reads %q, want it to contain %q", lines[0], want)
	}

	if got, want := len(lines)-1, len(ambiguous.Choices); got != want {
		t.Errorf("the refusal puts %d lines under its heading, want the %d it says it named:\n%s", got, want, err)
	}
	for i, line := range lines[1:] {
		if strings.HasPrefix(line, "and ") && strings.HasSuffix(line, " more") {
			t.Errorf("candidate %d is a sentence about the missing names rather than a list: %q", i+1, line)
		}
	}
}

func TestRefusalsCountInEnglish(t *testing.T) {
	cases := []struct {
		err  *AmbiguousError
		want string
	}{
		{&AmbiguousError{What: "task", Query: "x", Choices: []string{"a"}}, `"x" matches 1 task:`},
		{&AmbiguousError{What: "task", Query: "x", Choices: []string{"a", "b"}}, `"x" matches 2 tasks:`},
		{&AmbiguousError{What: "task", Query: "x", Choices: cutOffTaskChoices(), More: true},
			fmt.Sprintf(`"x" matches more than %d tasks:`, candidateLimit)},
		{&AmbiguousError{What: "list", Query: "x", Choices: shortlistedListChoices(), Omitted: 8},
			fmt.Sprintf(`"x" matches %d lists, the first %d:`, namesInAnError+8, namesInAnError)},
	}
	for _, c := range cases {
		if got := strings.SplitN(c.err.Error(), "\n", 2)[0]; got != c.want {
			t.Errorf("the refusal reads %q, want %q", got, c.want)
		}
	}
}

func TestMatchListNameAgreesWithTheResolver(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seed(t, st, []model.Project{
		{Id: "p1", Name: "Office Hub", Kind: "TASK", SortOrder: 1},
		{Id: "p2", Name: "Home", Kind: "TASK", SortOrder: 2},
		{Id: "p3", Name: "Homework", Kind: "TASK", SortOrder: 3},
		{Id: "p4", Name: "Work ", Kind: "TASK", SortOrder: 4},
		{Id: "p5", Name: "Workshop", Kind: "TASK", SortOrder: 5},
		{Id: "p6", Name: "Личное", Kind: "TASK", SortOrder: 6},
		{Id: "p7", Name: "личное", Kind: "TASK", SortOrder: 7},
		{Id: "p8", Name: "Ремонт квартиры", Kind: "TASK", SortOrder: 8, Closed: true},
		{Id: "p9", Name: "Заметки", Kind: "NOTE", SortOrder: 9},
		{Id: "p10", Name: "Дубль", Kind: "TASK", SortOrder: 10},
		{Id: "p11", Name: "Дубль", Kind: "TASK", SortOrder: 11},
	})
	r, _, _ := newResolver(st, "")

	all, err := st.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := projectNames(all)

	for _, query := range []string{
		"",
		"   ",
		"Office Hub",
		"office hub",
		"office",
		"Home",
		"home",
		"Work",
		"  Work  ",
		"shop",
		"Личное",
		"личн",
		"квартир",
		"Заметки",
		"Дубль",
		"о",
		"молоко",
	} {
		match, matched := MatchListName(names, query)
		p, err := r.Project(ctx, query)

		var ambiguous *AmbiguousError
		var noMatch *NoMatchError
		switch {
		case err == nil:
			if match != ListNameOne || len(matched) != 1 || matched[0] != p.Name {
				t.Errorf("%q resolved to %q; MatchListName said %v, %v", query, p.Name, match, matched)
			}
		case errors.As(err, &ambiguous):
			if match != ListNameSeveral {
				t.Errorf("%q is ambiguous for the resolver; MatchListName said %v, %v", query, match, matched)
				continue
			}
			if got := projectChoices(matched); !slices.Equal(got, ambiguous.Choices) {
				t.Errorf("%q: MatchListName matched %v, the resolver named %v", query, got, ambiguous.Choices)
			}
		case errors.As(err, &noMatch):
			if match != ListNameNone || len(matched) != 0 {
				t.Errorf("%q matches no list for the resolver; MatchListName said %v, %v", query, match, matched)
			}
		default:

			if match != ListNameNone || len(matched) != 0 {
				t.Errorf("%q was refused as %v; MatchListName said %v, %v", query, err, match, matched)
			}
		}
	}
}
