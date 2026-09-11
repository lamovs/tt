package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestBrowserReadsOpenTasksWithoutChangingCacheOrCLINumbers(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceProjects(ctx, []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "Home"}}); err != nil {
		t.Fatal(err)
	}
	var tasks []model.Task
	for _, p := range []string{"work", "work", "home"} {
		task, err := st.CreateTask(ctx, model.Task{ProjectId: p, Title: "Same title", Content: "Body\nnext line"})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	if _, err := st.CompleteTask(ctx, tasks[1].Id, store.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	listing := []string{tasks[2].Id, tasks[0].Id}
	if err := st.SetListing(ctx, listing); err != nil {
		t.Fatal(err)
	}
	before := dumpCache(t, st)
	browser := NewBrowser(st)
	for range 20 {
		projects, err := browser.Projects(ctx)
		if err != nil || len(projects) != 2 {
			t.Fatalf("projects: %v, %v", projects, err)
		}
		for _, task := range []model.Task{tasks[0], tasks[2]} {
			got, err := browser.OpenTasks(ctx, task.ProjectId)
			if err != nil || len(got) != 1 || got[0].Id != task.Id || got[0].Content != task.Content {
				t.Fatalf("tasks for %s: %v, %v", task.ProjectId, got, err)
			}
		}
	}
	if got := dumpCache(t, st); got != before {
		t.Fatal("browser queries changed database rows")
	}
	got, err := st.ResolveRefs(ctx, []string{"1", "2"})
	if err != nil || !reflect.DeepEqual(got, listing) {
		t.Fatalf("CLI numbering changed: %v, %v", got, err)
	}
	if _, err := browser.OpenTasks(ctx, ""); err == nil {
		t.Fatal("empty project ID silently became an all-project query")
	}
	st.Close()
	if _, err := browser.Projects(ctx); err == nil {
		t.Fatal("closed database reported an empty success")
	}
	if _, err := browser.OpenTasks(ctx, "work"); err == nil {
		t.Fatal("closed database reported an empty task result")
	}
}

func dumpCache(t *testing.T, st *store.Store) string {
	t.Helper()
	names, err := st.DB().Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	names.Close()
	dump := map[string][]string{}
	for _, name := range tables {
		rows, err := st.DB().Query(fmt.Sprintf("SELECT * FROM %q ORDER BY rowid", name))
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range ptrs {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			dump[name] = append(dump[name], string(data))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	data, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
