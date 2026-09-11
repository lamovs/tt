package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

type FocusRecordActions interface {
	PrepareFocusRecord(context.Context, store.FocusRecord) (app.FocusRecordPreview, error)
	ApplyFocusRecord(context.Context, string) (store.TimerSession, error)
}

type focusRecordPrepared struct {
	generation uint64
	query      app.ResourceQuery
	record     string
	preview    app.FocusRecordPreview
	err        error
}

type focusRecordApplied struct {
	generation uint64
	query      app.ResourceQuery
	previewID  string
	session    store.TimerSession
	err        error
}

func (m browserModel) openFocusRecordForm(history bool) (browserModel, tea.Cmd) {
	if !history {
		if _, ok := m.resources.(FocusRecordActions); !ok {
			m.resource.err = "Manual focus history is unavailable."
			return m, nil
		}
	}
	now := time.Now().Truncate(time.Second)
	form := &resourceForm{kind: "focus-record", action: "create"}
	keys := []string{"type", "start", "end", "pause", "task", "note"}
	labels := []string{"Type: 0/1", "Start RFC3339", "End RFC3339", "Pause seconds", "Task ID (optional)", "Note"}
	values := []string{strconv.Itoa(int(m.resource.query.FocusType)), now.Add(-25 * time.Minute).Format(time.RFC3339), now.Format(time.RFC3339), "0", "", ""}
	if history {
		form.kind, form.action = "focus-history", "view"
		keys, labels = []string{"type", "start", "end"}, []string{"Type: 0/1", "From RFC3339", "To RFC3339"}
		values = []string{strconv.Itoa(int(m.resource.query.FocusType)), m.resource.query.From, m.resource.query.To}
	}
	for i, key := range keys {
		field := resourceField{key: key, label: labels[i], kind: "text", initial: values[i]}
		field.input.set(values[i])
		form.fields = append(form.fields, field)
	}
	m.resource.form, m.resource.focusPreview, m.resource.err = form, nil, ""
	return m, nil
}

func (f *resourceForm) focusRecord() (store.FocusRecord, error) {
	var record store.FocusRecord
	for _, field := range f.fields {
		value := string(field.input.value)
		var err error
		switch field.key {
		case "type":
			record.FocusType, err = strconv.Atoi(value)
		case "start":
			record.Start, err = time.Parse(time.RFC3339, value)
		case "end":
			record.End, err = time.Parse(time.RFC3339, value)
		case "pause":
			record.PauseSeconds, err = strconv.ParseInt(value, 10, 64)
		case "task":
			record.TaskID = strings.TrimPrefix(value, "id:")
			if strings.HasPrefix(record.TaskID, "name:") || record.TaskID != strings.TrimSpace(record.TaskID) {
				return record, errors.New("focus task must be an exact cached ID, or empty")
			}
		case "note":
			record.Note = value
		}
		if err != nil {
			return record, errors.New("focus type/pause need integers and dates need RFC3339 timestamps")
		}
	}
	if f.kind == "focus-history" {
		if record.FocusType != 0 && record.FocusType != 1 {
			return record, errors.New("focus type must be 0 or 1")
		}
		if !record.End.After(record.Start) {
			return record, errors.New("focus history needs an increasing date range")
		}
		return record, nil
	}
	return record, store.ValidateFocusRecord(record)
}

func focusRecordIdentity(record store.FocusRecord) string {
	raw, _ := json.Marshal(record)
	return string(raw)
}

func (m browserModel) reviewFocusRecord() (browserModel, tea.Cmd) {
	p := m.resource
	record, err := p.form.focusRecord()
	if err != nil {
		p.err = err.Error()
		return m, nil
	}
	if p.form.kind == "focus-history" {
		quit := p.form.quitAfter
		query := app.ResourceQuery{Kind: "focus", FocusType: api.FocusType(record.FocusType), From: record.Start.Format(time.RFC3339Nano), To: record.End.Format(time.RFC3339Nano)}
		if quit {
			m.closeResources()
			m.interrupted = true
			return m, tea.Quit
		}
		return m.openResources(query)
	}
	service, ok := m.resources.(FocusRecordActions)
	if !ok {
		p.err = "Manual focus history is unavailable."
		return m, nil
	}
	m.resourceGeneration++
	p.working, m.busy, p.err = true, true, ""
	generation, query, identity, ctx := m.resourceGeneration, p.query, focusRecordIdentity(record), m.ctx
	var once sync.Once
	var result focusRecordPrepared
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			preview, err := service.PrepareFocusRecord(ctx, record)
			result = focusRecordPrepared{generation: generation, query: query, record: identity, preview: preview, err: err}
		})
		return result
	})
}

func (m browserModel) finishFocusRecordPreview(msg focusRecordPrepared) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || p.form == nil || p.form.kind != "focus-record" || msg.generation != m.resourceGeneration || msg.query != p.query || !p.working {
		return m, nil
	}
	record, err := p.form.focusRecord()
	if err != nil || focusRecordIdentity(record) != msg.record {
		return m, nil
	}
	m.busy, p.working = false, false
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	p.focusPreview, p.offset = &msg.preview, 0
	return m, nil
}

func (m browserModel) applyFocusRecord() (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || p.focusPreview == nil || m.busy {
		return m, nil
	}
	service, ok := m.resources.(FocusRecordActions)
	if !ok {
		p.err = "Manual focus history is unavailable."
		return m, nil
	}
	m.resourceGeneration++
	p.working, m.busy = true, true
	generation, query, id, ctx := m.resourceGeneration, p.query, p.focusPreview.ID, m.ctx
	var once sync.Once
	var result focusRecordApplied
	return m, m.gate.command(func() tea.Msg {
		once.Do(func() {
			session, err := service.ApplyFocusRecord(ctx, id)
			result = focusRecordApplied{generation: generation, query: query, previewID: id, session: session, err: err}
		})
		return result
	})
}

func (m browserModel) finishFocusRecordApply(msg focusRecordApplied) (browserModel, tea.Cmd) {
	p := m.resource
	if p == nil || p.focusPreview == nil || msg.generation != m.resourceGeneration || msg.query != p.query || msg.previewID != p.focusPreview.ID || !p.working {
		return m, nil
	}
	p.working, m.busy = false, false
	p.focusPreview = nil
	if msg.err != nil {
		p.err = msg.err.Error()
		return m, nil
	}
	quit := p.form != nil && p.form.quitAfter
	p.form = nil
	m.notice = "Completed focus record saved locally: " + msg.session.ID + ". Open Local focus timer to upload, or use tt timer sync."
	if quit {
		m.interrupted = true
		return m, tea.Quit
	}
	return m, nil
}

func (m browserModel) focusRecordPreviewLines() []string {
	p := m.resource.focusPreview
	if p == nil {
		return nil
	}
	width := max(1, m.width-6)
	lines := wrapText("This creates one completed local focus session. It does not start or stop a timer and does not send to TickTick.", width)
	lines = append(lines, wrapText("Upload is separate through the existing focus uploader. Repeat acceptance of this exact preview reuses the same local session.", width)...)
	lines = append(lines, wrapText("Preview: "+p.ID, width)...)
	raw, _ := json.Marshal(p.Record)
	return append(lines, rawResourceLines(raw, width)...)
}

func (m browserModel) changeFocusHistoryType(kind api.FocusType) (browserModel, tea.Cmd) {
	q := m.resource.query
	q.FocusType = kind
	return m.openResources(q)
}
