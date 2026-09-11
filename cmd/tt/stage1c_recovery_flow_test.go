package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

const (
	stage1CCommandProjectID = "command-project"
	stage1CCommandTaskID    = "command-task"
	stage1CCommandToken     = "command-flow-token"
)

type stage1CCommandRemote struct {
	t *testing.T

	mu                sync.Mutex
	task              map[string]any
	requests          []string
	updatePosts       int
	idlessOccurrences int
	nextItemID        int
	mismatchNext      bool
	denied            int
}

func newStage1CCommandRemote(t *testing.T) *stage1CCommandRemote {
	return &stage1CCommandRemote{
		t: t,
		task: map[string]any{
			"id": stage1CCommandTaskID, "projectId": stage1CCommandProjectID,
			"title": "recovery command", "status": 0, "kind": "CHECKLIST",
			"items": []any{stage1CCommandItem("remote-a", "alpha", 1)},
		},
		nextItemID: 1,
	}
}

func stage1CCommandItem(id, title string, rank int64) map[string]any {
	return map[string]any{
		"id": id, "title": title, "status": 0, "sortOrder": rank,
		"startDate": "", "isAllDay": false, "timeZone": "", "completedTime": "",
	}
}

func (f *stage1CCommandRemote) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	f.serveHTTP(recorder, request)
	return recorder.Result(), nil
}

func (f *stage1CCommandRemote) serveHTTP(w http.ResponseWriter, request *http.Request) {
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			f.t.Errorf("read intercepted request: %v", err)
		}
		request.Body.Close()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request.Method+" "+request.URL.Path)
	if request.Header.Get("Authorization") != "Bearer "+stage1CCommandToken {
		f.denied++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/open/v1/project":
		stage1CCommandWriteJSON(w, []map[string]any{{
			"id": stage1CCommandProjectID, "name": "Command Project", "kind": "TASK",
		}})
	case request.Method == http.MethodGet && request.URL.Path == "/open/v1/project/"+stage1CCommandProjectID+"/data":
		stage1CCommandWriteJSON(w, map[string]any{
			"project": map[string]any{"id": stage1CCommandProjectID, "name": "Command Project", "kind": "TASK"},
			"tasks":   []any{stage1CCommandClone(f.task)}, "columns": []any{},
		})
	case request.Method == http.MethodGet && request.URL.Path == "/open/v1/project/inbox/data":
		stage1CCommandWriteJSON(w, map[string]any{"tasks": []any{}, "columns": []any{}})
	case request.Method == http.MethodPost && request.URL.Path == "/open/v1/task/"+stage1CCommandTaskID:
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			f.denied++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.updatePosts++
		for name, value := range patch {
			if name != "id" && name != "projectId" {
				f.task[name] = stage1CCommandClone(value)
			}
		}
		if items, ok := f.task["items"].([]any); ok {
			for _, value := range items {
				item := value.(map[string]any)
				if id, _ := item["id"].(string); id == "" {
					f.nextItemID++
					f.idlessOccurrences++
					item["id"] = fmt.Sprintf("remote-%c", 'a'+f.nextItemID-1)
				}
			}
		}
		stage1CCommandWriteJSON(w, stage1CCommandClone(f.task))
	case request.Method == http.MethodGet && request.URL.Path == "/open/v1/project/"+stage1CCommandProjectID+"/task/"+stage1CCommandTaskID:
		response := stage1CCommandClone(f.task).(map[string]any)
		if f.mismatchNext {
			f.mismatchNext = false
			items := response["items"].([]any)
			items[len(items)-1].(map[string]any)["title"] = "mismatching confirmation"
		}
		stage1CCommandWriteJSON(w, response)
	default:
		f.denied++
		w.WriteHeader(http.StatusTeapot)
		stage1CCommandWriteJSON(w, map[string]any{"error": "unexpected intercepted request"})
	}
}

func stage1CCommandWriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func stage1CCommandClone(value any) any {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		panic(err)
	}
	return out
}

func (f *stage1CCommandRemote) mismatchConfirmationOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mismatchNext = true
}

func (f *stage1CCommandRemote) counts() (posts, idless, denied int, requests []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updatePosts, f.idlessOccurrences, f.denied, append([]string(nil), f.requests...)
}

func stage1CCommandSync(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cmdSync(context.Background(), &stdout, &stderr, args)
	return code, stdout.String(), stderr.String()
}

func stage1CCommandOpenStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "")
	if err != nil {
		t.Fatalf("open command cache: %v", err)
	}
	return st
}

func stage1CCommandTask(t *testing.T, st *store.Store) model.Task {
	t.Helper()
	task, err := st.Task(context.Background(), stage1CCommandTaskID)
	if err != nil {
		t.Fatalf("read command task: %v", err)
	}
	return task
}

func stage1CCommandKeys(task model.Task) []string {
	keys := make([]string, len(task.Items))
	for i := range task.Items {
		keys[i] = task.Items[i].Key
	}
	sort.Strings(keys)
	return keys
}

func stage1CCommandControl(t *testing.T, st *store.Store) string {
	t.Helper()
	var state string
	if err := st.DB().QueryRowContext(context.Background(), `
		SELECT state FROM item_identities WHERE task_id = ? AND item_key = ''`, stage1CCommandTaskID).Scan(&state); err != nil {
		t.Fatalf("read command recovery control: %v", err)
	}
	return state
}

func TestStage1CCommandDiscardAdoptsRemoteChecklistAndPreservesUndoBoundaries(t *testing.T) {
	isolate(t)
	t.Setenv(tokenEnvVar, stage1CCommandToken)
	remote := newStage1CCommandRemote(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = oauthRoundTripFunc(remote.RoundTrip)
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	code, stdout, stderr := stage1CCommandSync(t)
	if code != exitOK {
		t.Fatalf("initial cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	st := stage1CCommandOpenStore(t)
	initial := stage1CCommandTask(t, st)
	if len(initial.Items) != 1 || initial.Items[0].Id != "remote-a" {
		t.Fatalf("initial pulled checklist = %+v", initial.Items)
	}
	mutated, err := st.AddTaskItem(context.Background(), stage1CCommandTaskID, "allocated remotely")
	if err != nil {
		t.Fatalf("queue allocating checklist edit: %v", err)
	}
	mutated, err = st.RenameTaskItem(context.Background(), stage1CCommandTaskID, 1, "dependent local intent")
	if err != nil {
		t.Fatalf("queue dependent checklist edit: %v", err)
	}
	preDiscardKeys := stage1CCommandKeys(mutated)
	preDiscardUndo, err := st.LastUndo(context.Background())
	if err != nil || preDiscardUndo.Feature == nil || !preDiscardUndo.Feature.Fields.Items {
		t.Fatalf("pre-discard undo = %+v, err=%v; want keyed item history", preDiscardUndo, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	remote.mismatchConfirmationOnce()
	code, stdout, stderr = stage1CCommandSync(t)
	if code != exitError {
		t.Fatalf("mismatching cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "failed 1") {
		t.Errorf("mismatching cmdSync stdout = %q, want failed count", stdout)
	}
	for _, want := range []string{`not\x20confirmed`, "parked", `run "tt sync --retry-failed"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("mismatching cmdSync stderr = %q, want %q", stderr, want)
		}
	}
	st = stage1CCommandOpenStore(t)
	counts, err := st.OutboxCounts(context.Background())
	if err != nil || counts.Failed != 1 || counts.Pending != 1 {
		t.Fatalf("parked predecessor queue = %+v, %v; want one failed and one pending dependent", counts, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = stage1CCommandSync(t, "--drop-parked")
	if code != exitError {
		t.Fatalf("first drop cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{"threw away 1 parked entry(s)", store.OpTaskUpdate} {
		if !strings.Contains(stdout, want) {
			t.Errorf("drop cmdSync stdout = %q, want %q", stdout, want)
		}
	}
	if !strings.Contains(stderr, "1 parked entry(s) thrown away") {
		t.Errorf("drop cmdSync stderr = %q, want visible discard summary", stderr)
	}
	if !strings.Contains(stdout, "failed 1") || !strings.Contains(stderr, "parked") {
		t.Fatalf("first drop did not visibly park dependent: stdout %q, stderr %q", stdout, stderr)
	}

	posts, idless, denied, requests := remote.counts()
	if posts != 1 || idless != 2 || denied != 0 {
		t.Fatalf("first discard repeated allocating POST: posts=%d idless=%d denied=%d all=%v", posts, idless, denied, requests)
	}
	st = stage1CCommandOpenStore(t)
	heldAfterFirstDrop := stage1CCommandTask(t, st)
	if len(heldAfterFirstDrop.Items) != 2 || heldAfterFirstDrop.Items[0].Title != "dependent local intent" ||
		!reflectStringSlices(stage1CCommandKeys(heldAfterFirstDrop), preDiscardKeys) {
		t.Fatalf("first discard adopted over dependent intent: %+v", heldAfterFirstDrop.Items)
	}
	counts, err = st.OutboxCounts(context.Background())
	if err != nil || counts.Failed != 1 || counts.Pending != 0 || stage1CCommandControl(t, st) != string(store.RecoveryPending) {
		t.Fatalf("queue after first discard = %+v, %v, control=%q; want parked dependent and pending recovery", counts, err, stage1CCommandControl(t, st))
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = stage1CCommandSync(t, "--drop-parked")
	if code != exitOK {
		t.Fatalf("second drop-and-adopt cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{"threw away 1 parked entry(s)", store.OpTaskUpdate} {
		if !strings.Contains(stdout, want) {
			t.Errorf("second drop cmdSync stdout = %q, want %q", stdout, want)
		}
	}
	if !strings.Contains(stderr, "1 parked entry(s) thrown away") {
		t.Errorf("second drop cmdSync stderr = %q, want visible discard summary", stderr)
	}
	posts, idless, denied, requests = remote.counts()
	if posts != 1 || idless != 2 || denied != 0 {
		t.Fatalf("second discard repeated allocating POST: posts=%d idless=%d denied=%d all=%v", posts, idless, denied, requests)
	}
	st = stage1CCommandOpenStore(t)
	adopted := stage1CCommandTask(t, st)
	if len(adopted.Items) != 2 {
		t.Fatalf("adopted checklist = %+v, want both remote items", adopted.Items)
	}
	adoptedKeys := stage1CCommandKeys(adopted)
	for _, oldKey := range preDiscardKeys {
		for _, newKey := range adoptedKeys {
			if oldKey == newKey {
				t.Fatalf("adoption reused abandoned key %q: old=%v new=%v", oldKey, preDiscardKeys, adoptedKeys)
			}
		}
	}
	counts, err = st.OutboxCounts(context.Background())
	if err != nil || counts != (store.OutboxCounts{}) || stage1CCommandControl(t, st) != string(store.RecoveryIdle) {
		t.Fatalf("queue after command recovery = %+v, %v", counts, err)
	}
	top, err := st.LastUndo(context.Background())
	if err != nil || top.Seq != preDiscardUndo.Seq {
		t.Fatalf("pre-discard history was not retained: top=%+v err=%v", top, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = stage1CCommandOpenStore(t)
	beforeNewWork := stage1CCommandTask(t, st)
	if _, err := st.RenameTaskItem(context.Background(), stage1CCommandTaskID, 1, "alpha after adoption"); err != nil {
		t.Fatalf("post-adoption keyed edit: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = stage1CCommandSync(t)
	if code != exitOK {
		t.Fatalf("post-adoption keyed cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	st = stage1CCommandOpenStore(t)
	afterNewWork := stage1CCommandTask(t, st)
	if afterNewWork.Items[0].Title != "alpha after adoption" ||
		!reflectStringSlices(stage1CCommandKeys(afterNewWork), adoptedKeys) {
		t.Fatalf("post-adoption sync changed fresh identities or missed edit: %+v", afterNewWork.Items)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = undoRun(t)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "undid the last change") {
		t.Fatalf("post-adoption cmdUndo = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	st = stage1CCommandOpenStore(t)
	afterUndo := stage1CCommandTask(t, st)
	if afterUndo.Items[0].Title != beforeNewWork.Items[0].Title ||
		!reflectStringSlices(stage1CCommandKeys(afterUndo), stage1CCommandKeys(beforeNewWork)) {
		t.Fatalf("post-adoption undo changed identity or failed to restore values: before=%+v after=%+v", beforeNewWork.Items, afterUndo.Items)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = stage1CCommandSync(t)
	if code != exitOK {
		t.Fatalf("final cmdSync = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	st = stage1CCommandOpenStore(t)
	if !reflectStringSlices(stage1CCommandKeys(stage1CCommandTask(t, st)), adoptedKeys) {
		t.Fatalf("undo sync changed adopted identities")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr = undoRun(t)
	if code != exitError || stdout != "" || !strings.Contains(stderr, "abandoned item identity") {
		t.Fatalf("cmdUndo over pre-discard history = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	st = stage1CCommandOpenStore(t)
	top, err = st.LastUndo(context.Background())
	if err != nil || top.Seq != preDiscardUndo.Seq {
		t.Fatalf("refused cmdUndo consumed history: top=%+v err=%v", top, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = undoRun(t, undoSkipOption)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "dropped the record") {
		t.Fatalf("cmdUndo --skip = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	posts, idless, denied, requests = remote.counts()

	if posts != 3 || idless != 6 || denied != 0 {
		t.Fatalf("final intercepted requests: posts=%d idless=%d denied=%d all=%v", posts, idless, denied, requests)
	}
	st = stage1CCommandOpenStore(t)
	defer st.Close()
	counts, err = st.OutboxCounts(context.Background())
	if err != nil || counts != (store.OutboxCounts{}) {
		t.Fatalf("final queue = %+v, %v", counts, err)
	}
}

func reflectStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
