package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func checklistModel(t *testing.T) (browserModel, *store.Store) {
	t.Helper()
	m, st := writableModel(t)
	for i := 0; i < 2; i++ {
		if _, err := st.AddTaskItem(m.ctx, m.taskID, "same"); err != nil {
			t.Fatal(err)
		}
	}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	m, _ = press(m, 'c', 0)
	if m.checklist == nil {
		t.Fatal("checklist did not open")
	}
	return m, st
}

func TestChecklistWorkflowDuplicateTitlesUndoAndLostSelection(t *testing.T) {
	m, st := checklistModel(t)
	first, second := m.detail.Items[0].Key, m.detail.Items[1].Key
	m, _ = press(m, 'j', 0)
	if m.checklist.key != second {
		t.Fatal("duplicate title selected wrong key")
	}
	m, _ = press(m, 'e', 0)
	m, _ = press(m, 'u', tea.ModCtrl)
	title := "  qaeus+_123?\r\nexact  "
	m, _ = step(m, tea.PasteMsg{Content: title})
	m, cmd := press(m, 's', tea.ModCtrl)
	if !m.busy {
		t.Fatal("save was not fenced")
	}
	_, duplicate := press(m, 's', tea.ModCtrl)
	if duplicate != nil {
		t.Fatal("duplicate submission")
	}
	m = finishLocal(t, m, cmd)
	if m.detail.Items[1].Title != title || m.checklist.key != second || m.detail.Items[0].Title != "same" {
		t.Fatal("rename targeted wrong duplicate or changed bytes")
	}
	m, cmd = press(m, tea.KeyUp, tea.ModAlt)
	m = finishLocal(t, m, cmd)
	if m.checklist.key != second || m.detail.Items[0].Key != second {
		t.Fatal("move changed selection")
	}
	m, cmd = press(m, tea.KeySpace, 0)
	m = finishLocal(t, m, cmd)
	if !m.detail.Items[0].Status.Done() {
		t.Fatal("completion failed")
	}
	m, cmd = press(m, tea.KeySpace, 0)
	m = finishLocal(t, m, cmd)
	if m.detail.Items[0].Status.Done() {
		t.Fatal("reopen failed")
	}
	m, _ = press(m, 'd', tea.ModCtrl)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if m.checklist.key != "" || !m.checklist.lost || m.detail.Items[0].Key != first {
		t.Fatal("removal selected adjacent row")
	}
	before := rowsIn(t, st, "events")
	m, cmd = press(m, tea.KeySpace, 0)
	if cmd != nil || rowsIn(t, st, "events") != before {
		t.Fatal("missing selection acted on next item")
	}
	m, cmd = press(m, 'u', 0)
	m = finishLocal(t, m, cmd)
	m, cmd = press(m, tea.KeyEnter, 0)
	m = finishLocal(t, m, cmd)
	if len(m.detail.Items) != 2 || m.detail.Items[0].Key != second || m.checklist.key != "" {
		t.Fatal("undo lost identity or silently selected a row")
	}
	m, _ = press(m, 'a', 0)
	m, _ = step(m, tea.PasteMsg{Content: "same"})
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if len(m.detail.Items) != 3 || m.checklist.key != m.detail.Items[2].Key {
		t.Fatal("new item not selected from committed outcome")
	}
}

func TestChecklistRefreshKeepsDraftAndRefusesConcurrentMutation(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{true: "rename", false: "remove"}[edit], func(t *testing.T) {
			m, st := checklistModel(t)
			key := m.checklist.key
			if edit {
				m, _ = press(m, 'e', 0)
				m, _ = step(m, tea.PasteMsg{Content: " draft"})
			} else {
				m, _ = press(m, 'd', tea.ModCtrl)
			}
			if _, err := st.RemoveTaskItem(m.ctx, m.taskID, 1); err != nil {
				t.Fatal(err)
			}
			m, cmd := m.load()
			m = settle(t, m, cmd)
			if m.checklist.key != "" {
				t.Fatal("refresh substituted a row")
			}
			before := rowsIn(t, st, "events")
			if edit {
				m, cmd = press(m, 's', tea.ModCtrl)
			} else {
				m, cmd = press(m, tea.KeyEnter, 0)
			}
			m = finishLocal(t, m, cmd)
			if !strings.Contains(m.notice, "Not saved") || rowsIn(t, st, "events") != before {
				t.Fatal("stale action wrote")
			}
			if edit && (m.checklist.form == nil || m.checklist.form.intent.change.Key != key || string(m.checklist.form.title.value) != "same draft") {
				t.Fatal("conflicted draft was lost")
			}
		})
	}
}

func TestChecklistLateDetailAndActionFences(t *testing.T) {
	m, _ := checklistModel(t)
	original := *m.detail
	old := detailLoaded{id: m.taskID, task: original, generation: m.detailGeneration, snapshot: m.generation}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	old.task.Items = []model.Item{{Key: "unrelated", Title: "same"}}
	m, _ = step(m, old)
	if m.detail.Items[0].Key != original.Items[0].Key {
		t.Fatal("late details replaced checklist")
	}
	m, _ = press(m, 'a', 0)
	m, _ = step(m, tea.PasteMsg{Content: "new"})
	m, cmd = press(m, 's', tea.ModCtrl)
	m, _ = step(m, actionFinished{kind: "checklist", generation: m.actionGeneration - 1})
	if !m.busy || m.checklist.form == nil {
		t.Fatal("late action changed current form")
	}
	m = finishLocal(t, m, cmd)
}

func TestChecklistFormsHelpFocusAndSmallTerminal(t *testing.T) {
	m, st := checklistModel(t)
	before := rowsIn(t, st, "events")
	m, _ = press(m, '?', 0)
	if !strings.Contains(m.View().Content, "Checklist keys") {
		t.Fatal("missing context help")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'e', 0)
	m, _ = step(m, tea.PasteMsg{Content: " changed"})
	m, _ = step(m, tea.WindowSizeMsg{Width: 31, Height: 9})
	_, cmd := press(m, 's', tea.ModCtrl)
	if cmd != nil || rowsIn(t, st, "events") != before {
		t.Fatal("hidden form submitted")
	}
	m, _ = step(m, tea.WindowSizeMsg{Width: 32, Height: 10})
	m, _ = press(m, 'c', tea.ModCtrl)
	if !m.checklist.form.discard || !m.checklist.form.quitAfter {
		t.Fatal("interrupt discarded dirty input")
	}
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, tea.KeyEscape, 0)
	m, _ = press(m, 'd', 0)
	if m.checklist.form != nil || rowsIn(t, st, "events") != before {
		t.Fatal("discard wrote data")
	}
	m, _ = press(m, 'o', tea.ModCtrl)
	m, _ = press(m, '2', 0)
	if m.checklist != nil || m.focus != tasksPane {
		t.Fatal("panel jump did not return")
	}
}

func TestChecklistInspectionAndViewAreReadOnly(t *testing.T) {
	m, st := checklistModel(t)
	before := rowsIn(t, st, "events")
	m.checklistState = store.ChecklistState{Refusal: strings.Repeat("unsafe raw ", 40), Uncertain: true}
	key := m.checklist.key
	m, _ = press(m, tea.KeyEnter, 0)
	if !strings.Contains(m.View().Content, "Checklist details") {
		t.Fatal("full details missing")
	}
	for i := 0; i < 10; i++ {
		m.View()
	}
	m, _ = press(m, tea.KeyEscape, 0)
	for _, key := range []tea.KeyPressMsg{{Code: 'a'}, {Code: 'e'}, {Code: tea.KeySpace}, {Code: 'd', Mod: tea.ModCtrl}, {Code: tea.KeyDown, Mod: tea.ModAlt}} {
		var cmd tea.Cmd
		m, cmd = step(m, key)
		if cmd != nil {
			t.Fatal("read-only action started")
		}
	}
	if rowsIn(t, st, "events") != before || m.checklist.key != key {
		t.Fatal("inspection wrote or changed selection")
	}
	current, err := st.Task(m.ctx, m.taskID)
	if err != nil || !reflect.DeepEqual(current.Items, m.detail.Items) {
		t.Fatal("view changed items")
	}
}

func TestChecklistTaskAndServerItemPromotionKeepKeyAndFenceDraft(t *testing.T) {
	m, st := checklistModel(t)
	oldID, key := m.taskID, m.checklist.key
	m, _ = press(m, 'e', 0)
	m, _ = step(m, tea.PasteMsg{Content: " retained draft"})
	if err := st.ReplaceLocalID(m.ctx, oldID, "server-task"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("UPDATE item_identities SET server_id=?,state=? WHERE task_id=? AND item_key=?", "server-item", "bound", "server-task", key); err != nil {
		t.Fatal(err)
	}
	m, cmd := m.load()
	m = settle(t, m, cmd)
	if m.taskID != "server-task" || m.checklist.taskID != "server-task" || m.checklist.key != key || m.detail.Items[0].Id != "server-item" {
		t.Fatal("committed identities lost selection")
	}
	if m.checklist.form.intent.original.Id != oldID {
		t.Fatal("draft retargeted after promotion")
	}
	before := rowsIn(t, st, "events")
	m, cmd = press(m, 's', tea.ModCtrl)
	m = finishLocal(t, m, cmd)
	if m.checklist.form == nil || m.checklist.form.err == "" || rowsIn(t, st, "events") != before {
		t.Fatal("stale identity draft was saved")
	}
}

func TestChecklistUnsafePasteLeavesTitleUnchanged(t *testing.T) {
	m, _ := checklistModel(t)
	m, _ = press(m, 'e', 0)
	for _, value := range []string{"bad\x00title", string([]byte{0xff}), strings.Repeat("x", (4<<20)+1)} {
		m, _ = step(m, tea.PasteMsg{Content: value})
		if string(m.checklist.form.title.value) != "same" || m.checklist.form.err == "" {
			t.Fatal("unsafe paste changed title")
		}
	}
}
