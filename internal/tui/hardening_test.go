package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
)

func TestF1KeepsEveryDraftAndConfirmation(t *testing.T) {
	setups := []struct {
		name, help string
		setup      func(*browserModel)
	}{
		{"browser", "Global keys", func(m *browserModel) {}},
		{"search", "Search input", func(m *browserModel) { m.searching = true; m.input.set("exact?") }},
		{"task", "Task/note form", func(m *browserModel) { m.openEdit(*m.detail); m.form.body.set("  exact\nbody?  ") }},
		{"schedule", "Schedule form", func(m *browserModel) { m.openSchedule(*m.detail) }},
		{"item", "Checklist", func(m *browserModel) {
			m.openChecklist()
			m.checklist.form = &itemForm{}
			m.checklist.form.title.set("exact?")
		}},
		{"queue", "Sync queue", func(m *browserModel) { m.queue = &queuePanel{} }},
		{"timer", "Focus", func(m *browserModel) { m.timer = &timerPanel{form: &timerForm{}} }},
		{"settings", "Settings and diagnostics", func(m *browserModel) { m.systemPanel = &systemPanel{preview: &SystemPreview{Action: "config init"}} }},
		{"confirmation", "Task confirmation", func(m *browserModel) { m.dialog = &actionDialog{kind: "complete", original: *m.detail} }},
		{"move", "Delete/move confirmation", func(m *browserModel) { m.taskOperation = &taskOperationPanel{move: true} }},
		{"sync", "Explicit sync", func(m *browserModel) {
			ctx, cancel := context.WithCancel(m.ctx)
			t.Cleanup(cancel)
			m.sync = &syncPanel{context: ctx, cancel: cancel, running: true}
		}},
		{"editor recovery", "Editor recovery", func(m *browserModel) { m.recovery = &recoveryPanel{path: "private-draft"} }},
	}
	for _, tc := range setups {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := loadedModel(t)
			tc.setup(&m)
			beforeForm, beforeItem, beforeTimer, beforeDialog := m.form, m.checklist, m.timer, m.dialog
			m, _ = press(m, tea.KeyF1, 0)
			if !m.help.ShowAll || !strings.Contains(m.View().Content, tc.help) {
				t.Fatal("missing visible context help", m.View().Content)
			}
			m, _ = step(m, tea.PasteMsg{Content: "must not enter draft"})
			m, _ = press(m, 's', tea.ModCtrl)
			m, _ = press(m, tea.KeyEscape, 0)
			if m.help.ShowAll || m.form != beforeForm || m.checklist != beforeItem || m.timer != beforeTimer || m.dialog != beforeDialog {
				t.Fatal("help changed draft or confirmation")
			}
			if m.form != nil && m.form.schedule == nil && string(m.form.body.value) != "  exact\nbody?  " {
				t.Fatal("help altered raw body")
			}
			if !strings.HasPrefix(mustGlobalFooter(m), "F1 help") {
				t.Fatal("F1 hint clipped from footer")
			}
		})
	}
}

func mustGlobalFooter(m browserModel) string { global, _ := m.footers(); return global }

func TestDetailFormattingRejectsOldWidthAndSelection(t *testing.T) {
	m, q := loadedModel(t)
	m.focus = previewPane
	m.width = 140
	m.detailWidth, m.renderWidth = 0, 0
	first := m.reflow()
	if first == nil {
		t.Fatal("no deferred formatter")
	}
	old := first().(detailRendered)
	m.width = 80
	m.renderWidth = 0
	second := m.reflow()
	current := second().(detailRendered)
	m, _ = step(m, old)
	if m.detailWidth == old.width {
		t.Fatal("stale width accepted")
	}
	m, _ = step(m, current)
	if m.detailWidth != current.width || !strings.Contains(strings.Join(m.detailLines, "\n"), "First body") {
		t.Fatal("current formatter lost body")
	}
	m.focus = tasksPane
	m, cmd := press(m, 'j', 0)
	m = settle(t, m, cmd)
	m, _ = step(m, current)
	if m.detail.Id != "work-b" || strings.Contains(strings.Join(m.detailLines, "\n"), "First body") {
		t.Fatal("old rendering replaced another identity")
	}
	if q.calls == 0 {
		t.Fatal("fixture never loaded")
	}
}

func TestFocusRefreshIsDeferredAndPreservesDraft(t *testing.T) {
	m, q := loadedModel(t)
	m.openEdit(*m.detail)
	m.form.body.set("unsaved exact\n")
	calls := q.calls
	m, cmd := step(m, tea.BlurMsg{})
	if cmd != nil || q.calls != calls {
		t.Fatal("blur performed work")
	}
	m, cmd = step(m, tea.FocusMsg{})
	if cmd == nil || q.calls != calls {
		t.Fatal("focus refresh not deferred")
	}
	generation := m.generation
	m, again := step(m, tea.FocusMsg{})
	if again != nil || m.generation != generation {
		t.Fatal("duplicate focus refresh")
	}
	m = settle(t, m, cmd)
	if string(m.form.body.value) != "unsaved exact\n" {
		t.Fatal("focus refresh overwrote draft")
	}
}

type identityRecorder struct {
	Actions
	ids []string
}

func (a *identityRecorder) State(_ context.Context, ids []string) (app.CacheState, error) {
	a.ids = append([]string(nil), ids...)
	return app.CacheState{}, nil
}

func TestRefreshReconcilesSelectionsWithoutReadingEveryRowIdentity(t *testing.T) {
	m, _ := writableModel(t)
	record := &identityRecorder{Actions: m.actions}
	m.actions = record
	for i := 0; i < 10000; i++ {
		m.tasks = append(m.tasks, model.Task{Id: fmt.Sprintf("extra-%d", i)})
	}
	m.selections[app.BrowseQuery{View: app.CompletedView}] = "remembered-id"
	cmd := m.snapshotCmd()
	if record.ids != nil {
		t.Fatal("snapshot performed inline I/O")
	}
	msg := cmd().(snapshotLoaded)
	if len(record.ids) != 2 || record.ids[0] != m.taskID || record.ids[1] != "remembered-id" {
		t.Fatal("identity query did not bind the selections")
	}

	m.taskID = "newly-selected"
	next, reload := step(m, msg)
	if next.taskID != "newly-selected" || next.selectionLost || reload == nil {
		t.Fatal("in-flight selection was cleared")
	}
}

func TestSmallTerminalStillCancelsForegroundOperations(t *testing.T) {
	m, _ := loadedModel(t)
	m.width, m.height = 20, 5
	ctx, cancel := context.WithCancel(m.ctx)
	m.systemPanel = &systemPanel{}
	m.systemWorking, m.busy = true, true
	m.systemCancel = cancel
	m, _ = press(m, tea.KeyEscape, 0)
	if ctx.Err() == nil {
		t.Fatal("small terminal blocked cancel")
	}
	ctx, cancel = context.WithCancel(m.ctx)
	m.systemWorking = false
	m.timerForeground = true
	m.timerAction = "upload"
	m.timerCancel = cancel
	m, _ = press(m, tea.KeyEscape, 0)
	if ctx.Err() == nil {
		t.Fatal("small terminal blocked focus cancel")
	}
}

func TestBoundedSummaryAndFieldRenderingPreserveRawData(t *testing.T) {
	raw := strings.Repeat("\u754c e\u0301 \U0001f469\U0001f3fd\u200d\U0001f4bb\x1b]52;c;hidden\a", 50000)
	var input searchInput
	input.set(raw)
	for _, width := range []int{1, 32, 80, 140} {
		for _, pos := range []int{0, len(input.value) / 2, len(input.value)} {
			input.pos = pos
			text := input.view(width)
			if !utf8.ValidString(text) || strings.ContainsAny(text, "\x1b\a") || ansi.StringWidth(text) > max(1, width) {
				t.Fatal("unbounded/unsafe field rendering")
			}
		}
		text := displayClipped(raw, width)
		if ansi.StringWidth(text) > width || strings.ContainsAny(text, "\x1b\a") {
			t.Fatal("unsafe summary")
		}
	}
	if string(input.value) != raw {
		t.Fatal("render changed raw bytes")
	}
	input.set("BODY_START\n" + raw + "\nBODY_END")
	for _, pos := range []int{0, len(input.value)} {
		input.pos = pos
		lines := input.bodyView(80, 12)
		if len(lines) > 12 {
			t.Fatal("body rendered outside its viewport")
		}
		want := "BODY_START"
		if pos != 0 {
			want = "BODY_END"
		}
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Fatal("body boundary unreachable", want)
		}
	}
}

func TestLongErrorsRemainScrollableAndInert(t *testing.T) {
	raw := "ERROR_START\n" + strings.Repeat("long \u754c e\u0301 error\n", 100) + "\x1b]52;c;hostile\a ERROR_END"
	m := newModel(context.Background(), nil, Options{Err: errors.New(raw)})
	if strings.Contains(m.View().Content, "ERROR_END") {
		t.Fatal("fixture fits one viewport")
	}
	m, _ = press(m, tea.KeyEnd, 0)
	if !strings.Contains(m.View().Content, "ERROR_END") || strings.Contains(m.View().Content, "\x1b]52") {
		t.Fatal("error tail hidden or injected")
	}
	m, _ = press(m, tea.KeyHome, 0)
	if !strings.Contains(m.View().Content, "ERROR_START") {
		t.Fatal("error beginning unreachable")
	}
}

func BenchmarkLargeFieldView(b *testing.B) {
	var input searchInput
	input.set(strings.Repeat("long body \u754c e\u0301\n", 200000))
	input.pos = len(input.value) / 2
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = input.view(80)
	}
}

func BenchmarkLargeBodyFormView(b *testing.B) {
	m := newModel(context.Background(), nil, Options{})
	m.openEdit(model.Task{Id: "one", Title: "Large body", Content: strings.Repeat("large body \u754c\n", 200000)})
	m.form.field = 1
	m.form.body.pos = len(m.form.body.value) / 2
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = m.View()
	}
}

func BenchmarkTenThousandTaskView(b *testing.B) {
	m := newModel(context.Background(), nil, Options{})
	m.width, m.height = 140, 32
	m.loading = false
	m.now = time.Now()
	m.focus = tasksPane
	for i := 0; i < 10000; i++ {
		m.tasks = append(m.tasks, model.Task{Id: fmt.Sprintf("task-%05d", i), Title: fmt.Sprintf("Task %05d", i)})
	}
	m.taskID = m.tasks[9999].Id
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = m.View()
	}
}
