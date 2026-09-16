package app

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

type AIRerank int

const (
	// AIRerankAuto ranks candidates with a second call only when there are
	// more than AIFindRerankThreshold of them and ai.context is today or all:
	// under minimal, titles are sent only when a ranking is asked for.
	AIRerankAuto AIRerank = iota
	AIRerankAlways
	AIRerankNever
)

const (
	AIFindRerankThreshold = 15

	aiFindCandidateLimit = 100
	aiFindKeywordLimit   = 12
)

type aiFindReply struct {
	Keywords []string `json:"keywords"`
	Project  *string  `json:"project"`
	Status   *string  `json:"status"`
	DueFrom  *string  `json:"due_from"`
	DueTo    *string  `json:"due_to"`
	DoneFrom *string  `json:"done_from"`
	DoneTo   *string  `json:"done_to"`
}

type aiFindContext struct {
	Now      string   `json:"now"`
	Weekday  string   `json:"weekday"`
	TimeZone string   `json:"time_zone"`
	Lists    []string `json:"lists"`
	Tags     []string `json:"tags"`
}

type aiRerankReply struct {
	Ranked []int `json:"ranked"`
}

// AIFindFilter is the filter as tt understood the model's reply.
type AIFindFilter struct {
	Keywords  []string   `json:"keywords"`
	ProjectID string     `json:"project_id,omitempty"`
	Project   string     `json:"project,omitempty"`
	Status    string     `json:"status"`
	DueFrom   model.Time `json:"due_from,omitzero"`
	DueTo     model.Time `json:"due_to,omitzero"`
	DoneFrom  model.Time `json:"done_from,omitzero"`
	DoneTo    model.Time `json:"done_to,omitzero"`
}

// AIFindResult is a search the model wrote and what it found. LeftOut counts
// the list and tag names, and the candidate titles, that were not sent
// because they look like secrets; those candidates follow the ranked ones.
type AIFindResult struct {
	Filter     AIFindFilter `json:"filter"`
	Candidates int          `json:"candidates"`
	Reranked   bool         `json:"reranked"`
	LeftOut    AILeftOut    `json:"left_out"`
	Tasks      []model.Task `json:"tasks"`
}

// AIInvalidError reports a reply that parsed but asks for something tt cannot
// do. Its issues carry the model's words; they are escaped wherever shown.
type AIInvalidError struct {
	Issues []AIIssue
}

func (e *AIInvalidError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, issue.Reason)
	}
	return "the model's filter cannot be used: " + strings.Join(parts, "; ")
}

// AIFinder searches the cache with a filter the model writes. The model sees
// the request, the current time, and the names of lists and tags; ranking
// sends it candidate titles only. Context is ai.context, which decides whether
// a ranking is made without being asked for; names and titles that look like
// secrets stay out of both calls unless AllowSecrets is set.
type AIFinder struct {
	Store        *store.Store
	Runner       AIRunner
	Now          time.Time
	Context      config.AIContext
	AllowSecrets bool
}

// Find returns the candidates in search order, or ranked when a second call
// ranked them. A ranking sends only the titles that pass the secret check;
// the candidates it could not send follow the ranked ones in search order,
// since nothing judged them. When a call fails or the filter is invalid, the
// result still carries LeftOut.
func (f AIFinder) Find(ctx context.Context, query string, rerank AIRerank, req ai.Request) (AIFindResult, error) {
	cat, err := aiLoadCatalog(ctx, f.Store, f.Now, "", false, f.AllowSecrets)
	if err != nil {
		return AIFindResult{}, err
	}
	fc := aiFindContext{Lists: cat.listNames(cat.projects), Tags: cat.tagNames()}
	fc.Now, fc.Weekday, fc.TimeZone = aiNowContext(f.Now, cat.zone)
	data, err := aiMarshal(fc)
	if err != nil {
		return AIFindResult{}, err
	}
	first := req
	first.Task, first.System, first.Schema = config.AITaskFind, aiFindSystem, AIFindSchema()
	first.Prompt = "Context (JSON):\n" + data + "\n\nSearch request:\n" + query
	resp, err := f.Runner.Run(ctx, first)
	if err != nil {
		return AIFindResult{LeftOut: cat.withheld.count()}, err
	}
	reply, err := decodeAIReply[aiFindReply](resp.JSON)
	if err != nil {
		return AIFindResult{LeftOut: cat.withheld.count()}, err
	}
	filter, base, err := f.filter(reply, cat)
	if err != nil {
		return AIFindResult{LeftOut: cat.withheld.count()}, err
	}
	candidates, err := f.candidates(ctx, filter, base)
	if err != nil {
		return AIFindResult{}, err
	}
	out := AIFindResult{Filter: filter, Candidates: len(candidates), Tasks: candidates, LeftOut: cat.withheld.count()}
	scope := f.Context
	if scope == "" {
		scope = config.AIContextMinimal
	}
	wanted := rerank == AIRerankAlways ||
		rerank == AIRerankAuto && scope != config.AIContextMinimal && len(candidates) > AIFindRerankThreshold
	if !wanted || rerank == AIRerankNever || len(candidates) < 2 {
		return out, nil
	}
	var shown, hidden []model.Task
	for _, t := range candidates {
		if cat.sendable(t.Title) {
			shown = append(shown, t)
		} else {
			hidden = append(hidden, t)
			cat.withheld.tasks[t.Id] = true
		}
	}
	out.LeftOut = cat.withheld.count()
	if len(shown) < 2 {
		return out, nil
	}
	var list strings.Builder
	for i, t := range shown {
		list.WriteString(strconv.Itoa(i+1) + ". " + strings.Join(strings.Fields(t.Title), " ") + "\n")
	}
	second := req
	second.Task, second.System, second.Schema = config.AITaskFind, aiRerankSystem, AIRerankSchema()
	second.Prompt = "Search request:\n" + query + "\n\nTitles:\n" + list.String()
	resp, err = f.Runner.Run(ctx, second)
	if err != nil {
		return AIFindResult{LeftOut: out.LeftOut}, err
	}
	ranked, err := decodeAIReply[aiRerankReply](resp.JSON)
	if err != nil {
		return AIFindResult{LeftOut: out.LeftOut}, err
	}
	out.Tasks, out.Reranked = []model.Task{}, true
	seen := map[int]bool{}
	for _, n := range ranked.Ranked {
		if n < 1 || n > len(shown) || seen[n] {
			continue
		}
		seen[n] = true
		out.Tasks = append(out.Tasks, shown[n-1])
	}
	out.Tasks = append(out.Tasks, hidden...)
	return out, nil
}

func (f AIFinder) filter(reply aiFindReply, cat aiCatalog) (AIFindFilter, store.TaskFilter, error) {
	var issues []AIIssue
	bad := func(reason, value string) { issues = append(issues, AIIssue{Reason: reason, Value: value}) }
	filter := AIFindFilter{Keywords: []string{}}
	var base store.TaskFilter
	for _, word := range reply.Keywords {
		word = strings.ToLower(strings.Join(strings.Fields(word), " "))
		if word == "" || slices.Contains(filter.Keywords, word) {
			continue
		}
		if len(filter.Keywords) == aiFindKeywordLimit {
			break
		}
		filter.Keywords = append(filter.Keywords, word)
	}
	if name := aiText(reply.Project); name != "" {
		var matches []model.Project
		for _, project := range cat.projects {
			if strings.EqualFold(strings.TrimSpace(project.Name), name) {
				matches = append(matches, project)
			}
		}
		switch len(matches) {
		case 0:
			bad("there is no cached list by this name", name)
		case 1:
			filter.ProjectID, filter.Project, base.ProjectID = matches[0].Id, matches[0].Name, matches[0].Id
		default:
			bad("more than one cached list has this name", name)
		}
	}
	bound := func(words *string, end bool) model.Time {
		text := aiText(words)
		if text == "" {
			return model.Time{}
		}
		result, err := dates.Parse(text, f.Now)
		switch {
		case err != nil:
			bad(err.Error(), text)
			return model.Time{}
		case result.Clear:
			bad("a search bound needs a date", text)
			return model.Time{}
		}
		if end && result.AllDay {
			return model.NewTime(result.Time.AddDate(0, 0, 1))
		}
		return result.Time
	}
	filter.DueFrom, filter.DueTo = bound(reply.DueFrom, false), bound(reply.DueTo, true)
	filter.DoneFrom, filter.DoneTo = bound(reply.DoneFrom, false), bound(reply.DoneTo, true)
	base.DueFrom, base.DueTo, base.DoneFrom, base.DoneTo = filter.DueFrom, filter.DueTo, filter.DoneFrom, filter.DoneTo

	switch status := strings.ToLower(aiText(reply.Status)); status {
	case "":
		filter.Status, base.Status = "open", store.StatusOpen
		if !filter.DoneFrom.IsZero() || !filter.DoneTo.IsZero() {
			filter.Status, base.Status = "done", store.StatusDone
		}
	case "open":
		filter.Status, base.Status = status, store.StatusOpen
	case "done":
		filter.Status, base.Status = status, store.StatusDone
	case "all":
		filter.Status, base.Status = status, store.StatusAll
	default:
		bad("status must be open, done or all", status)
	}
	if len(issues) != 0 {
		return filter, base, &AIInvalidError{Issues: issues}
	}
	return filter, base, nil
}

// candidates runs the filter once per keyword and joins the results, tasks
// that more keywords matched first.
func (f AIFinder) candidates(ctx context.Context, filter AIFindFilter, base store.TaskFilter) ([]model.Task, error) {
	if len(filter.Keywords) == 0 {
		base.Limit = aiFindCandidateLimit
		tasks, err := f.Store.Tasks(ctx, base)
		if tasks == nil {
			tasks = []model.Task{}
		}
		return tasks, err
	}
	var order []model.Task
	hits := map[string]int{}
	for _, word := range filter.Keywords {
		query := base
		query.Search = word
		tasks, err := f.Store.Tasks(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("search the cache: %w", err)
		}
		for _, t := range tasks {
			if hits[t.Id] == 0 {
				order = append(order, t)
			}
			hits[t.Id]++
		}
	}
	slices.SortStableFunc(order, func(a, b model.Task) int { return hits[b.Id] - hits[a.Id] })
	if len(order) > aiFindCandidateLimit {
		order = order[:aiFindCandidateLimit]
	}
	if order == nil {
		order = []model.Task{}
	}
	return order, nil
}
