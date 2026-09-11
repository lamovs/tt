package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/store"
)

type ResourceQuery struct {
	Kind      string        `json:"kind"`
	ProjectID string        `json:"project_id,omitempty"`
	TaskID    string        `json:"task_id,omitempty"`
	HabitID   string        `json:"habit_id,omitempty"`
	From      string        `json:"from,omitempty"`
	To        string        `json:"to,omitempty"`
	FocusType api.FocusType `json:"focus_type,omitempty"`
	ID        string        `json:"id,omitempty"`
}

func (q ResourceQuery) parent() string {
	switch q.Kind {
	case "column":
		return q.ProjectID
	case "comment":
		return q.TaskID
	case "checkin":
		return q.HabitID
	case "focus":
		return strconv.Itoa(int(q.FocusType))
	}
	return ""
}

func (q ResourceQuery) cacheKey() string {
	raw, _ := json.Marshal(q)
	sum := sha256.Sum256(raw)
	return "resource_query_" + hex.EncodeToString(sum[:])
}

type ResourceListing struct {
	Entities []store.ResourceEntity `json:"entities"`
	Meta     ResultMeta             `json:"meta"`
}

type Resources struct {
	store  *store.Store
	client func() (*api.Client, error)
}

func NewResources(st *store.Store, client func() (*api.Client, error)) *Resources {
	return &Resources{store: st, client: client}
}

type resourceCache struct {
	Meta  ResultMeta     `json:"meta"`
	IDs   []string       `json:"ids"`
	Query *ResourceQuery `json:"query,omitempty"`
}

func (r *Resources) List(ctx context.Context, q ResourceQuery, remote bool) (ResourceListing, error) {
	if err := validateResourceQuery(q); err != nil {
		return ResourceListing{}, err
	}
	if !remote {
		entities, err := r.store.Entities(ctx, q.Kind, q.parent(), false)
		if err != nil {
			return ResourceListing{}, err
		}
		cached := resourceCache{Meta: ResultMeta{Completeness: "unknown"}}
		text, exists, err := r.store.Meta(ctx, q.cacheKey())
		if err != nil {
			return ResourceListing{}, err
		}
		if exists {
			if err := json.Unmarshal([]byte(text), &cached); err != nil {
				return ResourceListing{}, errors.New("invalid resource query metadata")
			}
		}
		if q.Kind == "focus" {
			if q.ID != "" {
				entities = filterResourceIDs(entities, []string{strconv.Itoa(int(q.FocusType)) + "/" + q.ID})
			} else {
				entities = filterResourceIDs(entities, cached.IDs)
			}
		}
		if q.Kind == "checkin" {
			entities = filterCheckinDates(entities, q.From, q.To)
		}
		cached.Meta.Source = "local"
		cached.Meta.Pending = resourcePending(entities)
		return ResourceListing{Entities: entities, Meta: cached.Meta}, nil
	}
	if r.client == nil {
		return ResourceListing{}, errors.New("remote resource access is unavailable")
	}
	client, err := r.client()
	if err != nil {
		return ResourceListing{}, err
	}
	fence, err := r.store.CaptureEntityCollectionFence(ctx, q.Kind, q.parent())
	if err != nil {
		return ResourceListing{}, err
	}
	entities, coverage, err := fetchResources(ctx, client, q)
	if err != nil {
		return ResourceListing{}, err
	}
	// A collection response does not prove deletion or a stable server snapshot.
	if err := r.store.MergeEntitiesFenced(ctx, entities, false, fence); err != nil {
		return ResourceListing{}, err
	}
	ids := make([]string, 0, len(entities))
	for _, entity := range entities {
		ids = append(ids, entity.ServerID)
	}
	current, err := r.store.Entities(ctx, q.Kind, q.parent(), false)
	if err != nil {
		return ResourceListing{}, err
	}
	if q.Kind == "focus" {
		current = filterResourceIDs(current, ids)
	} else {
		current = filterRemoteResourceIDs(current, ids)
	}
	if q.Kind == "checkin" {
		current, err = r.store.Entities(ctx, q.Kind, q.parent(), false)
		if err != nil {
			return ResourceListing{}, err
		}
		current = filterCheckinDates(current, q.From, q.To)
	}
	meta := ResultMeta{Source: "remote", FetchedAt: time.Now().UTC(), Completeness: coverage, Pending: resourcePending(current)}
	raw, err := json.Marshal(resourceCache{Meta: meta, IDs: ids, Query: &q})
	if err != nil {
		return ResourceListing{}, err
	}
	if err := r.store.SetMeta(ctx, q.cacheKey(), string(raw)); err != nil {
		return ResourceListing{}, err
	}
	return ResourceListing{Entities: current, Meta: meta}, nil
}

func validateResourceQuery(q ResourceQuery) error {
	switch q.Kind {
	case "project", "folder", "tag", "habit", "countdown":
	case "column":
		if q.ProjectID == "" {
			return errors.New("select a project for columns")
		}
	case "comment":
		if q.ProjectID == "" || q.TaskID == "" {
			return errors.New("select a task and its project for comments")
		}
	case "focus":
		if q.FocusType != api.FocusPomodoro && q.FocusType != api.FocusTiming {
			return errors.New("focus type must be 0 (Pomodoro) or 1 (Timing)")
		}
		if q.ID != "" {
			return nil
		}
		start, e1 := time.Parse(time.RFC3339, q.From)
		end, e2 := time.Parse(time.RFC3339, q.To)
		if e1 != nil || e2 != nil || !end.After(start) {
			return errors.New("focus history needs an increasing RFC3339 from/to range")
		}
	case "checkin":
		if q.HabitID == "" {
			return errors.New("select a habit for check-in history")
		}
		start, e1 := time.Parse("20060102", q.From)
		end, e2 := time.Parse("20060102", q.To)
		if e1 != nil || e2 != nil || end.Before(start) {
			return errors.New("check-in history needs a valid YYYYMMDD from/to range")
		}
	default:
		return errors.New("unsupported resource kind")
	}
	return nil
}

func resourcePending(entities []store.ResourceEntity) int {
	n := 0
	for _, entity := range entities {
		if entity.Dirty {
			n++
		}
	}
	return n
}

func filterCheckinDates(entities []store.ResourceEntity, from, to string) []store.ResourceEntity {
	start, _ := strconv.Atoi(from)
	end, _ := strconv.Atoi(to)
	out := []store.ResourceEntity{}
	for _, entity := range entities {
		var value struct {
			Stamp int `json:"stamp"`
		}
		if json.Unmarshal(entity.Data, &value) == nil && value.Stamp >= start && value.Stamp <= end {
			out = append(out, entity)
		}
	}
	return out
}

func filterResourceIDs(entities []store.ResourceEntity, ids []string) []store.ResourceEntity {
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	out := []store.ResourceEntity{}
	for _, entity := range entities {
		if allowed[entity.ServerID] {
			out = append(out, entity)
		}
	}
	return out
}

func filterRemoteResourceIDs(entities []store.ResourceEntity, ids []string) []store.ResourceEntity {
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	out := []store.ResourceEntity{}
	for _, entity := range entities {
		if entity.Dirty || allowed[entity.ServerID] {
			out = append(out, entity)
		}
	}
	return out
}

func fetchResources(ctx context.Context, c *api.Client, q ResourceQuery) ([]store.ResourceEntity, string, error) {
	out := []store.ResourceEntity{}
	add := func(id string, raw json.RawMessage, value any) error {
		if len(raw) == 0 {
			var err error
			raw, err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
		if id == "" {
			return errors.New("resource response has no identity")
		}
		out = append(out, store.ResourceEntity{Ref: store.EntityRef{Kind: q.Kind, Key: id}, ServerID: id, ProjectKey: q.parent(), Data: raw})
		return nil
	}
	var err error
	coverage := "unknown"
	switch q.Kind {
	case "project":
		seen := map[string]bool{}
		for offset := 0; ; offset += 200 {
			if offset >= 20000 {
				return nil, "partial", errors.New("project pagination exceeded the bounded page budget")
			}
			items, fetchErr := c.ListProjectsPage(ctx, offset, 200)
			if fetchErr != nil {
				return nil, "partial", fetchErr
			}
			for _, item := range items {
				if seen[item.ID] {
					return nil, "partial", errors.New("project pagination repeated an ID; refresh the collection")
				}
				seen[item.ID] = true
				if err := add(item.ID, item.Raw, item); err != nil {
					return nil, "unknown", err
				}
			}
			if len(items) < 200 {
				coverage = "complete"
				break
			}
		}
	case "folder":
		var items []api.ProjectGroup
		items, err = c.ListProjectGroups(ctx)
		for _, item := range items {
			if e := add(item.ID, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "column":
		var items []api.Column
		items, err = c.ListColumns(ctx, q.ProjectID)
		for _, item := range items {
			if item.ProjectID != q.ProjectID {
				return nil, coverage, errors.New("column response belongs to a different project")
			}
			if e := add(item.ID, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "tag":
		var items []api.Tag
		items, err = c.ListTags(ctx)
		for _, item := range items {
			if e := add(item.Name, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "habit":
		var items []api.Habit
		items, err = c.ListHabits(ctx)
		for _, item := range items {
			if e := add(item.ID, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "comment":
		var items []api.Comment
		items, err = c.ListTaskComments(ctx, q.ProjectID, q.TaskID)
		for _, item := range items {
			if e := add(item.ID, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "countdown":
		var items []api.Countdown
		items, err = c.ListCountdowns(ctx)
		for _, item := range items {
			if e := add(item.ID, item.Raw, item); e != nil {
				return nil, coverage, e
			}
		}
	case "checkin":
		from, _ := strconv.Atoi(q.From)
		to, _ := strconv.Atoi(q.To)
		var groups []api.HabitCheckin
		groups, err = c.GetHabitCheckins(ctx, api.HabitCheckinQuery{HabitIDs: []string{q.HabitID}, From: from, To: to})
		for _, group := range groups {
			if group.HabitID != q.HabitID {
				return nil, coverage, errors.New("check-in response belongs to a different habit")
			}
			for _, item := range group.Checkins {
				if item.Stamp < from || item.Stamp > to {
					continue
				}
				id := group.HabitID + "/" + strconv.Itoa(item.Stamp)
				if e := add(id, item.Raw, item); e != nil {
					return nil, coverage, e
				}
			}
		}
	case "focus":
		if q.ID != "" {
			item, err := c.GetFocus(ctx, q.ID, q.FocusType)
			if err != nil {
				return nil, coverage, err
			}
			if item.ID != q.ID || item.Type != q.FocusType {
				return nil, coverage, errors.New("remote focus identity mismatch")
			}
			if err := add(fmt.Sprintf("%d/%s", item.Type, item.ID), item.Raw, item); err != nil {
				return nil, coverage, err
			}
			return out, "complete", nil
		}
		start, _ := time.Parse(time.RFC3339, q.From)
		end, _ := time.Parse(time.RFC3339, q.To)
		seen := map[string]bool{}
		for page := 0; start.Before(end); page++ {
			if page >= 120 {
				return nil, "partial", errors.New("focus history exceeded the bounded page budget")
			}
			next := start.Add(30 * 24 * time.Hour)
			if next.After(end) {
				next = end
			}
			items, fetchErr := c.GetFocuses(ctx, start.Format(time.RFC3339), next.Format(time.RFC3339), q.FocusType)
			if fetchErr != nil {
				return nil, "partial", fetchErr
			}
			for _, item := range items {
				if item.Type != q.FocusType {
					return nil, coverage, errors.New("focus response has an unexpected type")
				}
				id := fmt.Sprintf("%d/%s", item.Type, item.ID)
				if seen[id] {
					continue
				}
				seen[id] = true
				if e := add(id, item.Raw, item); e != nil {
					return nil, coverage, e
				}
			}
			start = next
		}
	}
	return out, coverage, err
}
