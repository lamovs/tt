package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type aiFakeRunner struct {
	replies  []string
	requests []ai.Request
	err      error
}

func (f *aiFakeRunner) Run(_ context.Context, req ai.Request) (ai.Response, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return ai.Response{}, f.err
	}
	if len(f.replies) == 0 {
		return ai.Response{}, errors.New("the fake has no reply left")
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return ai.Response{JSON: []byte(reply), Profile: "fast", Engine: "claude"}, nil
}

var aiCmdNow = time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

func aiCmdSetup(t *testing.T, replies ...string) (*store.Store, *aiFakeRunner) {
	t.Helper()
	isolate(t)
	st := undoCache(t)
	undoProjects(t, st)
	fake := &aiFakeRunner{replies: replies}
	previousRunner, previousNow := newAIRunner, aiNow
	newAIRunner = func(config.AI) app.AIRunner { return fake }
	aiNow = func() time.Time { return aiCmdNow }
	t.Cleanup(func() { newAIRunner, aiNow = previousRunner, previousNow })
	return st, fake
}

func aiCmdOp(op string, fields map[string]any) map[string]any {
	out := map[string]any{"op": op}
	for _, name := range []string{"ref", "title", "project", "tags", "content", "checklist", "due", "schedule", "reminders", "priority"} {
		out[name] = nil
	}
	for name, value := range fields {
		out[name] = value
	}
	return out
}

func aiCmdReply(t *testing.T, notes any, ops ...map[string]any) string {
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

func aiRun(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func aiLinesFit(t *testing.T, name, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if n := cli.DisplayWidth(line); n > cli.Width {
			t.Errorf("a line of %s is %d columns wide:\n%q", name, n, line)
		}
		if strings.ContainsAny(line, "\x1b\x07\u202e") {
			t.Errorf("a line of %s carries a raw control or direction character:\n%q", name, line)
		}
	}
}

func aiPreviewID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(line, "tt ai --accept "); ok {
			return id
		}
	}
	t.Fatalf("no preview ID in:\n%s", out)
	return ""
}

func aiTaskCount(t *testing.T, st *store.Store) int {
	t.Helper()
	tasks, err := st.Tasks(context.Background(), store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		t.Fatal(err)
	}
	return len(tasks)
}

func aiSavedPreviews(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM meta WHERE key GLOB 'ai_preview_*'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAIPreviewThenAcceptAppliesChangesThatOneUndoReverses(t *testing.T) {
	st, fake := aiCmdSetup(t)
	fake.replies = []string{aiCmdReply(t, nil,
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко", "due": "tmr 10:00", "reminders": []string{"at"}}),
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "Отчёт", "project": "работа", "checklist": []string{"черновик"}}),
	)}

	code, stdout, stderr := aiRun(t, "", "ai", "Купить молоко завтра в 10, и отчёт", "--preview")
	if code != exitOK || stderr != "" {
		t.Fatalf("tt ai --preview = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		`2 proposed changes, applied together and reversed together by one "tt undo":`,
		` 1. add "Купить молоко"`,
		`     to list "Личное"`,
		"     due Thu 2026-09-17 10:00",
		`     reminder "at"`,
		`     to list "Работа"`,
		`     item "черновик"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("preview does not say %q:\n%s", want, stdout)
		}
	}
	aiLinesFit(t, "the preview", stdout)
	id := aiPreviewID(t, stdout)
	if aiTaskCount(t, st) != 0 {
		t.Fatal("--preview created tasks")
	}
	if req := fake.requests[0]; req.Task != config.AITaskAI || !strings.Contains(req.Prompt, "Купить молоко завтра в 10") {
		t.Errorf("request = %+v", req)
	}

	code, stdout, stderr = aiRun(t, "", "ai", "--accept", id)
	if code != exitOK {
		t.Fatalf("tt ai --accept = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{`applied 2 changes together; one "tt undo" reverses them all`, `  added "Купить молоко"`, `  added "Отчёт"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report does not say %q:\n%s", want, stdout)
		}
	}
	if aiTaskCount(t, st) != 2 {
		t.Fatalf("accept made %d tasks, want 2", aiTaskCount(t, st))
	}

	if code, _, stderr := aiRun(t, "", "ai", "--accept", id); code != exitUsage || !strings.Contains(stderr, "unavailable") {
		t.Errorf("a second accept = %d (stderr: %s), want the preview gone", code, stderr)
	}

	code, stdout, stderr = aiRun(t, "", "undo")
	if code != exitOK || !strings.Contains(stdout, "undid 2 changes made together") {
		t.Fatalf("tt undo = %d\n%s%s", code, stdout, stderr)
	}
	if aiTaskCount(t, st) != 0 {
		t.Error("one undo did not reverse the whole request")
	}
}

func TestAIAsksInATerminalAndKeepsThePreviewOnNo(t *testing.T) {
	st, fake := aiCmdSetup(t)
	previous := canAsk
	canAsk = func(*invocation) bool { return true }
	t.Cleanup(func() { canAsk = previous })
	reply := aiCmdReply(t, "Список покупок не нашёл", aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко"}))

	fake.replies = []string{reply}
	code, stdout, stderr := aiRun(t, "n\n", "ai", "купить молоко")
	if code != exitOK || stdout != "" {
		t.Fatalf("tt ai answered no = %d, stdout %q", code, stdout)
	}
	for _, want := range []string{`add "Купить молоко"`, "note from the model:", "  Список покупок не нашёл", "apply this change? [y/N] ", "nothing was changed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	if aiTaskCount(t, st) != 0 || aiSavedPreviews(t, st) != 1 {
		t.Fatalf("a no changed tasks or dropped the preview")
	}

	fake.replies = []string{reply}
	code, stdout, stderr = aiRun(t, "y\n", "ai", "купить молоко")
	if code != exitOK || !strings.Contains(stdout, `applied 1 change`) {
		t.Fatalf("tt ai answered yes = %d\n%s%s", code, stdout, stderr)
	}
	if aiTaskCount(t, st) != 1 {
		t.Error("a yes did not apply the change")
	}
}

func TestAIRefusesAnInvalidBundleAndSavesNothing(t *testing.T) {
	st, fake := aiCmdSetup(t)
	hostile := "Evil\x1b[31m\u202e" + strings.Repeat("list", 30)
	fake.replies = []string{aiCmdReply(t, "note \x1b]0;title\x07 "+strings.Repeat("longword", 20),
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко", "project": hostile}),
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "x", "due": "someday \x1b[2J"}),
	)}
	code, stdout, stderr := aiRun(t, "", "ai", "купить молоко")
	if code != exitError || stdout != "" {
		t.Fatalf("tt ai = %d, stdout %q", code, stdout)
	}
	for _, want := range []string{
		"tt: ai: the proposed changes cannot be applied; nothing was saved",
		"change 1:",
		"no cached list by this name",
		`the model wrote "Evil\x1b[31m\u202e`,
		"change 2:",
		"note from the model:",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	aiLinesFit(t, "stderr", stderr)
	if aiTaskCount(t, st) != 0 || aiSavedPreviews(t, st) != 0 {
		t.Error("an invalid bundle changed tasks or saved a preview")
	}
}

func TestAIRefusesTextThatLooksLikeASecretBeforeCallingTheModel(t *testing.T) {
	_, fake := aiCmdSetup(t)
	code, _, stderr := aiRun(t, "", "ai", "wifi password: hunter2")
	if code != exitError || !strings.Contains(stderr, "a password or secret") || !strings.Contains(stderr, "--allow-secrets") {
		t.Fatalf("tt ai = %d (stderr: %s)", code, stderr)
	}
	if strings.Contains(stderr, "hunter2") {
		t.Errorf("the refusal repeats the secret:\n%s", stderr)
	}
	aiLinesFit(t, "stderr", stderr)
	if len(fake.requests) != 0 {
		t.Fatal("the model was called with a secret")
	}
	fake.replies = []string{aiCmdReply(t, nil)}
	if code, stdout, stderr := aiRun(t, "", "ai", "wifi password: hunter2", "--allow-secrets"); code != exitOK || len(fake.requests) != 1 {
		t.Errorf("--allow-secrets = %d\n%s%s", code, stdout, stderr)
	}
}

func TestAIModelFailureIsEscapedAndBounded(t *testing.T) {
	st, fake := aiCmdSetup(t)
	fake.err = errors.New("ai: every profile failed:\nfast: exit status 1 (\x1b]0;pwned\x07 " + strings.Repeat("stderr-noise", 40) + ")")
	code, stdout, stderr := aiRun(t, "", "ai", "купить молоко")
	if code != exitError || stdout != "" || !strings.Contains(stderr, "tt: ai: the model call failed; nothing was changed") ||
		!strings.Contains(stderr, `\x1b]0;pwned\a`) {
		t.Fatalf("tt ai = %d\n%s", code, stderr)
	}
	aiLinesFit(t, "stderr", stderr)
	if aiSavedPreviews(t, st) != 0 {
		t.Error("a failed call saved a preview")
	}

	fake.err = nil
	fake.replies = []string{`{"ops": [], "notes": null, "extra": 1}`}
	if code, _, stderr := aiRun(t, "", "ai", "купить молоко"); code != exitError || !strings.Contains(stderr, "reply could not be used") {
		t.Errorf("a reply of the wrong shape = %d\n%s", code, stderr)
	}
}

func TestAIApplyFailureReversesTheChangesBeforeIt(t *testing.T) {
	st, fake := aiCmdSetup(t)
	t.Setenv("TZ", "UTC")
	fake.replies = []string{aiCmdReply(t, nil,
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "First"}),
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:00 + 1h"}),
	)}
	code, stdout, stderr := aiRun(t, "", "ai", "plan", "--preview")
	if code != exitOK {
		t.Fatalf("preview = %d (stderr: %s)", code, stderr)
	}
	id := aiPreviewID(t, stdout)
	interval, err := dates.ParseInterval("tmr 14:00 + 1h", "UTC", aiCmdNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(context.Background(), model.Task{ProjectId: "p2", Title: "Busy", StartDate: interval.Start, DueDate: interval.End, TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	before, _ := undoTop(t, st)

	code, stdout, stderr = aiRun(t, "", "ai", "--accept", id)
	if code != exitError || stdout != "" {
		t.Fatalf("accept = %d, stdout %q", code, stdout)
	}
	for _, want := range []string{"change 2 of the request failed", "nothing is left applied", "overlaps a task the preview did not show"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	aiLinesFit(t, "stderr", stderr)
	if aiTaskCount(t, st) != 1 {
		t.Errorf("tasks = %d, want only the one made after the preview", aiTaskCount(t, st))
	}
	if top, _ := undoTop(t, st); top.Seq != before.Seq {
		t.Errorf("undo stack is at %d, want %d", top.Seq, before.Seq)
	}
	if aiSavedPreviews(t, st) != 1 {
		t.Error("a request that was reversed whole lost its preview")
	}
}

func TestAIYesRefusesOverlapsUnlessAllowed(t *testing.T) {
	st, fake := aiCmdSetup(t)
	t.Setenv("TZ", "UTC")
	interval, err := dates.ParseInterval("tmr 14:00 + 1h", "UTC", aiCmdNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(context.Background(), model.Task{ProjectId: "p2", Title: "Busy", StartDate: interval.Start, DueDate: interval.End, TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	reply := aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:30 + 1h"}))
	fake.replies = []string{reply}
	code, stdout, stderr := aiRun(t, "", "ai", "plan", "-y")
	if code != exitError || !strings.Contains(stdout, `     overlaps "Busy"`) || !strings.Contains(stderr, "--allow-overlap") {
		t.Fatalf("-y over an overlap = %d\n%s%s", code, stdout, stderr)
	}
	aiLinesFit(t, "stderr", stderr)
	if aiTaskCount(t, st) != 1 {
		t.Fatal("-y applied an overlap it was not allowed")
	}
	fake.replies = []string{reply}
	if code, stdout, stderr := aiRun(t, "", "ai", "plan", "-y", "--allow-overlap"); code != exitOK || aiTaskCount(t, st) != 2 {
		t.Fatalf("-y --allow-overlap = %d\n%s%s", code, stdout, stderr)
	}
}

func TestAIJSONPreviewAcceptAndRefusal(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	fake.replies = []string{aiCmdReply(t, "done", aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко"}))}
	result, code := runMachine(t, ctx, "ai", "купить молоко")
	if code != exitOK || result.Status != "preview" {
		t.Fatalf("json preview = %d %+v", code, result)
	}
	var preview struct {
		ID    string     `json:"id"`
		Plan  app.AIPlan `json:"plan"`
		Notes string     `json:"notes"`
	}
	if err := json.Unmarshal(result.Data, &preview); err != nil || len(preview.ID) != 64 || len(preview.Plan.Steps) != 1 || preview.Notes != "done" {
		t.Fatalf("preview data = %s (%v)", result.Data, err)
	}

	result, code = runMachine(t, ctx, "ai", "--accept", preview.ID)
	var applied struct {
		GroupID string             `json:"group_id"`
		Changes []app.AIStepResult `json:"changes"`
	}
	if code != exitOK || json.Unmarshal(result.Data, &applied) != nil || applied.GroupID == "" || len(applied.Changes) != 1 {
		t.Fatalf("json accept = %d %s", code, result.Data)
	}
	if aiTaskCount(t, st) != 1 {
		t.Error("json accept did not apply")
	}

	fake.replies = []string{aiCmdReply(t, nil, aiCmdOp(app.AIOpDone, map[string]any{"ref": "t1"}))}
	result, code = runMachine(t, ctx, "ai", "finish it")
	if code != exitError || result.Error == nil || result.Error.Code != "invalid_proposal" || !strings.Contains(string(result.Data), "ai.context is minimal") {
		t.Fatalf("json refusal = %d %+v %s", code, result.Error, result.Data)
	}
}

func TestAIFindMakesTheNumberedListing(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	milk, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Купить молоко", Status: model.TaskOpen})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Позвонить маме", Status: model.TaskOpen}); err != nil {
		t.Fatal(err)
	}
	filter := `{"keywords":["молок"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`
	fake.replies = []string{filter}
	code, stdout, stderr := aiRun(t, "", "ai", "find", "молоко", "--no-ai-rerank")
	if code != exitOK || !strings.Contains(stdout, "Купить молоко") || strings.Contains(stdout, "Позвонить") {
		t.Fatalf("tt ai find = %d\n%s%s", code, stdout, stderr)
	}
	if req := fake.requests[0]; req.Task != config.AITaskFind || !strings.Contains(req.Prompt, "молоко") {
		t.Errorf("find request = %+v", req)
	}
	if code, _, stderr := aiRun(t, "", "done", "1"); code != exitOK {
		t.Fatalf("tt done 1 = %d (stderr: %s)", code, stderr)
	}
	if !undoTask(t, st, milk.Id).Status.Done() {
		t.Error("tt done 1 did not act on the first result")
	}

	fake.replies = []string{`{"keywords":["x"],"project":"Evil\u001b[31m","status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`}
	code, stdout, stderr = aiRun(t, "", "ai", "find", "anything")
	if code != exitError || !strings.Contains(stderr, "the search the model wrote cannot be used") || !strings.Contains(stderr, `"Evil\x1b[31m"`) {
		t.Errorf("find with an unknown list = %d\n%s%s", code, stdout, stderr)
	}
	aiLinesFit(t, "stderr", stderr)

	fake.replies = []string{filter}
	result, code := runMachine(t, ctx, "ai", "find", "молоко")
	var found app.AIFindResult
	if code != exitOK || json.Unmarshal(result.Data, &found) != nil || len(found.Filter.Keywords) != 1 {
		t.Errorf("json find = %d %s", code, result.Data)
	}
}

func aiFlat(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func TestAILeavesSecretLookingTitlesOutOfTheContext(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	writeConfig(t, "[ai]\ncontext = \"today\"\n")
	due := model.NewTime(aiCmdNow.Add(time.Hour))
	for _, title := range []string{"wifi password: hunter2", "Позвонить маме"} {
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: title, Status: model.TaskOpen, DueDate: due}); err != nil {
			t.Fatal(err)
		}
	}
	reply := aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко"}))
	said := "1 task was left out because it looks like it contains a secret; --allow-secrets sends it"

	fake.replies = []string{reply}
	code, stdout, stderr := aiRun(t, "", "ai", "купить молоко", "--preview")
	if code != exitOK {
		t.Fatalf("tt ai --preview = %d (stderr: %s)", code, stderr)
	}
	if prompt := fake.requests[0].Prompt; strings.Contains(prompt, "hunter2") || !strings.Contains(prompt, "Позвонить маме") {
		t.Errorf("prompt:\n%s", prompt)
	}
	if !strings.Contains(aiFlat(stdout), said) || strings.Contains(stdout, "hunter2") {
		t.Errorf("the preview does not say what was left out, or repeats it:\n%s", stdout)
	}
	aiLinesFit(t, "the preview", stdout)

	fake.replies = []string{aiCmdReply(t, nil, aiCmdOp(app.AIOpDone, map[string]any{"ref": "t2"}))}
	code, _, stderr = aiRun(t, "", "ai", "finish the wifi task")
	if code != exitError || !strings.Contains(stderr, "not in the context") || !strings.Contains(aiFlat(stderr), said) {
		t.Errorf("a ref to the task left out = %d\n%s", code, stderr)
	}

	for _, args := range [][]string{{"ai", "купить молоко"}, {"ai", "купить молоко", "-y"}} {
		fake.replies = []string{reply}
		result, code := runMachine(t, ctx, args...)
		var data struct {
			LeftOut *app.AILeftOut `json:"left_out"`
		}
		if code != exitOK || json.Unmarshal(result.Data, &data) != nil || data.LeftOut == nil || *data.LeftOut != (app.AILeftOut{Tasks: 1}) {
			t.Errorf("json %v = %d %s", args, code, result.Data)
		}
	}

	fake.replies = []string{reply}
	code, stdout, stderr = aiRun(t, "", "ai", "купить молоко", "--preview", "--allow-secrets")
	if code != exitOK || strings.Contains(stdout, "left out") || !strings.Contains(fake.requests[len(fake.requests)-1].Prompt, "hunter2") {
		t.Errorf("--allow-secrets = %d\n%s%s", code, stdout, stderr)
	}
}

func TestAIFindRanksTitlesOnlyWhenTheContextOrTheUserAsks(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	hidden := "молоко wifi password: hunter2"
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: hidden, Status: model.TaskOpen}); err != nil {
		t.Fatal(err)
	}
	for i := range app.AIFindRerankThreshold {
		if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Купить молоко " + strconv.Itoa(i+1), Status: model.TaskOpen}); err != nil {
			t.Fatal(err)
		}
	}
	filter := `{"keywords":["молок"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`

	fake.replies = []string{filter}
	code, stdout, stderr := aiRun(t, "", "ai", "find", "молоко")
	if code != exitOK || len(fake.requests) != 1 || strings.Contains(stdout, "left out") {
		t.Fatalf("tt ai find under the minimal context = %d, %d calls\n%s%s", code, len(fake.requests), stdout, stderr)
	}

	fake.requests, fake.replies = nil, []string{filter, `{"ranked":[1]}`}
	code, stdout, stderr = aiRun(t, "", "ai", "find", "молоко", "--rerank")
	if code != exitOK || len(fake.requests) != 2 {
		t.Fatalf("tt ai find --rerank = %d, %d calls (stderr: %s)", code, len(fake.requests), stderr)
	}
	if rerank := fake.requests[1].Prompt; strings.Contains(rerank, "hunter2") || !strings.Contains(rerank, "1. Купить молоко") {
		t.Errorf("rerank prompt:\n%s", rerank)
	}
	ranked, left := strings.Index(stdout, "Купить молоко"), strings.Index(stdout, "hunter2")
	if ranked < 0 || left < ranked || !strings.Contains(aiFlat(stdout), "1 task was left out because it looks like it contains a secret") {
		t.Errorf("the listing does not put the task left out after the ranked one, or does not say so:\n%s", stdout)
	}
	aiLinesFit(t, "the listing", stdout)

	fake.replies = []string{filter, `{"ranked":[1]}`}
	result, code := runMachine(t, ctx, "ai", "find", "молоко", "--rerank")
	var found app.AIFindResult
	if code != exitOK || json.Unmarshal(result.Data, &found) != nil || found.LeftOut != (app.AILeftOut{Tasks: 1}) || !found.Reranked ||
		len(found.Tasks) != 2 || found.Tasks[1].Title != hidden {
		t.Errorf("json find --rerank = %d %s", code, result.Data)
	}

	// The ranking call fails: the report still says what it was built without.
	fake.replies = []string{filter}
	code, _, stderr = aiRun(t, "", "ai", "find", "молоко", "--rerank")
	if code != exitError || !strings.Contains(aiFlat(stderr), "1 task was left out because it looks like it contains a secret") {
		t.Errorf("a failed ranking = %d\n%s", code, stderr)
	}
	aiLinesFit(t, "stderr", stderr)
	fake.replies = []string{filter}
	result, code = runMachine(t, ctx, "ai", "find", "молоко", "--rerank")
	var failed struct {
		LeftOut *app.AILeftOut `json:"left_out"`
	}
	if code != exitError || result.Error == nil || result.Error.Code != "ai_failed" || json.Unmarshal(result.Data, &failed) != nil ||
		failed.LeftOut == nil || *failed.LeftOut != (app.AILeftOut{Tasks: 1}) {
		t.Errorf("json of a failed ranking = %d %+v %s", code, result.Error, result.Data)
	}

	writeConfig(t, "[ai]\ncontext = \"today\"\n")
	fake.requests, fake.replies = nil, []string{filter, `{"ranked":[1]}`}
	if code, _, stderr := aiRun(t, "", "ai", "find", "молоко"); code != exitOK || len(fake.requests) != 2 {
		t.Errorf("tt ai find under the today context = %d, %d calls (stderr: %s)", code, len(fake.requests), stderr)
	}
}

func TestAIAcceptKeepsThePreviewOnlyWhenNothingIsLeftApplied(t *testing.T) {
	st, fake := aiCmdSetup(t)
	reply := aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "First"}), aiCmdOp(app.AIOpAdd, map[string]any{"title": "Second"}))
	preview := func() string {
		t.Helper()
		fake.replies = []string{reply}
		code, stdout, stderr := aiRun(t, "", "ai", "plan", "--preview")
		if code != exitOK {
			t.Fatalf("preview = %d (stderr: %s)", code, stderr)
		}
		return aiPreviewID(t, stdout)
	}
	previous := aiApplyPlan
	t.Cleanup(func() { aiApplyPlan = previous })

	id := preview()
	stopped, stop := context.WithCancel(context.Background())
	defer stop()
	aiApplyPlan = func(ctx context.Context, _ *store.Store, _ app.AIPlan) (app.AIApplied, error) {
		stop()
		return app.AIApplied{}, &app.AIApplyError{Step: 2, Op: app.AIOpAdd, Err: ctx.Err()}
	}
	var stdout, stderr bytes.Buffer
	code := run(stopped, []string{"ai", "--accept", id}, strings.NewReader(""), &stdout, &stderr)
	if code != exitInterrupted || stdout.Len() != 0 {
		t.Fatalf("accept interrupted = %d, stdout %q (stderr: %s)", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"change 2 of the request failed; the changes before it were reversed, so nothing is left applied", "why: tt was interrupted"} {
		if !strings.Contains(aiFlat(stderr.String()), want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr.String())
		}
	}
	aiLinesFit(t, "stderr", stderr.String())
	if aiSavedPreviews(t, st) != 1 {
		t.Error("an interrupt that left nothing applied lost the preview")
	}

	aiApplyPlan = func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
		return app.AIApplied{}, errors.New("the undo log is busy")
	}
	if code, _, stderr := aiRun(t, "", "ai", "--accept", id); code != exitError || !strings.Contains(stderr, "tt: ai: the undo log is busy") || aiSavedPreviews(t, st) != 1 {
		t.Errorf("a failure before the first change = %d, %d previews (stderr: %s)", code, aiSavedPreviews(t, st), stderr)
	}

	aiApplyPlan = func(_ context.Context, opened *store.Store, _ app.AIPlan) (app.AIApplied, error) {
		opened.Close()
		return app.AIApplied{}, &app.AIApplyError{Step: 1, Op: app.AIOpAdd, Err: errors.New("disk full")}
	}
	code, _, stderrText := aiRun(t, "", "ai", "--accept", id)
	if flat := aiFlat(stderrText); code != exitError || !strings.Contains(flat, "nothing is left applied") || !strings.Contains(flat, "the preview could not be kept") {
		t.Errorf("a preview that could not be put back = %d\n%s", code, stderrText)
	}
	aiLinesFit(t, "stderr", stderrText)

	stuck := errors.New("database is locked")
	aiApplyPlan = func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
		return app.AIApplied{}, &app.AIApplyError{Step: 2, Op: app.AIOpAdd, Err: errors.New("disk full"), Rollback: stuck}
	}
	id = preview()
	code, _, stderrText = aiRun(t, "", "ai", "--accept", id)
	for _, want := range []string{"change 2 of the request failed, and reversing the changes before it failed too; run tt undo to reverse what is left",
		"why: disk full", "why reversing failed: " + stuck.Error()} {
		if !strings.Contains(aiFlat(stderrText), want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderrText)
		}
	}
	aiLinesFit(t, "stderr", stderrText)
	if code != exitError || aiSavedPreviews(t, st) != 0 {
		t.Errorf("a request left partly applied = %d and kept %d previews, want it refused for another accept", code, aiSavedPreviews(t, st))
	}

	id = preview()
	result, code := runMachine(t, context.Background(), "ai", "--accept", id)
	var data struct {
		Step        int    `json:"step"`
		RolledBack  *bool  `json:"rolled_back"`
		ID          string `json:"id"`
		PreviewKept *bool  `json:"preview_kept"`
	}
	if code != exitError || result.Error == nil || result.Error.Code != "apply_failed" || json.Unmarshal(result.Data, &data) != nil ||
		data.Step != 2 || data.RolledBack == nil || *data.RolledBack || data.ID != id || data.PreviewKept == nil || *data.PreviewKept ||
		!strings.Contains(result.Error.Message, stuck.Error()) {
		t.Errorf("json accept left partly applied = %d %+v %s", code, result.Error, result.Data)
	}

	id = preview()
	aiApplyPlan = func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
		return app.AIApplied{}, app.ErrAIPlanStale
	}
	code, _, stderrText = aiRun(t, "", "ai", "--accept", id)
	if code != exitError || !strings.Contains(aiFlat(stderrText), "has changed since the preview") || aiSavedPreviews(t, st) != 0 {
		t.Errorf("a stale preview = %d, %d previews\n%s", code, aiSavedPreviews(t, st), stderrText)
	}
}

func TestAIAcceptDropsAPreviewItsReversalLeftStale(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	t.Setenv("TZ", "UTC")
	writeConfig(t, "[ai]\ncontext = \"today\"\n")
	report, err := st.CreateTask(ctx, model.Task{ProjectId: "p1", Title: "Отчёт", Status: model.TaskOpen, DueDate: model.NewTime(aiCmdNow.Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	fake.replies = []string{aiCmdReply(t, nil,
		aiCmdOp(app.AIOpEdit, map[string]any{"ref": "t1", "title": "Отчёт v2"}),
		aiCmdOp(app.AIOpAdd, map[string]any{"title": "Planned", "schedule": "tmr 14:00 + 1h"}),
	)}
	code, stdout, stderr := aiRun(t, "", "ai", "plan", "--preview")
	if code != exitOK {
		t.Fatalf("preview = %d (stderr: %s)", code, stderr)
	}
	id := aiPreviewID(t, stdout)
	interval, err := dates.ParseInterval("tmr 14:00 + 1h", "UTC", aiCmdNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, model.Task{ProjectId: "p2", Title: "Busy", StartDate: interval.Start, DueDate: interval.End, TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	// The reversal stamps the task at a later millisecond than the preview saw.
	time.Sleep(2 * time.Millisecond)

	code, _, stderr = aiRun(t, "", "ai", "--accept", id)
	flat := aiFlat(stderr)
	for _, want := range []string{"change 2 of the request failed; the changes before it were reversed, so nothing is left applied",
		"the preview was not kept: a task it changes is no longer as the preview saw it; run tt ai again"} {
		if !strings.Contains(flat, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
	aiLinesFit(t, "stderr", stderr)
	if code != exitError || strings.Contains(flat, "accept it again with") || aiSavedPreviews(t, st) != 0 {
		t.Errorf("accept whose reversal left the preview stale = %d, %d previews\n%s", code, aiSavedPreviews(t, st), stderr)
	}
	if got := undoTask(t, st, report.Id); got.Title != "Отчёт" {
		t.Errorf("the edit is still applied: %q", got.Title)
	}
}

func TestAIRollbackBlockedByAnotherChangeSaysWhatIsLeft(t *testing.T) {
	st, fake := aiCmdSetup(t)
	left, err := st.CreateTask(context.Background(), model.Task{ProjectId: "p1", Title: "Купить молоко", Status: model.TaskOpen})
	if err != nil {
		t.Fatal(err)
	}
	previous := aiApplyPlan
	t.Cleanup(func() { aiApplyPlan = previous })
	reply := aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "First"}), aiCmdOp(app.AIOpAdd, map[string]any{"title": "Second"}))
	lead := "change 2 of the request failed, and reversing the changes before it stopped: another change was recorded in between, " +
		"so 1 change of this request is still applied"
	review := "review what is still applied with tt show and change it back by hand"
	for _, c := range []struct {
		name                string
		above               int
		contiguous, touches bool
		say, deny           string
	}{
		{"one change on top", 1, true, false, `run "tt undo --skip", which drops the record of that change without reversing it, then "tt undo"`, "tt show"},
		{"one change on top of the same tasks", 1, true, true, "may touch the same tasks, so reversing this request's changes could undo it as well; " + review, "--skip"},
		{"changes on top of the same tasks", 2, true, true, "those changes may touch the same tasks, so reversing this request's changes could undo them as well; " + review, "--skip"},
		{"more changes on top", 2, true, false, review, "--skip"},
		{"other changes among the request's", 1, false, false, review, "--skip"},
		{"the request back on top", 0, true, false, `the rest of this request is back on top of the undo history, so "tt undo" reverses it`, "--skip"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake.replies = []string{reply}
			code, stdout, stderr := aiRun(t, "", "ai", "plan", "--preview")
			if code != exitOK {
				t.Fatalf("preview = %d (stderr: %s)", code, stderr)
			}
			id := aiPreviewID(t, stdout)
			aiApplyPlan = func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
				return app.AIApplied{}, &app.AIApplyError{Step: 2, Op: app.AIOpAdd, Err: errors.New("disk full"),
					Rollback: &app.AIRollbackBlockedError{Remaining: 1, TaskIDs: []string{left.Id}, Above: c.above, Contiguous: c.contiguous, AboveTouches: c.touches}}
			}
			code, _, stderr = aiRun(t, "", "ai", "--accept", id)
			flat := aiFlat(stderr)
			for _, want := range []string{lead, "why: disk full", `still applied to: "Купить молоко"`, c.say} {
				if !strings.Contains(flat, want) {
					t.Errorf("stderr does not say %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(flat, c.deny) || strings.Contains(flat, "run tt undo to reverse what is left") {
				t.Errorf("stderr gives advice that does not hold here:\n%s", stderr)
			}
			aiLinesFit(t, "stderr", stderr)
			if code != exitError || aiSavedPreviews(t, st) != 0 {
				t.Errorf("a request left partly applied = %d, %d previews; want it refused and its preview gone", code, aiSavedPreviews(t, st))
			}
		})
	}
}

func TestAIFailedApplyShowsTheKeptPreviewToTheRunThatMadeIt(t *testing.T) {
	st, fake := aiCmdSetup(t)
	ctx := context.Background()
	previous, previousAsk := aiApplyPlan, canAsk
	t.Cleanup(func() { aiApplyPlan, canAsk = previous, previousAsk })
	reply := aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко"}))
	failing := func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
		return app.AIApplied{}, &app.AIApplyError{Step: 1, Op: app.AIOpAdd, Err: errors.New("disk full")}
	}
	stopped := func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error) {
		return app.AIApplied{}, errors.New("the undo log is busy")
	}
	for _, c := range []struct {
		name  string
		stdin string
		ask   bool
		args  []string
		apply func(context.Context, *store.Store, app.AIPlan) (app.AIApplied, error)
	}{
		{"-y", "", false, []string{"ai", "купить молоко", "-y"}, failing},
		{"-y stopped before the first change", "", false, []string{"ai", "купить молоко", "-y"}, stopped},
		{"a yes in a terminal", "y\n", true, []string{"ai", "купить молоко"}, failing},
	} {
		t.Run(c.name, func(t *testing.T) {
			canAsk = func(*invocation) bool { return c.ask }
			aiApplyPlan = c.apply
			fake.replies = []string{reply}
			code, _, stderr := aiRun(t, c.stdin, c.args...)
			if code != exitError || !strings.Contains(stderr, "the preview is kept; accept it again with:") || aiSavedPreviews(t, st) != 1 {
				t.Fatalf("%v = %d, %d previews\n%s", c.args, code, aiSavedPreviews(t, st), stderr)
			}
			// A terminal ends the question with the answer the user types.
			aiLinesFit(t, "stderr", strings.ReplaceAll(stderr, "[y/N] ", "[y/N]\n"))
			id := aiPreviewID(t, stderr)
			aiApplyPlan = previous
			if code, stdout, stderr := aiRun(t, "", "ai", "--accept", id); code != exitOK || !strings.Contains(stdout, "applied 1 change") {
				t.Errorf("accept of the kept preview = %d\n%s%s", code, stdout, stderr)
			}
		})
	}

	canAsk = previousAsk
	aiApplyPlan = failing
	fake.replies = []string{reply}
	code, stdout, stderr := aiRun(t, "", "ai", "купить молоко", "--preview")
	if code != exitOK {
		t.Fatalf("preview = %d (stderr: %s)", code, stderr)
	}
	id := aiPreviewID(t, stdout)
	if code, _, stderr := aiRun(t, "", "ai", "--accept", id); code != exitError || strings.Contains(stderr, "accept it again") || aiSavedPreviews(t, st) != 1 {
		t.Errorf("accept by ID = %d, %d previews; want the preview kept without repeating the ID\n%s", code, aiSavedPreviews(t, st), stderr)
	}

	fake.replies = []string{reply}
	result, code := runMachine(t, ctx, "ai", "купить молоко", "-y")
	var data struct {
		ID          string         `json:"id"`
		PreviewKept *bool          `json:"preview_kept"`
		LeftOut     *app.AILeftOut `json:"left_out"`
	}
	if code != exitError || json.Unmarshal(result.Data, &data) != nil || data.ID != id || data.PreviewKept == nil || !*data.PreviewKept || data.LeftOut == nil {
		t.Errorf("json -y that failed = %d %s", code, result.Data)
	}
}

func TestAIRunsRemoveExpiredPreviews(t *testing.T) {
	st, fake := aiCmdSetup(t)
	stale := func() string {
		t.Helper()
		aiNow = func() time.Time { return aiCmdNow.Add(-app.AIPlanLifetime - time.Hour) }
		defer func() { aiNow = func() time.Time { return aiCmdNow } }()
		fake.replies = []string{aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "Старое"}))}
		code, stdout, stderr := aiRun(t, "", "ai", "старое", "--preview")
		if code != exitOK {
			t.Fatalf("an old preview = %d (stderr: %s)", code, stderr)
		}
		return aiPreviewID(t, stdout)
	}
	filter := `{"keywords":["молок"],"project":null,"status":null,"due_from":null,"due_to":null,"done_from":null,"done_to":null}`

	stale()
	fake.replies = []string{filter}
	if code, _, stderr := aiRun(t, "", "ai", "find", "молоко"); code != exitOK || aiSavedPreviews(t, st) != 0 {
		t.Errorf("tt ai find = %d and left %d previews (stderr: %s)", code, aiSavedPreviews(t, st), stderr)
	}

	stale()
	fake.err = errors.New("offline")
	if code, _, _ := aiRun(t, "", "ai", "купить молоко"); code != exitError || aiSavedPreviews(t, st) != 0 {
		t.Errorf("tt ai whose call failed = %d and left %d previews", code, aiSavedPreviews(t, st))
	}
	fake.err = nil

	id := stale()
	code, _, stderr := aiRun(t, "", "ai", "--accept", id)
	if code != exitError || !strings.Contains(stderr, "tt: ai: "+app.ErrAIPreviewExpired.Error()) || aiSavedPreviews(t, st) != 0 {
		t.Errorf("accept of an old preview = %d, %d previews\n%s", code, aiSavedPreviews(t, st), stderr)
	}
	aiLinesFit(t, "stderr", stderr)

	stale()
	fake.replies = []string{aiCmdReply(t, nil, aiCmdOp(app.AIOpAdd, map[string]any{"title": "Купить молоко"}))}
	code, stdout, stderr := aiRun(t, "", "ai", "купить молоко", "--preview")
	if code != exitOK || aiSavedPreviews(t, st) != 1 {
		t.Fatalf("a new preview = %d, %d previews (stderr: %s)", code, aiSavedPreviews(t, st), stderr)
	}
	stale()
	if code, _, stderr := aiRun(t, "", "ai", "--accept", aiPreviewID(t, stdout)); code != exitOK || aiSavedPreviews(t, st) != 0 {
		t.Errorf("accept of a new preview = %d and left %d previews (stderr: %s)", code, aiSavedPreviews(t, st), stderr)
	}

	code, _, stderr = aiRun(t, "", "ai", "--accept", strings.Repeat("0", 64))
	if code != exitUsage || !strings.Contains(stderr, "tt: ai: "+app.ErrAIPreviewUnavailable.Error()) {
		t.Errorf("accept of an unknown preview = %d\n%s", code, stderr)
	}
	aiLinesFit(t, "stderr", stderr)
}

func TestAIRefusesOptionsItDoesNotTake(t *testing.T) {
	aiCmdSetup(t)
	for _, args := range [][]string{
		{"ai", "milk", "--rerank"},
		{"ai", "find", "milk", "-y"},
		{"ai", "milk", "--preview", "-y"},
		{"ai", "--accept", "abc", "milk"},
		{"ai", "milk", "--profile"},
		{"ai"},
		{"ai", "find"},
	} {
		if code, _, stderr := aiRun(t, "", args...); code != exitUsage {
			t.Errorf("%v = %d, want %d (stderr: %s)", args, code, exitUsage, stderr)
		}
	}
}
