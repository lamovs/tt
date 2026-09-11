package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/movsar/tt/internal/model"
)

const LocalIDPrefix = "local-"

func NewLocalID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate local id: %w", err)
	}
	return LocalIDPrefix + hex.EncodeToString(b[:]), nil
}

func IsLocalID(id string) bool { return strings.HasPrefix(id, LocalIDPrefix) }

const noQueuedCreate = `NOT EXISTS (
	SELECT 1 FROM outbox WHERE outbox.task_id = tasks.id AND outbox.op = 'task.create')`

type OrphanedLocalTask struct {
	ID          string
	ProjectID   string
	Title       string
	CreatedTime model.Time
}

func (s *Store) OrphanedLocalTasks(ctx context.Context) ([]OrphanedLocalTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, title, created_time FROM tasks
		 WHERE local = 1 AND `+noQueuedCreate+`
		 ORDER BY created_time IS NULL, created_time, id`)
	if err != nil {
		return nil, fmt.Errorf("read orphaned local tasks: %w", err)
	}
	defer rows.Close()
	var out []OrphanedLocalTask
	for rows.Next() {
		var (
			t       OrphanedLocalTask
			created sql.NullString
		)
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Title, &created); err != nil {
			return nil, fmt.Errorf("read orphaned local tasks: %w", err)
		}
		if t.CreatedTime, err = readStamp(created); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read orphaned local tasks: %w", err)
	}
	return out, nil
}

func dropOrphanedLocalTask(ctx context.Context, q execer, id string) (bool, error) {
	if !IsLocalID(id) {
		return false, nil
	}
	res, err := q.ExecContext(ctx,
		`DELETE FROM tasks WHERE id = ? AND local = 1 AND `+noQueuedCreate, id)
	if err != nil {
		return false, fmt.Errorf("drop orphaned local task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("drop orphaned local task %s: %w", id, err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM outbox WHERE task_id = ?`, id); err != nil {
		return false, fmt.Errorf("drop orphaned local task %s: %w", id, err)
	}
	return true, nil
}

func (s *Store) ReplaceLocalID(ctx context.Context, localID, serverID string) error {
	if err := checkIDSwap(localID, serverID); err != nil {
		return err
	}
	if localID == serverID {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		return replaceLocalID(ctx, tx, localID, serverID)
	})
}

func ReplaceLocalIDTx(ctx context.Context, tx *sql.Tx, localID, serverID string) error {
	if err := checkIDSwap(localID, serverID); err != nil {
		return err
	}
	if localID == serverID {
		return nil
	}
	return replaceLocalID(ctx, tx, localID, serverID)
}

func checkIDSwap(localID, serverID string) error {
	if !IsLocalID(localID) {
		return fmt.Errorf("replace id: %q is not a local id", localID)
	}
	if serverID == "" || IsLocalID(serverID) {
		return fmt.Errorf("replace id: %q is not a server id", serverID)
	}
	return nil
}

const IDReplacedKind = "task.id_replaced"

type idSwapStep struct {
	what  string
	query string
	args  []any
}

func replaceLocalID(ctx context.Context, q execer, localID, serverID string) error {

	merged, err := taskExists(ctx, q, serverID)
	if err != nil {
		return err
	}

	head := []idSwapStep{{"tasks", `UPDATE tasks SET id = ?, local = 0 WHERE id = ?`, []any{serverID, localID}}}
	if merged {

		head = []idSwapStep{
			{"tasks", `DELETE FROM tasks WHERE id = ?`, []any{localID}},
			{"outbox rev", `UPDATE outbox SET rev = NULL WHERE task_id = ?`, []any{localID}},
		}
	}
	steps := append(head, []idSwapStep{
		{"outbox", `UPDATE outbox SET task_id = ? WHERE task_id = ?`, []any{serverID, localID}},
		{"listing", `UPDATE listing SET task_id = ? WHERE task_id = ?`, []any{serverID, localID}},
		{"focus_sessions", `UPDATE focus_sessions SET task_id = ? WHERE task_id = ?`, []any{serverID, localID}},
		{"timer_state", `UPDATE timer_state SET task_id = ? WHERE task_id = ?`, []any{serverID, localID}},
		{"reminder_deliveries", `UPDATE OR IGNORE reminder_deliveries SET task_id = ? WHERE task_id = ?`, []any{serverID, localID}},
		{"reminder_deliveries old id", `DELETE FROM reminder_deliveries WHERE task_id = ?`, []any{localID}},

		{"outbox payload", `UPDATE outbox SET payload = replace(payload, ?, ?) WHERE instr(payload, ?) > 0`, []any{localID, serverID, localID}},
		{"undo_log payload", `UPDATE undo_log SET payload = replace(payload, ?, ?) WHERE instr(payload, ?) > 0`, []any{localID, serverID, localID}},
	}...)
	for _, s := range steps {
		if _, err := q.ExecContext(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("replace id in %s: %w", s.what, err)
		}
	}
	payload, err := json.Marshal(struct {
		OldID string `json:"old_id"`
		NewID string `json:"new_id"`
	}{OldID: localID, NewID: serverID})
	if err != nil {
		return fmt.Errorf("replace id: %w", err)
	}
	if _, err := appendEvent(ctx, q, IDReplacedKind, payload); err != nil {
		return err
	}
	return nil
}

func taskExists(ctx context.Context, q execer, id string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("replace id: %w", err)
	}
	return n > 0, nil
}

func (s *Store) CurrentTaskID(ctx context.Context, id string) (string, error) {
	var after int64
	seen := map[string]bool{id: true}
	for range 1000 {
		var next, kind string
		err := s.db.QueryRowContext(ctx, `SELECT seq,kind,
			CASE WHEN kind=? THEN json_extract(payload,'$.new_id') ELSE json_extract(payload,'$.id') END
			FROM events WHERE seq>? AND ((kind=? AND json_extract(payload,'$.old_id')=?)
			OR (kind=? AND json_extract(payload,'$.from_id')=?)) ORDER BY seq LIMIT 1`,
			IDReplacedKind, after, IDReplacedKind, id, TaskMovedKind, id).Scan(&after, &kind, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", fmt.Errorf("read task ID replacement: %w", err)
		}
		if next == "" || seen[next] || kind == IDReplacedKind && IsLocalID(next) {
			return "", errors.New("invalid committed task ID replacement")
		}
		seen[next] = true
		id = next
	}
	return "", errors.New("task ID replacement chain exceeds 1000 events")
}
