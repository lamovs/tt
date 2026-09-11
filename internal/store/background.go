package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/schedule"
)

const (
	BackgroundLastTickKey    = "background_last_tick"
	BackgroundLastAttemptKey = "background_last_attempt"
	BackgroundLastSuccessKey = "background_last_success"
	BackgroundLastErrorKey   = "background_last_error"
)

type ReminderOccurrence struct {
	TaskID          string
	Title           string
	Project         string
	Due             model.Time
	Trigger         string
	At              time.Time
	ServerConfirmed bool
}

func (s *Store) DueReminderOccurrences(ctx context.Context, from, through time.Time) ([]ReminderOccurrence, int, error) {
	if through.Before(from) {
		return nil, 0, errors.New("reminder window ends before it starts")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.title, p.name, t.due_date, t.reminders,
		       t.local, t.dirty,
		       EXISTS(SELECT 1 FROM outbox o WHERE o.task_id = t.id)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE t.status = ? AND p.closed = 0 AND t.is_all_day = 0
		  AND t.due_date IS NOT NULL AND t.reminders <> '[]'
		ORDER BY t.due_date, t.id`, model.TaskOpen.Wire())
	if err != nil {
		return nil, 0, fmt.Errorf("read reminder candidates: %w", err)
	}
	defer rows.Close()
	var out []ReminderOccurrence
	unsupported := 0
	for rows.Next() {
		var (
			id, title, project, dueText, remindersText string
			local, dirty, queued                       int64
		)
		if err := rows.Scan(&id, &title, &project, &dueText, &remindersText, &local, &dirty, &queued); err != nil {
			return nil, 0, fmt.Errorf("read reminder candidate: %w", err)
		}
		due, err := model.ParseStoreTime(dueText)
		if err != nil || due.IsZero() {
			unsupported++
			continue
		}
		reminders, err := decodeStrings(remindersText)
		if err != nil {
			return nil, 0, fmt.Errorf("read reminders for task %s: %w", id, err)
		}
		for _, trigger := range reminders {
			at, ok := schedule.ReminderTime(trigger, due.Time)
			if !ok {
				unsupported++
				continue
			}
			if at.Before(from) || at.After(through) {
				continue
			}
			out = append(out, ReminderOccurrence{
				TaskID: id, Title: title, Project: project, Due: due, Trigger: trigger, At: at,
				ServerConfirmed: local == 0 && dirty == 0 && queued == 0,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read reminder candidates: %w", err)
	}
	return out, unsupported, nil
}

func (s *Store) ClaimReminderOccurrence(ctx context.Context, occurrence ReminderOccurrence, decision string, now time.Time) (bool, error) {
	if decision != "provider" && decision != "local" {
		return false, errors.New("invalid reminder decision")
	}
	confirmed := decision == "provider"
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO reminder_deliveries(task_id, due_date, trigger, decision, claimed_at)
		SELECT ?, ?, ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM tasks t JOIN projects p ON p.id = t.project_id
			WHERE t.id = ? AND t.status = ? AND p.closed = 0 AND t.is_all_day = 0
			  AND t.due_date = ? AND json_valid(t.reminders)
			  AND EXISTS(SELECT 1 FROM json_each(t.reminders) WHERE value = ?)
			  AND ((? = 1 AND t.local = 0 AND t.dirty = 0
			        AND NOT EXISTS(SELECT 1 FROM outbox o WHERE o.task_id = t.id))
			       OR (? = 0 AND (t.local <> 0 OR t.dirty <> 0
			        OR EXISTS(SELECT 1 FROM outbox o WHERE o.task_id = t.id))))
		)`, occurrence.TaskID, occurrence.Due.StoreString(), occurrence.Trigger, decision, now.Unix(),
		occurrence.TaskID, model.TaskOpen.Wire(), occurrence.Due.StoreString(), occurrence.Trigger,
		confirmed, confirmed)
	if err != nil {
		return false, fmt.Errorf("claim reminder occurrence: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim reminder occurrence: %w", err)
	}
	return n == 1, nil
}

func (s *Store) AutomaticTaskWork(ctx context.Context) (bool, OutboxCounts, error) {
	counts, err := s.OutboxCounts(ctx)
	if err != nil {
		return false, counts, err
	}
	return counts.Pending > 0 || counts.Inflight > 0, counts, nil
}

func BackgroundTime(value string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

func (s *Store) SetBackgroundTime(ctx context.Context, key string, value time.Time) error {
	return s.SetMeta(ctx, key, fmt.Sprintf("%d", value.Unix()))
}

func (s *Store) BackgroundTime(ctx context.Context, key string) (time.Time, bool, error) {
	value, ok, err := s.Meta(ctx, key)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	stamp, ok := BackgroundTime(value)
	if !ok {
		return time.Time{}, false, fmt.Errorf("invalid %s value", key)
	}
	return stamp, true, nil
}

func (s *Store) PruneReminderDeliveries(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM reminder_deliveries WHERE claimed_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune reminder deliveries: %w", err)
	}
	return result.RowsAffected()
}
