package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	stdsync "sync"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestChecklistProviderWireCreateAndCompleteKeepIdentity(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedProject(t, st, model.Project{Id: "p1", Name: "P1"})
	var confirmationBody []byte
	var confirmationMu stdsync.Mutex
	readConfirmation := func() []byte {
		confirmationMu.Lock()
		defer confirmationMu.Unlock()
		return append([]byte(nil), confirmationBody...)
	}
	fake := newStage1CWireFake(t, stage1CWireWithResponseHook(func(request stage1CWireRequest, response stage1CWireResponse) stage1CWireResponse {
		var task map[string]any
		if err := json.Unmarshal(response.Body, &task); err != nil {
			return response
		}
		items, ok := task["items"].([]any)
		if !ok {
			return response
		}
		for _, value := range items {
			item := value.(map[string]any)
			if item["timeZone"] == "" {
				item["timeZone"] = task["timeZone"]
			}
			if item["startDate"] == "" {
				delete(item, "startDate")
			}
			if item["completedTime"] == "" {
				delete(item, "completedTime")
			} else if text, ok := item["completedTime"].(string); ok {
				stamp, err := model.ParseTime(text)
				if err != nil {
					t.Errorf("decode simulated provider completion: %v", err)
					return response
				}
				item["completedTime"] = stamp.UnixMilli()
			}
		}
		response = stage1CWireJSONResponse(response.Status, task)
		if request.Method == http.MethodGet {
			confirmationMu.Lock()
			confirmationBody = append([]byte(nil), response.Body...)
			confirmationMu.Unlock()
		}
		return response
	}))
	fake.SeedProject("p1", "P1")
	created, err := st.CreateTask(ctx, model.Task{
		ProjectId: "p1", Title: "provider checklist", Kind: "CHECKLIST", TimeZone: "Europe/Moscow",
		Items: []model.Item{{Title: "same", Status: model.ItemOpen}, {Title: "same", Status: model.ItemOpen}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := stage1CPush(t, st, fake); result.Pushed != 1 || result.Failed != 0 {
		t.Fatalf("create confirmation = %+v", result)
	}
	requests := fake.Requests()
	if len(requests) != 2 || requests[0].Method != http.MethodPost || requests[0].Path != "/open/v1/task" || requests[1].Method != http.MethodGet || requests[1].Path != "/open/v1/project/p1/task/remote-task-1" {
		t.Fatalf("create requests = %+v", requests)
	}
	requestItems := stage1CDecodeItems(t, stage1CDecodeObject(t, requests[0].Body)["items"])
	if len(requestItems) != 2 || string(requestItems[0]["sortOrder"]) != "1" || string(requestItems[1]["sortOrder"]) != "2" {
		t.Fatalf("same-title occurrence ranks = %s", requests[0].Body)
	}
	for _, item := range requestItems {
		for _, field := range []string{"startDate", "completedTime", "timeZone"} {
			if string(item[field]) != `""` {
				t.Errorf("request field %s was changed: %s", field, requests[0].Body)
			}
		}
	}
	got, err := st.Task(ctx, "remote-task-1")
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("created task = %+v, %v", got, err)
	}
	ids := []string{got.Items[0].Id, got.Items[1].Id}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("duplicate-title identity mapping = %v", ids)
	}
	for i := range got.Items {
		if got.Items[i].Key != created.Items[i].Key || got.Items[i].TimeZone != "" {
			t.Fatalf("local frozen item changed: %+v", got.Items[i])
		}
		id, state := stage1CIdentity(t, st, got.Id, created.Items[i].Key)
		if id != ids[i] || state != store.ItemBound {
			t.Fatalf("binding %d = %q %s", i, id, state)
		}
	}
	assertRaw := func() {
		t.Helper()
		var raw []byte
		if err := st.DB().QueryRowContext(ctx, `SELECT raw FROM tasks WHERE id = ?`, got.Id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if want := readConfirmation(); !bytes.Equal(raw, want) {
			t.Fatalf("confirmation raw was not retained exactly: got %s want %s", raw, want)
		}
	}
	assertRaw()
	if _, err := st.SetTaskItemDone(ctx, got.Id, 2, true); err != nil {
		t.Fatalf("complete confirmed item: %v", err)
	}
	stage1CVersionOneFixture(t, st, got.Id)
	if result := stage1CPush(t, st, fake); result.Pushed != 1 || result.Failed != 0 {
		t.Fatalf("numeric completion confirmation = %+v", result)
	}
	assertRaw()
	completionRaw := readConfirmation()
	confirmedItems := stage1CDecodeItems(t, stage1CDecodeObject(t, completionRaw)["items"])
	if len(confirmedItems[1]["completedTime"]) == 0 || confirmedItems[1]["completedTime"][0] == '"' {
		t.Fatalf("confirmation did not exercise numeric completion: %s", completionRaw)
	}
	posts := stage1CPostRequests(fake)
	if len(posts) != 2 {
		t.Fatalf("posts = %+v, want one create and one update", posts)
	}
	updateItems := stage1CDecodeItems(t, stage1CDecodeObject(t, posts[1].Body)["items"])
	if posts[1].Path != "/open/v1/task/remote-task-1" || len(updateItems) != 2 || len(updateItems[1]["completedTime"]) == 0 || updateItems[1]["completedTime"][0] != '"' {
		t.Fatalf("completion request changed wire contract: %+v", posts)
	}
	for i, key := range []string{created.Items[0].Key, created.Items[1].Key} {
		id, state := stage1CIdentity(t, st, got.Id, key)
		if id != ids[i] || state != store.ItemBound {
			t.Fatalf("completion changed binding %d: %q %s", i, id, state)
		}
	}

	if _, err := st.RenameTaskItem(ctx, got.Id, 1, "still bound"); err != nil {
		t.Fatalf("mutate retained numeric completion: %v", err)
	}
}
