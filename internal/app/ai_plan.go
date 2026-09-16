package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/movsar/tt/internal/ai"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/config"
	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
	"github.com/movsar/tt/internal/store"
)

// AIRunner is the part of ai.Runner the assistant commands use. Tests pass a
// fake; nothing here reaches a model on its own.
type AIRunner interface {
	Run(ctx context.Context, req ai.Request) (ai.Response, error)
}

const (
	AIOpAdd          = "add"
	AIOpEdit         = "edit"
	AIOpSchedule     = "schedule"
	AIOpDone         = "done"
	AIOpMove         = "move"
	AIOpChecklistAdd = "checklist_add"
)

var aiOpNames = []string{AIOpAdd, AIOpEdit, AIOpSchedule, AIOpDone, AIOpMove, AIOpChecklistAdd}

const (
	aiPlanVersion      = 1
	aiContextTaskLimit = 200
	aiMaxOps           = 50

	// AIPlanLifetime is how long a saved preview can be accepted. Relative
	// dates were resolved when it was made, so an old one would plan the past.
	AIPlanLifetime = 24 * time.Hour
)

// aiOp is one operation as the model returns it. Every field is present in a
// reply that matches the schema; null, "" and [] all mean the field is unused.
type aiOp struct {
	Op        string   `json:"op"`
	Ref       *string  `json:"ref"`
	Title     *string  `json:"title"`
	Project   *string  `json:"project"`
	Tags      []string `json:"tags"`
	Content   *string  `json:"content"`
	Checklist []string `json:"checklist"`
	Due       *string  `json:"due"`
	Schedule  *string  `json:"schedule"`
	Reminders []string `json:"reminders"`
	Priority  *string  `json:"priority"`
}

type aiReply struct {
	Ops   []aiOp  `json:"ops"`
	Notes *string `json:"notes"`
}

var aiOpFields = map[string]struct{ required, optional []string }{
	AIOpAdd:          {[]string{"title"}, []string{"project", "tags", "content", "checklist", "due", "schedule", "reminders", "priority"}},
	AIOpEdit:         {[]string{"ref"}, []string{"title", "tags", "content", "due", "reminders", "priority"}},
	AIOpSchedule:     {[]string{"ref", "schedule"}, nil},
	AIOpDone:         {[]string{"ref"}, nil},
	AIOpMove:         {[]string{"ref", "project"}, nil},
	AIOpChecklistAdd: {[]string{"ref", "checklist"}, nil},
}

func (op aiOp) present() []string {
	var out []string
	text := func(name string, v *string) {
		if v != nil && strings.TrimSpace(*v) != "" {
			out = append(out, name)
		}
	}
	list := func(name string, v []string) {
		if len(v) != 0 {
			out = append(out, name)
		}
	}
	text("ref", op.Ref)
	text("title", op.Title)
	text("project", op.Project)
	list("tags", op.Tags)
	text("content", op.Content)
	list("checklist", op.Checklist)
	text("due", op.Due)
	text("schedule", op.Schedule)
	list("reminders", op.Reminders)
	text("priority", op.Priority)
	return out
}

func aiText(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// AIPlan is a validated bundle of changes. It is what a preview saves and what
// an accept applies; it never holds the prompt, the context or the model's
// notes.
type AIPlan struct {
	Version int              `json:"version"`
	Created time.Time        `json:"created"`
	Context config.AIContext `json:"context"`
	Steps   []AIStep         `json:"steps"`
}

// AIStep is one change of a plan, with every name resolved and every date
// already turned into a time.
type AIStep struct {
	Op string `json:"op"`

	// Task is the existing task as it was when the preview was made; accept
	// refuses when it has changed since.
	Task *model.Task `json:"task,omitempty"`

	Title     string          `json:"title,omitempty"`
	Project   *model.Project  `json:"project,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Content   string          `json:"content,omitempty"`
	Checklist []string        `json:"checklist,omitempty"`
	Due       *dates.Result   `json:"due,omitempty"`
	Interval  *dates.Interval `json:"interval,omitempty"`
	Reminders []string        `json:"reminders,omitempty"`
	Priority  *model.Priority `json:"priority,omitempty"`

	// Overlaps are cached tasks whose planned interval this step's interval
	// overlapped at preview time; Clashes are other steps of the same plan
	// (numbered from 1) whose intervals it overlaps.
	Overlaps []AIOverlap `json:"overlaps,omitempty"`
	Clashes  []int       `json:"clashes,omitempty"`
}

type AIOverlap struct {
	TaskID string     `json:"task_id"`
	Title  string     `json:"title"`
	List   string     `json:"list"`
	Start  model.Time `json:"start"`
	End    model.Time `json:"end"`
}

// HasOverlaps reports whether any step of the plan overlaps a cached task or
// another step.
func (p AIPlan) HasOverlaps() bool {
	for _, step := range p.Steps {
		if len(step.Overlaps) != 0 || len(step.Clashes) != 0 {
			return true
		}
	}
	return false
}

// AIIssue is one reason a proposed bundle cannot be applied. Step counts from
// 1, or is 0 for the bundle as a whole. Reason and Value can carry text from
// the model or from the cache and are escaped wherever they are shown.
type AIIssue struct {
	Step   int    `json:"step"`
	Op     string `json:"op,omitempty"`
	Reason string `json:"reason"`
	Value  string `json:"value,omitempty"`
}

// AIProposal is what one planning call produced: a plan that can be applied
// only when Issues is empty. LeftOut counts the cached tasks and names the
// call did not send because they look like secrets.
type AIProposal struct {
	Plan    AIPlan    `json:"plan"`
	Notes   string    `json:"notes,omitempty"`
	Issues  []AIIssue `json:"issues"`
	Profile string    `json:"profile"`
	Engine  string    `json:"engine"`
	LeftOut AILeftOut `json:"left_out"`
}

// AIPlanner turns a request into a proposal. Now fixes the moment every date
// word is resolved against. Cached titles and names that look like secrets
// stay out of the prompt unless AllowSecrets is set.
type AIPlanner struct {
	Store        *store.Store
	Runner       AIRunner
	Config       config.Config
	Now          time.Time
	AllowSecrets bool
}

type aiContextTask struct {
	Ref   string `json:"ref"`
	Title string `json:"title"`
	List  string `json:"list"`
	Due   string `json:"due"`
}

type aiPromptContext struct {
	Now      string          `json:"now"`
	Weekday  string          `json:"weekday"`
	TimeZone string          `json:"time_zone"`
	Scope    string          `json:"scope"`
	Lists    []string        `json:"lists"`
	Tags     []string        `json:"tags"`
	Tasks    []aiContextTask `json:"tasks"`
}

// aiCatalog is what the cache says about lists, tags and tasks. projects,
// names, tags and refs hold everything, for checking a reply; what a prompt
// carries goes through sendable, and withheld records what it left out.
type aiCatalog struct {
	projects []model.Project
	usable   []model.Project
	names    map[string]string
	tags     map[string]string
	tagList  []string
	refs     map[string]model.Task
	tasks    []aiContextTask
	zone     string

	allowSecrets bool
	withheld     aiWithheld
}

// sendable reports whether cached text may go into a prompt: text that looks
// like a secret stays out unless --allow-secrets was given.
func (cat aiCatalog) sendable(text string) bool {
	return cat.allowSecrets || AISecretKind(text) == ""
}

// listName is the name of the list id as a prompt may carry it, or "" when
// the name looks like a secret.
func (cat aiCatalog) listName(id string) string {
	name := cat.names[id]
	if name == "" || cat.sendable(name) {
		return name
	}
	cat.withheld.lists[id] = true
	return ""
}

// listNames returns the names of projects a prompt may carry, each name once.
func (cat aiCatalog) listNames(projects []model.Project) []string {
	names := make([]string, 0, len(projects))
	for _, project := range projects {
		if name := cat.listName(project.Id); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// tagNames returns the cached tag names a prompt may carry.
func (cat aiCatalog) tagNames() []string {
	names := make([]string, 0, len(cat.tagList))
	for _, tag := range cat.tagList {
		if cat.sendable(tag) {
			names = append(names, tag)
		} else {
			cat.withheld.tags[strings.ToLower(tag)] = true
		}
	}
	return names
}

// Propose sends text and the configured context to the model, then validates
// every operation it returns against the cache. req carries only profile,
// model and effort overrides. When the call fails, the proposal returned with
// the error still carries LeftOut.
func (p AIPlanner) Propose(ctx context.Context, text string, req ai.Request) (AIProposal, error) {
	scope := p.Config.AI.Context
	if scope == "" {
		scope = config.AIContextMinimal
	}
	cat, err := aiLoadCatalog(ctx, p.Store, p.Now, scope, scope != config.AIContextMinimal, p.AllowSecrets)
	if err != nil {
		return AIProposal{}, err
	}
	prompt, err := aiPlanPrompt(cat, scope, p.Now, text)
	if err != nil {
		return AIProposal{}, err
	}
	req.Task, req.System, req.Prompt, req.Schema = config.AITaskAI, aiPlanSystem, prompt, AIPlanSchema()
	resp, err := p.Runner.Run(ctx, req)
	if err != nil {
		return AIProposal{LeftOut: cat.withheld.count()}, err
	}
	reply, err := decodeAIReply[aiReply](resp.JSON)
	if err != nil {
		return AIProposal{LeftOut: cat.withheld.count()}, err
	}
	plan, issues := p.validate(ctx, reply, cat, scope)
	out := AIProposal{Plan: plan, Notes: aiText(reply.Notes), Issues: issues, Profile: resp.Profile, Engine: resp.Engine,
		LeftOut: cat.withheld.count()}
	if out.Issues == nil {
		out.Issues = []AIIssue{}
	}
	return out, nil
}

// ErrAIReplyShape reports a reply that parsed as JSON but not as the shape the
// schema asked for.
var ErrAIReplyShape = errors.New("the model's reply does not match the expected shape")

func decodeAIReply[T any](raw []byte) (T, error) {
	var out T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("%w: %v", ErrAIReplyShape, err)
	}
	if dec.More() {
		return out, fmt.Errorf("%w: more than one JSON value", ErrAIReplyShape)
	}
	return out, nil
}

// aiLoadCatalog reads lists and tags, and with withTasks the tasks scope
// lists. A task whose title looks like a secret is left out, and gets no ref,
// unless allowSecrets is set.
func aiLoadCatalog(ctx context.Context, st *store.Store, now time.Time, scope config.AIContext, withTasks, allowSecrets bool) (aiCatalog, error) {
	cat := aiCatalog{names: map[string]string{}, tags: map[string]string{}, refs: map[string]model.Task{},
		allowSecrets: allowSecrets, withheld: newAIWithheld()}
	cat.zone = dates.DefaultIntervalZone("")
	var err error
	cat.projects, err = st.Projects(ctx)
	if err != nil {
		return cat, err
	}
	for _, project := range cat.projects {
		cat.names[project.Id] = project.Name
		if project.CreateUnavailable() == "" {
			cat.usable = append(cat.usable, project)
		}
	}
	if err := aiLoadTags(ctx, st, &cat); err != nil {
		return cat, err
	}
	if !withTasks {
		return cat, nil
	}
	tomorrow, err := dates.Parse("tmr", now)
	if err != nil {
		return cat, err
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusOpen, DueTo: tomorrow.Time, Order: store.OrderDue, Limit: aiContextTaskLimit})
	if err != nil {
		return cat, err
	}
	if scope == config.AIContextAll && len(tasks) < aiContextTaskLimit {
		rest, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusOpen, Order: store.OrderDue})
		if err != nil {
			return cat, err
		}
		seen := map[string]bool{}
		for _, t := range tasks {
			seen[t.Id] = true
		}
		for _, t := range rest {
			if len(tasks) >= aiContextTaskLimit {
				break
			}
			if !seen[t.Id] {
				tasks = append(tasks, t)
			}
		}
	}
	for _, listed := range tasks {
		// A listing leaves checklists out; the plan keeps the whole task, the
		// copy accept compares with the cache.
		t, err := st.Task(ctx, listed.Id)
		if err != nil {
			return cat, err
		}
		if !cat.sendable(t.Title) {
			cat.withheld.tasks[t.Id] = true
			continue
		}
		ref := fmt.Sprintf("t%d", len(cat.tasks)+1)
		cat.refs[ref] = t
		cat.tasks = append(cat.tasks, aiContextTask{Ref: ref, Title: t.Title, List: cat.listName(t.ProjectId), Due: aiDueWords(t)})
	}
	return cat, nil
}

func aiLoadTags(ctx context.Context, st *store.Store, cat *aiCatalog) error {
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, seen := cat.tags[key]; seen {
			return
		}
		cat.tags[key] = name
		cat.tagList = append(cat.tagList, name)
	}
	entities, err := st.Entities(ctx, "tag", "", false)
	if err != nil {
		return err
	}
	for _, entity := range entities {
		var fields struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		}
		if json.Unmarshal(entity.Data, &fields) != nil {
			continue
		}
		if fields.Name != "" {
			add(fields.Name)
		} else {
			add(fields.Label)
		}
	}
	tasks, err := st.Tasks(ctx, store.TaskFilter{Status: store.StatusAll})
	if err != nil {
		return err
	}
	for _, t := range tasks {
		for _, tag := range t.Tags {
			add(tag)
		}
	}
	slices.SortFunc(cat.tagList, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return nil
}

func aiDueWords(t model.Task) string {
	if t.DueDate.IsZero() {
		return ""
	}
	if t.IsAllDay {
		return t.DueDate.In(dates.Zone(t.TimeZone)).Format("2006-01-02")
	}
	return t.DueDate.In(time.Local).Format("2006-01-02 15:04")
}

func aiNowContext(now time.Time, zone string) (string, string, string) {
	local := now.In(time.Local)
	if zone == "" {
		zone = time.Local.String()
	}
	return local.Format("2006-01-02 15:04"), strings.ToLower(local.Weekday().String()[:3]), zone
}

func aiMarshal(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func aiPlanPrompt(cat aiCatalog, scope config.AIContext, now time.Time, text string) (string, error) {
	pc := aiPromptContext{Scope: string(scope), Lists: cat.listNames(cat.usable), Tags: cat.tagNames(), Tasks: cat.tasks}
	pc.Now, pc.Weekday, pc.TimeZone = aiNowContext(now, cat.zone)
	if pc.Tasks == nil {
		pc.Tasks = []aiContextTask{}
	}
	data, err := aiMarshal(pc)
	if err != nil {
		return "", err
	}
	note := "Existing tasks are listed; ref names one of them."
	if scope == config.AIContextMinimal {
		note = "No existing tasks are listed (ai.context is minimal), so only add operations are possible."
	}
	return "Context (JSON):\n" + data + "\n" + note + "\n\nRequest:\n" + text, nil
}

func (p AIPlanner) validate(ctx context.Context, reply aiReply, cat aiCatalog, scope config.AIContext) (AIPlan, []AIIssue) {
	plan := AIPlan{Version: aiPlanVersion, Created: p.Now.UTC(), Context: scope, Steps: []AIStep{}}
	var issues []AIIssue
	if len(reply.Ops) > aiMaxOps {
		return plan, []AIIssue{{Reason: fmt.Sprintf("the model proposed %d changes, more than the %d one request may make", len(reply.Ops), aiMaxOps)}}
	}
	touches := map[string][]int{}
	type span struct {
		step       int
		start, end time.Time
	}
	var spans []span
	for i, op := range reply.Ops {
		n := i + 1
		before := len(issues)
		bad := func(reason, value string) {
			issues = append(issues, AIIssue{Step: n, Op: op.Op, Reason: reason, Value: value})
		}
		step, fields := AIStep{Op: op.Op}, aiOpFields[op.Op]
		if !slices.Contains(aiOpNames, op.Op) {
			bad("unknown operation", op.Op)
			plan.Steps = append(plan.Steps, step)
			continue
		}
		present := op.present()
		for _, name := range present {
			if !slices.Contains(fields.required, name) && !slices.Contains(fields.optional, name) {
				bad("this operation does not take the field", name)
			}
		}
		for _, name := range fields.required {
			if !slices.Contains(present, name) {
				bad("this operation needs the field", name)
			}
		}
		if len(issues) != before {
			plan.Steps = append(plan.Steps, step)
			continue
		}

		if op.Op != AIOpAdd {
			ref := aiText(op.Ref)
			task, ok := cat.refs[ref]
			switch {
			case scope == config.AIContextMinimal:
				bad("this changes an existing task, and ai.context is minimal, so no tasks were sent to name one by; set ai.context to today or all", ref)
			case !ok:
				bad("the model named a task that was not in the context it was given", ref)
			default:
				step.Task = &task
				touches[task.Id] = append(touches[task.Id], n)
			}
		}
		if title := aiText(op.Title); title != "" {
			if strings.ContainsFunc(title, unicode.IsControl) {
				bad("a title must not contain control characters", title)
			}
			step.Title = title
		}
		if op.Content != nil && strings.TrimSpace(*op.Content) != "" {
			if strings.ContainsRune(*op.Content, 0) {
				bad("notes cannot contain NUL", "")
			}
			step.Content = strings.TrimSpace(*op.Content)
		}
		if name := aiText(op.Project); name != "" {
			project, reason := aiResolveProject(cat.projects, name)
			if reason != "" {
				bad(reason, name)
			} else {
				step.Project = &project
			}
		}
		for _, tag := range op.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			canonical, ok := cat.tags[strings.ToLower(tag)]
			if !ok {
				bad("there is no cached tag by this name, and tt ai does not create tags", tag)
				continue
			}
			if !slices.Contains(step.Tags, canonical) {
				step.Tags = append(step.Tags, canonical)
			}
		}
		for _, item := range op.Checklist {
			if strings.TrimSpace(item) == "" {
				continue
			}
			if err := ValidateItemTitle(item); err != nil {
				bad(err.Error(), item)
				continue
			}
			step.Checklist = append(step.Checklist, strings.TrimSpace(item))
		}
		if len(op.Checklist) != 0 && len(step.Checklist) == 0 && op.Op == AIOpChecklistAdd {
			bad("this operation needs the field", "checklist")
		}
		if words := aiText(op.Due); words != "" {
			result, err := dates.Parse(words, p.Now)
			switch {
			case err != nil:
				bad(err.Error(), words)
			case result.Clear && op.Op == AIOpAdd:
				bad("a new task has no due date to clear", words)
			default:
				step.Due = &result
			}
		}
		if words := aiText(op.Schedule); words != "" {
			zone := cat.zone
			if step.Task != nil {
				zone = dates.DefaultIntervalZone(step.Task.TimeZone)
			}
			interval, err := dates.ParseInterval(words, zone, p.Now)
			if err != nil {
				bad(err.Error(), words)
			} else {
				step.Interval = &interval
			}
		}
		if op.Op == AIOpAdd && step.Due != nil && step.Interval != nil {
			bad("choose due or schedule for a new task, not both", "")
		}
		for _, words := range op.Reminders {
			words = strings.TrimSpace(words)
			if words == "" {
				continue
			}
			trigger, err := schedule.ParseReminder(words)
			if err != nil {
				bad(err.Error(), words)
				continue
			}
			if !slices.Contains(step.Reminders, trigger) {
				step.Reminders = append(step.Reminders, trigger)
			}
		}
		if len(step.Reminders) != 0 && op.Op == AIOpAdd && step.Due == nil && step.Interval == nil {
			bad("a reminder needs a due date or a schedule to be relative to", "")
		}
		if words := aiText(op.Priority); words != "" {
			priority, err := model.ParsePriority(strings.ToLower(words))
			if err != nil {
				bad(err.Error(), words)
			} else {
				step.Priority = &priority
			}
		}
		if len(issues) != before {
			plan.Steps = append(plan.Steps, step)
			continue
		}

		start, end, reason := p.precheck(ctx, cat, &step)
		if reason != "" {
			bad(reason, "")
		}
		if !start.IsZero() {
			spans = append(spans, span{step: n, start: start, end: end})
		}
		plan.Steps = append(plan.Steps, step)
	}

	for _, steps := range touches {
		if len(steps) < 2 {
			continue
		}
		for _, n := range steps {
			if plan.Steps[n-1].Op == AIOpMove {
				issues = append(issues, AIIssue{Step: n, Op: AIOpMove, Reason: "a move must be the only change to its task in one request: a queued move blocks other changes to the task until it is synced"})
			}
		}
	}
	for i := range spans {
		for j := range spans {
			if i != j && spans[i].start.Before(spans[j].end) && spans[j].start.Before(spans[i].end) {
				step := &plan.Steps[spans[i].step-1]
				step.Clashes = append(step.Clashes, spans[j].step)
			}
		}
	}
	return plan, issues
}

func aiResolveProject(projects []model.Project, name string) (model.Project, string) {
	var matches []model.Project
	for _, project := range projects {
		if strings.EqualFold(strings.TrimSpace(project.Name), name) {
			matches = append(matches, project)
		}
	}
	switch len(matches) {
	case 0:
		return model.Project{}, "there is no cached list by this name, and tt ai does not create lists"
	case 1:
		if reason := matches[0].CreateUnavailable(); reason != "" {
			return model.Project{}, reason
		}
		return matches[0], ""
	default:
		return model.Project{}, "more than one cached list has this name"
	}
}

// precheck runs what the store can say about a step before anything is
// written: it fills the default list, the overlaps of a planned interval, and
// returns that interval so steps can be checked against each other.
func (p AIPlanner) precheck(ctx context.Context, cat aiCatalog, step *AIStep) (start, end time.Time, reason string) {
	switch step.Op {
	case AIOpAdd:
		if step.Project == nil {
			project, err := cli.ResolveDefaultProject(cat.projects, p.Config.DefaultProject)
			if err != nil {
				return start, end, err.Error()
			}
			step.Project = &project
		}
		if step.Interval == nil {
			return start, end, ""
		}
		preview, err := p.Store.PrepareInterval(ctx, nil, model.TaskEdit{}, aiNewTask(*step))
		if err != nil {
			return start, end, err.Error()
		}
		step.Overlaps = aiOverlaps(preview)
		return step.Interval.Start.Time, step.Interval.End.Time, ""
	case AIOpEdit:
		edit, err := aiEdit(*step.Task, *step)
		if err != nil {
			return start, end, err.Error()
		}
		if !schedule.ReviewsInterval(*step.Task, edit) {
			return start, end, ""
		}
		preview, err := p.Store.PrepareInterval(ctx, step.Task, edit, model.Task{})
		if err != nil {
			return start, end, err.Error()
		}
		step.Overlaps = aiOverlaps(preview)
		return preview.Task.StartDate.Time, preview.Task.DueDate.Time, ""
	case AIOpSchedule:
		edit, err := schedule.IntervalEdit(*step.Task, *step.Interval)
		if err != nil {
			return start, end, err.Error()
		}
		preview, err := p.Store.PrepareInterval(ctx, step.Task, edit, model.Task{})
		if err != nil {
			return start, end, err.Error()
		}
		step.Overlaps = aiOverlaps(preview)
		return step.Interval.Start.Time, step.Interval.End.Time, ""
	case AIOpMove:
		if _, err := p.Store.PreviewNativeMove(ctx, *step.Task, step.Project.Id); err != nil {
			return start, end, err.Error()
		}
	case AIOpChecklistAdd:
		if strings.EqualFold(step.Task.Kind, "NOTE") {
			return start, end, "a note has no checklist"
		}
	}
	return start, end, ""
}

func aiOverlaps(preview store.IntervalPreview) []AIOverlap {
	out := make([]AIOverlap, 0, len(preview.Overlaps))
	for _, o := range preview.Overlaps {
		out = append(out, AIOverlap{TaskID: o.Task.Id, Title: o.Task.Title, List: o.List, Start: o.Start, End: o.End})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func aiNewTask(step AIStep) model.Task {
	task := model.Task{Title: step.Title, Content: step.Content}
	if step.Project != nil {
		task.ProjectId = step.Project.Id
	}
	if len(step.Tags) != 0 {
		task.Tags = slices.Clone(step.Tags)
	}
	if len(step.Checklist) != 0 {
		task.Items = make([]model.Item, len(step.Checklist))
		for i, title := range step.Checklist {
			task.Items[i].Title = title
		}
	}
	if step.Priority != nil {
		task.Priority = *step.Priority
	}
	if len(step.Reminders) != 0 {
		task.Reminders = slices.Clone(step.Reminders)
	}
	if step.Due != nil {
		task.DueDate, task.IsAllDay = step.Due.Time, step.Due.AllDay
	}
	if step.Interval != nil {
		task.StartDate, task.DueDate, task.TimeZone = step.Interval.Start, step.Interval.End, step.Interval.Zone
	}
	return task
}

// aiEdit builds the edit an edit step makes to original. Tags and reminders
// are added to what the task has, and notes are appended, because the model
// never sees those fields and so cannot be trusted to restate them.
func aiEdit(original model.Task, step AIStep) (model.TaskEdit, error) {
	change := schedule.Change{Due: step.Due}
	if len(step.Reminders) != 0 {
		merged := slices.Clone(original.Reminders)
		for _, trigger := range step.Reminders {
			if !slices.Contains(merged, trigger) {
				merged = append(merged, trigger)
			}
		}
		change.Reminders = &merged
	}
	edit, err := change.Edit(original)
	if err != nil {
		return model.TaskEdit{}, err
	}
	if step.Title != "" && step.Title != original.Title {
		edit.Title = model.Ptr(step.Title)
	}
	if step.Content != "" {
		content := step.Content
		if strings.TrimSpace(original.Content) != "" {
			content = strings.TrimRight(original.Content, "\n") + "\n\n" + step.Content
		}
		edit.Content = model.Ptr(content)
	}
	if step.Priority != nil && *step.Priority != original.Priority {
		edit.Priority = model.Ptr(*step.Priority)
	}
	if len(step.Tags) != 0 {
		merged := slices.Clone(original.Tags)
		for _, tag := range step.Tags {
			if !slices.ContainsFunc(merged, func(have string) bool { return strings.EqualFold(have, tag) }) {
				merged = append(merged, tag)
			}
		}
		if len(merged) != len(original.Tags) {
			edit.Tags = model.NewEditList(merged)
		}
	}
	return edit, nil
}
