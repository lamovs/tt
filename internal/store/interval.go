package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/movsar/tt/internal/dates"
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

var ErrIntervalChanged = errors.New("interval preview changed; review the current schedule and overlaps again")
var ErrIntervalOverlap = errors.New("overlapping tasks require explicit save anyway confirmation")

type IntervalOverlap struct {
	Task       model.Task
	List       string
	Start, End model.Time
}

type IntervalPreview struct {
	Task     model.Task
	Overlaps []IntervalOverlap
	Context  []string
	LastSync string
	Projects []model.Project
	original *model.Task
	edit     model.TaskEdit
	create   model.Task
	stamp    [32]byte
}

func (s *Store) PrepareInterval(ctx context.Context, original *model.Task, edit model.TaskEdit, create model.Task) (IntervalPreview, error) {
	var out IntervalPreview
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = prepareInterval(ctx, tx, original, edit, create)
		return err
	})
	return out, err
}

func prepareInterval(ctx context.Context, q execer, original *model.Task, edit model.TaskEdit, create model.Task) (IntervalPreview, error) {
	p := IntervalPreview{original: original, edit: edit, create: create, Task: create}
	if original != nil {
		if original.Status.Done() || original.IsAllDay || original.RepeatFlag != "" {
			return p, errors.New("interval editing requires an open non-all-day task without repeats")
		}
		current, err := loadTask(ctx, q, original.Id)
		if err != nil {
			return p, err
		}
		if !reflect.DeepEqual(current, *original) {
			return p, ErrTaskChanged
		}
		p.Task = applyEdit(current, edit)
	}
	if p.Task.Status.Done() || p.Task.IsAllDay || p.Task.RepeatFlag != "" {
		return p, errors.New("interval editing requires an open timed task without repeats")
	}
	if err := (dates.Interval{Start: p.Task.StartDate, End: p.Task.DueDate, Zone: p.Task.TimeZone}).Validate(); err != nil {
		return p, err
	}
	var err error
	p.Projects, err = queryProjects(ctx, q, `SELECT `+projectColumns+` FROM projects ORDER BY id`)
	if err != nil {
		return p, err
	}
	projects := make(map[string]model.Project)
	for _, project := range p.Projects {
		projects[project.Id] = project
	}
	project, ok := projects[p.Task.ProjectId]
	if !ok || project.CreateUnavailable() != "" {
		return p, errors.New("interval target needs an available cached list")
	}
	p.LastSync, _, err = getMeta(ctx, q, "last_sync")
	if err != nil {
		return p, err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE status = ? ORDER BY id`, model.TaskOpen.Wire())
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return p, err
		}
		if original != nil && t.Id == original.Id {
			continue
		}
		project, exists := projects[t.ProjectId]
		label := fmt.Sprintf("%q [%q]", t.Title, t.Id)
		if !exists || project.CreateUnavailable() != "" {
			p.Context = append(p.Context, "Excluded list context: "+label)
			continue
		}
		if t.RepeatFlag != "" {
			p.Context = append(p.Context, "Future repeats unchecked: "+label)
		}
		if t.IsAllDay {
			p.Context = append(p.Context, fmt.Sprintf("All-day context: %s; start %s, due %s, zone %q", label, t.StartDate.String(), t.DueDate.String(), t.TimeZone))
			continue
		}
		if !schedule.TimedInterval(t) {
			if !t.StartDate.IsZero() && !t.DueDate.IsZero() && t.StartDate.After(t.DueDate.Time) {
				p.Context = append(p.Context, "Invalid reversed interval excluded: "+label)
			} else if !t.StartDate.IsZero() || !t.DueDate.IsZero() {
				p.Context = append(p.Context, "Point/partial schedule, no duration inferred: "+label)
			}
			continue
		}
		if _, err := dates.IntervalZone(t.TimeZone); err != nil {
			p.Context = append(p.Context, fmt.Sprintf("Unknown stored zone; absolute instants only: %s; zone %q", label, t.TimeZone))
		}
		start, end := t.StartDate, t.DueDate
		if p.Task.StartDate.After(start.Time) {
			start = p.Task.StartDate
		}
		if p.Task.DueDate.Before(end.Time) {
			end = p.Task.DueDate
		}
		if end.After(start.Time) {
			p.Overlaps = append(p.Overlaps, IntervalOverlap{Task: t, List: project.Name, Start: start, End: end})
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	slices.SortFunc(p.Overlaps, func(a, b IntervalOverlap) int {
		if v := a.Start.Compare(b.Start.Time); v != 0 {
			return v
		}
		return strings.Compare(a.Task.Id, b.Task.Id)
	})
	data, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	p.stamp = sha256.Sum256(data)
	return p, nil
}

func (s *Store) ApplyInterval(ctx context.Context, expected IntervalPreview, allowOverlap bool) (TaskMutationOutcome, error) {
	var out TaskMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := prepareInterval(ctx, tx, expected.original, expected.edit, expected.create)
		if err != nil {
			return err
		}
		if current.stamp != expected.stamp {
			return ErrIntervalChanged
		}
		if len(current.Overlaps) != 0 && !allowOverlap {
			return ErrIntervalOverlap
		}
		if expected.original != nil {
			out, err = editTaskTxOutcome(ctx, tx, expected.original.Id, OpTaskUpdate, TaskUpdatedKind,
				func(context.Context, *sql.Tx, model.Task) (model.TaskEdit, []string, error) {
					return expected.edit, nil, nil
				}, true)
			return err
		}
		task := current.Task
		if task.Id != "" {
			return errors.New("new interval task must not have an ID")
		}
		task.Id, err = NewLocalID()
		if err != nil {
			return err
		}
		task.CreatedTime, task.ModifiedTime = model.NewTime(time.Now()), model.NewTime(time.Now())
		if err := createTaskTx(ctx, tx, &task, false, true); err != nil {
			return err
		}
		out = TaskMutationOutcome{Task: task, Changed: true}
		return nil
	})
	if err != nil {
		return TaskMutationOutcome{}, err
	}
	return out, nil
}

func (p IntervalPreview) Lines() []string {
	const stamp = "2006-01-02 15:04:05.000 -07:00"
	loc, _ := dates.IntervalZone(p.Task.TimeZone)
	if loc == nil {
		loc = time.UTC
	}
	lines := []string{fmt.Sprintf("Task: %q [%q]", p.Task.Title, p.Task.Id)}
	if p.original != nil {
		lines = append(lines, "Old start: "+p.original.StartDate.String(), "Old due: "+p.original.DueDate.String())
	}
	lines = append(lines, "Start: "+p.Task.StartDate.In(loc).Format(stamp), "End/due: "+p.Task.DueDate.In(loc).Format(stamp),
		fmt.Sprintf("Duration: %s; zone %q", dates.ElapsedLabel(p.Task.DueDate.Sub(p.Task.StartDate.Time)), p.Task.TimeZone),
		fmt.Sprintf("%d overlaps in cached open timed tasks across lists", len(p.Overlaps)))
	if p.LastSync == "" {
		lines = append(lines, "Cache last sync: unknown")
	} else {
		lines = append(lines, fmt.Sprintf("Cache last sync: %q", p.LastSync))
	}
	lines = append(lines, "Cache only; future repeats and full server availability are unchecked.")
	if len(p.Task.Reminders) != 0 {
		lines = append(lines, "Existing reminders kept; provider timing may change.")
	}
	for _, overlap := range p.Overlaps {
		t := overlap.Task
		suffix := ""
		if t.RepeatFlag != "" {
			suffix = " (stored recurring interval only)"
		}
		lines = append(lines, fmt.Sprintf("%q / %q [%q]: %s -> %s; overlap %s%s", overlap.List, t.Title, t.Id,
			t.StartDate.In(loc).Format(stamp), t.DueDate.In(loc).Format(stamp), dates.ElapsedLabel(overlap.End.Sub(overlap.Start.Time)), suffix))
	}
	return append(lines, p.Context...)
}
