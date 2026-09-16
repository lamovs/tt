package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type aiFake struct {
	replies  []string
	requests []ai.Request
}

func (f *aiFake) Run(_ context.Context, req ai.Request) (ai.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.replies) == 0 {
		return ai.Response{}, errors.New("the fake has no reply left")
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return ai.Response{JSON: []byte(reply), Profile: "fast", Engine: "claude"}, nil
}

var aiTestNow = time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

func aiTestOp(op string, fields map[string]any) map[string]any {
	out := map[string]any{"op": op}
	for _, name := range []string{"ref", "title", "project", "tags", "content", "checklist", "due", "schedule", "reminders", "priority"} {
		out[name] = nil
	}
	for name, value := range fields {
		out[name] = value
	}
	return out
}

func aiReplyOf(t *testing.T, notes any, ops ...map[string]any) string {
	t.Helper()
	if ops == nil {
		ops = []map[string]any{}
	}
	data, err := json.Marshal(map[string]any{"ops": ops, "notes": notes})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func aiConfig(scope config.AIContext) config.Config {
	cfg := config.Default()
	cfg.DefaultProject = "Work"
	cfg.AI.Context = scope
	return cfg
}

func aiPropose(t *testing.T, st *store.Store, scope config.AIContext, reply string) (AIProposal, *aiFake) {
	t.Helper()
	fake := &aiFake{replies: []string{reply}}
	planner := AIPlanner{Store: st, Runner: fake, Config: aiConfig(scope), Now: aiTestNow}
	proposal, err := planner.Propose(context.Background(), "Купить молоко завтра в 10", ai.Request{Model: "m1"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return proposal, fake
}

func aiSchemaProblems(path string, node any) []string {
	var problems []string
	switch v := node.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			if v["additionalProperties"] != false {
				problems = append(problems, path+": additionalProperties is not false")
			}
			var keys, required []string
			for name := range props {
				keys = append(keys, name)
			}
			for _, name := range v["required"].([]any) {
				required = append(required, name.(string))
			}
			slices.Sort(keys)
			slices.Sort(required)
			if !slices.Equal(keys, required) {
				problems = append(problems, path+": required "+strings.Join(required, ",")+" is not every property "+strings.Join(keys, ","))
			}
		}
		if v["type"] == "object" && v["properties"] == nil {
			problems = append(problems, path+": an object without properties")
		}
		for name, child := range v {
			problems = append(problems, aiSchemaProblems(path+"."+name, child)...)
		}
	case []any:
		for _, child := range v {
			problems = append(problems, aiSchemaProblems(path+"[]", child)...)
		}
	}
	return problems
}

func TestAISchemasAreClosedAndRequireEveryProperty(t *testing.T) {
	for name, schema := range map[string][]byte{"plan": AIPlanSchema(), "find": AIFindSchema(), "rerank": AIRerankSchema()} {
		var root any
		if err := json.Unmarshal(schema, &root); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if problems := aiSchemaProblems(name, root); len(problems) != 0 {
			t.Errorf("%s schema is not strict:\n%s", name, strings.Join(problems, "\n"))
		}
		if strings.Contains(string(schema), "oneOf") {
			t.Errorf("%s schema uses oneOf, which strict structured output refuses", name)
		}
	}
}

func TestAIProposeResolvesDatesNamesAndReminders(t *testing.T) {
	_, st := actionStore(t)
	reply := aiReplyOf(t, "ok", aiTestOp(AIOpAdd, map[string]any{
		"title": "Купить молоко", "due": "tmr 10:00", "reminders": []string{"at", "-10min"},
		"checklist": []string{"молоко", " "}, "priority": "High", "content": "",
	}))
	proposal, fake := aiPropose(t, st, config.AIContextMinimal, reply)
	if len(proposal.Issues) != 0 {
		t.Fatalf("issues: %+v", proposal.Issues)
	}
	if len(proposal.Plan.Steps) != 1 {
		t.Fatalf("steps: %+v", proposal.Plan.Steps)
	}
	step := proposal.Plan.Steps[0]
	want := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	if step.Due == nil || !step.Due.Time.Equal(want) || step.Due.AllDay {
		t.Errorf("due = %+v, want %v with a clock", step.Due, want)
	}
	if step.Project == nil || step.Project.Id != "work" {
		t.Errorf("project = %+v, want the default list", step.Project)
	}
	if !slices.Equal(step.Reminders, []string{"TRIGGER:PT0S", "TRIGGER:-PT10M"}) {
		t.Errorf("reminders = %v", step.Reminders)
	}
	if !slices.Equal(step.Checklist, []string{"молоко"}) || step.Priority == nil || *step.Priority != model.PriorityHigh || step.Content != "" {
		t.Errorf("step = %+v", step)
	}
	if proposal.Notes != "ok" || proposal.Profile != "fast" {
		t.Errorf("proposal = %+v", proposal)
	}

	req := fake.requests[0]
	if req.Task != config.AITaskAI || req.Model != "m1" || len(req.Schema) == 0 || req.System == "" {
		t.Errorf("request = %+v", req)
	}
	for _, want := range []string{"Купить молоко завтра в 10", `"now":"2026-09-16 10:00"`, `"weekday":"wed"`, `"Work"`, `"Home"`, `"tasks":[]`} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt does not carry %q:\n%s", want, req.Prompt)
		}
	}
}

func TestAIContextSendsOnlyWhatItsScopeAllows(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	today, err := dates.Parse("today 18:00", aiTestNow)
	if err != nil {
		t.Fatal(err)
	}
	due, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Позвонить маме", Content: "private notes",
		DueDate: today.Time, Items: []model.Item{{Title: "private item"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "home", Title: "Someday task"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		scope      config.AIContext
		want, deny []string
	}{
		{config.AIContextMinimal, nil, []string{"Позвонить маме", "Someday task"}},
		{config.AIContextToday, []string{`"ref":"t1"`, "Позвонить маме", `"due":"2026-09-16 18:00"`}, []string{"Someday task"}},
		{config.AIContextAll, []string{"Позвонить маме", "Someday task", `"ref":"t2"`}, nil},
	} {
		_, fake := aiPropose(t, st, c.scope, aiReplyOf(t, nil))
		prompt := fake.requests[0].Prompt
		for _, want := range c.want {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s: prompt does not carry %q:\n%s", c.scope, want, prompt)
			}
		}
		for _, deny := range append(c.deny, "private notes", "private item", due.Id) {
			if strings.Contains(prompt, deny) {
				t.Errorf("%s: prompt carries %q:\n%s", c.scope, deny, prompt)
			}
		}
	}
}

func TestAIProposeRefusesWhatItCannotResolve(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	task, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Report", DueDate: model.NewTime(aiTestNow)})
	if err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	cases := []struct {
		name   string
		scope  config.AIContext
		op     map[string]any
		reason string
	}{
		{"unknown list", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": "x", "project": "Nope"}), "no cached list"},
		{"unknown tag", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": "x", "tags": []string{"nope"}}), "no cached tag"},
		{"bad date", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": "x", "due": "someday"}), "unrecognized date"},
		{"due and schedule", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": "x", "due": "tmr", "schedule": "tmr 10:00 + 1h"}), "not both"},
		{"a reminder with no date", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": "x", "reminders": []string{"at"}}), "needs a due date"},
		{"no title", config.AIContextMinimal, aiTestOp(AIOpAdd, map[string]any{"title": " "}), "needs the field"},
		{"a field the op does not take", config.AIContextToday, aiTestOp(AIOpDone, map[string]any{"ref": "t1", "title": "x"}), "does not take"},
		{"a ref under minimal context", config.AIContextMinimal, aiTestOp(AIOpDone, map[string]any{"ref": "t1"}), "ai.context is minimal"},
		{"an invented ref", config.AIContextToday, aiTestOp(AIOpDone, map[string]any{"ref": "t9"}), "not in the context"},
		{"an unknown op", config.AIContextToday, aiTestOp("delete", map[string]any{"ref": "t1"}), "unknown operation"},
		{"a move of an unsynced task", config.AIContextToday, aiTestOp(AIOpMove, map[string]any{"ref": "t1", "project": "home"}), "sync the task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proposal, _ := aiPropose(t, st, c.scope, aiReplyOf(t, nil, c.op))
			if len(proposal.Issues) == 0 || !strings.Contains(proposal.Issues[0].Reason, c.reason) {
				t.Fatalf("issues = %+v, want one saying %q", proposal.Issues, c.reason)
			}
			if proposal.Issues[0].Step != 1 {
				t.Errorf("issue step = %d, want 1", proposal.Issues[0].Step)
			}
		})
	}
	if after := dumpCache(t, st); after != before {
		t.Error("validation wrote to the cache")
	}

	moveAndEdit := aiReplyOf(t, nil,
		aiTestOp(AIOpMove, map[string]any{"ref": "t1", "project": "home"}),
		aiTestOp(AIOpEdit, map[string]any{"ref": "t1", "title": "Report 2"}))
	proposal, _ := aiPropose(t, st, config.AIContextToday, moveAndEdit)
	found := false
	for _, issue := range proposal.Issues {
		found = found || strings.Contains(issue.Reason, "only change to its task")
	}
	if !found {
		t.Errorf("issues = %+v, want the move refused beside another change to %s", proposal.Issues, task.Id)
	}
}

func aiSeedTasks(t *testing.T, st *store.Store) (edited, completed, listed model.Task) {
	t.Helper()
	ctx := context.Background()
	var err error
	if edited, err = st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Report", Content: "draft", DueDate: model.NewTime(aiTestNow),
		Items: []model.Item{{Title: "outline"}}}); err != nil {
		t.Fatal(err)
	}
	if completed, err = st.CreateTask(ctx, model.Task{ProjectId: "work", Title: "Call", DueDate: model.NewTime(aiTestNow.Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if listed, err = st.CreateTask(ctx, model.Task{ProjectId: "home", Title: "Trip", DueDate: model.NewTime(aiTestNow.Add(2 * time.Minute))}); err != nil {
		t.Fatal(err)
	}
	return edited, completed, listed
}

func aiRoundTrip(t *testing.T, plan AIPlan) AIPlan {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var out AIPlan
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAIApplyMakesOneUndoGroupThatReversesTogether(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	edited, completed, listed := aiSeedTasks(t, st)
	floor, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proposal, _ := aiPropose(t, st, config.AIContextToday, aiReplyOf(t, nil,
		aiTestOp(AIOpAdd, map[string]any{"title": "Купить молоко", "due": "tmr", "project": "home"}),
		aiTestOp(AIOpEdit, map[string]any{"ref": "t1", "title": "Report v2", "content": "more", "priority": "low"}),
		aiTestOp(AIOpEdit, map[string]any{"ref": "t1", "reminders": []string{"at"}}),
		aiTestOp(AIOpDone, map[string]any{"ref": "t2"}),
		aiTestOp(AIOpChecklistAdd, map[string]any{"ref": "t3", "checklist": []string{"passport", "tickets"}}),
	))
	if len(proposal.Issues) != 0 {
		t.Fatalf("issues: %+v", proposal.Issues)
	}
	applied, err := ApplyAIPlan(ctx, st, aiRoundTrip(t, proposal.Plan))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied.Results) != 5 || !strings.HasPrefix(applied.GroupID, "group-") {
		t.Fatalf("applied = %+v", applied)
	}
	got := readTask(t, st, edited.Id)
	if got.Title != "Report v2" || got.Content != "draft\n\nmore" || got.Priority != model.PriorityLow || !slices.Equal(got.Reminders, []string{"TRIGGER:PT0S"}) {
		t.Errorf("edited task = %+v", got)
	}
	if !readTask(t, st, completed.Id).Status.Done() {
		t.Error("the done step did not complete its task")
	}
	if items := readTask(t, st, listed.Id).Items; len(items) != 2 || items[1].Title != "tickets" {
		t.Errorf("checklist = %+v", items)
	}

	group, err := st.LastUndoGroup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range group {
		if e.Group != applied.GroupID {
			t.Fatalf("undo record %+v is outside the plan's group", e)
		}
	}
	if len(group) != 6 {
		t.Errorf("group has %d records, want one per change and one per checklist item", len(group))
	}
	if _, err := st.ApplyUndoGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, st, edited.Id); got.Title != "Report" || got.Content != "draft" || len(got.Reminders) != 0 {
		t.Errorf("edited task after undo = %+v", got)
	}
	if readTask(t, st, completed.Id).Status.Done() || len(readTask(t, st, listed.Id).Items) != 0 {
		t.Error("undo left part of the plan applied")
	}
	if _, err := st.Task(ctx, applied.Results[0].Task.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the added task survived undo: %v", err)
	}
	if top, err := st.LastUndo(ctx); err != nil || top.Seq != floor.Seq {
		t.Errorf("undo stack top = %+v, %v; want %d", top, err, floor.Seq)
	}
}

func TestAIApplyReversesWhatItDidWhenAStepFails(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	edited, _, _ := aiSeedTasks(t, st)
	floor, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	home, err := st.Project(ctx, "home")
	if err != nil {
		t.Fatal(err)
	}
	work, err := st.Project(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := readTask(t, st, edited.Id)
	plan := AIPlan{Version: aiPlanVersion, Created: aiTestNow, Steps: []AIStep{
		{Op: AIOpAdd, Title: "Added first", Project: &work},
		{Op: AIOpEdit, Task: &snapshot, Title: "Renamed"},
		{Op: AIOpMove, Task: &snapshot, Project: &home},
	}}
	_, err = ApplyAIPlan(ctx, st, plan)
	var failed *AIApplyError
	if !errors.As(err, &failed) || failed.Step != 3 || failed.Rollback != nil {
		t.Fatalf("apply = %v, want step 3 to fail and everything before it reversed", err)
	}
	if got := readTask(t, st, edited.Id); got.Title != "Report" {
		t.Errorf("the edit before the failure is still applied: %+v", got)
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll, Search: "Added first"})
	if err != nil || len(tasks) != 0 {
		t.Errorf("the add before the failure is still applied: %+v %v", tasks, err)
	}
	if top, err := st.LastUndo(ctx); err != nil || top.Seq != floor.Seq {
		t.Errorf("undo stack top = %+v, %v; want %d", top, err, floor.Seq)
	}
}

func TestAIPlanStaysFreshOnlyWhenAFailedApplyReversedNoEdit(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	edited, _, _ := aiSeedTasks(t, st)
	home, err := st.Project(ctx, "home")
	if err != nil {
		t.Fatal(err)
	}
	work, err := st.Project(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := readTask(t, st, edited.Id)
	// The move of a task sync has never seen fails, after the steps before it.
	addOnly := aiRoundTrip(t, AIPlan{Version: aiPlanVersion, Created: aiTestNow, Steps: []AIStep{
		{Op: AIOpAdd, Title: "Added first", Project: &work},
		{Op: AIOpMove, Task: &snapshot, Project: &home},
	}})
	var failed *AIApplyError
	if _, err := ApplyAIPlan(ctx, st, addOnly); !errors.As(err, &failed) || failed.Rollback != nil {
		t.Fatalf("apply = %v, want the move refused and the add reversed", err)
	}
	if err := AIPlanFresh(ctx, st, addOnly); err != nil {
		t.Errorf("a plan whose reversed steps only added tasks = %v, want it still fresh", err)
	}

	withEdit := aiRoundTrip(t, AIPlan{Version: aiPlanVersion, Created: aiTestNow, Steps: []AIStep{
		{Op: AIOpEdit, Task: &snapshot, Title: "Renamed"},
		{Op: AIOpMove, Task: &snapshot, Project: &home},
	}})
	// A reversal stamps the task with the time it runs, a later millisecond
	// than the preview saw.
	time.Sleep(2 * time.Millisecond)
	if _, err := ApplyAIPlan(ctx, st, withEdit); !errors.As(err, &failed) || failed.Rollback != nil {
		t.Fatalf("apply = %v, want the move refused and the edit reversed", err)
	}
	if got := readTask(t, st, edited.Id); got.Title != "Report" {
		t.Fatalf("the edit is still applied: %+v", got)
	}
	if err := AIPlanFresh(ctx, st, withEdit); !errors.Is(err, ErrAIPlanStale) {
		t.Errorf("a plan whose edit was reversed = %v, want it stale", err)
	}
}

func TestAIApplyRefusesAPlanWhoseTaskChanged(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	edited, _, _ := aiSeedTasks(t, st)
	proposal, _ := aiPropose(t, st, config.AIContextToday, aiReplyOf(t, nil,
		aiTestOp(AIOpAdd, map[string]any{"title": "Should not appear"}),
		aiTestOp(AIOpEdit, map[string]any{"ref": "t1", "title": "Mine"})))
	if len(proposal.Issues) != 0 {
		t.Fatalf("issues: %+v", proposal.Issues)
	}
	if _, err := st.UpdateTask(ctx, edited.Id, model.TaskEdit{Title: model.Ptr("Theirs")}); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	if _, err := ApplyAIPlan(ctx, st, aiRoundTrip(t, proposal.Plan)); !errors.Is(err, ErrAIPlanStale) {
		t.Fatalf("apply = %v, want the stale plan refused", err)
	}
	if dumpCache(t, st) != before {
		t.Error("a refused plan changed the cache")
	}
}

func TestAIApplyRefusesAnOverlapThePreviewDidNotShow(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	t.Setenv("TZ", "UTC")
	proposal, _ := aiPropose(t, st, config.AIContextMinimal, aiReplyOf(t, nil,
		aiTestOp(AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:00 + 1h"})))
	if len(proposal.Issues) != 0 || proposal.Plan.HasOverlaps() {
		t.Fatalf("proposal = %+v", proposal)
	}
	step := proposal.Plan.Steps[0]
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "home", Title: "Busy", StartDate: step.Interval.Start,
		DueDate: step.Interval.End, TimeZone: step.Interval.Zone}); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyAIPlan(ctx, st, proposal.Plan)
	var failed *AIApplyError
	if !errors.As(err, &failed) || !errors.Is(err, ErrAIOverlapChanged) || failed.Rollback != nil {
		t.Fatalf("apply = %v, want the new overlap refused", err)
	}

	again, _ := aiPropose(t, st, config.AIContextMinimal, aiReplyOf(t, nil,
		aiTestOp(AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:00 + 1h"}),
		aiTestOp(AIOpAdd, map[string]any{"title": "Second", "schedule": "tmr 14:30 + 1h"})))
	if len(again.Issues) != 0 || len(again.Plan.Steps[0].Overlaps) != 1 || !slices.Equal(again.Plan.Steps[0].Clashes, []int{2}) {
		t.Fatalf("proposal = %+v", again)
	}
	if _, err := ApplyAIPlan(ctx, st, aiRoundTrip(t, again.Plan)); err != nil {
		t.Fatalf("apply of shown overlaps = %v", err)
	}
}

func TestAIPreviewIsClaimedOnceAndExpires(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	plan := AIPlan{Version: aiPlanVersion, Created: aiTestNow.UTC(), Steps: []AIStep{{Op: AIOpAdd, Title: "x"}}}
	id, err := SaveAIPreview(ctx, st, plan, aiTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimAIPreview(ctx, st, strings.Repeat("0", 64), aiTestNow); !errors.Is(err, ErrAIPreviewUnavailable) {
		t.Errorf("claim of an unknown id = %v", err)
	}
	if _, _, err := ClaimAIPreview(ctx, st, "short", aiTestNow); !errors.Is(err, ErrAIPreviewInvalid) {
		t.Errorf("claim of a malformed id = %v", err)
	}
	got, text, err := ClaimAIPreview(ctx, st, id, aiTestNow)
	if err != nil || len(got.Steps) != 1 || got.Steps[0].Title != "x" {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if _, _, err := ClaimAIPreview(ctx, st, id, aiTestNow); !errors.Is(err, ErrAIPreviewUnavailable) {
		t.Errorf("second claim = %v, want the preview gone", err)
	}
	if err := RestoreAIPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(ctx, aiPreviewPrefix+id, text+" "); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimAIPreview(ctx, st, id, aiTestNow); !errors.Is(err, ErrAIPreviewInvalid) {
		t.Errorf("claim of an edited preview = %v", err)
	}

	later := aiTestNow.Add(AIPlanLifetime + time.Minute)
	fresh := plan
	fresh.Created = later.UTC()
	if err := RestoreAIPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClaimAIPreview(ctx, st, id, later); !errors.Is(err, ErrAIPreviewExpired) {
		t.Errorf("claim of an old preview = %v", err)
	}
	if _, ok, _ := st.Meta(ctx, aiPreviewPrefix+id); ok {
		t.Error("the claim of an expired preview left it in the cache")
	}

	if err := RestoreAIPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	otherID, err := SaveAIPreview(ctx, st, fresh, aiTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Meta(ctx, aiPreviewPrefix+id); !ok {
		t.Fatal("a save pruned a preview that was not yet old")
	}
	if _, _, err := ClaimAIPreview(ctx, st, otherID, later); err != nil {
		t.Fatalf("claim of a fresh preview = %v", err)
	}
	if _, ok, _ := st.Meta(ctx, aiPreviewPrefix+id); ok {
		t.Error("an expired preview was not pruned by the claim of another one")
	}

	if err := RestoreAIPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	if err := PruneAIPreviews(ctx, st, later); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Meta(ctx, aiPreviewPrefix+id); ok {
		t.Error("an expired preview was not pruned")
	}
	if _, err := SaveAIPreview(ctx, st, fresh, later); err != nil {
		t.Fatal(err)
	}
	if err := RestoreAIPreview(ctx, st, id, text); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAIPreview(ctx, st, fresh, later); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Meta(ctx, aiPreviewPrefix+id); ok {
		t.Error("an expired preview was not pruned by the next save")
	}
}

// System reaches the agent's command line, which any local user can read, so
// every call must carry only tt's own fixed instructions there.
func TestAISystemTextIsOnlyTheFixedInstructions(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	for _, title := range []string{"milk from the private list", "milk for the neighbours"} {
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: title, DueDate: aiDueToday(t, "11:00")}); err != nil {
			t.Fatal(err)
		}
	}
	fake := &aiFake{replies: []string{aiReplyOf(t, nil)}}
	planner := AIPlanner{Store: st, Runner: fake, Config: aiConfig(config.AIContextAll), Now: aiTestNow, AllowSecrets: true}
	if _, err := planner.Propose(ctx, "request text meant for stdin", ai.Request{System: "caller text"}); err != nil {
		t.Fatal(err)
	}
	fake.replies = []string{
		`{"keywords":["milk"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`,
		`{"ranked":[1,2]}`,
	}
	finder := AIFinder{Store: st, Runner: fake, Now: aiTestNow, Context: config.AIContextAll, AllowSecrets: true}
	if _, err := finder.Find(ctx, "query meant for stdin", AIRerankAlways, ai.Request{System: "caller text"}); err != nil {
		t.Fatal(err)
	}
	want := []string{aiPlanSystem, aiFindSystem, aiRerankSystem}
	if len(fake.requests) != len(want) {
		t.Fatalf("calls = %d, want %d", len(fake.requests), len(want))
	}
	for i, req := range fake.requests {
		if req.System != want[i] {
			t.Errorf("call %d System = %q, want tt's fixed instructions", i+1, req.System)
		}
		for _, private := range []string{"meant for stdin", "private list", "neighbours", "caller text", "Work"} {
			if strings.Contains(req.System, private) {
				t.Errorf("call %d System carries %q", i+1, private)
			}
		}
	}
}

func TestAIFindUnionsKeywordsRanksAndSendsOnlyTitles(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	for _, task := range []model.Task{
		{ProjectId: "home", Title: "Купить молоко и хлеб", Content: "private notes"},
		{ProjectId: "home", Title: "Молоко для кофе"},
		{ProjectId: "work", Title: "Хлеб"},
		{ProjectId: "work", Title: "Unrelated"},
	} {
		if _, err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	filter := `{"keywords":["молок","Хлеб","молок"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`
	fake := &aiFake{replies: []string{filter}}
	finder := AIFinder{Store: st, Runner: fake, Now: aiTestNow}
	result, err := finder.Find(ctx, "молоко", AIRerankNever, ai.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, task := range result.Tasks {
		titles = append(titles, task.Title)
	}
	if !slices.Equal(titles, []string{"Купить молоко и хлеб", "Молоко для кофе", "Хлеб"}) || result.Reranked {
		t.Errorf("titles = %q, want the task both keywords matched first", titles)
	}
	if req := fake.requests[0]; req.Task != config.AITaskFind || !strings.Contains(req.Prompt, "молоко") || strings.Contains(req.Prompt, "кофе") {
		t.Errorf("filter request = %+v", req)
	}

	fake.replies = []string{filter, `{"ranked":[3, 9, 3, 1]}`}
	result, err = finder.Find(ctx, "хлеб", AIRerankAlways, ai.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 2 || result.Tasks[0].Title != "Хлеб" || !result.Reranked || result.Candidates != 3 {
		t.Errorf("reranked = %+v", result)
	}
	rerank := fake.requests[len(fake.requests)-1].Prompt
	if !strings.Contains(rerank, "1. Купить молоко и хлеб") || strings.Contains(rerank, "private notes") || strings.Contains(rerank, "home") {
		t.Errorf("rerank prompt carries more than titles:\n%s", rerank)
	}
}

func TestAIFindBoundsAreInclusiveAndChecked(t *testing.T) {
	_, st := actionStore(t)
	finder := AIFinder{Store: st, Now: aiTestNow}
	cat, err := aiLoadCatalog(context.Background(), st, aiTestNow, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	filter, base, err := finder.filter(aiFindReply{DueTo: model.Ptr("fri"), DoneFrom: model.Ptr("2026-09-01"), Project: model.Ptr("work")}, cat)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 19, 0, 0, 0, 0, time.Local); !base.DueTo.Equal(want) || filter.Status != "done" || base.ProjectID != "work" {
		t.Errorf("filter = %+v, base = %+v; want due before %v, done tasks, list work", filter, base, want)
	}
	_, _, err = finder.filter(aiFindReply{Project: model.Ptr("Nope"), Status: model.Ptr("maybe")}, cat)
	var invalid *AIInvalidError
	if !errors.As(err, &invalid) || len(invalid.Issues) != 2 {
		t.Errorf("filter error = %v, want the list and the status refused", err)
	}
}

func TestAISecretKind(t *testing.T) {
	// Provider keys and tokens are split into two literals so that secret
	// scanners do not flag the file; keep them split.
	for text, want := range map[string]string{
		"my key sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz": "an API key",
		"token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789": "a GitHub token",
		"wifi password: hunter2":                              "a password or secret",
		"пароль: 12345":                                       "a password or secret",
		"deploy with AKIA" + "ABCDEFGHIJKLMNOP":               "an AWS access key",
		"use Zx9Qm2Lp8Rt4Vw7Ks1Nb6Hj3Gf5Dc0Aa here":           "a long random-looking token",

		"wifi password hunter2":                  "a password or secret",
		"пароль от wifi qwerty123":               "a password or secret",
		"login admin pass hunter2":               "a password or secret",
		"Пароль от роутера на даче: qwerty123":   "a password or secret",
		"пароль hUnTeRpAsS":                      "a password or secret",
		"пароль p@ss!word":                       "a password or secret",
		"пароль 123456":                          "a password or secret",
		"PIN 4821 for the card":                  "a PIN or code",
		"PIN-код 1987":                           "a PIN or code",
		"код от домофона 4589#":                  "a PIN or code",
		"door code 7788":                         "a PIN or code",
		"CVC 123":                                "a card security code",
		"api key AbC123xyz789":                   "a key or token",
		"Токен доступа 7f3kq9x2":                 "a key or token",
		"card 4276 1234 5678 9012 cvv 123":       "a card number",
		"Оплатить картой 4111111111111111":       "a card number",
		"amex 3782 822463 10005":                 "a card number",
		"login admin pass: hunter":               "a password or secret",
		"пароль от почты: солнышко":              "a password or secret",
		"OTP 482913":                             "a PIN or code",
		"backup codes 38291047 55012934":         "a PIN or code",
		"2FA recovery codes 3829-1047 5501-2934": "a card number",
		"reset link https://example.com/reset?token=8f14e45fceea167a5a36dedd4bea2543": "a key or token",
		"open https://example.com/share?id=Zq8x4Nw2Lp7Kd9Rt":                          "a long random-looking token",
		"https://example.com/cb#id_token=abc":                                         "a key or token",
		"https://user:S3cretPass@host.example.com/db":                                 "a password or secret",
		"Сменить пароль для роутера на Qwerty123":                                     "a password or secret",
		"Change password for wifi to Hunter2024!":                                     "a password or secret",
		"Reset password for user123 to Winter2026!":                                   "a password or secret",
		"Update password for bank: S3cure!pass":                                       "a password or secret",
		"Поменять пароль для почты: солнышко":                                         "a password or secret",
		"Please reset the password for user123 to Winter2026!":                        "a password or secret",
		"reset link example.com/reset?token=8f14e45fceea167a5a36dedd4bea2543":         "a key or token",
		"[reset](https://example.com/reset?token=8f14e45fceea167a5a36dedd4bea2543)":   "a key or token",
		"Пароль от сейфа: 2010":                                                       "a password or secret",
		"Пароль от wifi: correct horse battery staple":                                "a password or secret",
		"Секретное слово: ромашка":                                                    "a password or secret",
		"Guest pass: Welcome2024":                                                     "a password or secret",
		"OTP 48291057":                                                                "a PIN or code",

		"Купить молоко завтра в 10":                                                                  "",
		"Купить молоко в 10":                                                                         "",
		"call the bank about card 4276":                                                              "",
		"see https://example.com/some/long/path/for/reading":                                         "",
		"Позвонить на 8 800":                                                                         "",
		"Позвонить на 8 800 000 00 00":                                                               "",
		"Позвонить на +86 000 0000 0000":                                                             "",
		"PIN code reminder for bank visit":                                                           "",
		"Pin the post about the 2025 sale":                                                           "",
		"Проверить пароль политику":                                                                  "",
		"Сменить пароль до 15 октября":                                                               "",
		"Сменить пароль в 2025":                                                                      "",
		"reset password in 2 days":                                                                   "",
		"Включить 2FA и сменить пароль":                                                              "",
		"Сменить пароль от WiFi и iCloud":                                                            "",
		"boarding pass 12B":                                                                          "",
		"Secret Santa budget 1000":                                                                   "",
		"Купить гаечный ключ 17мм":                                                                   "",
		"Позвонить в банк, код ошибки 1234":                                                          "",
		"code review PR 1234":                                                                        "",
		"Обновить 1Password":                                                                         "",
		"Заказ 4276123456789012":                                                                     "",
		"Счет INV4111111111111111":                                                                   "",
		"Get boarding pass for flight SU1234":                                                        "",
		"Move passwords from Chrome to 1Password":                                                    "",
		"Reset password for user123":                                                                 "",
		"Сбросить пароль для user123":                                                                "",
		"Pin issue #4821 to the board":                                                               "",
		"Ключ от ячейки A12B забрать":                                                                "",
		"Код заказа 123456 забрать в пятницу":                                                        "",
		"Секретный Санта бюджет 100000":                                                              "",
		"Номер заказа 482913":                                                                        "",
		"Buy tokens for metro 2 pcs":                                                                 "",
		"Промокод SUMMER2026 до 31.10":                                                               "",
		"Позвонить +7 000 000-00-00":                                                                 "",
		"Оплатить счёт 00000000000000000000":                                                         "",
		"Встреча 15.10 в 14:30 кв. 45 подъезд 2":                                                     "",
		"see https://example.com/docs?page=2&lang=ru":                                                "",
		"https://example.com/search?q=milk+and+bread+for+the+week":                                   "",
		"Watch https://youtu.be/dQw4w9WgXcQ?si=B_RxG4f5nJ3kLmNo":                                     "",
		"Listen https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLAbCdEfGhIjKlMnOpQrStUvWxYz012345": "",
		"Song https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT?si=1a2b3c4d5e6f7a8b":             "",
		"Design https://www.figma.com/design/AbCdEfGhIjKl/App?node-id=12-345&t=Qx7LmN2pR8sT4vW9-0":   "",
		"Post https://www.instagram.com/p/C9xYz12AbCd/?igsh=RVhBTVBMRTAxMjM0NTY3ODk=":                "",
		"Read https://example.com/article?fbclid=IwAR2xYzAbCdEfGh1234567890IjKlMnOp":                 "",
		"Ticket https://acme.atlassian.net/browse/PROJ-123?atlOrigin=eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYiLCJwIjoiaiJ9": "",
		"Place https://www.google.com/maps/place/Cafe/@55.75,37.61,17z?entry=ttu&g_ep=EgoyMDI0MDkxMC4wIKXMDSoASAFQAw%3D%3D":            "",
		"Buy https://www.amazon.com/dp/B08N5WRWNW?pd_rd_r=1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d&th=1":                                   "",
		"File https://drive.google.com/open?id=1AbCdEfGhIjKlMnOpQrStUvWxYz0123456":                                                     "",
		"https://example.com/?utm_source=newsletter&utm_campaign=autumn_sale_2026_week3":                                               "",
		"Ozon https://www.ozon.ru/product/chaynik-123456789/?asb=AbCdEfGhIj1234567890KlMn&avtc=1":                                      "",
		"Tasks: buy milk": "",
		"Ключ: забрать у консьержа":                   "",
		"Ключи: отдать соседу":                        "",
		"Код от домофона: спросить у соседей":         "",
		"Door code: ask the host":                     "",
		"Access code: pending, call support":          "",
		"SMS code: не приходит, написать в поддержку": "",
		"PIN for the new card: set it up in the app":  "",
		"Token for API: renew next week":              "",
		"Пин-код: забыла, сходить в банк":             "",
		"Ключ: под ковриком":                          "",
		"Boarding pass: seat 12B":                     "",
		"Ski pass: top up online":                     "",
		"Season pass: renew before May":               "",
		"Pass CompTIA Security+ exam":                 "",
		"Pick up conference pass A3F9":                "",
		"Enable OTP before 2027":                      "",
		"Backup code rotation 2026":                   "",
		"Please reset the password for user123":       "",
	} {
		got := AISecretKind(text)
		if got != want {
			t.Errorf("AISecretKind(%q) = %q, want %q", text, got, want)
		}
		if strings.ContainsAny(got, "0123456789") {
			t.Errorf("AISecretKind(%q) = %q repeats part of the text", text, got)
		}
	}
}

// A request is checked whole, so a long one must not take the check quadratic
// time: each name is looked after for a bounded number of words, even when
// every word after it is another name with a colon.
func TestAISecretKindStaysLinear(t *testing.T) {
	for name, text := range map[string]string{
		"reset for":      strings.Repeat("reset password for ", 20000),
		"colon chains":   strings.Repeat("boarding pass: 1: ", 20000),
		"name chains":    strings.Repeat("pw: ", 20000),
		"label chains":   strings.Repeat("пароль для ", 20000),
		"markdown links": strings.Repeat("[x](https://e.com/?q=1) ", 15000),
	} {
		start := time.Now()
		AISecretKind(text)
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("%s: %d bytes took %v", name, len(text), took)
		}
	}
}

func TestAIApplySchedulesAndMovesSyncedTasks(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	t.Setenv("TZ", "UTC")
	moved := model.Task{Id: "server-move", ProjectId: "work", Title: "Move me", DueDate: model.NewTime(aiTestNow.Add(time.Hour))}
	planned := model.Task{Id: "server-plan", ProjectId: "work", Title: "Plan me", DueDate: model.NewTime(aiTestNow.Add(2 * time.Hour)),
		Items: []model.Item{{Id: "item-1", Title: "step"}}}
	if _, err := st.SyncProject(ctx, "work", []store.ServerTask{
		{Task: moved, Raw: json.RawMessage(`{"id":"server-move","projectId":"work","title":"Move me"}`)},
		{Task: planned, Raw: json.RawMessage(`{"id":"server-plan","projectId":"work","title":"Plan me"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	proposal, _ := aiPropose(t, st, config.AIContextToday, aiReplyOf(t, nil,
		aiTestOp(AIOpMove, map[string]any{"ref": "t1", "project": "Home"}),
		aiTestOp(AIOpSchedule, map[string]any{"ref": "t2", "schedule": "tmr 09:00 + 45min"})))
	if len(proposal.Issues) != 0 {
		t.Fatalf("issues: %+v", proposal.Issues)
	}
	applied, err := ApplyAIPlan(ctx, st, aiRoundTrip(t, proposal.Plan))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readTask(t, st, "server-move"); got.ProjectId != "home" {
		t.Errorf("moved task is in %s, want home", got.ProjectId)
	}
	got := readTask(t, st, "server-plan")
	if got.DueDate.Sub(got.StartDate.Time) != 45*time.Minute || got.TimeZone != "UTC" {
		t.Errorf("scheduled task = start %v due %v zone %q", got.StartDate, got.DueDate, got.TimeZone)
	}
	group, err := st.LastUndoGroup(ctx)
	if err != nil || len(group) != 2 || group[0].Group != applied.GroupID || group[1].Action.Op != store.OpTaskMoveNative {
		t.Fatalf("group = %+v, %v", group, err)
	}
	if _, err := st.ApplyUndoGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, st, "server-move"); got.ProjectId != "work" {
		t.Errorf("undo left the task in %s", got.ProjectId)
	}
}

func aiDueToday(t *testing.T, clock string) model.Time {
	t.Helper()
	due, err := dates.Parse("today "+clock, aiTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return due.Time
}

func aiSecretProjects(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.ReplaceProjects(context.Background(), []model.Project{
		{Id: "work", Name: "Work"}, {Id: "home", Name: "Home"}, {Id: "vault", Name: "api key: zx81vault"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAIContextLeavesOutWhatLooksLikeASecret(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	aiSecretProjects(t, st)
	for _, task := range []model.Task{
		{ProjectId: "work", Title: "wifi password: hunter2", DueDate: aiDueToday(t, "09:00")},
		{ProjectId: "vault", Title: "Позвонить маме", DueDate: aiDueToday(t, "11:00")},
		{ProjectId: "work", Title: "Report", Tags: []string{"secret=swordfish", "draft"}, DueDate: aiDueToday(t, "12:00")},
	} {
		if _, err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	secrets := []string{"hunter2", "zx81vault", "swordfish"}

	proposal, fake := aiPropose(t, st, config.AIContextToday, aiReplyOf(t, nil, aiTestOp(AIOpDone, map[string]any{"ref": "t3"})))
	prompt := fake.requests[0].Prompt
	for _, secret := range secrets {
		if strings.Contains(prompt, secret) {
			t.Errorf("the prompt carries %q:\n%s", secret, prompt)
		}
	}
	for _, want := range []string{`{"ref":"t1","title":"Позвонить маме","list":"","due":"2026-09-16 11:00"}`, `"ref":"t2","title":"Report"`, `"draft"`, `"Work"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
	if want := (AILeftOut{Tasks: 1, Lists: 1, Tags: 1}); proposal.LeftOut != want {
		t.Errorf("left out = %+v, want %+v", proposal.LeftOut, want)
	}
	if len(proposal.Issues) != 1 || !strings.Contains(proposal.Issues[0].Reason, "not in the context") {
		t.Errorf("issues = %+v, want no ref for the task that was left out", proposal.Issues)
	}

	fake = &aiFake{replies: []string{aiReplyOf(t, nil, aiTestOp(AIOpDone, map[string]any{"ref": "t1"}))}}
	planner := AIPlanner{Store: st, Runner: fake, Config: aiConfig(config.AIContextToday), Now: aiTestNow, AllowSecrets: true}
	proposal, err := planner.Propose(ctx, "finish the wifi task", ai.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if !strings.Contains(fake.requests[0].Prompt, secret) {
			t.Errorf("with secrets allowed the prompt does not carry %q", secret)
		}
	}
	if proposal.LeftOut != (AILeftOut{}) || len(proposal.Issues) != 0 || proposal.Plan.Steps[0].Task.Title != "wifi password: hunter2" {
		t.Errorf("with secrets allowed: left out %+v, issues %+v, steps %+v", proposal.LeftOut, proposal.Issues, proposal.Plan.Steps)
	}
}

func TestAIFindRanksOnlyTheTitlesItMaySend(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	aiSecretProjects(t, st)
	hidden := "milk and wifi password: hunter2"
	for _, title := range []string{hidden, "milk for coffee", "milk bread"} {
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "home", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	filter := `{"keywords":["milk"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`
	fake := &aiFake{replies: []string{filter, `{"ranked":[2,1]}`}}
	finder := AIFinder{Store: st, Runner: fake, Now: aiTestNow}
	result, err := finder.Find(ctx, "milk", AIRerankAlways, ai.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want a filter and a ranking", len(fake.requests))
	}
	if first := fake.requests[0].Prompt; strings.Contains(first, "zx81vault") || !strings.Contains(first, `"Home"`) {
		t.Errorf("filter prompt:\n%s", first)
	}
	rerank := fake.requests[1].Prompt
	first, second := "milk for coffee", "milk bread"
	if strings.Contains(rerank, "1. "+second) {
		first, second = second, first
	}
	if !strings.Contains(rerank, "1. "+first) || !strings.Contains(rerank, "2. "+second) || strings.Contains(rerank, "3. ") || strings.Contains(rerank, "hunter2") {
		t.Errorf("rerank prompt does not carry exactly the two titles it may:\n%s", rerank)
	}
	var titles []string
	for _, task := range result.Tasks {
		titles = append(titles, task.Title)
	}
	if want := []string{second, first, hidden}; !slices.Equal(titles, want) || !result.Reranked || result.Candidates != 3 {
		t.Errorf("titles = %q, reranked %v; want %q, the title left out after the ranked ones", titles, result.Reranked, want)
	}
	if want := (AILeftOut{Tasks: 1, Lists: 1}); result.LeftOut != want {
		t.Errorf("left out = %+v, want %+v", result.LeftOut, want)
	}

	fake.replies = []string{filter, `{"ranked":[1]}`}
	finder.AllowSecrets = true
	result, err = finder.Find(ctx, "milk", AIRerankAlways, ai.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.requests[2].Prompt, "zx81vault") || !strings.Contains(fake.requests[3].Prompt, hidden) ||
		result.LeftOut != (AILeftOut{}) || len(result.Tasks) != 1 {
		t.Errorf("with secrets allowed: left out %+v, %d tasks\n%s", result.LeftOut, len(result.Tasks), fake.requests[3].Prompt)
	}

	// The ranking call fails: what it was built without is still reported.
	fake.replies = []string{filter}
	finder.AllowSecrets = false
	result, err = finder.Find(ctx, "milk", AIRerankAlways, ai.Request{})
	if err == nil || result.LeftOut != (AILeftOut{Tasks: 1, Lists: 1}) {
		t.Errorf("a failed ranking = %v, left out %+v; want the error and what was left out", err, result.LeftOut)
	}
	fake.replies = nil
	result, err = finder.Find(ctx, "milk", AIRerankAlways, ai.Request{})
	if err == nil || result.LeftOut != (AILeftOut{Lists: 1}) {
		t.Errorf("a failed filter call = %v, left out %+v; want the error and the list left out", err, result.LeftOut)
	}
}

func TestAIFindRanksUnaskedOnlyUnderAWiderContext(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	for i := range AIFindRerankThreshold + 1 {
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "home", Title: "milk " + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	filter := `{"keywords":["milk"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`
	for _, c := range []struct {
		scope  config.AIContext
		rerank AIRerank
		calls  int
	}{
		{"", AIRerankAuto, 1},
		{config.AIContextMinimal, AIRerankAuto, 1},
		{config.AIContextMinimal, AIRerankAlways, 2},
		{config.AIContextToday, AIRerankAuto, 2},
		{config.AIContextAll, AIRerankAuto, 2},
		{config.AIContextAll, AIRerankNever, 1},
	} {
		fake := &aiFake{replies: []string{filter, `{"ranked":[1]}`}}
		finder := AIFinder{Store: st, Runner: fake, Now: aiTestNow, Context: c.scope}
		result, err := finder.Find(ctx, "milk", c.rerank, ai.Request{})
		if err != nil {
			t.Fatalf("context %q, rerank %d: %v", c.scope, c.rerank, err)
		}
		if len(fake.requests) != c.calls || result.Reranked != (c.calls == 2) || result.Candidates != AIFindRerankThreshold+1 {
			t.Errorf("context %q, rerank %d: %d calls, reranked %v; want %d calls", c.scope, c.rerank, len(fake.requests), result.Reranked, c.calls)
		}
	}
}

func TestAIRollbackReversesWhatItCanAndSaysWhatIsLeft(t *testing.T) {
	ctx := context.Background()
	add := func(st *store.Store, ctx context.Context, title string) model.Task {
		t.Helper()
		task, err := st.CreateTask(ctx, model.Task{ProjectId: "work", Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	type record struct{ group, title string }
	for _, c := range []struct {
		name     string
		records  []record
		reversed []string
		left     *AIRollbackBlockedError
	}{
		{"the request alone", []record{{"ours", "A"}, {"ours", "B"}}, []string{"A", "B"}, nil},
		{"a change before the request", []record{{"", "X"}, {"ours", "A"}, {"ours", "B"}}, []string{"A", "B"}, nil},
		{"a change and nothing of the request", []record{{"", "X"}}, nil, nil},
		{"a change on top", []record{{"ours", "A"}, {"", "X"}}, nil,
			&AIRollbackBlockedError{Remaining: 1, TaskIDs: []string{"A"}, Above: 1, Contiguous: true}},
		{"a change in between", []record{{"ours", "A"}, {"", "X"}, {"ours", "B"}}, []string{"B"},
			&AIRollbackBlockedError{Remaining: 1, TaskIDs: []string{"A"}, Above: 1, Contiguous: true}},
		{"two changes on top", []record{{"ours", "A"}, {"", "X"}, {"", "Y"}}, nil,
			&AIRollbackBlockedError{Remaining: 1, TaskIDs: []string{"A"}, Above: 2, Contiguous: true}},
		{"a group on top and a change in between", []record{{"ours", "A"}, {"", "X"}, {"ours", "B"}, {"other", "H1"}, {"other", "H2"}}, nil,
			&AIRollbackBlockedError{Remaining: 2, TaskIDs: []string{"B", "A"}, Above: 1, Contiguous: false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, st := actionStore(t)
			add(st, ctx, "Older")
			floor, err := aiUndoFloor(ctx, st)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]string{}
			for _, r := range c.records {
				recorded := ctx
				if r.group != "" {
					recorded = store.WithUndoGroup(ctx, "group-"+r.group)
				}
				ids[r.title] = add(st, recorded, r.title).Id
			}
			err = rollbackAIGroup(ctx, st, "group-ours", floor)
			if c.left == nil {
				if err != nil {
					t.Fatalf("rollback = %v, want the request reversed", err)
				}
			} else {
				var left *AIRollbackBlockedError
				if !errors.As(err, &left) || !errors.Is(err, ErrAIRollbackBlocked) {
					t.Fatalf("rollback = %v, want it blocked", err)
				}
				want := *c.left
				want.TaskIDs = nil
				for _, title := range c.left.TaskIDs {
					want.TaskIDs = append(want.TaskIDs, ids[title])
				}
				if !reflect.DeepEqual(*left, want) {
					t.Errorf("left = %+v, want %+v", *left, want)
				}
			}
			for _, r := range c.records {
				_, err := st.Task(ctx, ids[r.title])
				if gone := errors.Is(err, store.ErrNotFound); gone != slices.Contains(c.reversed, r.title) {
					t.Errorf("%s reversed = %v (%v)", r.title, gone, err)
				}
			}
		})
	}

	_, st := actionStore(t)
	add(st, ctx, "Older")
	floor, err := aiUndoFloor(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	planned := add(st, store.WithUndoGroup(ctx, "group-ai-blocked"), "Planned")
	add(st, ctx, "Recorded on top")
	var left *AIRollbackBlockedError
	if err := rollbackAIGroup(ctx, st, "group-ai-blocked", floor); !errors.As(err, &left) || left.Above != 1 || !left.Contiguous || left.AboveTouches {
		t.Fatalf("rollback under another change = %v, want it blocked with one change on top of other tasks", err)
	}
	// What a blocked rollback advises: drop the record on top, then undo.
	if _, err := st.PopUndo(ctx); err != nil {
		t.Fatal(err)
	}
	group, err := st.LastUndoGroup(ctx)
	if err != nil || group[0].Group != "group-ai-blocked" {
		t.Fatalf("after dropping the record on top the history holds %+v, %v; want the request's change", group, err)
	}
	if _, err := st.ApplyUndoGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Task(ctx, planned.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the advised undo left the change applied: %v", err)
	}

	// Another run renamed the task the request renamed: reversing the
	// request's change would put back the title from before both, so the
	// rollback must not be read as safe to finish with --skip.
	_, st = actionStore(t)
	report := add(st, ctx, "Report")
	floor, err = aiUndoFloor(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(store.WithUndoGroup(ctx, "group-ai-same"), report.Id, model.TaskEdit{Title: model.Ptr("Renamed by the request")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTask(ctx, report.Id, model.TaskEdit{Title: model.Ptr("Renamed by another run")}); err != nil {
		t.Fatal(err)
	}
	left = nil
	if err := rollbackAIGroup(ctx, st, "group-ai-same", floor); !errors.As(err, &left) ||
		!reflect.DeepEqual(*left, AIRollbackBlockedError{Remaining: 1, TaskIDs: []string{report.Id}, Above: 1, Contiguous: true, AboveTouches: true}) {
		t.Fatalf("rollback under another change to the same task = %v %+v, want it blocked with that change marked", err, left)
	}
	if got := readTask(t, st, report.Id).Title; got != "Renamed by another run" {
		t.Errorf("the blocked rollback changed the task: title %q", got)
	}

	_, st = actionStore(t)
	older := add(st, ctx, "Older")
	add(st, ctx, "Newer")
	top, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	floor = top.Seq
	// Another tt undo reverses the change at the floor while the request runs.
	if _, err := st.ApplyUndo(ctx, top); err != nil {
		t.Fatal(err)
	}
	grouped := store.WithUndoGroup(ctx, "group-ai-below")
	planned = add(st, grouped, "Planned one")
	again := add(st, grouped, "Planned two")
	if err := rollbackAIGroup(ctx, st, "group-ai-below", floor); err != nil {
		t.Fatalf("rollback with the history below its floor = %v, want the request reversed", err)
	}
	for _, task := range []model.Task{planned, again} {
		if _, err := st.Task(ctx, task.Id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s survived the rollback: %v", task.Title, err)
		}
	}
	if top, err := st.LastUndo(ctx); err != nil || top.Action.TaskID != older.Id {
		t.Errorf("undo stack top = %+v, %v; want the change below the floor", top, err)
	}
}

func TestAIApplyReversesANativeMoveWhenALaterChangeFails(t *testing.T) {
	_, st := actionStore(t)
	ctx := context.Background()
	t.Setenv("TZ", "UTC")
	moved := model.Task{Id: "server-move", ProjectId: "work", Title: "Move me", DueDate: model.NewTime(aiTestNow.Add(time.Hour))}
	if _, err := st.SyncProject(ctx, "work", []store.ServerTask{
		{Task: moved, Raw: json.RawMessage(`{"id":"server-move","projectId":"work","title":"Move me"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	proposal, _ := aiPropose(t, st, config.AIContextToday, aiReplyOf(t, nil,
		aiTestOp(AIOpMove, map[string]any{"ref": "t1", "project": "Home"}),
		aiTestOp(AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:00 + 1h"})))
	if len(proposal.Issues) != 0 || proposal.Plan.HasOverlaps() {
		t.Fatalf("proposal = %+v", proposal)
	}
	step := proposal.Plan.Steps[1]
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "home", Title: "Busy", StartDate: step.Interval.Start,
		DueDate: step.Interval.End, TimeZone: step.Interval.Zone}); err != nil {
		t.Fatal(err)
	}
	floor, err := st.LastUndo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyAIPlan(ctx, st, aiRoundTrip(t, proposal.Plan))
	var failed *AIApplyError
	if !errors.As(err, &failed) || failed.Step != 2 || !errors.Is(err, ErrAIOverlapChanged) || failed.Rollback != nil {
		t.Fatalf("apply = %v, want the add refused and the move before it reversed", err)
	}
	if got := readTask(t, st, "server-move"); got.ProjectId != "work" {
		t.Errorf("the move is still applied: the task is in %s", got.ProjectId)
	}
	var queued int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE task_id = ? AND op = ?`, "server-move", store.OpTaskMoveNative).Scan(&queued); err != nil || queued != 0 {
		t.Errorf("queued moves of the task = %d (%v), want the move cancelled", queued, err)
	}
	if top, err := st.LastUndo(ctx); err != nil || top.Seq != floor.Seq {
		t.Errorf("undo stack top = %+v, %v; want %d", top, err, floor.Seq)
	}
}
