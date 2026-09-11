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
	"github.com/movsar/tt/internal/store"
)

func boardModel(t *testing.T) (browserModel, *fakeResourceActions, *fakeQueries) {
	t.Helper()
	q := &fakeQueries{tasks: []model.Task{
		{Id: "a", ProjectId: "p", Title: "First", ColumnId: "todo", SortOrder: 1},
		{Id: "b", ProjectId: "p", Title: "Second", ColumnId: "todo", SortOrder: 2},
		{Id: "c", ProjectId: "p", Title: "Done card", ColumnId: "done"},
		{Id: "u", ProjectId: "p", Title: "Missing column", ColumnId: "missing"},
	}}
	service := &fakeResourceActions{listing: app.ResourceListing{Entities: []store.ResourceEntity{
		{Ref: store.EntityRef{Kind: "column", Key: "todo"}, ServerID: "todo", ProjectKey: "p", Data: json.RawMessage(`{"id":"todo","projectId":"p","name":"Todo","sortOrder":1}`)},
		{Ref: store.EntityRef{Kind: "column", Key: "done"}, ServerID: "done", ProjectKey: "p", Data: json.RawMessage(`{"id":"done","projectId":"p","name":"Done","sortOrder":2}`)},
	}}}
	m := newModel(context.Background(), q, Options{Resources: service})
	t.Cleanup(m.cancelLoad)
	m.query = app.BrowseQuery{View: app.ProjectView, ProjectID: "p"}
	m.projects = []model.Project{{Id: "p", Name: "Project"}}
	m.tasks, m.taskID, m.loading = q.tasks, "b", false
	m, cmd := m.toggleKanban()
	m = settle(t, m, cmd)
	return m, service, q
}

func TestKanbanUsesExistingStableTaskSelectionAndKeepsUnknownColumns(t *testing.T) {
	m, service, q := boardModel(t)
	if m.board.columnKey != "column:todo" {
		t.Fatal("board lost selected task column")
	}
	groups := m.boardGroups()
	if len(groups) != 3 || groups[2].key != "unknown" || len(groups[2].tasks) != 1 || groups[2].tasks[0].Id != "u" {
		t.Fatalf("unknown column card lost: %+v", groups)
	}
	m, cmd := press(m, 'k', 0)
	m = settle(t, m, cmd)
	if m.taskID != "a" || m.detail == nil || m.detail.Id != "a" {
		t.Fatal("card navigation bypassed existing detail identity")
	}
	m, cmd = press(m, tea.KeyRight, tea.ModAlt)
	m = settle(t, m, cmd)
	if m.taskID != "c" || m.board.columnKey != "column:done" {
		t.Fatal("column navigation changed wrong task")
	}
	if service.prepares != 0 || service.applies != 0 {
		t.Fatal("navigation mutated resources")
	}
	before := q.calls
	m, cmd = press(m, tea.KeyRight, tea.ModAlt|tea.ModShift)
	if cmd != nil || q.calls != before || !strings.Contains(m.notice, "unavailable in this session") {
		t.Fatal("card move without column actions was not refused")
	}
}

func TestKanbanFitsNarrowAndWideWithoutViewIO(t *testing.T) {
	m, service, q := boardModel(t)
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m.width, m.height = size[0], size[1]
		resourceCalls, queryCalls := len(service.queries), q.calls
		view := m.View().Content
		lines := strings.Split(view, "\n")
		if len(lines) != m.height {
			t.Fatalf("height=%d want%d", len(lines), m.height)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) != m.width {
				t.Fatalf("width=%d want%d line=%q", ansi.StringWidth(line), m.width, line)
			}
		}
		if len(service.queries) != resourceCalls || q.calls != queryCalls {
			t.Fatal("board View performed I/O")
		}
		if m.width == 32 && strings.Contains(view, "Done card") {
			t.Fatal("narrow board showed hidden column")
		}
		if m.width == 160 && (!strings.Contains(view, "Done card") || !strings.Contains(view, "Missing column")) {
			t.Fatal("wide board lost columns")
		}
	}
}

func TestKanbanLateMetadataAndForeignControlsCannotRetarget(t *testing.T) {
	m, service, _ := boardModel(t)
	m, late := m.loadKanban(true)
	m, _ = m.changeQuery(app.BrowseQuery{View: app.ProjectView, ProjectID: "another"})
	m = settle(t, m, late)
	if m.board != nil {
		t.Fatal("late metadata reopened prior board")
	}
	m.query = app.BrowseQuery{View: app.ProjectView, ProjectID: "p"}
	service.listing.Entities[0].Data = json.RawMessage(`{"id":"todo","projectId":"p","name":"bad\u001b]52;c;x\u0007"}`)
	m, cmd := m.toggleKanban()
	m = settle(t, m, cmd)
	m.tasks = []model.Task{{Id: "x", ProjectId: "p", Title: "bad\x1b]52;c;x\a", ColumnId: "todo"}}
	m.taskID = "x"
	m.reconcileBoardSelection()
	view := m.View().Content
	if strings.Contains(view, "\x1b]52") || strings.ContainsRune(view, '\a') {
		t.Fatal("board emitted foreign terminal control")
	}
}

func TestWorkspacePalettePreservesLegacyTaskKeys(t *testing.T) {
	m, _, _ := boardModel(t)
	m.board = nil
	m, _ = press(m, ':', 0)
	if m.palette == nil {
		t.Fatal("palette did not open")
	}
	m, _ = press(m, 'h', 0)
	m, _ = press(m, 'a', 0)
	m, _ = press(m, 'b', 0)
	if m.palette == nil || m.board != nil {
		t.Fatal("palette text activated task shortcuts")
	}
	m, cmd := press(m, tea.KeyEnter, 0)
	m = settle(t, m, cmd)
	if m.resource == nil || m.resource.query.Kind != "habit" {
		t.Fatal("palette opened wrong workspace")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.resource != nil || m.taskID != "b" {
		t.Fatal("workspace return lost task identity")
	}
}
