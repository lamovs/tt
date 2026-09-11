package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
)

type fakeServerTasks struct {
	queries []app.ServerTaskQuery
	remote  []bool
	listing app.ServerTaskListing
}

func (f *fakeServerTasks) List(_ context.Context, q app.ServerTaskQuery, remote bool) (app.ServerTaskListing, error) {
	f.queries = append(f.queries, q)
	f.remote = append(f.remote, remote)
	return f.listing, nil
}

func serverTaskModel(t *testing.T) (browserModel, *fakeServerTasks) {
	t.Helper()
	service := &fakeServerTasks{listing: app.ServerTaskListing{Tasks: []model.Task{{Id: "server", ProjectId: "p", Title: "Server row"}}, Raw: []json.RawMessage{json.RawMessage(`{"id":"server","projectId":"p","title":"Server row"}`)}, Meta: app.ResultMeta{Source: "local", Completeness: "unknown"}}}
	m := newModel(context.Background(), &fakeQueries{}, Options{ServerTasks: service})
	t.Cleanup(m.cancelLoad)
	m.loading, m.taskID = false, "local"
	m.tasks = []model.Task{{Id: "local", Title: "Local task"}}
	m, cmd := m.openServerTasks("completed")
	if len(service.remote) != 0 {
		t.Fatal("opening performed I/O")
	}
	m = settle(t, m, cmd)
	return m, service
}

func TestServerQueryTUIIsReadOnlyLocalDefaultAndExplicitRemote(t *testing.T) {
	m, service := serverTaskModel(t)
	if service.remote[0] || m.serverTasks.key != "server" || m.taskID != "local" {
		t.Fatal("wrong initial source or task identity")
	}
	for _, key := range []rune{'a', 'e', 'd'} {
		m, _ = press(m, key, tea.ModCtrl)
		if m.form != nil || m.dialog != nil || m.taskOperation != nil {
			t.Fatal("query launched a task mutation")
		}
	}
	m, cmd := press(m, 'R', 0)
	m = settle(t, m, cmd)
	if !service.remote[len(service.remote)-1] {
		t.Fatal("remote refresh did not remain explicit")
	}
	m, _ = press(m, tea.KeyEnter, 0)
	if !m.serverTasks.detail || m.detail != nil || m.taskID != "local" {
		t.Fatal("query source reused local task detail/action identity")
	}
	if !strings.Contains(strings.Join(m.serverTaskDetailLines(), "\n"), "server") {
		t.Fatal("raw query source unavailable")
	}
	service.listing.Tasks = []model.Task{{Id: "replacement", Title: "Server row"}}
	m, _ = press(m, tea.KeyEsc, 0)
	m, cmd = press(m, 'r', 0)
	m = settle(t, m, cmd)
	if m.serverTasks.key != "" || !m.serverTasks.lost {
		t.Fatal("same title replaced lost query selection")
	}
	m, _ = press(m, 'j', 0)
	if m.serverTasks.key != "replacement" || m.taskID != "local" {
		t.Fatal("query selection changed authoritative selection")
	}
}

func TestServerQueryTUIFencesEditedQueriesAndClosedPanels(t *testing.T) {
	m, service := serverTaskModel(t)
	m, late := m.loadServerTasks(true)
	message := late()
	m, _ = press(m, 'e', 0)
	m, _ = press(m, 'q', 0)
	m, _ = step(m, message)
	if !m.serverTasks.editing || string(m.serverTasks.fields[0].input.value) != "q" {
		t.Fatal("late result replaced edited query")
	}
	m, late = m.loadServerTasks(false)
	m.closeServerTasks()
	m, _ = step(m, late())
	if m.serverTasks != nil {
		t.Fatal("late result reopened closed workspace")
	}
	m, cmd := m.openServerTasks("search")
	if cmd != nil || m.serverTasks == nil {
		t.Fatal("blank search performed request")
	}
	m, _ = press(m, 'q', 0)
	m, _ = press(m, '?', 0)
	if m.help.ShowAll || string(m.serverTasks.fields[0].input.value) != "q?" {
		t.Fatal("query text interpreted as task shortcuts")
	}
	m, cmd = press(m, 'r', tea.ModCtrl)
	m = settle(t, m, cmd)
	if !service.remote[len(service.remote)-1] || service.queries[len(service.queries)-1].Text != "q?" {
		t.Fatal("exact query was not submitted")
	}
}

func TestServerQueryTUIWidthEscapesAndNoViewIO(t *testing.T) {
	m, service := serverTaskModel(t)
	m.serverTasks.listing.Tasks[0].Title = "\x1b]52;c;bad\x07\u754c\u754c"
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m.width, m.height = size[0], size[1]
		for _, editing := range []bool{false, true} {
			m.serverTasks.editing = editing
			m.serverTasks.field = len(m.serverTasks.fields) - 1
			calls := len(service.queries)
			view := m.View().Content
			lines := strings.Split(view, "\n")
			if len(lines) != m.height {
				t.Fatalf("height %d want %d", len(lines), m.height)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) != m.width {
					t.Fatalf("width %d want %d", ansi.StringWidth(line), m.width)
				}
			}
			if strings.Contains(view, "\x1b]52") || len(service.queries) != calls {
				t.Fatal("unescaped output or View I/O")
			}
			if editing && !strings.Contains(view, "To:") {
				t.Fatal("active narrow query field is hidden", view)
			}
		}
	}
}
