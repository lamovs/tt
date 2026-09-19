package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
	"github.com/movsar/tt/internal/tasklist"
)

func batchDocument(t *testing.T, src string) tasklist.Document {
	t.Helper()
	doc, err := tasklist.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func batchList(name, id string) model.Project { return model.Project{Id: id, Name: name} }

func batchPlanError(t *testing.T, err error) *BatchPlanError {
	t.Helper()
	var planErr *BatchPlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("error = %v, want a *BatchPlanError", err)
	}
	return planErr
}

func TestPlanBatchResolvesListsAndKeepsChildrenBehindTheirParents(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC)
	doc := batchDocument(t, "# Work\nBuy milk\n\t[ ] two liters\n\t[x] receipt\nTrip\n\tBook tickets\n# Home\n[x] Water plants\n")

	plan, err := PlanBatch(ctx, st, doc, model.Project{}, now)
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if plan.Version != batchPlanVersion || !plan.Created.Equal(now) {
		t.Fatalf("plan head = %d %v, want version %d at %v", plan.Version, plan.Created, batchPlanVersion, now)
	}
	if len(plan.Steps) != 4 {
		t.Fatalf("steps = %d, want 4: %+v", len(plan.Steps), plan.Steps)
	}
	for i, want := range []struct {
		title    string
		list     string
		id       string
		parent   int
		complete bool
		line     int
	}{
		{"Buy milk", "Work", "work", -1, false, 2},
		{"Trip", "Work", "work", -1, false, 5},
		{"Book tickets", "Work", "work", 1, false, 6},
		{"Water plants", "Home", "home", -1, true, 8},
	} {
		step := plan.Steps[i]
		if step.Task.Title != want.title || step.List != want.list || step.Task.ProjectId != want.id {
			t.Errorf("step %d = %q in %q (%q), want %q in %q (%q)", i+1,
				step.Task.Title, step.List, step.Task.ProjectId, want.title, want.list, want.id)
		}
		if step.Parent != want.parent || step.Complete != want.complete || step.Line != want.line {
			t.Errorf("step %d = parent %d, complete %v, line %d; want %d, %v, %d", i+1,
				step.Parent, step.Complete, step.Line, want.parent, want.complete, want.line)
		}
	}
	items := plan.Steps[0].Task.Items
	if len(items) != 2 || items[0].Title != "two liters" || items[1].Title != "receipt" {
		t.Fatalf("checklist = %+v, want the two items in order", items)
	}
	if items[0].Status.Done() || !items[0].CompletedTime.IsZero() {
		t.Errorf("open item = %+v, want it open and without a completion time", items[0])
	}
	if items[1].Status != model.ItemDone || !items[1].CompletedTime.Equal(now) {
		t.Errorf("done item = %+v, want it done at %v", items[1], now)
	}
	if len(plan.Steps[2].Task.Items) != 0 {
		t.Errorf("a child step carries a checklist: %+v", plan.Steps[2].Task.Items)
	}
	if got := plan.Lists(); len(got) != 2 || got[0] != "Work" || got[1] != "Home" {
		t.Errorf("Lists() = %v, want Work then Home", got)
	}
	if got := plan.CountItems(); got != 2 {
		t.Errorf("CountItems() = %d, want 2", got)
	}
}

func TestPlanBatchTakesTheDefaultListOnlyForTasksAboveTheFirstHeading(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	doc := batchDocument(t, "Loose\n# Home\nInside\n")

	plan, err := PlanBatch(ctx, st, doc, batchList("Work", "work"), now)
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].List != "Work" || plan.Steps[1].List != "Home" {
		t.Fatalf("steps = %+v, want the loose task in Work and the other in Home", plan.Steps)
	}

	_, err = PlanBatch(ctx, st, doc, model.Project{}, now)
	planErr := batchPlanError(t, err)
	if planErr.Line != 1 || !strings.Contains(planErr.Msg, "no default list") {
		t.Fatalf("error = %+v, want line 1 saying there is no default list", planErr)
	}
}

func TestPlanBatchRefusesListsItCannotWriteTo(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()

	_, err := PlanBatch(ctx, st, batchDocument(t, "# Nowhere\nBuy milk\n"), model.Project{}, now)
	planErr := batchPlanError(t, err)
	if !strings.Contains(planErr.Msg, "line 1") || !strings.Contains(planErr.Msg, "Nowhere") || !strings.Contains(planErr.Msg, "tt project add") {
		t.Fatalf("unknown list error = %+v, want line 1 naming the list and tt project add", planErr)
	}

	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}, {Id: "twin", Name: "Work"}}); err != nil {
		t.Fatal(err)
	}
	_, err = PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\n"), model.Project{}, now)
	planErr = batchPlanError(t, err)
	if !strings.Contains(planErr.Msg, "more than one cached list") {
		t.Fatalf("ambiguous list error = %+v, want it to say the name is not unique", planErr)
	}

	_, err = PlanBatch(ctx, st, batchDocument(t, "Buy milk\n"), model.Project{Id: "shut", Name: "Shut", Closed: true}, now)
	planErr = batchPlanError(t, err)
	if !strings.Contains(planErr.Msg, "list is closed") {
		t.Fatalf("closed list error = %+v, want it to say the list is closed", planErr)
	}
}

// batchHeadingRefusal plans a one task document under heading and returns why
// the heading was refused.
func batchHeadingRefusal(t *testing.T, st *store.Store, heading string) *BatchPlanError {
	t.Helper()
	_, err := PlanBatch(context.Background(), st, batchDocument(t, "# "+heading+"\nBuy milk\n"), model.Project{}, time.Now())
	return batchPlanError(t, err)
}

func TestPlanBatchNamesTheListsCloseToAHeadingNothingMatches(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()

	nothingClose := batchHeadingRefusal(t, st, "Nowhere")
	// A rendered refusal is fitted for the width the verb alone leaves in
	// front of it, so it names its line in the block instead of carrying a
	// "line N: " that a caller would print in front of the whole thing.
	if !nothingClose.Rendered || nothingClose.Line != 0 {
		t.Fatalf("refusal = %+v, want it rendered for reading with no line in front", nothingClose)
	}
	for _, want := range []string{`no list matches "Nowhere"`, "there is: Home, Work", "tt project add", "line 1 of the document"} {
		if !strings.Contains(nothingClose.Msg, want) {
			t.Errorf("a heading nothing is close to does not say %q:\n%s", want, nothingClose.Msg)
		}
	}

	closest := batchHeadingRefusal(t, st, "Hom")
	if !strings.Contains(closest.Msg, "there is: Home") || strings.Contains(closest.Msg, "Work") {
		t.Errorf("a heading one list is close to = %q, want Home named on its own", closest.Msg)
	}

	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}, {Id: "twin", Name: "Work"}}); err != nil {
		t.Fatal(err)
	}
	twins := batchHeadingRefusal(t, st, "Work")
	if twins.Rendered || !strings.Contains(twins.Msg, "more than one cached list") {
		t.Errorf("two lists of one name = %+v, want the name refused as not unique", twins)
	}
}

// A rendered refusal is printed behind the verb that reports it, so the room
// it is fitted to is what the verb leaves of the terminal line, not the whole
// of it. A heading may be as long as whoever wrote the document liked.
func TestPlanBatchFitsTheRefusalBehindTheVerbThatPrintsIt(t *testing.T) {
	_, st := actionStore(t)
	const verb = "tt: batch: "

	refusal := batchHeadingRefusal(t, st, strings.Repeat("Nowhere", 20))
	lines := strings.Split(refusal.Error(), "\n")
	if n := cli.DisplayWidth(verb + lines[0]); n > cli.Width {
		t.Errorf("printed behind %q the refusal takes %d columns, want at most %d:\n%s", verb, n, cli.Width, lines[0])
	}
	for _, line := range lines[1:] {
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a refusal line takes %d columns, want at most %d:\n%s", n, cli.Width, line)
		}
	}
	if !strings.Contains(refusal.Msg, "line 1 of the document") {
		t.Errorf("refusal = %q, want it to name the line the heading is on", refusal.Msg)
	}
}

func TestPlanBatchEscapesAndShortensTheListsItNames(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()

	const zeroWidth = "Work\u200bshop"
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "shop", Name: zeroWidth}}); err != nil {
		t.Fatal(err)
	}
	foreign := batchHeadingRefusal(t, st, "Work")
	if strings.Contains(foreign.Msg, zeroWidth) || !strings.Contains(foreign.Msg, `Work\u200bshop`) {
		t.Errorf("a list name of its own = %q, want the invisible rune escaped", foreign.Msg)
	}

	var many []model.Project
	for i := 0; i < 40; i++ {
		many = append(many, model.Project{Id: fmt.Sprintf("p%02d", i), Name: fmt.Sprintf("List number %02d", i)})
	}
	if err := st.ReplaceProjects(ctx, many); err != nil {
		t.Fatal(err)
	}
	crowd := batchHeadingRefusal(t, st, "Nowhere")
	lines := strings.Split(crowd.Msg, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "there is: ") {
		t.Fatalf("refusal = %q, want the lists on the second line", crowd.Msg)
	}
	if n := cli.DisplayWidth(lines[1]); n > cli.Width {
		t.Errorf("the lists take %d columns, want at most %d:\n%q", n, cli.Width, lines[1])
	}
	if !strings.Contains(lines[1], " more") {
		t.Errorf("the lists = %q, want it to say how many it left out", lines[1])
	}
	for _, line := range lines {
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a refusal line takes %d columns, want at most %d:\n%q", n, cli.Width, line)
		}
	}
}

func TestPlanBatchRefusesTitlesItemsAndSizesItCannotWrite(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	work := batchList("Work", "work")

	one := func(task tasklist.Task) tasklist.Document {
		return tasklist.Document{Groups: []tasklist.Group{{Tasks: []tasklist.Task{task}, Line: 1}}}
	}
	for _, c := range []struct {
		name string
		doc  tasklist.Document
		line int
		want string
	}{
		{"blank title", one(tasklist.Task{Title: "  ", Line: 3}), 3, "title is required"},
		{"control character", one(tasklist.Task{Title: "bell\ahere", Line: 4}), 4, "control characters"},
		{"blank item", one(tasklist.Task{Title: "Trip", Line: 5,
			Items: []tasklist.Item{{Title: " ", Line: 6}}}), 6, "item title is required"},
		{"no tasks", tasklist.Document{}, 0, "no tasks"},
	} {
		_, err := PlanBatch(ctx, st, c.doc, work, now)
		planErr := batchPlanError(t, err)
		if planErr.Line != c.line || !strings.Contains(planErr.Msg, c.want) {
			t.Errorf("%s = %+v, want line %d saying %q", c.name, planErr, c.line, c.want)
		}
	}

	wide := tasklist.Task{Title: "Trip", Line: 1}
	for i := 0; i <= tasklist.MaxItemsPerTask; i++ {
		wide.Items = append(wide.Items, tasklist.Item{Title: "item", Line: i + 2})
	}
	_, err := PlanBatch(ctx, st, one(wide), work, now)
	if planErr := batchPlanError(t, err); !strings.Contains(planErr.Msg, "checklist items") {
		t.Errorf("wide task = %+v, want the checklist limit", planErr)
	}

	long := tasklist.Group{Line: 1}
	for i := 0; i <= tasklist.MaxTasks; i++ {
		long.Tasks = append(long.Tasks, tasklist.Task{Title: "task", Line: i + 1})
	}
	_, err = PlanBatch(ctx, st, tasklist.Document{Groups: []tasklist.Group{long}}, work, now)
	if planErr := batchPlanError(t, err); planErr.Line != 0 || !strings.Contains(planErr.Msg, "more than the") {
		t.Errorf("long document = %+v, want the task limit for the document as a whole", planErr)
	}
}

func TestPlanBatchRefusesADoneTaskThatHasChildTasks(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()

	_, err := PlanBatch(ctx, st, batchDocument(t, "# Work\n[x] Trip\n\tBook tickets\n"), model.Project{}, now)
	planErr := batchPlanError(t, err)
	if planErr.Line != 2 || !strings.Contains(planErr.Msg, "cannot have child tasks") {
		t.Errorf("refusal = %+v, want line 2 saying a task marked done may have no children", planErr)
	}
	// The refusal comes before anything is written: neither the cache nor a
	// preview holds a batch tt would never be able to send whole.
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d tasks were created for a document that was refused: %+v", len(tasks), tasks)
	}
	if left := batchPreviewCount(t, st); left != 0 {
		t.Errorf("%d previews were saved for a document that was refused", left)
	}

	// Only child tasks are refused. A done task with a checklist under it is
	// sent as one create and completed after it.
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\n[x] Trip\n\t[ ] passport\n"), model.Project{}, now)
	if err != nil {
		t.Fatalf("a done task with checklist items = %v, want it planned", err)
	}
	if len(plan.Steps) != 1 || !plan.Steps[0].Complete || len(plan.Steps[0].Task.Items) != 1 {
		t.Errorf("plan = %+v, want the one done task carrying its item", plan.Steps)
	}
}

func TestPlanBatchPassesOverAHeadingNothingIsWrittenUnder(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()

	// The heading names no cached list, but nothing is written under it, so
	// the document is not refused over a list it never writes to.
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Nowhere\n\n# Work\nBuy milk\n"), model.Project{}, now)
	if err != nil {
		t.Fatalf("PlanBatch: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Task.Title != "Buy milk" || plan.Steps[0].Task.ProjectId != "work" {
		t.Fatalf("plan = %+v, want the one task in Work", plan.Steps)
	}
	if lists := plan.Lists(); len(lists) != 1 || lists[0] != "Work" {
		t.Errorf("lists = %v, want Work alone", lists)
	}

	// A document of headings alone still holds no tasks, and says so rather
	// than naming one of them.
	_, err = PlanBatch(ctx, st, batchDocument(t, "# Nowhere\n# Elsewhere\n"), model.Project{}, now)
	if planErr := batchPlanError(t, err); planErr.Line != 0 || !strings.Contains(planErr.Msg, "no tasks") {
		t.Errorf("headings alone = %+v, want the document reported as holding no tasks", planErr)
	}
}

func TestBatchPreviewIsAcceptedOnceAndOnlyWhileItLasts(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\n"), model.Project{}, now)
	if err != nil {
		t.Fatal(err)
	}

	id, err := SaveBatchPreview(ctx, st, plan, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimBatchPreview(ctx, st, "beef", now); !errors.Is(err, ErrBatchPreviewInvalid) {
		t.Errorf("short id = %v, want %v", err, ErrBatchPreviewInvalid)
	}
	if _, _, err := ClaimBatchPreview(ctx, st, strings.Repeat("a", 64), now); !errors.Is(err, ErrBatchPreviewUnavailable) {
		t.Errorf("unknown id = %v, want %v", err, ErrBatchPreviewUnavailable)
	}
	claimed, text, err := ClaimBatchPreview(ctx, st, id, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed.Steps) != 1 || claimed.Steps[0].Task.Title != "Buy milk" || claimed.Steps[0].Parent != -1 {
		t.Fatalf("claimed plan = %+v, want the one task back", claimed.Steps)
	}
	if _, _, err := ClaimBatchPreview(ctx, st, id, now); !errors.Is(err, ErrBatchPreviewUnavailable) {
		t.Errorf("second claim = %v, want %v", err, ErrBatchPreviewUnavailable)
	}
	if err := RestoreBatchPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimBatchPreview(ctx, st, id, now); err != nil {
		t.Errorf("claim after restore = %v, want it accepted again", err)
	}

	if _, err := SaveBatchPreview(ctx, st, plan, now); err != nil {
		t.Fatal(err)
	}
	late := now.Add(BatchPlanLifetime + time.Minute)
	if _, _, err := ClaimBatchPreview(ctx, st, id, late); !errors.Is(err, ErrBatchPreviewExpired) {
		t.Errorf("stale claim = %v, want %v", err, ErrBatchPreviewExpired)
	}
	if left := batchPreviewCount(t, st); left != 0 {
		t.Errorf("%d expired previews were kept", left)
	}
}

func TestClaimBatchPreviewReadsTheListsOfThePlanAgain(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\n"), model.Project{}, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := SaveBatchPreview(ctx, st, plan, now)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name     string
		projects []model.Project
	}{
		{"the list left the cache", []model.Project{{Id: "home", Name: "Home"}}},
		{"the list closed", []model.Project{{Id: "work", Name: "Work", Closed: true}}},
	} {
		if err := st.ReplaceProjects(ctx, c.projects); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ClaimBatchPreview(ctx, st, id, now); !errors.Is(err, ErrBatchPreviewListChanged) {
			t.Errorf("%s: claim = %v, want %v", c.name, err, ErrBatchPreviewListChanged)
		}
		// A refusal spends nothing: the preview is still there, and the ID
		// the reader was given still means something.
		if left := batchPreviewCount(t, st); left != 1 {
			t.Errorf("%s: %d previews left, want the refused one kept", c.name, left)
		}
	}

	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := ClaimBatchPreview(ctx, st, id, now)
	if err != nil {
		t.Fatalf("claim with the list back = %v, want the plan", err)
	}
	if len(claimed.Steps) != 1 || claimed.Steps[0].Task.ProjectId != "work" {
		t.Errorf("claimed plan = %+v, want the one task in work", claimed.Steps)
	}
}

func TestBatchPreviewsAndAIPreviewsAreKeptApart(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\n"), model.Project{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveBatchPreview(ctx, st, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAIPreview(ctx, st, AIPlan{Version: aiPlanVersion, Created: now.UTC()}, now); err != nil {
		t.Fatal(err)
	}
	if err := PruneBatchPreviews(ctx, st, now.Add(BatchPlanLifetime+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := batchPreviewCount(t, st); got != 0 {
		t.Errorf("batch previews left = %d, want 0", got)
	}
	var ai int
	if err := st.DB().QueryRow(`SELECT count(*) FROM meta WHERE key GLOB 'ai_preview_*'`).Scan(&ai); err != nil {
		t.Fatal(err)
	}
	if ai != 1 {
		t.Errorf("ai previews left = %d, want the batch prune to leave them alone", ai)
	}
}

func batchPreviewCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM meta WHERE key GLOB 'batch_preview_*'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApplyBatchPlanCreatesEveryTaskUnderOneUndoGroup(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	doc := batchDocument(t, "# Work\nBuy milk\n\t[ ] two liters\n[x] Water plants\n")
	plan, err := PlanBatch(ctx, st, doc, model.Project{}, now)
	if err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}
	if applied.GroupID == "" || len(applied.Results) != 2 {
		t.Fatalf("applied = %+v, want two tasks under one group", applied)
	}
	if applied.Results[0].Step != 1 || applied.Results[1].Step != 2 {
		t.Errorf("results are not numbered from 1: %+v", applied.Results)
	}
	milk := readTask(t, st, applied.Results[0].Task.Id)
	if milk.Title != "Buy milk" || milk.ProjectId != "work" || milk.Status.Done() {
		t.Errorf("first task = %+v, want an open Buy milk in work", milk)
	}
	if len(milk.Items) != 1 || milk.Items[0].Title != "two liters" {
		t.Errorf("first task checklist = %+v, want the one item", milk.Items)
	}
	plants := readTask(t, st, applied.Results[1].Task.Id)
	if !plants.Status.Done() || !applied.Results[1].Done {
		t.Errorf("second task = %+v (done %v), want it completed right after it was created", plants, applied.Results[1].Done)
	}

	entries, err := st.LastUndoGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("undo group holds %d records, want the two creations and the completion", len(entries))
	}
	for _, entry := range entries {
		if entry.Group != applied.GroupID {
			t.Errorf("record %+v is outside the group %q", entry, applied.GroupID)
		}
	}
}

func TestApplyBatchPlanHangsAChildUnderItsParentInTheSameGroup(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nTrip\n\t[ ] passport\n\tBook tickets\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || plan.Steps[1].Parent != 0 {
		t.Fatalf("plan = %+v, want the child planned behind its parent", plan.Steps)
	}

	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}
	if len(applied.Results) != 2 {
		t.Fatalf("applied = %+v, want both tasks", applied)
	}
	trip := readTask(t, st, applied.Results[0].Task.Id)
	if trip.ParentId != "" || len(trip.Items) != 1 {
		t.Errorf("parent = %+v, want a top-level task with its one item", trip)
	}
	child := readTask(t, st, applied.Results[1].Task.Id)
	if child.Title != "Book tickets" || child.ParentId != trip.Id {
		t.Fatalf("child = %+v, want Book tickets under %s", child, trip.Id)
	}
	if applied.Results[1].Task.ParentId != trip.Id {
		t.Errorf("reported child = %+v, want the relationship it was left with", applied.Results[1].Task)
	}

	entries, err := st.LastUndoGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("undo group holds %d records, want the two creations and the relationship", len(entries))
	}
	for _, entry := range entries {
		if entry.Group != applied.GroupID {
			t.Errorf("record %+v is outside the group %q", entry, applied.GroupID)
		}
	}
	if _, err := st.ApplyUndoGroup(ctx, entries); err != nil {
		t.Fatalf("one undo of the whole batch: %v", err)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("one undo left %d tasks: %+v", len(tasks), tasks)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Errorf("last undo = %v, want the whole batch reversed", err)
	}
}

// A document may no longer ask for a parent marked done with children under
// it, so the plan is written out here rather than read from one. What it pins
// is the order of the two passes: every task first, the completions after, so
// that a child is hung under a parent while that parent is still open.
// The tasks a document marked [x] are completed in a pass of their own,
// once every task of the plan and every relationship between them is there.
func TestApplyBatchPlanCompletesTheTasksTheDocumentMarkedDone(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\n[x] Buy milk\nTrip\n\tBook tickets\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}
	if len(applied.Results) != 3 {
		t.Fatalf("applied = %+v, want all three tasks", applied)
	}
	milk := readTask(t, st, applied.Results[0].Task.Id)
	if !milk.Status.Done() || !applied.Results[0].Done {
		t.Errorf("the task marked [x] = %+v (done %v), want it completed", milk, applied.Results[0].Done)
	}
	if !applied.Results[0].Task.Status.Done() {
		t.Errorf("reported task = %+v, want the completion it was left with", applied.Results[0].Task)
	}
	trip := readTask(t, st, applied.Results[1].Task.Id)
	if trip.Status.Done() {
		t.Errorf("parent = %+v, want the task the document left open untouched", trip)
	}
	child := readTask(t, st, applied.Results[2].Task.Id)
	if child.Title != "Book tickets" || child.ParentId != trip.Id || child.Status.Done() {
		t.Fatalf("child = %+v, want an open Book tickets under %s", child, trip.Id)
	}

	entries, err := st.LastUndoGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("undo group holds %d records, want the three creations, the relationship and the completion", len(entries))
	}
	for _, entry := range entries {
		if entry.Group != applied.GroupID {
			t.Errorf("record %+v is outside the group %q", entry, applied.GroupID)
		}
	}
	if _, err := st.ApplyUndoGroup(ctx, entries); err != nil {
		t.Fatalf("one undo of the whole batch: %v", err)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("one undo left %d tasks: %+v", len(tasks), tasks)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Errorf("last undo = %v, want the whole batch reversed", err)
	}
}

// The document says which items are done and the preview prints exactly
// that, so completing the task it marked [x] leaves the items the reader left
// open alone. Closing a task by hand ticks its open items off; a batch may
// not, or what it applies is not what it showed.
func TestApplyBatchPlanLeavesTheOpenItemsOfADoneTaskOpen(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	doc := batchDocument(t, "# Work\n[x] Water plants\n\t[ ] front yard\n\t[x] back yard\n")
	plan, err := PlanBatch(ctx, st, doc, model.Project{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatalf("ApplyBatchPlan: %v", err)
	}
	task := readTask(t, st, applied.Results[0].Task.Id)
	if !task.Status.Done() {
		t.Fatalf("task = %+v, want the task the document marked [x] completed", task)
	}
	if len(task.Items) != 2 {
		t.Fatalf("items = %+v, want both items of the document", task.Items)
	}
	if task.Items[0].Title != "front yard" || task.Items[0].Status.Done() {
		t.Errorf("items[0] = %+v, want front yard left open, the way the preview showed it", task.Items[0])
	}
	if task.Items[1].Title != "back yard" || !task.Items[1].Status.Done() {
		t.Errorf("items[1] = %+v, want back yard done, the way the document wrote it", task.Items[1])
	}
}

// PlanBatch refuses a document that both marks a task [x] and hangs other
// tasks under it, so this plan can only be built by hand - and the store
// refuses it again in the completion pass, where the relationship of the
// child is still queued: closing the parent would park that relationship on
// the push, park it again on every retry, and lose it on the next pull.
func TestApplyBatchPlanRefusesToCompleteAParentOfAQueuedChild(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	now := time.Now()
	plan := BatchPlan{Version: batchPlanVersion, Created: now.UTC(), Steps: []BatchStep{
		{Task: model.Task{Title: "Trip", ProjectId: "work"}, List: "Work", Complete: true, Line: 2, Parent: -1},
		{Task: model.Task{Title: "Book tickets", ProjectId: "work"}, List: "Work", Line: 3, Parent: 0},
	}}

	_, err := ApplyBatchPlan(ctx, st, plan)
	var failed *BatchApplyError
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v, want a *BatchApplyError", err)
	}
	if failed.Step != 1 || failed.Line != 2 || failed.Rollback != nil {
		t.Fatalf("failure = %+v, want the completion of the first task, line 2, reversed whole", failed)
	}
	if !errors.Is(err, store.ErrChildLinkUnsent) {
		t.Errorf("failure = %v, want the unsent child relationship refused", failed.Err)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d tasks were left behind: %+v", len(tasks), tasks)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Errorf("last undo = %v, want the reversal to leave no record of the batch", err)
	}
}

func TestApplyBatchPlanRefusesAParentItDoesNotCreateFirst(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		parent int
	}{
		{"a step that comes later", 1},
		{"the step itself", 0},
		{"past the last step", 7},
		{"a negative that is not -1", -2},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\nFix the sink\n"), model.Project{}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			plan.Steps[0].Parent = c.parent

			_, err = ApplyBatchPlan(ctx, st, plan)
			planErr := batchPlanError(t, err)
			if planErr.Line != plan.Steps[0].Line || !strings.Contains(planErr.Msg, "does not create before it") {
				t.Errorf("error = %+v, want line %d refusing the parent", planErr, plan.Steps[0].Line)
			}
			tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 0 {
				t.Errorf("a refused plan created %d tasks: %+v", len(tasks), tasks)
			}
			if _, err := st.LastUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
				t.Errorf("last undo = %v, want a refused plan to record nothing", err)
			}
		})
	}
}

func TestApplyBatchPlanRemovesTheTasksItCreatedWhenOneFails(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nBuy milk\nFix the sink\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps[1].Task.ProjectId = ""

	_, err = ApplyBatchPlan(ctx, st, plan)
	var failed *BatchApplyError
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v, want a *BatchApplyError", err)
	}
	if failed.Step != 2 || failed.Line != 3 || failed.Rollback != nil {
		t.Fatalf("failure = %+v, want the second task, line 3, reversed whole", failed)
	}
	if !strings.Contains(failed.Error(), "task 2 (line 3)") {
		t.Errorf("Error() = %q, want it to name the task and its line", failed.Error())
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d tasks were left behind: %+v", len(tasks), tasks)
	}
	if _, err := st.LastUndo(ctx); !errors.Is(err, store.ErrNoUndo) {
		t.Errorf("last undo = %v, want the reversal to leave no record of the batch", err)
	}
}

func TestBatchAttachSaysWhoTookTheParentWhileTheBatchWasApplied(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	parent, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Trip"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Book tickets"})
	if err != nil {
		t.Fatal(err)
	}
	// What a tt auto running beside the batch does between these two steps:
	// it claims the create of the parent, which leaves the store unable to
	// tell what the relationship would point at.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE outbox SET state = 'inflight' WHERE task_id = ? AND op = ?`, parent.Id, store.OpTaskCreate); err != nil {
		t.Fatal(err)
	}

	_, err = batchAttach(ctx, st, child, parent.Id)
	if !errors.Is(err, ErrBatchParentTaken) {
		t.Fatalf("attach = %v, want %v", err, ErrBatchParentTaken)
	}
	// The refusal the store words for tt edit would send the reader to sync
	// while a sync is what took the parent.
	if strings.Contains(err.Error(), "sync the parent task") {
		t.Errorf("attach says %q, which asks for the sync that is already running", err)
	}
	if !strings.Contains(err.Error(), "another tt run") {
		t.Errorf("attach says %q, want it to name what took the parent", err)
	}
}

func TestBatchRollbackCountsDistinctTasks(t *testing.T) {
	ctx := context.Background()
	_, st := actionStore(t)
	floor, err := aiUndoFloor(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanBatch(ctx, st, batchDocument(t, "# Work\nParent\n  Child\n"), model.Project{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyBatchPlan(ctx, st, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{Title: "Other run", ProjectId: "work"}); err != nil {
		t.Fatal(err)
	}
	err = batchRollback(ctx, st, applied.GroupID, floor)
	var blocked *BatchRollbackBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("rollback=%v", err)
	}
	t.Logf("created=%d remaining=%d distinct_task_ids=%d error=%v", len(applied.Results), blocked.Remaining, len(blocked.TaskIDs), err)
	if blocked.Remaining != len(applied.Results) {
		t.Fatal("reports undo operation count as remaining task count")
	}
}
