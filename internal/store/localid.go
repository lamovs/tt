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

func dropOrphanedLocalTask(ctx context.Context, q *sql.Tx, id string) (bool, error) {
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
	if err := dropQueuedChildLinks(ctx, q, id); err != nil {
		return false, err
	}
	// A child of the dropped task keeps pointing at an id nothing holds any
	// more, and tt prints that id back to the reader. The next pull corrects
	// the rows it returns, so these two statements only keep a local id out
	// of the cache until then.
	for _, step := range []idSwapStep{
		{"tasks parent", `UPDATE tasks SET parent_id = '' WHERE parent_id = ?`, []any{id}},
		{"tasks children", `UPDATE tasks SET child_ids = (
			SELECT json_group_array(value) FROM json_each(child_ids) WHERE value <> ?)
			WHERE json_valid(child_ids) AND instr(child_ids, ?) > 0`, []any{id, id}},
	} {
		if _, err := q.ExecContext(ctx, step.query, step.args...); err != nil {
			return false, fmt.Errorf("drop orphaned local task %s from %s: %w", id, step.what, err)
		}
	}
	return true, nil
}

// dropQueuedChildLinks throws away the unsent relationships that hang a task
// under a dropped one. Such an entry belongs to the child, so its task_id is
// the id of the child and the delete of the entries of the dropped task
// leaves it behind; the next push then parks it over a parent the cache no
// longer holds, and a second tt sync --drop-parked is what it takes to be rid
// of it. Candidates are narrowed by the mention of the id, which is wider
// than the rule - the payload of the child names its own task too - and each
// one is decided by taskExtensionParent, the way validateTaskClosure decides
// them. An inflight entry is left where it is: it may be on the server
// already and only the push can settle it.
//
// Other fields of a prepared update stay queued. Frozen mixed updates cannot
// be rewritten safely, so they refuse the drop. Creates stay for the push to
// park, since dropping a relationship must not discard the child itself.
func dropQueuedChildLinks(ctx context.Context, q *sql.Tx, id string) error {
	rows, err := q.QueryContext(ctx, childLinkCandidateSQL+` AND op <> ? AND state <> ? AND instr(payload, ?) > 0 ORDER BY seq`,
		OpTaskCreate, string(OutboxInflight), id)
	if err != nil {
		return fmt.Errorf("drop the children queued under %s: %w", id, err)
	}
	links, err := scanChildLinks(rows)
	if err != nil {
		return fmt.Errorf("drop the children queued under %s: %w", id, err)
	}
	for _, link := range links {
		if link.parent != id {
			continue
		}
		var payload []byte
		var rev int64
		if err := q.QueryRowContext(ctx, `SELECT payload, coalesce(rev, 0) FROM outbox WHERE seq = ?`, link.seq).Scan(&payload, &rev); err != nil {
			return err
		}
		edit, metadata, err := DecodeTaskEditPayload(payload)
		if err != nil {
			return err
		}
		edit.ParentId = nil
		if edit.IsEmpty() {
			if _, err := q.ExecContext(ctx, `DELETE FROM outbox WHERE seq = ?`, link.seq); err != nil {
				return fmt.Errorf("drop the child queued under %s: %w", id, err)
			}
			if err := reconcileDroppedChildRevision(ctx, q, link.child, rev); err != nil {
				return err
			}
		} else {
			if metadata.Phase != FeaturePrepared {
				return fmt.Errorf("cannot drop parent %s: child operation %d has other fields in a frozen request; recover that operation first", id, link.seq)
			}
			metadata.ExtensionBaseline.ParentId = nil
			metadata.Fields.Extensions = hasTaskExtensions(edit)
			if !metadata.Fields.Extensions {
				metadata.ExtensionBaseline = nil
			}
			metadata.Version = newFeaturePayloadVersion(metadata.Fields)
			if metadata.Fields.empty() {
				payload, err = json.Marshal(edit)
			} else {
				payload, err = EncodeTaskEditPayload(edit, *metadata)
			}
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `UPDATE outbox SET payload = ? WHERE seq = ?`, payload, link.seq); err != nil {
				return err
			}
		}
		if err := bumpItemIdentityEpoch(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func reconcileDroppedChildRevision(ctx context.Context, q execer, id string, rev int64) error {
	if rev == 0 {
		return nil
	}
	var remaining, unversioned int
	var latest int64
	if err := q.QueryRowContext(ctx, `SELECT count(*), count(*) - count(rev), coalesce(max(rev), 0)
		FROM outbox WHERE task_id = ?`, id).Scan(&remaining, &unversioned, &latest); err != nil {
		return err
	}
	if remaining == 0 {
		_, err := clearDirtyAt(ctx, q, id, rev)
		return err
	}
	if unversioned != 0 || latest == 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `UPDATE tasks SET dirty = ? WHERE id = ? AND dirty = ?`, latest, id, rev)
	return err
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

		// A child may name a parent that is still waiting for this very
		// create, so the rows pointing at the old id are rewritten in both
		// branches: the local row is gone in a merge, the references to it
		// are not.
		{"tasks parent", `UPDATE tasks SET parent_id = ? WHERE parent_id = ?`, []any{serverID, localID}},
		// child_ids is written by the pull alone, and what the server
		// returns never holds a local id, so this step corrects nothing
		// today. It is kept as a guard: a future writer of child_ids would
		// otherwise leave a swapped id behind without a word.
		{"tasks children", `UPDATE tasks SET child_ids = replace(child_ids, ?, ?) WHERE instr(child_ids, ?) > 0`,
			[]any{localID, serverID, localID}},

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
