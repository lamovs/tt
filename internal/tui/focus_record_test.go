package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

type fakeFocusRecordActions struct {
	fakeResourceActions
	recordPrepares, recordApplies int
	record                        store.FocusRecord
	accepted                      string
	recordErr                     error
}

func (f *fakeFocusRecordActions) PrepareFocusRecord(_ context.Context, record store.FocusRecord) (app.FocusRecordPreview, error) {
	f.recordPrepares++
	f.record = record
	return app.FocusRecordPreview{ID: "focus-preview", Record: record, Upload: "existing uploader"}, f.recordErr
}
func (f *fakeFocusRecordActions) ApplyFocusRecord(_ context.Context, id string) (store.TimerSession, error) {
	f.recordApplies++
	f.accepted = id
	return store.TimerSession{ID: "completed-session", TaskID: f.record.TaskID, FocusType: f.record.FocusType, Outcome: "done"}, f.recordErr
}

func focusRecordModel(t *testing.T) (browserModel, *fakeFocusRecordActions) {
	t.Helper()
	service := &fakeFocusRecordActions{}
	m := newModel(context.Background(), &fakeQueries{}, Options{Resources: service})
	t.Cleanup(m.cancelLoad)
	m.loading = false
	m, cmd := m.openResources(app.ResourceQuery{Kind: "focus", From: "2026-09-01T00:00:00Z", To: "2026-09-11T00:00:00Z"})
	m = settle(t, m, cmd)
	return m, service
}

func TestFocusRecordTUIExactPreviewAndOneLocalAcceptance(t *testing.T) {
	m, service := focusRecordModel(t)
	m, _ = press(m, 'a', 0)
	f := m.resource.form
	if f == nil || f.kind != "focus-record" {
		t.Fatal("manual form missing")
	}
	f.fields[0].input.set("1")
	f.fields[1].input.set("2026-09-11T11:00:00+03:00")
	f.fields[2].input.set("2026-09-11T11:25:00+03:00")
	f.fields[3].input.set("30")
	f.fields[4].input.set("id:task-exact")
	f.fields[5].input.set("A completed record")
	m, cmd := press(m, 's', tea.ModCtrl)
	if service.recordPrepares != 0 || cmd == nil || !m.busy {
		t.Fatal("synchronous or missing preview")
	}
	first := cmd().(focusRecordPrepared)
	_ = cmd()
	if service.recordPrepares != 1 || service.record.TaskID != "task-exact" || service.record.FocusType != 1 || service.record.PauseSeconds != 30 {
		t.Fatal("preview did not freeze exact fields")
	}
	stale := first
	stale.record = "another-record"
	m, _ = step(m, stale)
	if m.resource.focusPreview != nil {
		t.Fatal("wrong-record preview accepted")
	}
	m, _ = step(m, first)
	if m.resource.focusPreview == nil || service.prepares != 0 || service.applies != 0 {
		t.Fatal("focus imported via generic resource queue")
	}
	m.width = 20
	m, cmd = press(m, tea.KeyEnter, 0)
	if cmd != nil {
		t.Fatal("hidden preview accepted")
	}
	m.width = 80
	m, cmd = press(m, tea.KeyEnter, 0)
	m, duplicate := press(m, tea.KeyEnter, 0)
	if duplicate != nil {
		t.Fatal("duplicate acceptance command")
	}
	done := cmd().(focusRecordApplied)
	_ = cmd()
	wrong := done
	wrong.previewID = "wrong"
	m, _ = step(m, wrong)
	if m.resource.focusPreview == nil {
		t.Fatal("wrong preview completion applied")
	}
	m, _ = step(m, done)
	if service.recordApplies != 1 || service.accepted != "focus-preview" || m.resource.form != nil || !strings.Contains(m.notice, "upload") {
		t.Fatal("completed record lost or duplicated")
	}
	for _, remote := range service.remote {
		if remote {
			t.Fatal("manual record used remote history fetch")
		}
	}
}

func TestFocusRecordTUIValidationFailureRetainsDraftAndQuitChoice(t *testing.T) {
	m, service := focusRecordModel(t)
	m, _ = press(m, 'a', 0)
	m.resource.form.fields[1].input.set("2026-09-11T10:00:00.123Z")
	m.resource.form.fields[2].input.set("2026-09-11T10:25:00Z")
	m, cmd := press(m, 's', tea.ModCtrl)
	if cmd != nil || !strings.Contains(m.resource.err, "whole-second") {
		t.Fatal("fractional manual record accepted")
	}
	m.resource.form.fields[1].input.set("2026-09-11T10:00:00Z")
	m.resource.form.fields[4].input.set("name:Task")
	m, cmd = press(m, 's', tea.ModCtrl)
	if cmd != nil || service.recordPrepares != 0 {
		t.Fatal("task name entered exact-ID flow")
	}
	m.resource.form.fields[4].input.set("missing-id")
	service.recordErr = errors.New("task no longer cached")
	m, cmd = press(m, 's', tea.ModCtrl)
	m = settle(t, m, cmd)
	if m.resource.form == nil || m.resource.focusPreview != nil || !strings.Contains(m.resource.err, "cached") {
		t.Fatal("failed preview lost draft")
	}
	service.recordErr = nil
	m, _ = press(m, 'c', tea.ModCtrl)
	if !m.resource.form.discard || !m.resource.form.quitAfter {
		t.Fatal("dirty quit skipped choice")
	}
	m, cmd = press(m, 's', 0)
	m = settle(t, m, cmd)
	if m.resource.focusPreview == nil {
		t.Fatal("quit review did not retain record")
	}
	m, cmd = press(m, tea.KeyEnter, 0)
	message := cmd()
	m, quit := step(m, message)
	if quit == nil || !m.interrupted || service.recordApplies != 1 {
		t.Fatal("quit after acceptance not respected")
	}
}

func TestFocusHistoryTypeRangeAndDeleteKeepExactCompositeIdentity(t *testing.T) {
	m, service := focusRecordModel(t)
	m, cmd := press(m, '1', 0)
	m = settle(t, m, cmd)
	if m.resource.query.FocusType != 1 || service.remote[len(service.remote)-1] {
		t.Fatal("type switch fetched remotely")
	}
	m, _ = press(m, 'e', 0)
	if m.resource.form.kind != "focus-history" {
		t.Fatal("history editor missing")
	}
	m.resource.form.fields[1].input.set("2026-08-01T00:00:00Z")
	m, cmd = press(m, 's', tea.ModCtrl)
	m = settle(t, m, cmd)
	if m.resource.query.From != "2026-08-01T00:00:00Z" || service.remote[len(service.remote)-1] {
		t.Fatal("range editor used network")
	}
	service.listing.Entities = []store.ResourceEntity{{Ref: store.EntityRef{Kind: "focus", Key: "1/shared"}, ServerID: "1/shared", ProjectKey: "1", Revision: 7, Data: json.RawMessage(`{"id":"shared","type":1}`)}}
	m, cmd = press(m, 'R', 0)
	m = settle(t, m, cmd)
	if !service.remote[len(service.remote)-1] {
		t.Fatal("explicit remote refresh missing")
	}
	m, cmd = press(m, 'd', tea.ModCtrl)
	m = settle(t, m, cmd)
	if service.mutation.Ref.Key != "1/shared" || service.mutation.Action != "delete" || service.mutation.ProjectKey != "1" {
		t.Fatal("delete lost focus type/ID")
	}
}

func TestFocusRecordTUIEscapesNoteAndFitsNarrowPreview(t *testing.T) {
	m, _ := focusRecordModel(t)
	m, _ = press(m, 'a', 0)
	m.resource.form.fields[5].input.set("bad\x1b]52;c;bad\x07\u754c")
	m, cmd := press(m, 's', tea.ModCtrl)
	m = settle(t, m, cmd)
	for _, size := range [][2]int{{32, 10}, {80, 24}, {160, 40}} {
		m.width, m.height = size[0], size[1]
		view := m.View().Content
		if strings.Contains(view, "\x1b]52") {
			t.Fatal("focus note leaked terminal control")
		}
		lines := strings.Split(view, "\n")
		if len(lines) != m.height {
			t.Fatal("preview height")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) != m.width {
				t.Fatalf("width %d want%d", ansi.StringWidth(line), m.width)
			}
		}
	}
}
