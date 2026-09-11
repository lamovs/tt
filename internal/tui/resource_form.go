package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/store"
)

type resourceBaselineLoaded struct {
	generation    uint64
	target, stamp string
	err           error
}

type resourceField struct {
	key, label, kind, initial string
	input                     searchInput
}

type resourceForm struct {
	kind, action, target, parent string
	fields                       []resourceField
	index                        int
	discard, quitAfter           bool
}

func (f *resourceForm) dirty() bool {
	for _, field := range f.fields {
		if string(field.input.value) != field.initial {
			return true
		}
	}
	return false
}

func resourceFormFields(kind string) []resourceField {
	text := func(key, label string) resourceField { return resourceField{key: key, label: label, kind: "text"} }
	number := func(key, label string) resourceField { return resourceField{key: key, label: label, kind: "number"} }
	switch kind {
	case "project":
		return []resourceField{text("name", "Name"), text("color", "Color"), text("viewMode", "View: list/kanban/timeline"), text("kind", "Kind: TASK/NOTE"), {key: "sortOrder", label: "Sort order", kind: "integer"}}
	case "folder", "column":
		return []resourceField{text("name", "Name")}
	case "tag":
		return []resourceField{text("name", "Name: lowercase"), text("label", "Display label")}
	case "habit":
		return []resourceField{text("name", "Name"), text("type", "Type"), number("goal", "Goal"), number("step", "Step"), text("unit", "Unit"), text("repeatRule", "Repeat rule"), text("color", "Color")}
	case "comment":
		return []resourceField{text("title", "Comment")}
	case "checkin":
		return []resourceField{{key: "stamp", label: "Date: YYYYMMDD", kind: "integer"}, number("value", "Value"), number("goal", "Goal")}
	}
	return nil
}

func (m browserModel) openResourceForm(edit bool) (browserModel, tea.Cmd) {
	p := m.resource
	if p.loading {
		p.err = "Wait for the resource list to finish."
		return m, nil
	}
	kind := p.query.Kind
	if kind == "focus" {
		return m.openFocusRecordForm(edit)
	}
	if kind == "countdown" || kind == "checkin" {
		p.err = "Use a selected habit to add a check-in; this collection has no editable form."
		return m, nil
	}
	if edit && (kind == "tag" || kind == "comment") {
		p.err = "This resource has no verified edit endpoint."
		return m, nil
	}
	form := &resourceForm{kind: kind, action: "create", fields: resourceFormFields(kind)}
	values := map[string]json.RawMessage{}
	if edit {
		entity := p.selected()
		if entity == nil {
			p.err = "Select a resource to edit."
			return m, nil
		}
		form.action, form.target, form.parent = "update", entity.Ref.Key, entity.ProjectKey
		if err := json.Unmarshal(entity.Data, &values); err != nil {
			p.err = "Cannot edit an invalid resource snapshot."
			return m, nil
		}
	} else {
		switch kind {
		case "column":
			form.parent = p.query.ProjectID
		case "comment":
			form.parent = p.query.TaskID
		case "project":
			values["viewMode"], values["kind"] = json.RawMessage(`"list"`), json.RawMessage(`"TASK"`)
		case "habit":
			values["type"], values["goal"], values["step"], values["unit"] = json.RawMessage(`"Boolean"`), json.RawMessage("1"), json.RawMessage("1"), json.RawMessage(`"Count"`)
		}
	}
	for i := range form.fields {
		field := &form.fields[i]
		var value string
		if field.kind == "text" {
			_ = json.Unmarshal(values[field.key], &value)
		} else if raw := values[field.key]; len(raw) > 0 && string(raw) != "null" {
			value = string(raw)
		}
		field.initial = value
		field.input.set(value)
	}
	p.form, p.err, p.offset = form, "", 0
	return m, nil
}

func (m browserModel) openCheckinForm() (browserModel, tea.Cmd) {
	p := m.resource
	entity := p.selected()
	if entity == nil || entity.ServerID == "" {
		p.err = "Select a synced habit before creating a check-in."
		return m, nil
	}
	form := &resourceForm{kind: "checkin", action: "create", parent: entity.ServerID, fields: resourceFormFields("checkin")}
	var habit struct {
		Goal *float64 `json:"goal"`
	}
	_ = json.Unmarshal(entity.Data, &habit)
	goal := "1"
	if habit.Goal != nil {
		goal = strconv.FormatFloat(*habit.Goal, 'f', -1, 64)
	}
	for i, value := range []string{time.Now().Format("20060102"), "1", goal} {
		form.fields[i].initial = value
		form.fields[i].input.set(value)
	}
	p.form, p.err = form, ""
	return m, nil
}

func (m browserModel) openHabitHistory() (browserModel, tea.Cmd) {
	entity := m.resource.selected()
	if entity == nil || entity.ServerID == "" {
		m.resource.err = "Select a synced habit for its history."
		return m, nil
	}
	now := time.Now()
	return m.openResources(app.ResourceQuery{Kind: "checkin", HabitID: entity.ServerID, From: now.AddDate(0, 0, -30).Format("20060102"), To: now.Format("20060102")})
}

func (m *browserModel) pasteResource(value string) {
	if m.resource == nil || m.resource.form == nil || m.resource.preview != nil || m.resource.focusPreview != nil || m.busy || m.resource.form.discard {
		return
	}
	form := m.resource.form
	form.fields[form.index].input.insert(value)
	m.resource.err = ""
}

func (m browserModel) resourceFormKey(msg tea.KeyPressMsg) (browserModel, tea.Cmd) {
	p, f, s := m.resource, m.resource.form, msg.String()
	if f.discard {
		switch s {
		case "esc":
			f.discard, f.quitAfter = false, false
		case "d":
			quit := f.quitAfter
			p.form, p.err = nil, ""
			if quit {
				m.interrupted = true
				return m, tea.Quit
			}
		case "s":
			f.discard = false
			return m.reviewResourceForm()
		}
		return m, nil
	}
	switch s {
	case "esc":
		if f.dirty() {
			f.discard = true
		} else {
			p.form = nil
		}
	case "ctrl+s":
		return m.reviewResourceForm()
	case "ctrl+r":
		if f.kind == "checkin" {
			return m.refreshCheckinBaseline()
		}
	case "tab", "enter":
		f.index = (f.index + 1) % len(f.fields)
	case "shift+tab":
		f.index = (f.index + len(f.fields) - 1) % len(f.fields)
	default:
		f.fields[f.index].input.update(msg)
		p.err = ""
	}
	return m, nil
}

func (m browserModel) refreshCheckinBaseline() (browserModel, tea.Cmd) {
	f := m.resource.form
	mutation, err := f.mutation()
	if err != nil {
		m.resource.err = err.Error()
		return m, nil
	}
	stamp := strings.TrimPrefix(mutation.Ref.Key, f.parent+"/")
	m.resourceGeneration++
	m.resource.working, m.busy = true, true
	generation, target, ctx, service := m.resourceGeneration, f.parent, m.ctx, m.resources
	return m, m.gate.command(func() tea.Msg {
		_, err := service.List(ctx, app.ResourceQuery{Kind: "checkin", HabitID: target, From: stamp, To: stamp}, true)
		return resourceBaselineLoaded{generation: generation, target: target, stamp: stamp, err: err}
	})
}

func (m browserModel) finishResourceBaseline(msg resourceBaselineLoaded) (browserModel, tea.Cmd) {
	if m.resource == nil || m.resource.form == nil || msg.generation != m.resourceGeneration {
		return m, nil
	}
	f := m.resource.form
	if f.kind != "checkin" || f.parent != msg.target || string(f.fields[0].input.value) != msg.stamp {
		return m, nil
	}
	m.resource.working, m.busy = false, false
	if msg.err != nil {
		m.resource.err = msg.err.Error()
	} else {
		m.resource.err = "Exact date refreshed. Ctrl+S reviews this check-in."
	}
	return m, nil
}

func (f *resourceForm) mutation() (store.EntityMutation, error) {
	patch := map[string]any{}
	for _, field := range f.fields {
		value := string(field.input.value)
		if f.action == "update" && value == field.initial {
			continue
		}
		if f.action == "create" && value == "" {
			continue
		}
		switch field.kind {
		case "number":
			number, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return store.EntityMutation{}, fmt.Errorf("%s must be a finite number", field.label)
			}
			patch[field.key] = number
		case "integer":
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return store.EntityMutation{}, fmt.Errorf("%s must be an integer", field.label)
			}
			patch[field.key] = number
		default:
			patch[field.key] = value
		}
	}
	if f.kind == "tag" {
		if label, ok := patch["label"]; !ok || label == "" {
			patch["label"] = patch["name"]
		}
	}
	if len(patch) == 0 {
		return store.EntityMutation{}, errors.New("No fields changed.")
	}
	key := f.target
	if f.kind == "checkin" {
		stamp, ok := patch["stamp"].(int64)
		if !ok {
			return store.EntityMutation{}, errors.New("Check-in date is required.")
		}
		if _, err := time.Parse("20060102", strconv.FormatInt(stamp, 10)); err != nil {
			return store.EntityMutation{}, errors.New("Check-in date must be a valid YYYYMMDD date.")
		}
		key = f.parent + "/" + strconv.FormatInt(stamp, 10)
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return store.EntityMutation{}, err
	}
	return store.EntityMutation{Ref: store.EntityRef{Kind: f.kind, Key: key}, Action: f.action, ProjectKey: f.parent, Patch: raw}, nil
}

func (m browserModel) reviewResourceForm() (browserModel, tea.Cmd) {
	if kind := m.resource.form.kind; kind == "focus-record" || kind == "focus-history" {
		return m.reviewFocusRecord()
	}
	mutation, err := m.resource.form.mutation()
	if err != nil {
		m.resource.err = err.Error()
		return m, nil
	}
	return m.prepareResourceMutation(mutation)
}

func (m browserModel) resourceFormView() (string, []string) {
	f := m.resource.form
	if f.discard {
		return "Unsaved resource fields", []string{"s: review changes", "d: discard fields", "Esc: continue editing"}
	}
	rows := max(1, m.height-6)
	start := max(0, f.index-rows+1)
	lines := []string{}
	for i := start; i < min(len(f.fields), start+rows); i++ {
		field := f.fields[i]
		prefix := marker(i == f.index) + field.label + ": "
		value := displayClipped(string(field.input.value), max(1, m.width-4-len(prefix)))
		if i == f.index {
			value = field.input.view(max(1, m.width-4-len(prefix)))
		}
		lines = append(lines, prefix+value)
	}
	return strings.Title(f.action) + " " + resourceTitle(f.kind), lines
}
