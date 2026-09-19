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

// privateBrowser loads a browser wide enough to paint the task rows and the
// details panel side by side, which is where private mode has to bite.
func privateBrowser(t *testing.T, opts Options, tasks ...model.Task) (browserModel, *fakeQueries) {
	t.Helper()
	q := &fakeQueries{tasks: tasks}
	m := newModel(context.Background(), q, opts)
	t.Cleanup(m.cancelLoad)
	m, _ = step(m, tea.WindowSizeMsg{Width: 140, Height: 32})
	return settle(t, m, m.Init()), q
}

func secretTask() model.Task {
	return model.Task{Id: "work-a", ProjectId: "work", Title: "Secret title", Content: "Secret body", Kind: "TEXT"}
}

func TestPrivateModeMasksTaskRowsAndDetailsAndNothingElse(t *testing.T) {
	off, _ := privateBrowser(t, Options{}, secretTask())
	shown := off.View().Content
	if !strings.Contains(shown, "Secret title") || !strings.Contains(shown, "Secret body") {
		t.Fatalf("the default mode hid the task:\n%s", shown)
	}
	if strings.Contains(shown, hiddenTitle) || strings.Contains(shown, hiddenPreview) {
		t.Fatal("a placeholder appeared while private mode was off")
	}

	on, _ := privateBrowser(t, Options{Private: true}, secretTask())
	hidden := on.View().Content
	if strings.Contains(hidden, "Secret") {
		t.Fatalf("private mode leaked task content:\n%s", hidden)
	}
	if !strings.Contains(hidden, hiddenTitle) || !strings.Contains(hidden, hiddenPreview) {
		t.Fatalf("private mode did not say what it hid and how to show it:\n%s", hidden)
	}
	if on.detail == nil || on.detail.Title != "Secret title" {
		t.Fatal("private mode changed the stored task instead of the painting")
	}
	for _, want := range []string{"Today", "All open", "Work", "Home", "Tasks (1)"} {
		if !strings.Contains(hidden, want) {
			t.Fatalf("private mode hid the navigation label %q:\n%s", want, hidden)
		}
	}
}

func TestPrivatePlaceholderKeepsTheTitleLengthSecret(t *testing.T) {
	short := secretTask()
	short.Title, short.Content = "x", "y"
	long := secretTask()
	long.Title, long.Content = strings.Repeat("long title ", 40), strings.Repeat("long body ", 40)
	first, _ := privateBrowser(t, Options{Private: true}, short)
	second, _ := privateBrowser(t, Options{Private: true}, long)
	if first.View().Content != second.View().Content {
		t.Fatalf("the placeholder reveals how long the title is:\n%s\n\n%s",
			first.View().Content, second.View().Content)
	}
}

func TestPrivateModeTogglesBothWaysEverywhere(t *testing.T) {
	m, _ := privateBrowser(t, Options{}, secretTask())
	if m.private {
		t.Fatal("private mode is on without the flag")
	}
	m, cmd := press(m, 'k', tea.ModCtrl)
	if cmd != nil || !m.private || strings.Contains(m.View().Content, "Secret") {
		t.Fatal("Ctrl+K did not hide the task content")
	}
	m, cmd = press(m, 'k', tea.ModCtrl)
	if cmd != nil || m.private || !strings.Contains(m.View().Content, "Secret title") {
		t.Fatal("Ctrl+K did not show the task content again")
	}

	m.openEdit(*m.detail)
	if m.form == nil {
		t.Fatal("no form to type into")
	}
	draft := string(m.form.title.value)
	m, _ = press(m, 'k', tea.ModCtrl)
	if !m.private || m.form == nil || string(m.form.title.value) != draft {
		t.Fatal("Ctrl+K inside a form missed the toggle or touched the draft")
	}
	m, _ = press(m, 'k', tea.ModCtrl)
	if m.private || m.form == nil || string(m.form.title.value) != draft {
		t.Fatal("Ctrl+K inside a form did not turn private mode off")
	}
}

func TestPrivateFlagHidesTheVeryFirstFrame(t *testing.T) {
	q := &fakeQueries{tasks: []model.Task{secretTask()}}
	m := newModel(context.Background(), q, Options{Private: true})
	t.Cleanup(m.cancelLoad)
	m, _ = step(m, tea.WindowSizeMsg{Width: 140, Height: 32})
	frames := []string{m.View().Content}
	m = recordFrames(t, m, m.Init(), &frames)
	for i, frame := range frames {
		if strings.Contains(frame, "Secret") {
			t.Fatalf("frame %d of %d painted task content:\n%s", i+1, len(frames), frame)
		}
	}
	if len(frames) < 4 || m.detail == nil || len(m.detailLines) == 0 {
		t.Fatalf("the run never reached a frame that had content to paint: %d frames", len(frames))
	}
	if !strings.Contains(frames[len(frames)-1], hiddenTitle) {
		t.Fatal("the loaded frame shows neither the title nor the placeholder")
	}
}

// recordFrames replays the load sequence of settle and keeps every frame the
// program would have painted on the way.
func recordFrames(t *testing.T, m browserModel, cmd tea.Cmd, frames *[]string) browserModel {
	t.Helper()
	msg := cmd()
	next, follow := step(m, msg)
	*frames = append(*frames, next.View().Content)
	if _, ok := msg.(snapshotLoaded); ok && next.taskID != "" && next.err == nil {
		batch, ok := follow().(tea.BatchMsg)
		if !ok || len(batch) != 2 {
			t.Fatal("snapshot did not schedule detail and refresh")
		}
		return recordFrames(t, next, batch[0], frames)
	}
	if _, ok := msg.(detailLoaded); ok && follow != nil {
		return recordFrames(t, next, follow, frames)
	}
	return next
}

// TestPrivateModeMasksTheSearchFilter covers both places the filter is
// painted: the live input while it is typed and the applied filter, which
// stays in the status line for as long as the filter stands.
func TestPrivateModeMasksTheSearchFilter(t *testing.T) {
	m, _ := privateBrowser(t, Options{}, secretTask())
	m, _ = press(m, '/', 0)
	if !m.searching {
		t.Fatal("/ did not open the search input")
	}
	m, _ = step(m, tea.PasteMsg{Content: "Secret"})
	if !strings.Contains(m.View().Content, "Search: Secret") {
		t.Fatalf("the default mode hid the typed filter:\n%s", m.View().Content)
	}

	m, cmd := press(m, 'k', tea.ModCtrl)
	if cmd != nil {
		t.Fatal("the toggle scheduled work instead of repainting")
	}
	typed := m.View().Content
	if strings.Contains(typed, "Secret") {
		t.Fatalf("private mode painted the filter while it was typed:\n%s", typed)
	}
	if !strings.Contains(typed, "Search: "+hiddenTitle) {
		t.Fatalf("the typed filter lost its field instead of masking it:\n%s", typed)
	}

	m, cmd = press(m, tea.KeyEnter, 0)
	m = settle(t, m, cmd)
	if m.query.Search != "Secret" {
		t.Fatalf("the masked filter was not applied: %q", m.query.Search)
	}
	applied := m.View().Content
	if strings.Contains(applied, "Secret") {
		t.Fatalf("private mode painted the applied filter:\n%s", applied)
	}
	if !strings.Contains(applied, "Search: "+hiddenTitle) {
		t.Fatalf("the applied filter left no sign that a filter stands:\n%s", applied)
	}
}

// TestPrivateModeKeepsCreatedTitlesOutOfTheStatusLine turns private mode on
// after the notice is assembled: the notice outlives the form, so the title
// cannot be resolved when the action finishes.
func TestPrivateModeKeepsCreatedTitlesOutOfTheStatusLine(t *testing.T) {
	m, _ := writableModel(t)
	m, _ = step(m, tea.WindowSizeMsg{Width: 200, Height: 32})
	m, _ = press(m, 'a', 0)
	m, _ = step(m, tea.PasteMsg{Content: "Secret title"})
	m, cmd := press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if shown := m.View().Content; !strings.Contains(shown, "Created: Secret title") {
		t.Fatalf("the default mode hid the created title:\n%s", shown)
	}

	m, cmd = press(m, 'k', tea.ModCtrl)
	if cmd != nil {
		t.Fatal("the toggle scheduled work instead of repainting")
	}
	hidden := m.View().Content
	if strings.Contains(hidden, "Secret title") {
		t.Fatalf("private mode painted the created title in the status line:\n%s", hidden)
	}
	if !strings.Contains(hidden, "Created: "+hiddenTitle) {
		t.Fatalf("the notice lost its title instead of masking it:\n%s", hidden)
	}
	if !strings.Contains(hidden, "Saved locally (create)") {
		t.Fatalf("private mode hid the part of the notice that carries no task text:\n%s", hidden)
	}
}

// TestPrivateHelpFitsTheDefaultTerminal guards the help text of Ctrl+K: the
// panel paints its body through fit, which truncates without any marker, so a
// sentence that does not fit an 80 column terminal disappears in silence.
func TestPrivateHelpFitsTheDefaultTerminal(t *testing.T) {
	q := &fakeQueries{tasks: []model.Task{secretTask()}}
	m := newModel(context.Background(), q, Options{})
	t.Cleanup(m.cancelLoad)
	m, _ = step(m, tea.WindowSizeMsg{Width: 80, Height: 60})
	m = settle(t, m, m.Init())
	m, _ = press(m, tea.KeyF1, 0)
	if !m.help.ShowAll {
		t.Fatal("F1 did not open the help panel")
	}
	for i, line := range m.helpLines() {
		if ansi.StringWidth(line) > m.width-4 {
			t.Fatalf("help line %d is wider than the panel body: %q", i, line)
		}
	}
	frame := m.View().Content
	for _, want := range []string{
		"Ctrl+K: private mode hides task titles, Kanban cards and details",
		"Private mode also hides the search filter; forms keep their own draft",
		"Ctrl+K answers in every context; the mode is never kept between runs",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("an 80 column help panel broke up %q:\n%s", want, frame)
		}
	}
}

// privateBoardModel opens the Kanban board for a project with one task
// per column, which is where boardView paints task text outside paneContent.
func privateBoardModel(t *testing.T, private bool) browserModel {
	t.Helper()
	q := &fakeQueries{tasks: []model.Task{
		{Id: "a", ProjectId: "p", Title: "Secret title", ColumnId: "todo"},
		{Id: "b", ProjectId: "p", Title: "Other card", ColumnId: "done"},
	}}
	service := &fakeResourceActions{listing: app.ResourceListing{Entities: []store.ResourceEntity{
		{Ref: store.EntityRef{Kind: "column", Key: "todo"}, ServerID: "todo", ProjectKey: "p", Data: json.RawMessage(`{"id":"todo","projectId":"p","name":"Todo","sortOrder":1}`)},
		{Ref: store.EntityRef{Kind: "column", Key: "done"}, ServerID: "done", ProjectKey: "p", Data: json.RawMessage(`{"id":"done","projectId":"p","name":"Done","sortOrder":2}`)},
	}}}
	m := newModel(context.Background(), q, Options{Resources: service, Private: private})
	t.Cleanup(m.cancelLoad)
	m.query = app.BrowseQuery{View: app.ProjectView, ProjectID: "p"}
	m.projects = []model.Project{{Id: "p", Name: "Project"}}
	m.tasks, m.taskID, m.loading = q.tasks, "a", false
	m, cmd := m.toggleKanban()
	return settle(t, m, cmd)
}

func TestPrivateModeMasksKanbanCardTitles(t *testing.T) {
	off := privateBoardModel(t, false)
	shown := off.View().Content
	if !strings.Contains(shown, "Secret title") {
		t.Fatalf("the default mode hid the card title:\n%s", shown)
	}
	if strings.Contains(shown, hiddenTitle) {
		t.Fatalf("a placeholder appeared on the board while private mode was off:\n%s", shown)
	}
	for _, want := range []string{"Todo", "Done"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("board hid column name %q:\n%s", want, shown)
		}
	}

	on := privateBoardModel(t, true)
	hidden := on.View().Content
	if strings.Contains(hidden, "Secret") {
		t.Fatalf("private mode leaked a kanban card title:\n%s", hidden)
	}
	if !strings.Contains(hidden, hiddenTitle) {
		t.Fatalf("private mode did not mask the kanban card title:\n%s", hidden)
	}
	for _, want := range []string{"Todo", "Done"} {
		if !strings.Contains(hidden, want) {
			t.Fatalf("private mode hid column name %q:\n%s", want, hidden)
		}
	}
}
