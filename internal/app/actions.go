package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type Draft struct {
	ProjectID, Title, Body string
	Kind                   string
	Priority               model.Priority
	ReminderWhen           string
	ReminderDate           dates.Result
	Interval               dates.Interval
}

func (d Draft) Validate() error {
	if !d.Interval.Start.IsZero() {
		if d.ReminderWhen != "" {
			return errors.New("choose Remind at or Start/Duration, not both")
		}
		if err := d.Interval.Validate(); err != nil {
			return err
		}
	}
	if d.Kind != "" && d.Kind != "TEXT" && d.Kind != "NOTE" {
		return errors.New("new kind must be TEXT or NOTE")
	}
	if d.ReminderWhen != "" && (d.ReminderDate.Time.IsZero() || d.ReminderDate.Clear || d.ReminderDate.AllDay) {
		return errors.New("a reminder shortcut needs a valid date and time")
	}
	if strings.TrimSpace(d.Title) == "" {
		return errors.New("title is required")
	}
	if strings.ContainsFunc(d.Title, unicode.IsControl) {
		return errors.New("title must not contain control characters")
	}
	if strings.ContainsRune(d.Body, 0) {
		return errors.New("body cannot contain NUL")
	}
	_, err := model.PriorityFromWire(d.Priority.Wire())
	return err
}

type CacheState struct {
	Queue        store.OutboxCounts
	Replacements map[string]string
}

type UndoPreview struct {
	Entry store.UndoEntry
	Title string
}

type Actions struct {
	store  *store.Store
	config config.Config
	client func() (*api.Client, error)
	topics focus.TopicClientFactory
}

func NewActions(st *store.Store, cfg config.Config, client func() (*api.Client, error)) *Actions {
	return &Actions{store: st, config: cfg, client: client}
}

func (a *Actions) WithTopicClient(client focus.TopicClientFactory) *Actions {
	a.topics = client
	return a
}

func (a *Actions) State(ctx context.Context, ids []string) (CacheState, error) {
	out := CacheState{Replacements: make(map[string]string)}
	var err error
	out.Queue, err = a.store.OutboxCounts(ctx)
	if err != nil {
		return out, err
	}
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		next, err := a.store.CurrentTaskID(ctx, id)
		if err != nil {
			return out, err
		}
		if next != id {
			out.Replacements[id] = next
		}
	}
	return out, nil
}

func (a *Actions) Create(ctx context.Context, d Draft) (store.TaskMutationOutcome, error) {
	if !d.Interval.Start.IsZero() {
		p, err := a.PreviewCreateInterval(ctx, d)
		if err != nil {
			return store.TaskMutationOutcome{}, err
		}
		return a.ApplyInterval(ctx, p, false)
	}
	if err := d.Validate(); err != nil {
		return store.TaskMutationOutcome{}, err
	}
	project, err := a.store.Project(ctx, d.ProjectID)
	if err != nil {
		return store.TaskMutationOutcome{}, err
	}
	if reason := project.CreateUnavailable(); reason != "" {
		return store.TaskMutationOutcome{}, errors.New(reason + "; refresh and choose another list")
	}
	kind := d.Kind
	if kind == "" {
		kind = "TEXT"
	}
	taskInput := model.Task{ProjectId: d.ProjectID, Title: d.Title, Content: d.Body, Priority: d.Priority, Kind: kind}
	if d.ReminderWhen != "" {
		taskInput.DueDate = d.ReminderDate.Time
		taskInput.Reminders = []string{"TRIGGER:PT0S"}
	}
	task, err := a.store.CreateTask(ctx, taskInput)
	return store.TaskMutationOutcome{Task: task, Changed: err == nil}, err
}

func (a *Actions) Edit(ctx context.Context, original model.Task, d Draft) (store.TaskMutationOutcome, error) {
	if err := d.Validate(); err != nil {
		return store.TaskMutationOutcome{}, err
	}
	if d.Kind != "" && d.Kind != original.Kind {
		return store.TaskMutationOutcome{}, errors.New("kind is read-only for an existing task")
	}
	if d.ProjectID != original.ProjectId {
		return store.TaskMutationOutcome{}, errors.New("editing cannot move a task")
	}
	var edit model.TaskEdit
	if d.Title != original.Title {
		edit.Title = model.Ptr(d.Title)
	}
	if d.Body != original.Content {
		edit.Content = model.Ptr(d.Body)
	}
	if d.Priority != original.Priority {
		edit.Priority = model.Ptr(d.Priority)
	}
	return a.store.UpdateTaskIfUnchanged(ctx, original, edit)
}

func (a *Actions) Complete(ctx context.Context, original model.Task, keepItems bool) (store.TaskMutationOutcome, error) {
	return a.store.CompleteTaskIfUnchanged(ctx, original, store.CompleteOptions{KeepItems: keepItems})
}

func (a *Actions) PreviewUndo(ctx context.Context) (UndoPreview, error) {
	entry, err := a.store.LastUndo(ctx)
	if err != nil {
		return UndoPreview{}, err
	}
	switch entry.Action.Op {
	case store.OpEntityMutation:
		entity, err := a.store.PreviewEntityUndo(ctx, entry)
		if err != nil {
			return UndoPreview{}, err
		}
		var fields struct {
			Name  string `json:"name"`
			Title string `json:"title"`
		}
		_ = json.Unmarshal(entity.Data, &fields)
		title := fields.Name
		if title == "" {
			title = fields.Title
		}
		if title == "" {
			title = entity.Ref.Key
		}
		return UndoPreview{Entry: entry, Title: title}, nil
	case store.OpTaskCreate, store.OpTaskUpdate, store.OpTaskComplete, store.OpTaskDelete, store.OpTaskMoveNative:
	default:
		return UndoPreview{}, fmt.Errorf("undo %s is unsupported; history retained", entry.Action.Op)
	}
	task, err := a.store.Task(ctx, entry.Action.TaskID)
	if err != nil {
		if entry.Action.Task == nil {
			return UndoPreview{}, err
		}
		task = *entry.Action.Task
	}
	return UndoPreview{Entry: entry, Title: task.Title}, nil
}

func (a *Actions) Undo(ctx context.Context, expected UndoPreview) (store.TaskMutationOutcome, error) {
	task, err := a.store.ApplyUndo(ctx, expected.Entry)
	return store.TaskMutationOutcome{Task: task, Changed: err == nil}, err
}

func (a *Actions) Sync(ctx context.Context, phase func(string)) SyncOutcome {
	return RunSync(ctx, a.store, a.config, a.client, phase, a.topics)
}
