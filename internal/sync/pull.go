package sync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/store"
)

func (s *Syncer) Pull(ctx context.Context) (Result, error) {
	var res Result
	projects, err := s.api.ListProjects(ctx)
	if err != nil {
		return res, fmt.Errorf("ask the server for the lists: %w", err)
	}
	cached, err := s.store.Projects(ctx)
	if err != nil {
		return res, err
	}

	inbox := s.pullInbox(ctx, cached)

	lists, weighed := projects, cached
	if inbox.id != "" {
		lists = append(slices.Clone(projects), inbox.listEntry())
		weighed = withoutProject(cached, inbox.id)
	}

	gone := vanished(cached, lists)
	stakes, err := s.completedUnder(ctx, gone)
	if err != nil {
		return res, err
	}
	if err := refuseTheDrop(weighed, projects, stakes, s.opts.AllowProjectDrop); err != nil {
		return res, err
	}
	before, err := s.cachedTaskCount(ctx, gone)
	if err != nil {
		return res, err
	}
	if err := s.store.ReplaceProjects(ctx, api.ProjectsToModel(lists)); err != nil {
		return res, err
	}
	after, err := s.cachedTaskCount(ctx, gone)
	if err != nil {
		return res, err
	}

	res.Deleted += before - after
	res.Kept += after

	if done := completedAtStake(stakes); done > 0 {
		left, err := s.completedUnder(ctx, gone)
		if err != nil {
			return res, err
		}
		res.CompletedDropped += done - completedAtStake(left)
	}
	complete := true
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		fence, err := s.store.CapturePullFence(ctx, p.ID)
		if err != nil {
			return res, err
		}
		data, err := s.api.GetProjectDataRaw(ctx, p.ID)
		if err != nil {
			reported, survivable := projectFetchFailure(p.Name, p.ID, err)
			if !survivable {
				return res, reported
			}
			res.addError(reported)
			complete = false
			continue
		}
		if err := s.applyProject(ctx, &res, p.ID, data, fence); err != nil {
			return res, err
		}
		if err := s.recoverCompleted(ctx, &res, p.ID, data, fence); err != nil {
			return res, err
		}
	}

	switch {
	case inbox.failure != nil:
		res.addError(inbox.failure)
		complete = false
	case inbox.data != nil:
		if err := s.applyProject(ctx, &res, inbox.id, inbox.data, inbox.fence); err != nil {
			return res, err
		}
		if err := s.recoverCompleted(ctx, &res, inbox.id, inbox.data, inbox.fence); err != nil {
			return res, err
		}
	}
	if !complete {
		return res, nil
	}
	if err := s.store.SetMeta(ctx, LastSyncKey, stamp(time.Now())); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Syncer) recoverCompleted(ctx context.Context, res *Result, projectID string, data *api.ProjectDataRaw, fence store.PullFence) error {
	present := make(map[string]bool, len(data.Tasks))
	for _, task := range data.Tasks {
		present[task.Task.ID] = true
	}
	for taskID, captured := range fence.Tasks {
		if !captured.RecoveryEligible || present[taskID] {
			continue
		}
		freshFence, err := s.store.CaptureTaskPullFence(ctx, projectID, taskID)
		if err != nil {
			return err
		}
		fresh, ok := freshFence.Tasks[taskID]
		if !ok || !fresh.RecoveryEligible {
			continue
		}
		raw, err := s.api.GetTaskRaw(ctx, projectID, taskID)
		if err != nil {
			res.addError(fmt.Errorf("recover completed task %s: %w", taskID, err))
			continue
		}
		modelTask, warnings := api.TaskToModel(raw.Task)
		for _, warning := range warnings {
			res.addWarning(warning)
		}
		sr, err := s.store.SyncTaskFenced(ctx, store.ServerTask{Task: modelTask, Raw: raw.Raw}, freshFence)
		if errors.Is(err, store.ErrPullAddressMismatch) {
			res.addError(fmt.Errorf("recover completed task %s: %w", taskID, err))
			continue
		}
		if err != nil {
			return err
		}
		res.Pulled += sr.Upserted
		res.Skipped += sr.Skipped
		res.Kept += sr.Kept
		if sr.Stale {
			res.addWarning(api.Warning{TaskID: taskID, Field: "identity", Detail: "fetched task evidence was stale and was skipped"})
		}
	}
	return nil
}

func (s *Syncer) applyProject(ctx context.Context, res *Result, projectID string, data *api.ProjectDataRaw, fence store.PullFence) error {
	tasks, warnings := serverTasks(projectID, data)
	for _, w := range warnings {
		res.addWarning(w)
	}
	sr, err := s.store.SyncProjectFenced(ctx, projectID, tasks, fence)
	if err != nil {
		return err
	}
	res.Projects++
	res.Pulled += sr.Upserted
	res.Skipped += sr.Skipped
	res.Deleted += sr.Deleted
	res.Kept += sr.Kept
	if sr.Stale {
		res.addWarning(api.Warning{Field: "identity", Detail: "fetched task evidence was stale and was skipped as one response"})
	}
	return nil
}

func projectFetchFailure(name, projectID string, err error) (reported error, survivable bool) {
	switch {
	case errors.Is(err, api.ErrNotFound):

		return fmt.Errorf("the list %s: %w", projectID, err), true
	case errors.Is(err, api.ErrIncompleteAnswer):

		return fmt.Errorf("the list %q (%s): the server answered with nothing in it, "+
			"which is not the same as answering that the list is empty - "+
			"the list was left as the cache holds it: %w", name, projectID, err), true
	}
	return fmt.Errorf("the list %s: %w", projectID, err), false
}

const inboxID = "inbox"

const inboxName = "Inbox"

const inboxKind = "TASK"

const inboxSortOrder = math.MinInt64

type inboxPull struct {
	id      string
	data    *api.ProjectDataRaw
	failure error
	fence   store.PullFence
}

func (in inboxPull) listEntry() api.Project {
	return api.Project{ID: in.id, Name: inboxName, Kind: inboxKind, SortOrder: inboxSortOrder}
}

func (s *Syncer) pullInbox(ctx context.Context, cached []model.Project) inboxPull {
	in := inboxPull{id: cachedInboxID(cached)}
	discovering := in.id == ""
	fenceID := in.id
	if fenceID == "" {
		fenceID = inboxID
	}
	fence, err := s.store.CapturePullFence(ctx, fenceID)
	if err != nil {
		in.failure = fmt.Errorf("capture the inbox pull fence: %w", err)
		return in
	}
	in.fence = fence
	data, err := s.api.GetProjectDataRaw(ctx, inboxID)
	if err != nil {
		reported, _ := projectFetchFailure(inboxName, inboxID, err)
		in.failure = reported
		return in
	}
	if id := inboxIDOf(data); isInboxID(id) {
		in.id = id
	}
	if in.id == "" {
		return in
	}
	if discovering {

		in.fence.ProjectID = in.id
	}
	in.data = data
	return in
}

func inboxIDOf(data *api.ProjectDataRaw) string {
	for _, rt := range data.Tasks {
		if rt.Task.ProjectID != "" {
			return rt.Task.ProjectID
		}
	}
	return ""
}

func cachedInboxID(cached []model.Project) string {
	for _, p := range cached {
		if isInboxID(p.Id) {
			return p.Id
		}
	}
	return ""
}

func isInboxID(id string) bool {
	digits, ok := strings.CutPrefix(id, inboxID)
	if !ok {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func withoutProject(ps []model.Project, id string) []model.Project {
	out := make([]model.Project, 0, len(ps))
	for _, p := range ps {
		if p.Id != id {
			out = append(out, p)
		}
	}
	return out
}

func vanished(cached []model.Project, listed []api.Project) []model.Project {
	if len(cached) == 0 {
		return nil
	}
	still := make(map[string]bool, len(listed))
	for _, p := range listed {
		still[p.ID] = true
	}
	var gone []model.Project
	for _, p := range cached {
		if !still[p.Id] {
			gone = append(gone, p)
		}
	}
	return gone
}

type stake struct {
	project model.Project

	completed int
}

const AllowProjectDropFlag = "--allow-project-drop"

const allowProjectDropHint = `run "tt sync ` + AllowProjectDropFlag + `" once and the cache will follow`

func refuseTheDrop(cached []model.Project, listed []api.Project, gone []stake, allow bool) error {
	if allow || len(gone) == 0 {
		return nil
	}
	if len(listed) == 0 {

		return fmt.Errorf("the server sent no lists at all while the cache holds %d: "+
			"refusing to empty the cache on an answer that carries nothing; "+
			"queued changes are still going out and the cache keeps what it has, only the refresh stopped; "+
			"an account really can have no lists of its own - this answer carries the lists the user made "+
			"and never the inbox, which is pulled on its own - so if you deleted them all yourself "+
			allowProjectDropHint,
			len(cached))
	}
	if done := completedAtStake(gone); done > 0 {
		return fmt.Errorf("the server did not send %d of the %d list(s) the cache holds, "+
			"and %d completed task(s) under them are held nowhere else (%s): "+
			"refusing to drop them; the server does not carry a completed task among a list's tasks, so a "+
			"pull that dropped one would be the last of it and no later pass brings it back; "+
			"queued changes are still going out, only the refresh stopped; "+
			"if you deleted those lists yourself, "+allowProjectDropHint,
			len(gone), len(cached), done, nameProjects(gone))
	}
	if len(gone)*2 <= len(cached) {
		return nil
	}
	return fmt.Errorf("the server did not send %d of the %d list(s) the cache holds (%s): "+
		"refusing to drop them and every task under them; an answer missing most of the account is what a "+
		"truncated page and a momentary outage both look like, and nothing in it says which it is; "+
		"queued changes are still going out, only the refresh stopped; "+
		"if you deleted those lists yourself, "+allowProjectDropHint,
		len(gone), len(cached), nameProjects(gone))
}

func completedAtStake(gone []stake) int {
	var n int
	for _, g := range gone {
		n += g.completed
	}
	return n
}

const maxNamedProjects = 10

func nameProjects(ps []stake) string {

	ps = slices.Clone(ps)
	slices.SortStableFunc(ps, func(a, b stake) int { return cmp.Compare(b.completed, a.completed) })
	var b strings.Builder
	for i, p := range ps {
		if i == maxNamedProjects {
			fmt.Fprintf(&b, ", and %d more", len(ps)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q (%s)", p.project.Name, p.project.Id)
		if p.completed > 0 {
			fmt.Fprintf(&b, ": %d completed", p.completed)
		}
	}
	return b.String()
}

func (s *Syncer) completedUnder(ctx context.Context, gone []model.Project) ([]stake, error) {
	if len(gone) == 0 {
		return nil, nil
	}
	out := make([]stake, 0, len(gone))
	for _, p := range gone {
		tasks, err := s.store.Tasks(ctx, store.TaskFilter{ProjectID: p.Id, Status: store.StatusDone})
		if err != nil {
			return nil, err
		}
		out = append(out, stake{project: p, completed: len(tasks)})
	}
	return out, nil
}

func (s *Syncer) cachedTaskCount(ctx context.Context, projects []model.Project) (int, error) {
	var n int
	for _, p := range projects {
		tasks, err := s.store.Tasks(ctx, store.TaskFilter{ProjectID: p.Id, Status: store.StatusAll})
		if err != nil {
			return 0, err
		}
		n += len(tasks)
	}
	return n, nil
}

func serverTasks(projectID string, data *api.ProjectDataRaw) ([]store.ServerTask, []api.Warning) {
	out := make([]store.ServerTask, 0, len(data.Tasks))
	var warnings []api.Warning
	for _, rt := range data.Tasks {
		t, w := api.TaskToModel(rt.Task)
		if t.Id == "" {

			warnings = append(warnings, api.Warning{
				Field: "id", Detail: "task without an id, not cached",
			})
			continue
		}
		warnings = append(warnings, w...)
		if t.ProjectId == "" {

			t.ProjectId = projectID
		}
		out = append(out, store.ServerTask{Task: t, Raw: rt.Raw})
	}
	return out, warnings
}
