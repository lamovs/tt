package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

// ServerTaskQuery is a read-only provider query, not an authoritative task view.
// From/To mean completion time, start time, or due time according to Mode.
type ServerTaskQuery struct {
	Mode       string   `json:"mode"`
	Text       string   `json:"text,omitempty"`
	ProjectIDs []string `json:"project_ids,omitempty"`
	From       string   `json:"from,omitempty"`
	To         string   `json:"to,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Priority   []int    `json:"priority,omitempty"`
	Status     []int    `json:"status,omitempty"`
	Kind       []string `json:"kind,omitempty"`
}

type ServerTaskListing struct {
	Query    ServerTaskQuery   `json:"query"`
	Tasks    []model.Task      `json:"tasks"`
	Raw      []json.RawMessage `json:"-"`
	Meta     ResultMeta        `json:"meta"`
	Warnings []api.Warning     `json:"warnings"`
}

type ServerTaskQueries struct {
	store  *store.Store
	client func() (*api.Client, error)
}

func NewServerTaskQueries(st *store.Store, client func() (*api.Client, error)) *ServerTaskQueries {
	return &ServerTaskQueries{store: st, client: client}
}

type serverTaskCache struct {
	Version int               `json:"version"`
	Query   ServerTaskQuery   `json:"query"`
	Rows    []json.RawMessage `json:"rows"`
	Meta    ResultMeta        `json:"meta"`
}

func (q ServerTaskQuery) cacheKey() string {
	raw, _ := json.Marshal(q)
	sum := sha256.Sum256(raw)
	return "server_task_query_" + hex.EncodeToString(sum[:])
}

func normalizeServerQuery(q ServerTaskQuery) (ServerTaskQuery, error) {
	switch q.Mode {
	case "completed":
		if q.Text != "" || len(q.Tags)+len(q.Priority)+len(q.Status)+len(q.Kind) != 0 {
			return q, errors.New("completed queries accept only projects and completion dates")
		}
	case "filter":
		if q.Text != "" {
			return q, errors.New("filter queries do not accept search text")
		}
	case "search":
		q.Text = strings.TrimSpace(q.Text)
		if q.Text == "" {
			return q, errors.New("server search needs nonblank text")
		}
		if len(q.Priority)+len(q.Kind) != 0 {
			return q, errors.New("search queries do not support priority or kind")
		}
	default:
		return q, errors.New("server query mode must be completed, filter, or search")
	}
	for _, values := range []*[]string{&q.ProjectIDs, &q.Tags, &q.Kind} {
		clean := append([]string(nil), (*values)...)
		seen := map[string]bool{}
		for _, value := range clean {
			if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || seen[value] {
				return q, errors.New("query values must be nonblank, trimmed, and unique")
			}
			seen[value] = true
		}
		sort.Strings(clean)
		*values = clean
	}
	for _, value := range q.Kind {
		if value != "TEXT" && value != "NOTE" && value != "CHECKLIST" {
			return q, errors.New("query kind must be TEXT, NOTE, or CHECKLIST")
		}
	}
	for _, field := range []*[]int{&q.Priority, &q.Status} {
		clean := append([]int(nil), (*field)...)
		sort.Ints(clean)
		for i, value := range clean {
			if i > 0 && value == clean[i-1] {
				return q, errors.New("query numeric values must be unique")
			}
		}
		*field = clean
	}
	for _, value := range q.Priority {
		if value != 0 && value != 1 && value != 3 && value != 5 {
			return q, errors.New("query priority must be 0, 1, 3, or 5")
		}
	}
	for _, value := range q.Status {
		if value != 0 && value != 2 {
			return q, errors.New("query status must be 0 or 2")
		}
	}
	var bounds [2]time.Time
	for i, field := range []*string{&q.From, &q.To} {
		if *field == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339, *field)
		if err != nil {
			return q, errors.New("query dates must be RFC3339 timestamps with an explicit offset")
		}
		bounds[i] = value
		*field = value.UTC().Format(time.RFC3339Nano)
	}
	if !bounds[0].IsZero() && !bounds[1].IsZero() && bounds[1].Before(bounds[0]) {
		return q, errors.New("query end must not be before its start")
	}
	return q, nil
}

func optionalQueryValue[T any](value T, present bool) *T {
	if !present {
		return nil
	}
	return &value
}

func fetchServerQuery(ctx context.Context, c *api.Client, q ServerTaskQuery) ([]api.RawTask, error) {
	projects := optionalQueryValue(q.ProjectIDs, len(q.ProjectIDs) > 0)
	from, to := optionalQueryValue(q.From, q.From != ""), optionalQueryValue(q.To, q.To != "")
	switch q.Mode {
	case "completed":
		return c.GetCompletedTasks(ctx, api.CompletedTaskFilter{ProjectIDs: projects, StartDate: from, EndDate: to})
	case "filter":
		return c.FilterTasks(ctx, api.TaskFilter{ProjectIDs: projects, StartDate: from, EndDate: to,
			Tag: optionalQueryValue(q.Tags, len(q.Tags) > 0), Priority: optionalQueryValue(q.Priority, len(q.Priority) > 0),
			Status: optionalQueryValue(q.Status, len(q.Status) > 0), Kind: optionalQueryValue(q.Kind, len(q.Kind) > 0)})
	default:
		return c.SearchTasks(ctx, api.TaskSearch{Keywords: &q.Text, ProjectIDs: projects, DueFrom: from, DueTo: to,
			Tags: optionalQueryValue(q.Tags, len(q.Tags) > 0), Status: optionalQueryValue(q.Status, len(q.Status) > 0)})
	}
}

func serverQueryListing(cache serverTaskCache) (ServerTaskListing, error) {
	out := ServerTaskListing{Query: cache.Query, Tasks: []model.Task{}, Raw: []json.RawMessage{}, Meta: cache.Meta, Warnings: []api.Warning{}}
	if cache.Version != 1 || cache.Rows == nil || cache.Meta.FetchedAt.IsZero() {
		return out, errors.New("invalid server query cache")
	}
	seen := map[string]bool{}
	for _, raw := range cache.Rows {
		var task api.Task
		if err := json.Unmarshal(raw, &task); err != nil || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.ProjectID) == "" || seen[task.ID] {
			return out, errors.New("invalid task identity or data in server query cache")
		}
		seen[task.ID] = true
		value, warnings := api.TaskToModel(task)
		out.Tasks = append(out.Tasks, value)
		out.Raw = append(out.Raw, append(json.RawMessage(nil), raw...))
		out.Warnings = append(out.Warnings, warnings...)
	}
	// No cursor or stable-snapshot promise is documented for these routes.
	out.Meta.Completeness = "unknown"
	if len(out.Tasks) >= api.TaskQueryLimit {
		out.Meta.Completeness = "partial"
	}
	out.Meta.Pending = 0
	return out, nil
}

func (s *ServerTaskQueries) List(ctx context.Context, q ServerTaskQuery, remote bool) (ServerTaskListing, error) {
	q, err := normalizeServerQuery(q)
	if err != nil {
		return ServerTaskListing{}, err
	}
	if s.store == nil {
		return ServerTaskListing{}, errors.New("server query cache is unavailable")
	}
	if !remote {
		value, ok, err := s.store.Meta(ctx, q.cacheKey())
		if err != nil {
			return ServerTaskListing{}, err
		}
		if !ok {
			return ServerTaskListing{}, errors.New("server query is not cached; repeat with --remote")
		}
		var cache serverTaskCache
		if json.Unmarshal([]byte(value), &cache) != nil || !reflect.DeepEqual(q, cache.Query) {
			return ServerTaskListing{}, errors.New("invalid server query cache")
		}
		cache.Meta.Source = "local"
		return serverQueryListing(cache)
	}
	if s.client == nil {
		return ServerTaskListing{}, errors.New("remote server queries are unavailable")
	}
	client, err := s.client()
	if err != nil {
		return ServerTaskListing{}, err
	}
	rows, err := fetchServerQuery(ctx, client, q)
	if err != nil {
		return ServerTaskListing{}, err
	}
	cache := serverTaskCache{Version: 1, Query: q, Rows: []json.RawMessage{}, Meta: ResultMeta{Source: "remote", FetchedAt: time.Now().UTC()}}
	for _, row := range rows {
		cache.Rows = append(cache.Rows, row.Raw)
	}
	out, err := serverQueryListing(cache)
	if err != nil {
		return ServerTaskListing{}, err
	}
	cache.Meta = out.Meta
	raw, err := json.Marshal(cache)
	if err != nil {
		return ServerTaskListing{}, err
	}
	// One atomic metadata record; never merge, prune, number, or enqueue tasks.
	if err := s.store.SetMeta(ctx, q.cacheKey(), string(raw)); err != nil {
		return ServerTaskListing{}, err
	}
	return out, nil
}
