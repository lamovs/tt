package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type stage1CWireRequest struct {
	Seq    int
	Method string
	Path   string
	Body   []byte
}

type stage1CWireResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

type stage1CWireExchange struct {
	Request  stage1CWireRequest
	Response stage1CWireResponse
}

type stage1CWireResponseHook func(stage1CWireRequest, stage1CWireResponse) stage1CWireResponse
type stage1CWireOption func(*stage1CWireFake)

func stage1CWireWithResponseHook(h stage1CWireResponseHook) stage1CWireOption {
	return func(f *stage1CWireFake) { f.responseHook = h }
}

type stage1CWireHold struct {
	method  string
	path    string
	reached chan stage1CWireExchange
	release chan struct{}
	once    stdsync.Once
}

type stage1CWireFake struct {
	t      *testing.T
	server *httptest.Server

	mu           stdsync.Mutex
	projects     map[string]map[string]any
	tasks        map[string]map[string]map[string]any
	requests     []stage1CWireRequest
	nextTaskID   int
	nextItemID   int
	responseHook stage1CWireResponseHook
	holds        []*stage1CWireHold
}

func newStage1CWireFake(t *testing.T, opts ...stage1CWireOption) *stage1CWireFake {
	t.Helper()
	f := &stage1CWireFake{
		t:        t,
		projects: map[string]map[string]any{},
		tasks:    map[string]map[string]map[string]any{},
	}
	for _, opt := range opts {
		opt(f)
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(func() {
		f.mu.Lock()
		holds := append([]*stage1CWireHold(nil), f.holds...)
		f.mu.Unlock()
		for _, hold := range holds {
			hold.once.Do(func() { close(hold.release) })
		}
		f.server.Close()
	})
	return f
}

func (f *stage1CWireFake) Server() *httptest.Server { return f.server }

func (f *stage1CWireFake) SeedProject(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[id] = map[string]any{"id": id, "name": name}
	if f.tasks[id] == nil {
		f.tasks[id] = map[string]map[string]any{}
	}
}

func (f *stage1CWireFake) SeedTask(projectID string, raw map[string]any) {
	f.SetTask(projectID, stringValue(raw["id"]), raw)
}

func (f *stage1CWireFake) SetTask(projectID, taskID string, raw map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tasks[projectID] == nil {
		f.tasks[projectID] = map[string]map[string]any{}
	}
	copyOfRaw := cloneJSONMap(raw)
	copyOfRaw["id"] = taskID
	copyOfRaw["projectId"] = projectID
	f.tasks[projectID][taskID] = copyOfRaw
}

func (f *stage1CWireFake) MutateTask(projectID, taskID string, mutate func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[projectID][taskID]
	if !ok {
		f.t.Fatalf("mutate missing remote task %s/%s", projectID, taskID)
	}
	mutate(task)
}

func (f *stage1CWireFake) RemoveTask(projectID, taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tasks[projectID], taskID)
}

func (f *stage1CWireFake) Task(projectID, taskID string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[projectID][taskID]
	if !ok {
		return nil, false
	}
	return cloneJSONMap(task), true
}

func (f *stage1CWireFake) Requests() []stage1CWireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]stage1CWireRequest, len(f.requests))
	for i, request := range f.requests {
		out[i] = request
		out[i].Body = append([]byte(nil), request.Body...)
	}
	return out
}

func (f *stage1CWireFake) Count(method, path string) int {
	requests := f.Requests()
	var count int
	for _, request := range requests {
		if request.Method == method && request.Path == path {
			count++
		}
	}
	return count
}

func (f *stage1CWireFake) SetResponseHook(h stage1CWireResponseHook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responseHook = h
}

func (f *stage1CWireFake) HoldNext(method, path string) (<-chan stage1CWireExchange, func()) {
	hold := &stage1CWireHold{
		method:  method,
		path:    path,
		reached: make(chan stage1CWireExchange, 1),
		release: make(chan struct{}),
	}
	f.mu.Lock()
	f.holds = append(f.holds, hold)
	f.mu.Unlock()
	return hold.reached, func() { hold.once.Do(func() { close(hold.release) }) }
}

func stage1CWireJSONResponse(status int, value any) stage1CWireResponse {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return stage1CWireResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   body,
	}
}

func stage1CWireRawResponse(status int, body []byte) stage1CWireResponse {
	return stage1CWireResponse{Status: status, Body: append([]byte(nil), body...)}
}

func (f *stage1CWireFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read wire request: %v", err)
	}
	f.mu.Lock()
	request := stage1CWireRequest{
		Seq: len(f.requests) + 1, Method: r.Method, Path: r.URL.Path,
		Body: append([]byte(nil), body...),
	}
	f.requests = append(f.requests, request)
	response := f.defaultResponseLocked(request)
	hook := f.responseHook
	var hold *stage1CWireHold
	for i, candidate := range f.holds {
		if candidate.method == request.Method && candidate.path == request.Path {
			hold = candidate
			f.holds = append(f.holds[:i], f.holds[i+1:]...)
			break
		}
	}
	f.mu.Unlock()

	if hook != nil {
		response = hook(request, cloneWireResponse(response))
	}
	response = cloneWireResponse(response)
	if hold != nil {
		exchange := stage1CWireExchange{Request: request, Response: response}
		hold.reached <- exchange
		select {
		case <-hold.release:
		case <-r.Context().Done():
			return
		}
	}
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(response.Body) != 0 {
		if _, err := w.Write(response.Body); err != nil {
			f.t.Errorf("write wire response: %v", err)
		}
	}
}

func (f *stage1CWireFake) defaultResponseLocked(request stage1CWireRequest) stage1CWireResponse {
	if request.Method == http.MethodGet && request.Path == "/open/v1/project" {
		ids := make([]string, 0, len(f.projects))
		for id := range f.projects {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		projects := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			projects = append(projects, cloneJSONMap(f.projects[id]))
		}
		return stage1CWireJSONResponse(http.StatusOK, projects)
	}
	if request.Method == http.MethodPost && request.Path == "/open/v1/task" {
		var task map[string]any
		if err := json.Unmarshal(request.Body, &task); err != nil {
			return stage1CWireRawResponse(http.StatusBadRequest, []byte(`{"error":"invalid json"}`))
		}
		f.nextTaskID++
		taskID := fmt.Sprintf("remote-task-%d", f.nextTaskID)
		projectID := stringValue(task["projectId"])
		task["id"] = taskID
		task["projectId"] = projectID
		f.allocateItemIDsLocked(task)
		f.storeTaskLocked(projectID, taskID, task)
		return stage1CWireJSONResponse(http.StatusOK, completeWireTask(task))
	}

	parts := splitWirePath(request.Path)
	if len(parts) == 4 && parts[0] == "project" && parts[2] == "task" && request.Method == http.MethodGet {
		projectID, taskID := parts[1], parts[3]
		task, ok := f.tasks[projectID][taskID]
		if !ok {
			return stage1CWireRawResponse(http.StatusNotFound, nil)
		}
		return stage1CWireJSONResponse(http.StatusOK, completeWireTask(task))
	}
	if len(parts) == 3 && parts[0] == "project" && parts[2] == "data" && request.Method == http.MethodGet {
		projectID := parts[1]
		project, ok := f.projects[projectID]
		if !ok {
			return stage1CWireRawResponse(http.StatusOK, nil)
		}
		ids := make([]string, 0, len(f.tasks[projectID]))
		for id := range f.tasks[projectID] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tasks := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			tasks = append(tasks, completeWireTask(f.tasks[projectID][id]))
		}
		return stage1CWireJSONResponse(http.StatusOK, map[string]any{
			"project": cloneJSONMap(project), "tasks": tasks, "columns": []any{},
		})
	}
	if len(parts) == 2 && parts[0] == "task" && request.Method == http.MethodPost {
		taskID := parts[1]
		var patch map[string]any
		if err := json.Unmarshal(request.Body, &patch); err != nil {
			return stage1CWireRawResponse(http.StatusBadRequest, []byte(`{"error":"invalid json"}`))
		}
		projectID := stringValue(patch["projectId"])
		task, ok := f.tasks[projectID][taskID]
		if !ok {
			return stage1CWireRawResponse(http.StatusOK, nil)
		}
		for name, value := range patch {
			if name != "id" && name != "projectId" {
				task[name] = cloneJSONValue(value)
			}
		}
		f.allocateItemIDsLocked(task)
		f.storeTaskLocked(projectID, taskID, task)
		return stage1CWireJSONResponse(http.StatusOK, completeWireTask(task))
	}
	if len(parts) == 5 && parts[0] == "project" && parts[2] == "task" && parts[4] == "complete" && request.Method == http.MethodPost {
		projectID, taskID := parts[1], parts[3]
		if task, ok := f.tasks[projectID][taskID]; ok {
			task["status"] = float64(2)
			task["completedTime"] = "2026-09-09T12:00:00.000+0000"
			return stage1CWireJSONResponse(http.StatusOK, completeWireTask(task))
		}
		return stage1CWireRawResponse(http.StatusNotFound, nil)
	}
	if len(parts) == 4 && parts[0] == "project" && parts[2] == "task" && request.Method == http.MethodDelete {
		projectID, taskID := parts[1], parts[3]
		if _, ok := f.tasks[projectID][taskID]; !ok {
			return stage1CWireRawResponse(http.StatusNotFound, nil)
		}
		delete(f.tasks[projectID], taskID)
		return stage1CWireRawResponse(http.StatusNoContent, nil)
	}
	return stage1CWireRawResponse(http.StatusNotFound, []byte(`{"error":"unknown route"}`))
}

func (f *stage1CWireFake) allocateItemIDsLocked(task map[string]any) {
	items, ok := task["items"].([]any)
	if !ok {
		return
	}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(item["id"]) == "" {
			f.nextItemID++
			item["id"] = fmt.Sprintf("remote-item-%d", f.nextItemID)
		}
	}
}

func (f *stage1CWireFake) storeTaskLocked(projectID, taskID string, task map[string]any) {
	if f.tasks[projectID] == nil {
		f.tasks[projectID] = map[string]map[string]any{}
	}
	f.tasks[projectID][taskID] = cloneJSONMap(task)
}

func splitWirePath(path string) []string {
	path = strings.TrimPrefix(path, "/open/v1/")
	raw := strings.Split(strings.Trim(path, "/"), "/")
	for i := range raw {
		if value, err := url.PathUnescape(raw[i]); err == nil {
			raw[i] = value
		}
	}
	return raw
}

func cloneWireResponse(in stage1CWireResponse) stage1CWireResponse {
	out := in
	out.Header = in.Header.Clone()
	out.Body = append([]byte(nil), in.Body...)
	return out
}

func cloneJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out, _ := cloneJSONValue(in).(map[string]any)
	return out
}

func cloneJSONValue(in any) any {
	body, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		panic(err)
	}
	return out
}

func completeWireTask(in map[string]any) map[string]any {
	task := cloneJSONMap(in)
	items, ok := task["items"].([]any)
	if !ok {
		return task
	}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		defaults := map[string]any{
			"id": "", "title": "", "status": float64(0), "sortOrder": float64(0),
			"startDate": "", "isAllDay": false, "timeZone": "", "completedTime": "",
		}
		for name, value := range defaults {
			if _, found := item[name]; !found {
				item[name] = value
			}
		}
	}
	return task
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func stage1CSeedRemoteTask(t *testing.T, st *store.Store, fake *stage1CWireFake, raw map[string]any) model.Task {
	t.Helper()
	projectID := stringValue(raw["projectId"])
	taskID := stringValue(raw["id"])
	fake.SeedTask(projectID, raw)
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var wire api.Task
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode seed task: %v", err)
	}
	task, warnings := api.TaskToModel(wire)
	if len(warnings) != 0 {
		t.Fatalf("seed task warnings: %v", warnings)
	}
	if _, err := st.SyncProject(context.Background(), projectID, []store.ServerTask{{Task: task, Raw: body}}); err != nil {
		t.Fatalf("seed cached task: %v", err)
	}
	got, err := st.Task(context.Background(), taskID)
	if err != nil {
		t.Fatalf("read seeded task: %v", err)
	}
	return got
}

func stage1CItem(title, id string, status int, rank int64, completed string) map[string]any {
	return map[string]any{
		"id": id, "title": title, "status": status, "sortOrder": rank,
		"startDate": "", "isAllDay": false, "timeZone": "", "completedTime": completed,
	}
}

func stage1CRemoteTask(taskID string, items ...map[string]any) map[string]any {
	values := make([]any, len(items))
	for i := range items {
		values[i] = items[i]
	}
	return map[string]any{
		"id": taskID, "projectId": "p1", "title": "checklist", "status": 0,
		"kind": "CHECKLIST", "items": values, "reminders": []any{"TRIGGER:P0DT9H0M0S"},
		"repeatFlag": "RRULE:FREQ=DAILY", "content": "keep me",
	}
}

func stage1CDecodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode request object: %v", err)
	}
	return out
}

func stage1CDecodeItems(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode request items: %v", err)
	}
	return out
}

func stage1CPostRequests(fake *stage1CWireFake) []stage1CWireRequest {
	var out []stage1CWireRequest
	for _, request := range fake.Requests() {
		if request.Method == http.MethodPost {
			out = append(out, request)
		}
	}
	return out
}

func stage1CIdentity(t *testing.T, st *store.Store, taskID, key string) (string, store.ItemIdentityState) {
	t.Helper()
	var serverID, state string
	err := st.DB().QueryRowContext(context.Background(), `
		SELECT ifnull(server_id, ''), state FROM item_identities
		WHERE task_id = ? AND item_key = ?`, taskID, key).Scan(&serverID, &state)
	if err != nil {
		t.Fatalf("read item identity %s/%s: %v", taskID, key, err)
	}
	return serverID, store.ItemIdentityState(state)
}

func stage1CLastUndo(t *testing.T, st *store.Store) store.UndoEntry {
	t.Helper()
	entry, err := st.LastUndo(context.Background())
	if err != nil {
		t.Fatalf("read undo: %v", err)
	}
	return entry
}

func stage1CPush(t *testing.T, st *store.Store, fake *stage1CWireFake) Result {
	t.Helper()
	result, err := testSyncer(t, st, fake.Server()).Push(context.Background())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return result
}

func stage1CWritableItemsEqual(a, b []model.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		left.Id, right.Id = "", ""
		if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

func TestStage1CWireCreateCarriesPrivateKeysOnlyToBinding(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")

	completed := model.NewTime(time.Date(2026, 9, 9, 8, 7, 6, 0, time.UTC))
	created, err := st.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "pack", Kind: "CHECKLIST",
		Items: []model.Item{
			{Title: "open", Status: model.ItemOpen},
			{Title: "done", Status: model.ItemDone, CompletedTime: completed, IsAllDay: true, TimeZone: "UTC"},
		},
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	keys := []string{created.Items[0].Key, created.Items[1].Key}

	result := stage1CPush(t, st, fake)
	if result.Pushed != 1 || result.Failed != 0 || result.Requeued != 0 {
		t.Fatalf("push result = %+v, want one confirmed create", result)
	}
	requests := fake.Requests()
	if len(requests) != 2 || requests[0].Method != http.MethodPost || requests[0].Path != "/open/v1/task" ||
		requests[1].Method != http.MethodGet || requests[1].Path != "/open/v1/project/p1/task/remote-task-1" {
		t.Fatalf("requests = %+v, want one create and its addressed confirmation", requests)
	}
	if bytes.Contains(requests[0].Body, []byte(`"_tt"`)) || bytes.Contains(requests[0].Body, []byte(`"key"`)) {
		t.Fatalf("private feature metadata leaked onto the wire: %s", requests[0].Body)
	}
	root := stage1CDecodeObject(t, requests[0].Body)
	items := stage1CDecodeItems(t, root["items"])
	writable := []string{"title", "status", "sortOrder", "startDate", "isAllDay", "timeZone", "completedTime"}
	for i, item := range items {
		for _, field := range writable {
			if _, ok := item[field]; !ok {
				t.Errorf("item %d omitted writable field %s: %s", i+1, field, requests[0].Body)
			}
		}
		if _, ok := item["id"]; ok {
			t.Errorf("new item %d sent an id", i+1)
		}
	}
	if string(items[0]["status"]) != "0" || string(items[0]["sortOrder"]) != "1" ||
		string(items[0]["startDate"]) != `""` || string(items[0]["isAllDay"]) != "false" ||
		string(items[0]["timeZone"]) != `""` || string(items[0]["completedTime"]) != `""` {
		t.Errorf("open item zero values were not explicit: %s", requests[0].Body)
	}

	got, err := st.Task(ctx, "remote-task-1")
	if err != nil {
		t.Fatalf("read confirmed task: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("confirmed items = %+v", got.Items)
	}
	for i := range got.Items {
		if got.Items[i].Key != keys[i] {
			t.Errorf("item %d key = %q, want %q", i+1, got.Items[i].Key, keys[i])
		}
		if got.Items[i].Id == "" || store.IsLocalID(got.Items[i].Id) {
			t.Errorf("item %d server id = %q", i+1, got.Items[i].Id)
		}
		serverID, state := stage1CIdentity(t, st, got.Id, keys[i])
		if serverID != got.Items[i].Id || state != store.ItemBound {
			t.Errorf("item %d binding = (%q,%s), want (%q,bound)", i+1, serverID, state, got.Items[i].Id)
		}
	}
	if got.Items[0].Id == got.Items[1].Id {
		t.Error("two ID-less occurrences received the same remote id")
	}
}

func TestStage1CWireUpdateRemovalAndUndoFreshIncarnation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	seeded := stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
		stage1CItem("first", "old-1", 0, 10, ""),
		stage1CItem("second", "old-2", 0, 20, "")))
	firstKey, removedKey := seeded.Items[0].Key, seeded.Items[1].Key

	if _, err := st.SetTaskItemDone(ctx, "t1", 2, true); err != nil {
		t.Fatalf("complete item: %v", err)
	}
	stage1CVersionOneFixture(t, st, "t1")
	if result := stage1CPush(t, st, fake); result.Pushed != 1 {
		t.Fatalf("complete push = %+v", result)
	}
	if _, err := st.ApplyUndo(ctx, stage1CLastUndo(t, st)); err != nil {
		t.Fatalf("undo item completion: %v", err)
	}
	stage1CVersionOneFixture(t, st, "t1")
	if result := stage1CPush(t, st, fake); result.Pushed != 1 {
		t.Fatalf("completion undo push = %+v", result)
	}
	posts := stage1CPostRequests(fake)
	undoItems := stage1CDecodeItems(t, stage1CDecodeObject(t, posts[len(posts)-1].Body)["items"])
	if string(undoItems[1]["status"]) != "0" || string(undoItems[1]["completedTime"]) != `""` {
		t.Fatalf("undone completion did not send explicit clear: %s", posts[len(posts)-1].Body)
	}

	if _, err := st.RemoveTaskItem(ctx, "t1", 2); err != nil {
		t.Fatalf("remove item: %v", err)
	}
	stage1CVersionOneFixture(t, st, "t1")
	if result := stage1CPush(t, st, fake); result.Pushed != 1 {
		t.Fatalf("removal push = %+v", result)
	}
	if serverID, state := stage1CIdentity(t, st, "t1", removedKey); serverID != "" || state != store.ItemUnbound {
		t.Fatalf("removed binding = (%q,%s), want (empty,unbound)", serverID, state)
	}
	if _, err := st.ApplyUndo(ctx, stage1CLastUndo(t, st)); err != nil {
		t.Fatalf("undo item removal: %v", err)
	}
	stage1CVersionOneFixture(t, st, "t1")
	restored, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("reload removal undo: %v", err)
	}
	if len(restored.Items) != 2 || restored.Items[0].Key != firstKey {
		t.Fatalf("restored items = %+v", restored.Items)
	}
	freshKey := restored.Items[1].Key
	if freshKey != removedKey || restored.Items[1].Id != "" {
		t.Fatalf("restored incarnation = %+v, want stable key %q and no old server id", restored.Items[1], removedKey)
	}
	if result := stage1CPush(t, st, fake); result.Pushed != 1 {
		t.Fatalf("removal undo push = %+v", result)
	}
	got, err := st.Task(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Key != firstKey || got.Items[0].Id != "old-1" || got.Items[1].Key != freshKey ||
		got.Items[1].Id == "" || got.Items[1].Id == "old-2" {
		t.Fatalf("confirmed dynamic bindings = %+v", got.Items)
	}
	if serverID, state := stage1CIdentity(t, st, "t1", freshKey); serverID != got.Items[1].Id || state != store.ItemBound {
		t.Errorf("fresh binding = (%q,%s), want (%q,bound)", serverID, state, got.Items[1].Id)
	}
}

func TestStage1CWireExplicitClearsAndTouchedFieldMasks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
		stage1CItem("only", "old-1", 0, 1, "")))
	empty := ""
	emptyStrings := []string{}
	emptyItems := []model.Item{}
	kind := "TEXT"
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{
		RepeatFlag: &empty, Reminders: model.NewEditList(emptyStrings),
		Items: model.NewEditList(emptyItems), Kind: &kind,
	}); err != nil {
		t.Fatalf("queue explicit clears: %v", err)
	}
	result := stage1CPush(t, st, fake)
	if result.Pushed != 1 {
		t.Fatalf("clear push = %+v", result)
	}
	posts := stage1CPostRequests(fake)
	if len(posts) != 1 {
		t.Fatalf("post count = %d, want 1", len(posts))
	}
	root := stage1CDecodeObject(t, posts[0].Body)
	for name, want := range map[string]string{
		"repeatFlag": `""`, "reminders": "[]", "items": "[]", "kind": `"TEXT"`,
	} {
		if got, ok := root[name]; !ok || string(got) != want {
			t.Errorf("%s = %s (present %t), want %s; body %s", name, got, ok, want, posts[0].Body)
		}
	}
	for _, name := range []string{"title", "content", "priority", "status", "dueDate", "startDate", "completedTime", "isAllDay", "timeZone", "tags", "sortOrder"} {
		if _, ok := root[name]; ok {
			t.Errorf("untouched field %s leaked into partial update: %s", name, posts[0].Body)
		}
	}
	remote, ok := fake.Task("p1", "t1")
	if !ok || remote["content"] != "keep me" || stringValue(remote["repeatFlag"]) != "" {
		t.Fatalf("partial update did not retain omitted fields and apply clear: %+v", remote)
	}
	if reminders, ok := remote["reminders"].([]any); !ok || len(reminders) != 0 {
		t.Errorf("remote reminders = %#v, want []", remote["reminders"])
	}
	if items, ok := remote["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("remote items = %#v, want []", remote["items"])
	}
}

func TestStage1CWireDuplicateTitleMatchingUsesCompleteWritableRecord(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ranks     []int64
		wantPush  int
		ambiguous bool
	}{
		{name: "distinct full records", ranks: []int64{10, 20}, wantPush: 1},
		{name: "indistinguishable new records", ranks: []int64{10, 10}, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
			fake := newStage1CWireFake(t)
			fake.SeedProject("p1", "P1")
			stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
				stage1CItem("same", "old-1", 0, tc.ranks[0], ""),
				stage1CItem("same", "old-2", 0, tc.ranks[1], "")))
			if err := st.DeleteTask(ctx, "t1"); err != nil {
				t.Fatalf("delete task: %v", err)
			}
			if result := stage1CPush(t, st, fake); result.Pushed != 1 {
				t.Fatalf("delete push = %+v", result)
			}
			restored, err := st.ApplyUndo(ctx, stage1CLastUndo(t, st))
			if err != nil {
				t.Fatalf("undo delete: %v", err)
			}
			if len(restored.Items) != 2 || restored.Items[0].Id != "" || restored.Items[1].Id != "" {
				t.Fatalf("restored items = %+v", restored.Items)
			}
			result := stage1CPush(t, st, fake)
			if result.Pushed != tc.wantPush {
				t.Fatalf("create confirmation = %+v, want pushed %d", result, tc.wantPush)
			}
			if tc.ambiguous {
				if result.Requeued+result.Failed == 0 || outboxCounts(t, st) == (store.OutboxCounts{}) {
					t.Fatalf("ambiguous create was not retained: result %+v counts %+v", result, outboxCounts(t, st))
				}
				return
			}
			got, err := st.Task(ctx, "remote-task-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Items[0].Key != restored.Items[0].Key || got.Items[1].Key != restored.Items[1].Key ||
				got.Items[0].Id == got.Items[1].Id {
				t.Fatalf("full-record matching = %+v, restored %+v", got.Items, restored.Items)
			}
		})
	}
}

func TestStage1CWireConfirmationMismatchMatrixPreservesIntentAndRetriesByReadOnlyConfirmation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any) stage1CWireResponse
	}{
		{name: "collapsed ranks", mutate: func(task map[string]any) stage1CWireResponse {
			for _, value := range task["items"].([]any) {
				value.(map[string]any)["sortOrder"] = float64(0)
			}
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "duplicate ids", mutate: func(task map[string]any) stage1CWireResponse {
			items := task["items"].([]any)
			items[1].(map[string]any)["id"] = items[0].(map[string]any)["id"]
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "missing record", mutate: func(task map[string]any) stage1CWireResponse {
			task["items"] = task["items"].([]any)[:1]
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "reordered records", mutate: func(task map[string]any) stage1CWireResponse {
			items := task["items"].([]any)
			items[0], items[1] = items[1], items[0]
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "extra record", mutate: func(task map[string]any) stage1CWireResponse {
			task["items"] = append(task["items"].([]any), stage1CItem("extra", "extra-id", 0, 30, ""))
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "wrong task", mutate: func(task map[string]any) stage1CWireResponse {
			task["id"] = "other-task"
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "wrong project", mutate: func(task map[string]any) stage1CWireResponse {
			task["projectId"] = "p2"
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "ignored item replacement", mutate: func(task map[string]any) stage1CWireResponse {
			task["items"] = []any{stage1CItem("old", "old-1", 0, 10, "")}
			return stage1CWireJSONResponse(http.StatusOK, task)
		}},
		{name: "malformed 2xx", mutate: func(map[string]any) stage1CWireResponse {
			return stage1CWireRawResponse(http.StatusOK, []byte(`{"items":`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
			fake := newStage1CWireFake(t)
			fake.SeedProject("p1", "P1")
			stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
				stage1CItem("old", "old-1", 0, 10, "")))
			desired, err := st.AddTaskItem(ctx, "t1", "new")
			if err != nil {
				t.Fatalf("add item: %v", err)
			}
			confirmPath := "/open/v1/project/p1/task/t1"
			fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
				if request.Method != http.MethodGet || request.Path != confirmPath {
					return response
				}
				remote, ok := fake.Task("p1", "t1")
				if !ok {
					t.Fatal("default update did not retain the remote task")
				}
				return tc.mutate(remote)
			})
			first := stage1CPush(t, st, fake)
			if first.Pushed != 0 || first.Requeued+first.Failed == 0 {
				t.Fatalf("mismatch result = %+v, want retained unpushed intent", first)
			}
			got, err := st.Task(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if !stage1CWritableItemsEqual(got.Items, desired.Items) {
				t.Fatalf("mismatch changed desired row: got %+v want %+v", got.Items, desired.Items)
			}
			if counts := outboxCounts(t, st); counts.Pending+counts.Inflight+counts.Failed == 0 {
				t.Fatalf("mismatch dropped intent: %+v", counts)
			}
			if fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 || fake.Count(http.MethodGet, confirmPath) != 1 {
				t.Fatalf("first requests = %+v", fake.Requests())
			}

			fake.SetResponseHook(nil)
			raised, err := st.RetryFailed(ctx)
			if err != nil || raised != 1 {
				t.Fatalf("raise mismatching operation = %d, %v; want 1", raised, err)
			}
			second := stage1CPush(t, st, fake)
			if second.Pushed != 1 || second.Failed != 0 {
				t.Fatalf("confirmation retry result = %+v", second)
			}
			if fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 {
				t.Fatalf("allocating update was repeated: %+v", fake.Requests())
			}
			if fake.Count(http.MethodGet, confirmPath) != 2 {
				t.Fatalf("confirmation GET count = %d, want 2", fake.Count(http.MethodGet, confirmPath))
			}
		})
	}
}

func TestStage1CWireIgnoredScalarAndListClearsRemainUnpushed(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	fake := newStage1CWireFake(t)
	fake.SeedProject("p1", "P1")
	stage1CSeedRemoteTask(t, st, fake, stage1CRemoteTask("t1",
		stage1CItem("old", "old-1", 0, 10, "")))
	empty := ""
	if _, err := st.UpdateTask(ctx, "t1", model.TaskEdit{
		RepeatFlag: &empty, Reminders: model.NewEditList([]string{}),
	}); err != nil {
		t.Fatal(err)
	}
	confirmPath := "/open/v1/project/p1/task/t1"
	fake.SetResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
		if request.Method == http.MethodGet && request.Path == confirmPath {
			remote, _ := fake.Task("p1", "t1")
			remote["repeatFlag"] = "RRULE:FREQ=DAILY"
			remote["reminders"] = []any{"TRIGGER:P0DT9H0M0S"}
			return stage1CWireJSONResponse(http.StatusOK, remote)
		}
		return response
	})
	first := stage1CPush(t, st, fake)
	if first.Pushed != 0 || first.Requeued+first.Failed == 0 {
		t.Fatalf("ignored clear result = %+v", first)
	}
	fake.SetResponseHook(nil)
	raised, err := st.RetryFailed(ctx)
	if err != nil || raised != 1 {
		t.Fatalf("raise ignored clear = %d, %v; want 1", raised, err)
	}
	second := stage1CPush(t, st, fake)
	if second.Pushed != 1 || fake.Count(http.MethodPost, "/open/v1/task/t1") != 1 || fake.Count(http.MethodGet, confirmPath) != 2 {
		t.Fatalf("clear confirmation retry = %+v, requests %+v", second, fake.Requests())
	}
}
