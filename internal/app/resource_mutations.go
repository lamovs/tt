package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

type ResourcePreview struct {
	ID      string                      `json:"id"`
	Preview store.EntityMutationPreview `json:"preview"`
	Undo    string                      `json:"undo"`
}

func (r *Resources) Queue(ctx context.Context) ([]store.EntityOperation, error) {
	return r.store.EntityOperationSummaries(ctx)
}

func (r *Resources) Cancel(ctx context.Context, seq, revision int64) error {
	return r.store.CancelEntityOperation(ctx, seq, revision)
}

func (r *Resources) Recover(ctx context.Context, seq, revision int64) error {
	return r.store.RetryEntityOperation(ctx, seq, revision)
}

func (r *Resources) Prepare(ctx context.Context, mutation store.EntityMutation) (ResourcePreview, error) {
	if err := validateResourceMutation(mutation); err != nil {
		return ResourcePreview{}, err
	}
	if mutation.Ref.Kind == "checkin" {
		var input api.HabitCheckinInput
		if err := decodeResourceInput(mutation.Patch, &input); err != nil {
			return ResourcePreview{}, err
		}
		stamp := strconv.Itoa(input.Stamp)
		mutation.Ref.Key = mutation.ProjectKey + "/" + stamp
		entity, err := r.store.Entity(ctx, mutation.Ref)
		if err == nil && !entity.Deleted {
			mutation.Action = "update"
		} else if !errors.Is(err, store.ErrNotFound) {
			if err == nil {
				err = store.ErrEntityChanged
			}
			return ResourcePreview{}, err
		} else {
			q := ResourceQuery{Kind: "checkin", HabitID: mutation.ProjectKey, From: stamp, To: stamp}
			_, exists, err := r.store.Meta(ctx, q.cacheKey())
			if err != nil {
				return ResourcePreview{}, err
			}
			if !exists {
				return ResourcePreview{}, errors.New("refresh this exact habit date with history --remote before preparing its first check-in")
			}
		}
	}
	if mutation.Action == "create" && mutation.Ref.Key == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return ResourcePreview{}, err
		}
		mutation.Ref.Key = "local:" + hex.EncodeToString(nonce[:])
	}
	if mutation.ProjectKey != "" && mutation.Ref.Kind == "column" {
		project, err := r.store.Project(ctx, mutation.ProjectKey)
		if err != nil {
			return ResourcePreview{}, err
		}
		if project.Closed {
			return ResourcePreview{}, errors.New("the project is closed")
		}
	}
	if mutation.Ref.Kind == "comment" {
		task, err := r.store.Task(ctx, mutation.ProjectKey)
		if err != nil {
			return ResourcePreview{}, err
		}
		if store.IsLocalID(task.Id) {
			return ResourcePreview{}, errors.New("sync this task before preparing a comment")
		}
	}
	preview, err := r.store.PreviewEntityMutation(ctx, mutation)
	if err != nil {
		return ResourcePreview{}, err
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		return ResourcePreview{}, err
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if err := r.store.SetMeta(ctx, "entity_preview_"+id, string(raw)); err != nil {
		return ResourcePreview{}, err
	}
	undo := "cancel before sending; remote reversal depends on the operation"
	if mutation.Action == "delete" {
		undo = "cancel before sending; remote deletion cannot be undone"
	}
	return ResourcePreview{ID: id, Preview: preview, Undo: undo}, nil
}

func (r *Resources) Apply(ctx context.Context, id, kind, action string) (store.EntityMutationOutcome, error) {
	if len(id) != 64 {
		return store.EntityMutationOutcome{}, errors.New("invalid preview ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return store.EntityMutationOutcome{}, errors.New("invalid preview ID")
	}
	text, exists, err := r.store.Meta(ctx, "entity_preview_"+id)
	if err != nil {
		return store.EntityMutationOutcome{}, err
	}
	if !exists {
		return store.EntityMutationOutcome{}, errors.New("preview is not available; prepare it again")
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != id {
		return store.EntityMutationOutcome{}, errors.New("preview metadata failed validation")
	}
	var preview store.EntityMutationPreview
	if err := decodeResourceInput(json.RawMessage(text), &preview); err != nil {
		return store.EntityMutationOutcome{}, err
	}
	if preview.Mutation.Ref.Kind != kind || preview.Mutation.Action != action && !(kind == "checkin" && action == "create" && preview.Mutation.Action == "update") {
		return store.EntityMutationOutcome{}, errors.New("preview belongs to a different operation")
	}
	if err := validateResourceMutation(preview.Mutation); err != nil {
		return store.EntityMutationOutcome{}, err
	}
	return r.store.ApplyEntityMutation(ctx, preview)
}

func decodeResourceInput(raw json.RawMessage, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing request data")
	}
	return nil
}

func validateResourceMutation(m store.EntityMutation) error {
	if len(m.References) != 0 {
		return errors.New("resource reference writes are not verified")
	}
	if m.Action != "create" && m.Action != "update" && m.Action != "delete" {
		return errors.New("unsupported resource action")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(m.Patch, &fields); err != nil || fields == nil {
		return errors.New("resource patch must be an object")
	}
	if m.Action == "delete" {
		if len(fields) != 0 {
			return errors.New("delete cannot contain fields")
		}
		switch m.Ref.Kind {
		case "comment", "focus":
			return nil
		case "project", "folder":
			return errors.New("container deletion is gated until cascade consequences and complete inventory are verified")
		default:
			return errors.New("resource deletion has no verified API contract")
		}
	}
	if len(fields) == 0 {
		return errors.New("provide at least one editable field")
	}
	var out any
	switch m.Ref.Kind {
	case "project":
		allowed := map[string]bool{"name": true, "color": true, "sortOrder": true, "viewMode": true, "kind": true}
		for field := range fields {
			if !allowed[field] {
				return fmt.Errorf("project field %s is not writable", field)
			}
		}
		var input api.ProjectUpdate
		out = &input
	case "folder":
		out = &api.ProjectGroupUpdate{}
	case "column":
		if m.ProjectKey == "" {
			return errors.New("column needs a project")
		}
		out = &api.ColumnUpdate{}
	case "tag":
		if m.Action != "create" {
			return errors.New("tag editing has no verified API contract")
		}
		out = &api.TagCreate{}
	case "habit":
		out = &api.HabitUpdate{}
	case "comment":
		if m.Action != "create" {
			return errors.New("comment editing has no verified API contract")
		}
		if m.ProjectKey == "" {
			return errors.New("comment needs a task")
		}
		out = &api.CommentCreate{}
	case "checkin":
		if m.ProjectKey == "" {
			return errors.New("check-in needs a habit")
		}
		out = &api.HabitCheckinInput{}
	default:
		return errors.New("resource writing is unavailable")
	}
	if err := decodeResourceInput(m.Patch, out); err != nil {
		return fmt.Errorf("invalid resource fields: %w", err)
	}
	if input, ok := out.(*api.HabitCheckinInput); ok {
		if _, err := time.Parse("20060102", strconv.Itoa(input.Stamp)); err != nil {
			return errors.New("check-in needs a valid YYYYMMDD stamp")
		}
		if input.Value == nil && input.Status == nil {
			return errors.New("check-in sets an explicit value or status, never an increment")
		}
		if input.Status != nil && (*input.Status < 0 || *input.Status > 2) {
			return errors.New("check-in status must be 0, 1, or 2")
		}
		for _, stamp := range []*string{input.Time, input.OpTime} {
			if stamp != nil {
				if _, err := time.Parse(time.RFC3339, *stamp); err != nil {
					return errors.New("check-in timestamps must use RFC3339")
				}
			}
		}
	}
	if m.Action == "create" {
		nameField := "name"
		if m.Ref.Kind == "comment" {
			nameField = "title"
		}
		if m.Ref.Kind != "checkin" {
			var name string
			if err := json.Unmarshal(fields[nameField], &name); err != nil || strings.TrimSpace(name) == "" {
				return errors.New("a nonempty name or title is required")
			}
		}
	}
	for _, field := range []string{"name", "title", "label", "color", "type", "unit", "viewMode", "kind"} {
		if raw, exists := fields[field]; exists {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
				return fmt.Errorf("%s must be valid text without controls", field)
			}
			if (field == "name" || field == "title") && strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s cannot be blank", field)
			}
			if field == "name" || field == "label" {
				limit := 1000
				if m.Ref.Kind == "folder" || m.Ref.Kind == "tag" {
					limit = 64
				}
				if utf8.RuneCountInString(value) > limit {
					return fmt.Errorf("%s exceeds the provider length limit", field)
				}
			}
		}
	}
	if m.Ref.Kind == "tag" {
		var input api.TagCreate
		if err := json.Unmarshal(m.Patch, &input); err != nil {
			return err
		}
		if input.Name != strings.ToLower(strings.TrimSpace(input.Name)) || strings.ToLower(input.Label) != input.Name {
			return errors.New("tag name must be normalized and match its display label")
		}
	}
	return nil
}
