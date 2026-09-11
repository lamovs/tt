package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/movsar/tt/internal/model"
)

const taskColumns = `id, project_id, title, content, status, priority, due_date, start_date,
	is_all_day, time_zone, repeat_flag, reminders, tags, kind, sort_order,
	created_time, modified_time, completed_time, parent_id, child_ids, column_id, column_name,
	estimated_duration, estimated_pomo, focus_summaries`

const itemColumns = `id, title, status, sort_order, start_date, is_all_day, time_zone, completed_time`

type ServerTask struct {
	Task model.Task
	Raw  json.RawMessage
}

type SyncResult struct {
	Upserted int
	Skipped  int
	Deleted  int
	Kept     int
	Stale    bool
}

func (s *Store) SyncProject(ctx context.Context, projectID string, tasks []ServerTask) (SyncResult, error) {
	var res SyncResult
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res = SyncResult{}
		name, err := projectName(ctx, tx, projectID)
		if err != nil {
			return err
		}
		cached, err := cachedTaskStates(ctx, tx, projectID)
		if err != nil {
			return err
		}
		incoming := make(map[string]bool, len(tasks))
		for _, st := range tasks {
			if st.Task.Id == "" {
				return errors.New("cache the list: task without an id")
			}
			incoming[st.Task.Id] = true
			ok, err := upsertServerTask(ctx, tx, st, name)
			if err != nil {
				return err
			}
			if !ok {

				res.Skipped++
				continue
			}
			if err := replaceItems(ctx, tx, st.Task.Id, st.Task.Items); err != nil {
				return err
			}
			res.Upserted++
		}
		for id, st := range cached {
			if incoming[id] {
				continue
			}
			if st.done || st.dirty || st.local {
				res.Kept++
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
				return fmt.Errorf("cache the list %s: %w", projectID, err)
			}
			res.Deleted++
		}
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	return res, nil
}

func (s *Store) PurgeCompleted(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tasks
		 WHERE status <> ? AND dirty = 0 AND local = 0
		   AND completed_time IS NOT NULL AND completed_time < ?`,
		model.TaskOpen.Wire(), FormatStamp(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge completed: %w", err)
	}
	return res.RowsAffected()
}

type StatusFilter int

const (
	StatusOpen StatusFilter = iota
	StatusAll
	StatusDone
)

type DueFilter int

const (
	DueAny DueFilter = iota
	DueSet
	DueNone
)

type TaskOrder int

const (
	OrderDue TaskOrder = iota
	OrderPriority
	OrderCreated
	OrderModified
)

func (o TaskOrder) clause() string {
	switch o {
	case OrderPriority:
		return `priority DESC, due_date IS NULL, due_date, id`
	case OrderCreated:
		return `created_time IS NULL, created_time DESC, id`
	case OrderModified:
		return `modified_time IS NULL, modified_time DESC, id`
	default:

		return `due_date IS NULL, due_date, priority DESC, sort_order, created_time IS NULL, created_time, id`
	}
}

type TaskFilter struct {
	ProjectID string
	Status    StatusFilter
	Due       DueFilter

	DueFrom model.Time
	DueTo   model.Time

	DoneFrom model.Time
	DoneTo   model.Time

	Search string
	Limit  int
	Order  TaskOrder
}

func (s *Store) Tasks(ctx context.Context, f TaskFilter) ([]model.Task, error) {
	var (
		where []string
		args  []any
	)
	if f.ProjectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, f.ProjectID)
	}
	switch f.Status {
	case StatusOpen:
		where = append(where, `status = ?`)
		args = append(args, model.TaskOpen.Wire())
	case StatusDone:
		where = append(where, `status <> ?`)
		args = append(args, model.TaskOpen.Wire())
	}
	switch f.Due {
	case DueSet:
		where = append(where, `due_date IS NOT NULL`)
	case DueNone:
		where = append(where, `due_date IS NULL`)
	}
	if !f.DueFrom.IsZero() {
		where = append(where, `due_date >= ?`)
		args = append(args, f.DueFrom.StoreString())
	}
	if !f.DueTo.IsZero() {
		where = append(where, `due_date < ?`)
		args = append(args, f.DueTo.StoreString())
	}
	if !f.DoneFrom.IsZero() {
		where = append(where, `completed_time >= ?`)
		args = append(args, f.DoneFrom.StoreString())
	}
	if !f.DoneTo.IsZero() {
		where = append(where, `completed_time < ?`)
		args = append(args, f.DoneTo.StoreString())
	}
	if needle := searchNeedle(f.Search); needle != "" {
		where = append(where, `instr(search, ?) > 0`)
		args = append(args, needle)
	}
	query := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY ` + f.Order.clause()
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	return out, nil
}

func (s *Store) Task(ctx context.Context, id string) (model.Task, error) {
	return loadTask(ctx, s.db, id)
}

func loadTask(ctx context.Context, q execer, id string) (model.Task, error) {
	row := q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Task{}, fmt.Errorf("task %s: %w", id, ErrNotFound)
	case err != nil:
		return model.Task{}, err
	}
	items, err := loadItems(ctx, q, id)
	if err != nil {
		return model.Task{}, err
	}
	t.Items = items
	return t, nil
}

func loadItems(ctx context.Context, q execer, taskID string) ([]model.Item, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT items.`+strings.ReplaceAll(itemColumns, ", ", ", items.")+`,
			item_identities.server_id, item_identities.state
		 FROM items
		 LEFT JOIN item_identities
		   ON item_identities.task_id = items.task_id
		  AND item_identities.item_key = items.id
		 WHERE items.task_id = ?
		 ORDER BY items.position, items.sort_order, items.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read items of %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []model.Item
	for rows.Next() {
		var (
			it            model.Item
			status        int
			startDate     sql.NullString
			completed     sql.NullString
			registryID    sql.NullString
			registryState sql.NullString
		)
		err := rows.Scan(&it.Key, &it.Title, &status, &it.SortOrder, &startDate,
			&it.IsAllDay, &it.TimeZone, &completed, &registryID, &registryState)
		if err != nil {
			return nil, fmt.Errorf("read items of %s: %w", taskID, err)
		}
		it.Status = model.ItemStatus(status)

		switch {
		case !registryState.Valid && !IsLocalID(it.Key):
			it.Id = it.Key
		case registryState.Valid && ItemIdentityState(registryState.String) == ItemBound:
			if !registryID.Valid {
				return nil, fmt.Errorf("read items of %s: bound item %s has no server id", taskID, it.Key)
			}
			it.Id = registryID.String
		}
		if it.StartDate, err = readStamp(startDate); err != nil {
			return nil, err
		}
		if it.CompletedTime, err = readStamp(completed); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read items of %s: %w", taskID, err)
	}
	return out, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanTask(sc scanner) (model.Task, error) {
	var (
		t              model.Task
		status         int
		priority       int
		due            sql.NullString
		start          sql.NullString
		created        sql.NullString
		modified       sql.NullString
		completed      sql.NullString
		reminders      string
		tags           string
		childIDs       string
		focusSummaries string
	)
	err := sc.Scan(&t.Id, &t.ProjectId, &t.Title, &t.Content, &status, &priority,
		&due, &start, &t.IsAllDay, &t.TimeZone, &t.RepeatFlag, &reminders, &tags,
		&t.Kind, &t.SortOrder, &created, &modified, &completed, &t.ParentId, &childIDs,
		&t.ColumnId, &t.ColumnName, &t.EstimatedDuration, &t.EstimatedPomo, &focusSummaries)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Task{}, err
	case err != nil:
		return model.Task{}, fmt.Errorf("read task: %w", err)
	}

	t.Status = model.TaskStatus(status)
	t.Priority = model.Priority(priority)
	if t.Reminders, err = decodeStrings(reminders); err != nil {
		return model.Task{}, err
	}
	if t.Tags, err = decodeStrings(tags); err != nil {
		return model.Task{}, err
	}
	if t.ChildIds, err = decodeStrings(childIDs); err != nil {
		return model.Task{}, err
	}
	if focusSummaries != "null" {
		if !json.Valid([]byte(focusSummaries)) {
			return model.Task{}, errors.New("invalid stored focus summaries")
		}
		t.FocusSummaries = json.RawMessage(focusSummaries)
	}
	for _, f := range []struct {
		dst *model.Time
		src sql.NullString
	}{
		{&t.DueDate, due}, {&t.StartDate, start}, {&t.CreatedTime, created},
		{&t.ModifiedTime, modified}, {&t.CompletedTime, completed},
	} {
		if *f.dst, err = readStamp(f.src); err != nil {
			return model.Task{}, err
		}
	}
	return t, nil
}

func readStamp(v sql.NullString) (model.Time, error) {
	t, err := ParseStamp(v)
	if err != nil {
		return model.Time{}, err
	}
	return model.NewTime(t), nil
}

type taskState struct {
	done  bool
	dirty bool
	local bool
}

func cachedTaskStates(ctx context.Context, q execer, projectID string) (map[string]taskState, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, status, dirty, local FROM tasks WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read tasks of %s: %w", projectID, err)
	}
	defer rows.Close()
	out := map[string]taskState{}
	for rows.Next() {
		var (
			id     string
			status int
			dirty  int64
			st     taskState
		)
		if err := rows.Scan(&id, &status, &dirty, &st.local); err != nil {
			return nil, fmt.Errorf("read tasks of %s: %w", projectID, err)
		}
		st.dirty = dirty != 0
		st.done = status != model.TaskOpen.Wire()
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tasks of %s: %w", projectID, err)
	}
	return out, nil
}

const taskInsertColumns = `id, project_id, title, content, status, priority, due_date, start_date,
	is_all_day, time_zone, repeat_flag, reminders, tags, kind, sort_order,
	created_time, modified_time, completed_time, search, parent_id, child_ids, column_id, column_name,
	estimated_duration, estimated_pomo, focus_summaries`

const taskUpdateAssignments = `
	project_id = excluded.project_id,
	title = excluded.title,
	content = excluded.content,
	status = excluded.status,
	priority = excluded.priority,
	due_date = excluded.due_date,
	start_date = excluded.start_date,
	is_all_day = excluded.is_all_day,
	time_zone = excluded.time_zone,
	repeat_flag = excluded.repeat_flag,
	reminders = excluded.reminders,
	tags = excluded.tags,
	kind = excluded.kind,
	sort_order = excluded.sort_order,
	created_time = excluded.created_time,
	modified_time = excluded.modified_time,
	completed_time = excluded.completed_time,
	search = excluded.search,
	parent_id = excluded.parent_id,
	child_ids = excluded.child_ids,
	column_id = excluded.column_id,
	column_name = excluded.column_name,
	estimated_duration = excluded.estimated_duration,
	estimated_pomo = excluded.estimated_pomo,
	focus_summaries = excluded.focus_summaries`

func upsertServerTask(ctx context.Context, q execer, st ServerTask, projectName string) (bool, error) {
	raw := "{}"
	if len(st.Raw) > 0 {
		raw = string(st.Raw)
	}
	args := append(taskValues(st.Task, taskSearch(st.Task, projectName)), raw)
	res, err := q.ExecContext(ctx,
		`INSERT INTO tasks (`+taskInsertColumns+`, raw, local, dirty)
		 VALUES (`+placeholders(27)+`, 0, 0)
		 ON CONFLICT(id) DO UPDATE SET`+taskUpdateAssignments+`,
		     raw = excluded.raw
		 WHERE tasks.dirty = 0 AND tasks.local = 0`, args...)
	if err != nil {
		return false, fmt.Errorf("cache task %s: %w", st.Task.Id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cache task %s: %w", st.Task.Id, err)
	}
	return n > 0, nil
}

func writeLocalTask(ctx context.Context, q execer, t model.Task, projectName string) (int64, error) {
	args := append(taskValues(t, taskSearch(t, projectName)), IsLocalID(t.Id))
	var rev int64
	err := q.QueryRowContext(ctx,
		`INSERT INTO tasks (`+taskInsertColumns+`, local, dirty)
		 VALUES (`+placeholders(27)+`, 1)
		 ON CONFLICT(id) DO UPDATE SET`+taskUpdateAssignments+`,
		     dirty = max(
		         tasks.dirty,
		         coalesce((SELECT max(o.rev) FROM outbox o WHERE o.task_id = tasks.id), 0)
		     ) + 1
		 RETURNING dirty`, args...).Scan(&rev)
	if err != nil {
		return 0, fmt.Errorf("cache task %s: %w", t.Id, err)
	}
	return rev, nil
}

func MarkPushedTx(ctx context.Context, tx *sql.Tx, taskID string, rev int64) (bool, error) {
	ok, err := clearDirtyAt(ctx, tx, taskID, rev)
	if err != nil {
		return false, fmt.Errorf("mark task %s pushed: %w", taskID, err)
	}
	return ok, nil
}

func clearDirtyAt(ctx context.Context, q execer, taskID string, rev int64) (bool, error) {
	if rev == 0 {
		return false, nil
	}
	res, err := q.ExecContext(ctx, `UPDATE tasks SET dirty = 0 WHERE id = ? AND dirty = ?
		AND NOT EXISTS(SELECT 1 FROM outbox WHERE task_id=tasks.id AND op='task.move_native')
		AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.task_id=tasks.id AND `+retainedColumnMutationSQL("o.")+`)`, taskID, rev)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func taskValues(t model.Task, search string) []any {
	return []any{
		t.Id, t.ProjectId, t.Title, t.Content, t.Status.Wire(), t.Priority.Wire(),
		FormatStamp(t.DueDate.Time), FormatStamp(t.StartDate.Time),
		t.IsAllDay, t.TimeZone, t.RepeatFlag,
		encodeStrings(t.Reminders), encodeStrings(t.Tags), t.Kind, t.SortOrder,
		FormatStamp(t.CreatedTime.Time), FormatStamp(t.ModifiedTime.Time),
		FormatStamp(t.CompletedTime.Time), search,
		t.ParentId, encodeStrings(t.ChildIds), t.ColumnId, t.ColumnName,
		t.EstimatedDuration, t.EstimatedPomo, taskFocusJSON(t.FocusSummaries),
	}
}

func taskFocusJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

func replaceItems(ctx context.Context, q execer, taskID string, items []model.Item) error {
	keys, err := persistedItemKeys(items)
	if err != nil {
		return fmt.Errorf("cache items of %s: %w", taskID, err)
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM items WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("cache items of %s: %w", taskID, err)
	}
	for i, it := range items {
		_, err := q.ExecContext(ctx,
			`INSERT INTO items (task_id, `+itemColumns+`, position)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, keys[i], it.Title, it.Status.Wire(), it.SortOrder,
			FormatStamp(it.StartDate.Time), it.IsAllDay, it.TimeZone,
			FormatStamp(it.CompletedTime.Time), i)
		if err != nil {
			return fmt.Errorf("cache items of %s: %w", taskID, err)
		}
	}
	return nil
}

func persistedItemKeys(items []model.Item) ([]string, error) {
	explicit := false
	for _, item := range items {
		explicit = explicit || item.Key != ""
	}
	if !explicit {
		return itemKeys(items), nil
	}
	keys := make([]string, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		if item.Key == "" {
			return nil, fmt.Errorf("item %d has no key", i+1)
		}
		if seen[item.Key] {
			return nil, fmt.Errorf("item %d repeats key %q", i+1, item.Key)
		}
		seen[item.Key] = true
		keys[i] = item.Key
	}
	return keys, nil
}

func itemKeys(items []model.Item) []string {
	taken := make(map[string]bool, len(items))
	for _, it := range items {
		if it.Id != "" {
			taken[it.Id] = true
		}
	}
	keys := make([]string, len(items))
	used := make(map[string]bool, len(items))
	for i, it := range items {
		key := it.Id
		if key == "" || used[key] {
			for n := i; ; n++ {
				key = fmt.Sprintf("%s%06d", LocalIDPrefix, n)
				if !taken[key] && !used[key] {
					break
				}
			}
		}
		used[key] = true
		keys[i] = key
	}
	return keys
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func encodeStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeStrings(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode %q: %w", s, err)
	}
	return out, nil
}
