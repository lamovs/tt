package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
)

type fakeQueries struct {
	calls int
	tasks []model.Task
	err   error
}

func (q *fakeQueries) Projects(context.Context) ([]model.Project, error) {
	q.calls++
	return []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "Home"}}, q.err
}
func (q *fakeQueries) Tasks(_ context.Context, query app.BrowseQuery, _ time.Time) ([]model.Task, error) {
	q.calls++
	var tasks []model.Task
	for _, task := range q.tasks {
		if query.View == app.ProjectView && task.ProjectId != query.ProjectID {
			continue
		}
		if query.Search != "" && !strings.Contains(task.Title+task.Content, query.Search) {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, q.err
}
func (q *fakeQueries) Task(_ context.Context, id string) (model.Task, error) {
	q.calls++
	for _, task := range q.tasks {
		if task.Id == id {
			return task, q.err
		}
	}
	return model.Task{}, errors.New("task disappeared")
}
func step(m browserModel, msg tea.Msg) (browserModel, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(browserModel), cmd
}
func press(m browserModel, code rune, mod tea.KeyMod) (browserModel, tea.Cmd) {
	return step(m, tea.KeyPressMsg{Code: code, Mod: mod})
}

func settle(t *testing.T, m browserModel, cmd tea.Cmd) browserModel {
	t.Helper()
	msg := cmd()
	next, follow := step(m, msg)
	if _, ok := msg.(snapshotLoaded); ok && next.taskID != "" && next.err == nil {
		batch, ok := follow().(tea.BatchMsg)
		if !ok || len(batch) != 2 {
			t.Fatal("snapshot did not schedule detail and refresh")
		}
		next = settle(t, next, batch[0])
	}
	if _, ok := msg.(detailLoaded); ok && follow != nil {
		next = settle(t, next, follow)
	}
	return next
}
func loadedModel(t *testing.T) (browserModel, *fakeQueries) {
	t.Helper()
	q := &fakeQueries{tasks: []model.Task{
		{Id: "work-a", ProjectId: "work", Title: "Same", Content: "First body", Kind: "TEXT"},
		{Id: "work-b", ProjectId: "work", Title: "Same", Content: "Second body", Kind: "NOTE"},
		{Id: "home-a", ProjectId: "home", Title: "Home", Content: "Home body"},
	}}
	m := newModel(context.Background(), q, Options{})
	t.Cleanup(m.cancelLoad)
	if !strings.Contains(m.View().Content, "Loading cache") {
		t.Fatal("missing loading state")
	}
	m = settle(t, m, m.Init())
	return m, q
}

func TestBrowseByIDAndRenderWithoutIO(t *testing.T) {
	m, q := loadedModel(t)
	if m.query.View != app.TodayView || m.taskID != "work-a" {
		t.Fatal("wrong initial view")
	}
	m, _ = press(m, tea.KeyEnter, 0)
	m, cmd := press(m, 'j', 0)
	m = settle(t, m, cmd)
	if m.taskID != "work-b" || m.detail.Content != "Second body" {
		t.Fatal("duplicate titles defeated identity")
	}
	calls := q.calls
	for range 100 {
		m.View()
	}
	if q.calls != calls {
		t.Fatal("View performed IO")
	}
	for _, k := range []rune{'a', 'e', ' ', 'u', 's', 'd', '1', '2', '3'} {
		m, cmd = press(m, k, 0)
		if cmd != nil || m.taskID != "work-b" {
			t.Fatalf("unsupported key %q changed data", k)
		}
	}
	m, cmd = m.changeQuery(app.BrowseQuery{View: app.ProjectView, ProjectID: "home"})
	if len(m.tasks) != 0 || m.detail != nil {
		t.Fatal("old view remains selectable")
	}
	m = settle(t, m, cmd)
	if m.taskID != "home-a" {
		t.Fatal("project query failed")
	}
	m, cmd = m.changeQuery(app.BrowseQuery{View: app.TodayView})
	m = settle(t, m, cmd)
	if m.taskID != "work-b" {
		t.Fatal("returning to a view lost its selected ID")
	}
}

func TestLateSnapshotAndDetailCannotReplaceCurrentSelection(t *testing.T) {
	m, _ := loadedModel(t)
	m, old := m.changeQuery(app.BrowseQuery{View: app.OpenView, Search: "First"})
	oldResult := old().(snapshotLoaded)
	m, latest := m.changeQuery(app.BrowseQuery{View: app.OpenView, Search: "Second"})
	m = settle(t, m, latest)
	m, _ = step(m, oldResult)
	oldResult.err = errors.New("obsolete failure")
	m, _ = step(m, oldResult)
	if m.taskID != "work-b" || m.err != nil {
		t.Fatal("stale search overwrote current selection")
	}
	m, cmd := m.changeQuery(app.BrowseQuery{View: app.OpenView})
	m = settle(t, m, cmd)
	m.focus = tasksPane
	m, cmd = press(m, 'j', 0)
	oldDetail := cmd().(detailLoaded)
	m, cmd = press(m, 'k', 0)
	m = settle(t, m, cmd)
	m, _ = step(m, oldDetail)
	if m.taskID != "work-a" || m.detail.Id != "work-a" {
		t.Fatal("old detail replaced another task")
	}

	m, cmd = m.loadDetail()
	oldDetail = cmd().(detailLoaded)
	m, cmd = m.load()
	m = settle(t, m, cmd)
	oldDetail.task.Content = "STALE BODY"
	m, _ = step(m, oldDetail)
	if m.detail.Content == "STALE BODY" {
		t.Fatal("same-ID stale detail was accepted")
	}
}

func TestExternalRefreshRetainsIDAndClearsDisappearedSelection(t *testing.T) {
	m, q := loadedModel(t)
	m.focus = tasksPane
	m, cmd := press(m, 'j', 0)
	m = settle(t, m, cmd)
	q.tasks[1].Content = "Updated outside UI"
	slices.Reverse(q.tasks)
	m, cmd = step(m, refreshDue{m.generation})
	m = settle(t, m, cmd)
	if m.taskID != "work-b" || m.detail.Content != "Updated outside UI" {
		t.Fatal("refresh lost identity or body")
	}
	q.tasks = slices.DeleteFunc(q.tasks, func(t model.Task) bool { return t.Id == "work-b" })
	m, cmd = step(m, refreshDue{m.generation})
	m = settle(t, m, cmd)
	if m.taskID != "" || m.detail != nil || !m.selectionLost {
		t.Fatal("disappeared task selected a neighbor")
	}
	m, cmd = step(m, refreshDue{m.generation})
	m = settle(t, m, cmd)
	if m.taskID != "" {
		t.Fatal("next refresh silently selected a neighbor")
	}
	m, cmd = press(m, 'j', 0)
	m = settle(t, m, cmd)
	if m.taskID == "" || m.selectionLost {
		t.Fatal("explicit selection could not resume")
	}
}

func TestRefreshDoesNotOverlapSlowLoadsAndRetainsNewUserSelection(t *testing.T) {
	m, _ := loadedModel(t)
	oldGeneration := m.generation
	m, slow := m.load()
	for _, generation := range []uint64{oldGeneration, m.generation} {
		var cmd tea.Cmd
		m, cmd = step(m, refreshDue{generation})
		if cmd != nil {
			t.Fatal("timer duplicated a running query")
		}
	}
	m.focus = tasksPane
	m, detail := press(m, 'j', 0)
	m = settle(t, m, detail)
	m = settle(t, m, slow)
	if m.taskID != "work-b" {
		t.Fatal("refresh used the selection captured before navigation")
	}
}

func TestSearchInputOwnsShortcutsAndCancelRestoresQuery(t *testing.T) {
	m, _ := loadedModel(t)
	m, _ = press(m, '/', 0)
	for _, r := range "q+_?/jkl123" {
		m, _ = press(m, r, 0)
	}
	if !m.searching || m.help.ShowAll || m.mode != normalMode || m.taskID != "work-a" {
		t.Fatal("typing triggered navigation")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	if m.query.Search != "" || m.searching {
		t.Fatal("cancel changed search")
	}
	m, _ = press(m, '/', 0)
	m, _ = step(m, tea.PasteMsg{Content: "Second"})
	m, cmd := press(m, tea.KeyEnter, 0)
	m = settle(t, m, cmd)
	if m.query.Search != "Second" || m.taskID != "work-b" {
		t.Fatal("search was not applied")
	}
	m, _ = press(m, '/', 0)
	m, _ = press(m, 'u', tea.ModCtrl)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = settle(t, m, cmd)
	if m.query.Search != "" || len(m.tasks) != 3 {
		t.Fatal("empty query did not clear search")
	}
}

func TestPanelJumpsAndScreenModesRetainIdentity(t *testing.T) {
	m, q := loadedModel(t)
	calls := q.calls
	for _, size := range [][2]int{{140, 32}, {90, 24}, {40, 12}} {
		m, _ = step(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, mode := range []screenMode{normalMode, halfMode, fullMode} {
			m.mode = mode
			for _, digit := range []rune{'3', '1', '2'} {
				m, _ = press(m, digit, 0)
				if m.focus != pane(digit-'1') || m.taskID != "work-a" {
					t.Fatal("direct panel selection changed task identity")
				}
				if _, ok := m.layout()[m.focus]; !ok {
					t.Fatal("directly focused panel is hidden")
				}
			}
			for _, digit := range []rune{'3', '1', '2'} {
				m, _ = press(m, 'o', tea.ModCtrl)
				for _, hint := range []string{"[1] Lists", "[2] Tasks", "[3] Preview"} {
					if !strings.Contains(m.View().Content, hint) {
						t.Fatalf("hidden jump target %s", hint)
					}
				}
				var cmd tea.Cmd
				m, cmd = press(m, 'j', 0)
				if cmd != nil || !m.jump {
					t.Fatal("jump mode allowed navigation")
				}
				m, _ = press(m, digit, 0)
				if m.focus != pane(digit-'1') || m.jump || m.taskID != "work-a" {
					t.Fatal("jump changed task identity")
				}
				if _, ok := m.layout()[m.focus]; !ok {
					t.Fatal("focused panel is hidden")
				}
			}
		}
	}
	m.mode = normalMode
	for _, want := range []screenMode{halfMode, fullMode, normalMode} {
		m, _ = press(m, '+', 0)
		if m.mode != want {
			t.Fatal("plus did not cycle forward")
		}
	}
	for _, want := range []screenMode{fullMode, halfMode, normalMode} {
		m, _ = press(m, '_', 0)
		if m.mode != want {
			t.Fatal("underscore did not cycle backward")
		}
	}
	m, _ = press(m, 'o', tea.ModCtrl)
	m, _ = press(m, tea.KeyEscape, 0)
	if m.jump {
		t.Fatal("Esc did not cancel jump")
	}
	if q.calls != calls {
		t.Fatal("layout or jumps performed queries")
	}
}

func TestCompleteDetailsScrollAndRefreshWithoutReset(t *testing.T) {
	m, q := loadedModel(t)
	q.tasks[0].Content = "  preserved spaces  \n\n" + strings.Repeat("Long paragraph line\n", 100) + "BODY_END"
	q.tasks[0].Items = []model.Item{{Key: "a", Title: "First checklist item"}, {Key: "b", Title: "Last checklist item", Status: model.ItemDone}}
	q.tasks[0].RepeatFlag = "RRULE:FREQ=DAILY"
	q.tasks[0].Reminders = []string{"TRIGGER:PT0S", "", "TRIGGER:-PT10M"}
	m.focus, m.mode = previewPane, fullMode
	m, cmd := m.load()
	m = settle(t, m, cmd)
	all := strings.Join(m.detailLines, "\n")
	for _, want := range []string{"  preserved spaces  \n\n", "Last checklist item", "RRULE:FREQ=DAILY", "Reminder 2: (empty)", "BODY_END"} {
		if !strings.Contains(all, want) {
			t.Fatalf("detail lost %q", want)
		}
	}
	m, _ = press(m, tea.KeyEnd, 0)
	if !strings.Contains(m.View().Content, "BODY_END") {
		t.Fatal("end of body is unreachable")
	}
	m, _ = press(m, tea.KeyHome, 0)
	m, _ = press(m, tea.KeyPgDown, 0)
	offset := m.detailOffset
	m, cmd = m.load()
	m = settle(t, m, cmd)
	if m.detailOffset != offset {
		t.Fatal("refresh reset scroll")
	}
	for range 3 {
		m, _ = press(m, '+', 0)
	}
	if m.detailOffset != offset || m.taskID != "work-a" {
		t.Fatal("size cycle reset view")
	}
}

func TestContextHelpAndExitWhileLoading(t *testing.T) {
	m, _ := loadedModel(t)
	for _, p := range []pane{listsPane, tasksPane, previewPane} {
		m.focus = p
		global, local := m.footers()
		if !strings.Contains(global, "Global:") || !strings.HasPrefix(local, paneName(p)) {
			t.Fatal("missing contextual footer")
		}
		m, _ = press(m, '?', 0)
		if !strings.Contains(strings.Join(m.helpLines(), "\n"), "Local keys - "+paneName(p)) {
			t.Fatal("wrong local help")
		}
		m, _ = press(m, 'o', tea.ModCtrl)
		if !m.jump || m.help.ShowAll {
			t.Fatal("Ctrl+O from help lost baseline behavior")
		}
		m, _ = press(m, tea.KeyEscape, 0)
	}
	for _, input := range []struct {
		code        rune
		mod         tea.KeyMod
		interrupted bool
	}{{'q', 0, false}, {tea.KeyEscape, 0, false}, {'c', tea.ModCtrl, true}} {
		m := newModel(context.Background(), &fakeQueries{}, Options{})
		next, cmd := press(m, input.code, input.mod)
		if cmd == nil || next.interrupted != input.interrupted {
			t.Fatal("loading trapped exit")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("not a quit command")
		}
		m.cancelLoad()
	}
}

func TestEmptyErrorsAndRecovery(t *testing.T) {
	m, q := loadedModel(t)
	q.tasks = nil
	m, cmd := m.load()
	m = settle(t, m, cmd)
	if !strings.Contains(m.View().Content, "No matching tasks") {
		t.Fatal("empty result is missing")
	}
	q.err = errors.New("broken\x1b]52;c;secret\a")
	m, cmd = m.load()
	m = settle(t, m, cmd)
	view := m.View().Content
	if !strings.Contains(view, "Cannot read tasks") || strings.ContainsAny(view, "\x1b\a") {
		t.Fatal("hidden or unsafe query error")
	}
	q.err = nil
	m, cmd = m.load()
	m = settle(t, m, cmd)
	if m.err != nil {
		t.Fatal("successful retry retained the error")
	}
	m = newModel(context.Background(), nil, Options{Err: errors.New("bad config")})
	if m.Init() != nil || !strings.Contains(m.View().Content, "Cannot read the cache") {
		t.Fatal("bad startup error handling")
	}
	m.cancelLoad()
}

func TestResizeAndHostileUnicodeDataAreBounded(t *testing.T) {
	m, q := loadedModel(t)
	q.tasks[0].Title = "\x1b[2J\rFake\u202e target \u754ce\u0301 \U0001f469\u200d\U0001f4bb"
	q.tasks[0].Content = strings.Repeat("\u754c e\u0301 \U0001f680 large \x1b]52;c;foo\a\n", 100)
	m, cmd := m.load()
	m = settle(t, m, cmd)
	calls := q.calls
	for _, size := range [][2]int{{140, 32}, {120, 24}, {90, 24}, {80, 24}, {79, 20}, {40, 10}, {32, 10}, {20, 8}, {1, 1}, {0, 0}} {
		for _, mode := range []screenMode{normalMode, halfMode, fullMode} {
			m.mode, m.focus = mode, previewPane
			m, _ = step(m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			if !view.AltScreen || strings.ContainsAny(view.Content, "\x1b\a\r\t\u202e") {
				t.Fatalf("unsafe display at %v", size)
			}
			if view.Content != "" && len(strings.Split(view.Content, "\n")) > size[1] {
				t.Fatal("height overflow")
			}
			for _, line := range strings.Split(view.Content, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("width overflow at %v: %q", size, line)
				}
			}
		}
	}
	if q.calls != calls || !strings.Contains(m.detail.Title, "\x1b") {
		t.Fatal("rendering changed data or performed IO")
	}
}

func TestSearchEditingAndSafePaste(t *testing.T) {
	var s searchInput
	s.insert("\u041f\u0440\u0438\u0432\u0435\u0442")
	s.update(tea.KeyPressMsg{Code: tea.KeyLeft})
	s.update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	s.insert("\x1b]52;c;secret\a\nq+_")
	if strings.ContainsAny(s.view(20), "\x1b\a\n") {
		t.Fatal("paste injected a control")
	}
	if ansi.StringWidth(s.view(20)) != 20 {
		t.Fatal("input viewport overflow")
	}
	s.update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if len(s.value) != 0 {
		t.Fatal("input clear failed")
	}
	for _, n := range []int{1, 20, 200} {
		s.set(fmt.Sprint(n))
		if s.pos != len(s.value) {
			t.Fatal("cursor not at end")
		}
	}
}

func TestHelpScrollMovesImmediatelyBackFromEnd(t *testing.T) {
	m, _ := loadedModel(t)
	m, _ = step(m, tea.WindowSizeMsg{Width: 40, Height: 12})
	m, _ = press(m, '?', 0)
	m, _ = press(m, tea.KeyEnd, 0)
	last := m.View().Content
	m, _ = press(m, 'k', 0)
	if m.View().Content == last {
		t.Fatal("help got stuck at the bottom after End")
	}
}

func TestDetailFailureDoesNotExposePreviousTask(t *testing.T) {
	m, q := loadedModel(t)
	m.focus, m.mode = tasksPane, fullMode
	q.err = errors.New("detail failed")
	m, cmd := press(m, 'j', 0)
	m = settle(t, m, cmd)
	m.focus = previewPane
	view := m.View().Content
	if !strings.Contains(view, "Cannot read task") || strings.Contains(view, "First body") {
		t.Fatal("failed detail showed the previous task")
	}
}
