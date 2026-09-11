package taskdoc

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/editor"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

type Fields struct {
	ID                string   `toml:"id"`
	ProjectID         string   `toml:"project_id"`
	Title             string   `toml:"title"`
	Kind              string   `toml:"kind"`
	Priority          string   `toml:"priority"`
	Due               string   `toml:"due"`
	Repeat            string   `toml:"repeat"`
	Reminders         []string `toml:"reminders"`
	Tags              []string `toml:"tags"`
	ParentID          *string  `toml:"parent_id,omitempty"`
	ColumnID          *string  `toml:"column_id,omitempty"`
	EstimatedDuration *int64   `toml:"estimated_duration,omitempty"`
	EstimatedPomo     *int     `toml:"estimated_pomo,omitempty"`
	SortOrder         *int64   `toml:"sort_order,omitempty"`
}

func FieldsOf(task model.Task) Fields {
	due := "none"
	if !task.DueDate.IsZero() {
		due = task.DueDate.In(time.Local).Format("02.01.2006")
		if !task.IsAllDay {
			due += task.DueDate.In(time.Local).Format(" 15:04")
		}
	}
	repeat := task.RepeatFlag
	if repeat == "" {
		repeat = "none"
	}
	return Fields{ID: task.Id, ProjectID: task.ProjectId, Title: task.Title, Kind: task.Kind,
		ParentID: model.Ptr(task.ParentId), ColumnID: model.Ptr(task.ColumnId),
		EstimatedDuration: model.Ptr(task.EstimatedDuration), EstimatedPomo: model.Ptr(task.EstimatedPomo), SortOrder: model.Ptr(task.SortOrder),
		Priority: task.Priority.String(), Due: due, Repeat: repeat,
		Reminders: append([]string{}, task.Reminders...), Tags: append([]string{}, task.Tags...)}
}

func Encode(task model.Task, hints model.EditorHints) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("+++\n")
	if hints != model.HintsNone {
		b.WriteString("# Edit fields above the closing +++; Markdown below it is the exact body.\n")
		if hints == model.HintsFull {
			b.WriteString("# Keep every field. id and project_id are read-only; choose the list before editing.\n" +
				"# New kind: TEXT or NOTE. Existing kind and checklist items stay unchanged.\n" +
				"# priority: none, low, med, high. due: none, tmr, fri 9:00, 21.10.2026.\n" +
				"# Clearing due also clears the start date, as required by TickTick.\n" +
				"# repeat: none, daily, weekly, monthly, yearly, or RRULE:...\n" +
				"# reminders: ordered TRIGGER:... strings. Empty arrays clear lists.\n" +
				"# Example values: due = \"21.10.2026 09:30\", priority = \"high\", repeat = \"weekly\".\n" +
				"# Example arrays: reminders = [\"TRIGGER:-PT10M\"], tags = [\"work\", \"urgent\"].\n" +
				"# Save and close to apply locally; tt sync sends the queued change.\n" +
				"# An unchanged document cancels. Failed edits keep this draft for recovery.\n")
		}
	}
	if err := toml.NewEncoder(&b).Encode(FieldsOf(task)); err != nil {
		return nil, err
	}
	b.WriteString("+++\n")
	b.WriteString(task.Content)
	if b.Len() > editor.MaxSize {
		return nil, errors.New("editor document exceeds the 4 MiB limit")
	}
	return b.Bytes(), nil
}

func Decode(data []byte) (Fields, string, error) {
	var fields Fields
	if len(data) > editor.MaxSize || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fields, "", errors.New("editor document must be UTF-8 text without NUL bytes, at most 4 MiB")
	}
	line, rest, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte("+++")) {
		return fields, "", errors.New("editor document must begin with a +++ TOML frontmatter line")
	}
	front := rest
	consumed := 0
	for {
		line, tail, newline := bytes.Cut(rest, []byte("\n"))
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte("+++")) {
			meta, err := toml.Decode(string(front[:consumed]), &fields)
			if err != nil {
				return fields, "", fmt.Errorf("invalid TOML frontmatter: %w", err)
			}
			if len(meta.Undecoded()) != 0 {
				return fields, "", errors.New("unknown frontmatter field; keep only the fields from the template")
			}
			keys := []string{"id", "project_id", "title", "kind", "priority", "due", "repeat", "reminders", "tags"}
			optionalKeys := []string{"parent_id", "column_id", "estimated_duration", "estimated_pomo", "sort_order"}
			for _, key := range meta.Keys() {
				if len(key) != 1 || !slices.Contains(keys, key[0]) && !slices.Contains(optionalKeys, key[0]) {
					return fields, "", errors.New("unknown frontmatter field; field names must match the template exactly")
				}
			}
			for _, key := range keys {
				if !meta.IsDefined(key) {
					return fields, "", fmt.Errorf("missing frontmatter field %s; restore it from the template", key)
				}
			}
			return fields, string(tail), nil
		}
		if !newline {
			return fields, "", errors.New("editor document needs a closing +++ frontmatter line")
		}
		consumed += len(line) + 1
		rest = tail
	}
}

func Apply(original model.Task, data []byte, creating bool) (model.Task, model.TaskEdit, error) {
	fields, body, err := Decode(data)
	if err != nil {
		return original, model.TaskEdit{}, err
	}
	before := FieldsOf(original)
	if reflect.DeepEqual(fields, before) && body == original.Content {
		return original, model.TaskEdit{}, nil
	}
	if fields.ID != before.ID || fields.ProjectID != before.ProjectID {
		return original, model.TaskEdit{}, errors.New("id and project_id are read-only; select a new task's list with -P or use tt mv for an existing task")
	}
	for _, value := range append([]string{fields.Title, fields.Kind, fields.Priority, fields.Due, fields.Repeat}, append(slices.Clone(fields.Reminders), fields.Tags...)...) {
		if strings.ContainsFunc(value, unicode.IsControl) {
			return original, model.TaskEdit{}, errors.New("frontmatter values must not contain control characters")
		}
	}
	if strings.TrimSpace(fields.Title) == "" {
		return original, model.TaskEdit{}, errors.New("title must not be empty")
	}
	if creating {
		if fields.Kind != "TEXT" && fields.Kind != "NOTE" {
			return original, model.TaskEdit{}, errors.New("new kind must be TEXT or NOTE")
		}
		if fields.Kind == "NOTE" && len(original.Items) != 0 {
			return original, model.TaskEdit{}, errors.New("NOTE cannot contain --item checklist entries")
		}
	} else if fields.Kind != original.Kind {
		return original, model.TaskEdit{}, errors.New("kind is read-only for an existing task")
	}
	priority, err := model.ParsePriority(fields.Priority)
	if err != nil {
		return original, model.TaskEdit{}, err
	}
	var edit model.TaskEdit
	next := original
	for _, value := range []*string{fields.ParentID, fields.ColumnID} {
		if value != nil && strings.ContainsFunc(*value, unicode.IsControl) {
			return original, model.TaskEdit{}, errors.New("task references must not contain control characters")
		}
	}
	if fields.EstimatedDuration != nil && *fields.EstimatedDuration < 0 {
		return original, model.TaskEdit{}, errors.New("estimated_duration must be nonnegative seconds")
	}
	if fields.EstimatedPomo != nil && (*fields.EstimatedPomo < 0 || *fields.EstimatedPomo > 60) {
		return original, model.TaskEdit{}, errors.New("estimated_pomo must be between 0 and 60")
	}
	if fields.ParentID != nil && *fields.ParentID != original.ParentId {
		next.ParentId = *fields.ParentID
		edit.ParentId = model.Ptr(*fields.ParentID)
	}
	if fields.ColumnID != nil && *fields.ColumnID != original.ColumnId {
		next.ColumnId = *fields.ColumnID
		next.ColumnName = ""
		edit.ColumnId = model.Ptr(*fields.ColumnID)
	}
	if fields.EstimatedDuration != nil && *fields.EstimatedDuration != original.EstimatedDuration {
		next.EstimatedDuration = *fields.EstimatedDuration
		edit.EstimatedDuration = model.Ptr(*fields.EstimatedDuration)
	}
	if fields.EstimatedPomo != nil && *fields.EstimatedPomo != original.EstimatedPomo {
		next.EstimatedPomo = *fields.EstimatedPomo
		edit.EstimatedPomo = model.Ptr(*fields.EstimatedPomo)
	}
	if fields.SortOrder != nil && *fields.SortOrder != original.SortOrder {
		next.SortOrder = *fields.SortOrder
		edit.SortOrder = model.Ptr(*fields.SortOrder)
	}
	if fields.Title != original.Title {
		next.Title = fields.Title
		edit.Title = model.Ptr(fields.Title)
	}
	if body != original.Content {
		next.Content = body
		edit.Content = model.Ptr(body)
	}
	if priority != original.Priority {
		next.Priority = priority
		edit.Priority = model.Ptr(priority)
	}
	if creating {
		next.Kind = fields.Kind
	}

	if fields.Due != before.Due {
		due, err := dates.ParseNow(fields.Due)
		if err != nil {
			return original, model.TaskEdit{}, err
		}
		next.DueDate, next.IsAllDay = due.Time, due.AllDay
		if due.Clear {
			next.DueDate = model.Time{}
			next.StartDate = model.Time{}
			next.IsAllDay = false
			edit.StartDate = model.NewEditTime(model.Time{})
			edit.DueDate = model.NewEditTime(model.Time{})
			edit.IsAllDay = model.Ptr(false)
		}
		if !next.DueDate.Equal(original.DueDate.Time) || next.DueDate.IsZero() != original.DueDate.IsZero() {
			edit.DueDate = model.NewEditTime(next.DueDate)
		}
		if next.IsAllDay != original.IsAllDay {
			edit.IsAllDay = model.Ptr(next.IsAllDay)
		}
		if !due.Clear && original.TimeZone != "" {
			next.TimeZone = ""
			edit.TimeZone = model.Ptr("")
		}
	}
	if fields.Repeat != before.Repeat {
		repeat, err := schedule.ParseRepeat(fields.Repeat)
		if err != nil {
			return original, model.TaskEdit{}, err
		}
		if repeat != original.RepeatFlag {
			next.RepeatFlag = repeat
			edit.RepeatFlag = model.Ptr(repeat)
		}
	}
	if creating || !slices.Equal(fields.Reminders, original.Reminders) {
		if err := schedule.ValidateReminders(fields.Reminders); err != nil {
			return original, model.TaskEdit{}, err
		}
	}
	for _, tag := range fields.Tags {
		if strings.TrimSpace(tag) == "" {
			return original, model.TaskEdit{}, errors.New("tags must not be empty")
		}
	}
	if !slices.Equal(fields.Reminders, original.Reminders) {
		next.Reminders = fields.Reminders
		edit.Reminders = model.NewEditList(fields.Reminders)
	}
	if !slices.Equal(fields.Tags, original.Tags) {
		next.Tags = fields.Tags
		edit.Tags = model.NewEditList(fields.Tags)
	}
	if !creating {
		var err error
		edit, err = schedule.ProtectIntervalEnd(original, edit)
		if err != nil {
			return original, model.TaskEdit{}, err
		}
		if schedule.ReviewsInterval(original, edit) {
			next.StartDate, next.TimeZone, next.IsAllDay = original.StartDate, original.TimeZone, false
		}
	}
	return next, edit, nil
}
