package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/focus"
	"github.com/movsar/tt/internal/store"
)

type SyncRefresh struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Count   int    `json:"count"`
	Message string `json:"message,omitempty"`
}

func (r SyncRefresh) Summary() string {
	text := fmt.Sprintf("%s: %s", r.Name, r.State)
	if r.State == "refreshed" {
		text += fmt.Sprintf(" %d", r.Count)
	}
	if r.Message != "" {
		text += "; " + r.Message
	}
	return text
}

func RefreshTimerTopics(ctx context.Context, st *store.Store, factory focus.TopicClientFactory) (SyncRefresh, focus.TopicClientFactory) {
	out := SyncRefresh{Name: "Timer topics", State: "skipped", Message: "Browser authorization not configured"}
	if factory == nil {
		return out, nil
	}
	client, err := factory()
	if errors.Is(err, focus.ErrBrowserNotConfigured) {
		return out, func() (focus.TopicClient, error) { return nil, err }
	}
	if err == nil {
		if client == nil {
			err = errors.New("Browser client is unavailable")
		} else {
			timers := NewTimers(st, config.Default(), nil, nil, nil).WithTopicClient(func() (focus.TopicClient, error) { return client, nil })
			out.Count, err = timers.RefreshTopics(ctx)
		}
	}
	if err != nil {
		out.State, out.Message = "failed", err.Error()
	} else {
		out.State, out.Message = "refreshed", ""
	}
	return out, func() (focus.TopicClient, error) { return client, err }
}

func (r *Resources) RefreshCatalogs(ctx context.Context) []SyncRefresh {
	queries := []ResourceQuery{{Kind: "project"}, {Kind: "folder"}, {Kind: "tag"}, {Kind: "habit"}, {Kind: "countdown"}}
	projects, err := r.store.Projects(ctx)
	if err != nil {
		return []SyncRefresh{{Name: "Resource catalogs", State: "failed", Message: err.Error()}}
	}
	for _, project := range projects {
		if !store.IsLocalID(project.Id) {
			queries = append(queries, ResourceQuery{Kind: "column", ProjectID: project.Id})
		}
	}
	known, err := r.rememberedQueries(ctx)
	if err != nil {
		return []SyncRefresh{{Name: "Resource catalogs", State: "failed", Message: err.Error()}}
	}
	queries = append(queries, known...)
	var out []SyncRefresh
	seen := map[string]bool{}
	for _, query := range queries {
		if ctx.Err() != nil {
			out = append(out, SyncRefresh{Name: "Remaining catalogs", State: "failed", Message: ctx.Err().Error()})
			break
		}
		if seen[query.cacheKey()] {
			continue
		}
		seen[query.cacheKey()] = true
		name := query.Kind
		if query.parent() != "" {
			name += " " + query.parent()
		}
		result, err := r.List(ctx, query, true)
		row := SyncRefresh{Name: name, State: "refreshed", Count: len(result.Entities)}
		if err != nil {
			row.State, row.Message = "failed", err.Error()
		}
		out = append(out, row)
		if errors.Is(err, api.ErrUnauthorized) {
			break
		}
	}
	return out
}

func (r *Resources) rememberedQueries(ctx context.Context) ([]ResourceQuery, error) {
	rows, err := r.store.DB().QueryContext(ctx, "SELECT value FROM meta WHERE key GLOB 'resource_query_*' ORDER BY key LIMIT 1001")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ResourceQuery
	count := 0
	for rows.Next() {
		count++
		if count > 1000 {
			return nil, errors.New("resource query refresh exceeds 1000 cached scopes; use individual --remote commands")
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var cached resourceCache
		if json.Unmarshal([]byte(raw), &cached) != nil {
			return nil, errors.New("invalid resource query metadata")
		}
		if cached.Query == nil {
			continue
		}
		switch cached.Query.Kind {
		case "comment", "checkin", "focus":
			if err := validateResourceQuery(*cached.Query); err != nil {
				return nil, err
			}
			result = append(result, *cached.Query)
		}
	}
	return result, rows.Err()
}
