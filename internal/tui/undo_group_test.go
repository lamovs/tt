package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func TestTUIGlobalUndoShowsAndReversesAWholeGroup(t *testing.T) {
	m, st := writableModel(t)
	grouped := store.WithUndoGroup(m.ctx, "group-tui")
	first, err := st.CreateTask(grouped, model.Task{ProjectId: "work", Title: "Sample grouped one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateTask(grouped, model.Task{ProjectId: "home", Title: "Sample grouped two"})
	if err != nil {
		t.Fatal(err)
	}

	m, cmd := press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	if m.dialog == nil || len(m.dialog.undo.Group) != 2 || m.dialog.undo.Entry.Action.TaskID != second.Id {
		t.Fatalf("group undo not previewed: %+v", m.dialog)
	}
	_, lines := m.dialogView()
	text := strings.Join(lines, "\n")
	for _, want := range []string{"Undo group: 2 operations", "Sample grouped one", "Sample grouped two", "reversed together"} {
		if !strings.Contains(text, want) {
			t.Fatalf("group dialog lacks %q:\n%s", want, text)
		}
	}

	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	for _, task := range []model.Task{first, second} {
		if _, err := st.Task(m.ctx, task.Id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%q survived the group undo: %v", task.Title, err)
		}
	}
	if !strings.Contains(m.notice, "2 grouped operations") {
		t.Fatalf("notice = %q, want it to count the group", m.notice)
	}
	top, err := st.LastUndoGroup(m.ctx)
	if err != nil || len(top) != 1 || top[0].Group != "" {
		t.Fatalf("stack after the group = %+v, %v; want the ungrouped record before it", top, err)
	}
}
