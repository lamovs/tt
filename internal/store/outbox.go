package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type OutboxTarget string

const (
	TargetOpenAPI OutboxTarget = "openapi"
	TargetV2      OutboxTarget = "v2"
)

type OutboxState string

const (
	OutboxPending  OutboxState = "pending"
	OutboxInflight OutboxState = "inflight"
	OutboxFailed   OutboxState = "failed"
)

var ErrNoOutboxRow = errors.New("outbox row not found")

var ErrLeaseLost = errors.New("outbox lease taken over")

type LeaseToken string

type OutboxEntry struct {
	Target    OutboxTarget
	Op        string
	TaskID    string
	ProjectID string
	Payload   json.RawMessage

	Rev int64
}

type OutboxItem struct {
	OutboxEntry
	Seq        int64
	CreatedAt  time.Time
	Attempts   int
	LastError  string
	State      OutboxState
	InflightAt time.Time

	LeaseToken LeaseToken
}

type OutboxCounts struct {
	Pending  int
	Inflight int
	Failed   int
	Unknown  int
}

const outboxColumns = `seq, target, op, task_id, project_id, payload, created_at, attempts, last_error, state, inflight_at, rev, lease_token`

func (s *Store) Enqueue(ctx context.Context, e OutboxEntry) (int64, error) {
	return enqueue(ctx, s.db, e)
}

func EnqueueTx(ctx context.Context, tx *sql.Tx, e OutboxEntry) (int64, error) {
	return enqueue(ctx, tx, e)
}

func enqueue(ctx context.Context, q execer, e OutboxEntry) (int64, error) {
	if e.Target != TargetOpenAPI && e.Target != TargetV2 {
		return 0, fmt.Errorf("outbox: unknown target %q", e.Target)
	}
	if e.Op == "" {
		return 0, errors.New("outbox: empty op")
	}
	payload := "{}"
	if len(e.Payload) > 0 {
		payload = string(e.Payload)
	}
	res, err := q.ExecContext(ctx,
		`INSERT INTO outbox (target, op, task_id, project_id, payload, created_at, rev, state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'pending')`,
		string(e.Target), e.Op, nullString(e.TaskID), nullString(e.ProjectID), payload,
		time.Now().Unix(), nullInt64(e.Rev))
	if err != nil {
		return 0, fmt.Errorf("outbox enqueue: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("outbox enqueue: %w", err)
	}
	return seq, nil
}

func (s *Store) Claim(ctx context.Context, limit int, lease time.Duration) ([]OutboxItem, int64, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, 0, err
	}
	var (
		out    []OutboxItem
		parked int64
	)
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		out = nil
		parked = 0

		now := time.Now().Unix()
		cutoff := now - leaseSeconds(lease)

		n, err := parkAmbiguousCreates(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		parked = n
		n, err = parkAmbiguousNativeMoves(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		parked += n

		rows, err := tx.QueryContext(ctx,
			`UPDATE outbox SET state = 'inflight', inflight_at = ?, lease_token = ?, sent_at = NULL,
			     attempts = attempts + 1
			 WHERE seq IN (
			     SELECT q.seq FROM outbox q
			     WHERE (
			         q.state = 'pending'
			         OR (q.state = 'inflight' AND (q.inflight_at IS NULL OR q.inflight_at <= ?))
			     )
			     AND q.op NOT LIKE 'entity.%'
			     AND NOT EXISTS (
			         SELECT 1 FROM outbox older
			         WHERE q.task_id IS NOT NULL
			           AND q.task_id <> ''
			           AND older.task_id = q.task_id
			           AND older.seq < q.seq
			           AND (
			               older.state IN ('pending', 'inflight')
			               OR (
			                   older.state = 'failed'
			                   AND json_valid(older.payload)
			                   AND json_extract(older.payload, '$._tt.fields.items') = 1
			                   AND json_valid(q.payload)
			                   AND json_extract(q.payload, '$._tt.fields.items') = 1
			               )
			           )
			     )
			     ORDER BY q.seq
			     LIMIT ?
			 )
			 RETURNING `+outboxColumns, now, string(token), cutoff, limit)
		if err != nil {
			return fmt.Errorf("outbox claim: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanOutbox(rows)
			if err != nil {
				return err
			}
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("outbox claim: %w", err)
		}

		return rows.Close()
	})
	if err != nil {
		return nil, 0, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, parked, nil
}

func (s *Store) HasQueuedCreate(ctx context.Context, taskID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox
		 WHERE task_id = ? AND op = ? AND state IN ('pending', 'inflight')`,
		taskID, OpTaskCreate).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("outbox queued create of %s: %w", taskID, err)
	}
	return n > 0, nil
}

func (s *Store) MarkDone(ctx context.Context, seq int64, token LeaseToken) error {
	return s.Tx(ctx, func(tx *sql.Tx) error { return markDone(ctx, tx, seq, token) })
}

func MarkDoneTx(ctx context.Context, tx *sql.Tx, seq int64, token LeaseToken) error {
	return markDone(ctx, tx, seq, token)
}

func (s *Store) MarkFailed(ctx context.Context, seq int64, token LeaseToken, cause string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		registered, err := registeredOutboxTask(ctx, tx, seq, token)
		if err != nil {
			return err
		}
		if err := finish(ctx, tx, seq, token, OutboxFailed, cause); err != nil {
			return err
		}
		if registered {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
}

func markFailedTx(ctx context.Context, tx *sql.Tx, seq int64, token LeaseToken, cause string) error {
	return finish(ctx, tx, seq, token, OutboxFailed, cause)
}

func (s *Store) Requeue(ctx context.Context, seq int64, token LeaseToken, cause string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		registered, err := registeredOutboxTask(ctx, tx, seq, token)
		if err != nil {
			return err
		}
		if err := finish(ctx, tx, seq, token, OutboxPending, cause); err != nil {
			return err
		}
		if registered {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
}

func requeueTx(ctx context.Context, tx *sql.Tx, seq int64, token LeaseToken, cause string) error {
	return finish(ctx, tx, seq, token, OutboxPending, cause)
}

func (s *Store) Unclaim(ctx context.Context, seq int64, token LeaseToken, cause string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		registered, err := registeredOutboxTask(ctx, tx, seq, token)
		if err != nil {
			return err
		}
		if err := unclaim(ctx, tx, seq, token, cause); err != nil {
			return err
		}
		if registered {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
}

func registeredOutboxTask(ctx context.Context, q execer, seq int64, token LeaseToken) (bool, error) {
	var taskID sql.NullString
	err := q.QueryRowContext(ctx, `SELECT task_id FROM outbox WHERE seq=? AND lease_token=?`, seq, string(token)).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, notAffected(ctx, q, seq)
	}
	if err != nil {
		return false, err
	}
	registered, err := taskHasItemControl(ctx, q, taskID.String)
	return registered, err
}

func unclaim(ctx context.Context, q execer, seq int64, token LeaseToken, cause string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE outbox SET state = 'pending', last_error = ?, inflight_at = NULL, lease_token = NULL,
		     attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END
		 WHERE seq = ? AND lease_token = ?`,
		cause, seq, string(token))
	if err != nil {
		return fmt.Errorf("outbox unclaim %d: %w", seq, err)
	}
	return affectedOne(ctx, q, res, seq)
}

func markDone(ctx context.Context, q execer, seq int64, token LeaseToken) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM outbox WHERE seq = ? AND lease_token = ?`, seq, string(token))
	if err != nil {
		return fmt.Errorf("outbox done %d: %w", seq, err)
	}
	return affectedOne(ctx, q, res, seq)
}

func discardQueued(ctx context.Context, q *sql.Tx, cond string, args ...any) (int64, []string, error) {
	rows, err := q.QueryContext(ctx,
		`DELETE FROM outbox WHERE `+cond+` RETURNING task_id, op`, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("outbox discard: %w", err)
	}
	var (
		matched int64
		creates []string
		seen    = map[string]bool{}
	)
	for rows.Next() {
		var (
			taskID sql.NullString
			op     string
		)
		if err := rows.Scan(&taskID, &op); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("outbox discard: %w", err)
		}
		matched++
		if op == OpTaskCreate && IsLocalID(taskID.String) && !seen[taskID.String] {
			seen[taskID.String] = true
			creates = append(creates, taskID.String)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("outbox discard: %w", err)
	}

	if err := rows.Close(); err != nil {
		return 0, nil, fmt.Errorf("outbox discard: %w", err)
	}
	var removed []string
	for _, id := range creates {
		gone, err := dropOrphanedLocalTask(ctx, q, id)
		if err != nil {
			return 0, nil, err
		}
		if gone {
			removed = append(removed, id)
		}
	}
	return matched, removed, nil
}

func finish(ctx context.Context, q execer, seq int64, token LeaseToken, state OutboxState, cause string) error {
	if state == OutboxFailed {
		return park(ctx, q, seq, token, cause)
	}
	res, err := q.ExecContext(ctx,
		`UPDATE outbox SET state = ?, last_error = ?, inflight_at = NULL, lease_token = NULL
		 WHERE seq = ? AND lease_token = ?`,
		string(state), cause, seq, string(token))
	if err != nil {
		return fmt.Errorf("outbox %s %d: %w", state, seq, err)
	}
	return affectedOne(ctx, q, res, seq)
}

func park(ctx context.Context, q execer, seq int64, token LeaseToken, cause string) error {
	rows, err := q.QueryContext(ctx,
		`UPDATE outbox SET state = 'failed', last_error = ?, inflight_at = NULL, lease_token = NULL,
		     failed_at = ?
		 WHERE seq = ? AND lease_token = ?
		 RETURNING task_id, rev`,
		cause, time.Now().Unix(), seq, string(token))
	if err != nil {
		return fmt.Errorf("outbox failed %d: %w", seq, err)
	}
	var (
		taskID sql.NullString
		rev    sql.NullInt64
		parked bool
	)
	for rows.Next() {
		if err := rows.Scan(&taskID, &rev); err != nil {
			rows.Close()
			return fmt.Errorf("outbox failed %d: %w", seq, err)
		}
		parked = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("outbox failed %d: %w", seq, err)
	}

	if err := rows.Close(); err != nil {
		return fmt.Errorf("outbox failed %d: %w", seq, err)
	}
	if !parked {
		return notAffected(ctx, q, seq)
	}
	if _, err := clearDirtyAt(ctx, q, taskID.String, rev.Int64); err != nil {
		return fmt.Errorf("outbox failed %d: %w", seq, err)
	}
	return nil
}

const ambiguousCreateError = "create outcome was not recorded; explicit retry required"

func parkAmbiguousCreates(ctx context.Context, q *sql.Tx, cutoff int64) (int64, error) {
	rows, err := q.QueryContext(ctx,
		`UPDATE outbox SET state = 'failed', last_error = ?, failed_at = ?,
		     inflight_at = NULL, lease_token = NULL
		 WHERE state = 'inflight'
		   AND (inflight_at IS NULL OR inflight_at <= ?)
		   AND op = ?
		   AND sent_at IS NOT NULL
		   AND substr(task_id, 1, ?) = ?
		 RETURNING task_id, rev`,
		ambiguousCreateError, time.Now().Unix(), cutoff, OpTaskCreate,
		len(LocalIDPrefix), LocalIDPrefix)
	if err != nil {
		return 0, fmt.Errorf("outbox park stale creates: %w", err)
	}
	var (
		ids  []string
		revs []int64
	)
	for rows.Next() {
		var (
			taskID sql.NullString
			rev    sql.NullInt64
		)
		if err := rows.Scan(&taskID, &rev); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox park stale creates: %w", err)
		}
		ids = append(ids, taskID.String)
		revs = append(revs, rev.Int64)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("outbox park stale creates: %w", err)
	}

	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("outbox park stale creates: %w", err)
	}
	identityChanged := false
	for i, id := range ids {
		registered, err := taskHasItemControl(ctx, q, id)
		if err != nil {
			return 0, fmt.Errorf("outbox park stale creates: %w", err)
		}
		if _, err := clearDirtyAt(ctx, q, id, revs[i]); err != nil {
			return 0, fmt.Errorf("outbox park stale creates: %w", err)
		}
		if registered {
			identityChanged = true
		}
	}
	if identityChanged {
		if err := bumpItemIdentityEpoch(ctx, q); err != nil {
			return 0, fmt.Errorf("outbox park stale creates: %w", err)
		}
	}
	return int64(len(ids)), nil
}

func (s *Store) ReclaimStale(ctx context.Context, lease time.Duration) (reclaimed int64, parked int64, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		reclaimed, parked = 0, 0
		cutoff := time.Now().Unix() - leaseSeconds(lease)
		n, err := parkAmbiguousCreates(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		parked = n
		n, err = parkAmbiguousNativeMoves(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		parked += n
		res, err := tx.ExecContext(ctx,
			`UPDATE outbox SET state = 'pending', inflight_at = NULL, lease_token = NULL
			 WHERE state = 'inflight' AND op NOT LIKE 'entity.%' AND (inflight_at IS NULL OR inflight_at <= ?)`, cutoff)
		if err != nil {
			return fmt.Errorf("outbox reclaim: %w", err)
		}
		reclaimed, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return reclaimed, parked, nil
}

func (s *Store) Renew(ctx context.Context, seq int64, token LeaseToken, lease time.Duration) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		cutoff := now - leaseSeconds(lease)
		res, err := tx.ExecContext(ctx,
			`UPDATE outbox SET inflight_at = ?
			 WHERE seq = ? AND state = 'inflight' AND lease_token = ?
			   AND inflight_at IS NOT NULL AND inflight_at > ?`,
			now, seq, string(token), cutoff)
		if err != nil {
			return fmt.Errorf("outbox renew %d: %w", seq, err)
		}
		return affectedOne(ctx, tx, res, seq)
	})
}

func (s *Store) MarkSent(ctx context.Context, seq int64, token LeaseToken) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE outbox SET sent_at = ?
			 WHERE seq = ? AND state = 'inflight' AND lease_token = ?`,
			time.Now().Unix(), seq, string(token))
		if err != nil {
			return fmt.Errorf("outbox sent %d: %w", seq, err)
		}
		return affectedOne(ctx, tx, res, seq)
	})
}

func (s *Store) RetryFailed(ctx context.Context) (int64, error) {
	return s.RetryFailedConfirmed(ctx, nil)
}

func (s *Store) RetryFailedConfirmed(ctx context.Context, confirm func(parkedCreates int) error) (int64, error) {
	var n int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		n = 0
		if confirm != nil {
			creates, err := countFailedCreates(ctx, tx)
			if err != nil {
				return err
			}
			if err := confirm(creates); err != nil {
				return err
			}
		}
		var registered int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM outbox o JOIN item_identities c ON c.task_id=o.task_id AND c.item_key=''
			WHERE o.state='failed')`).Scan(&registered); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,

			`UPDATE outbox SET state = 'pending', inflight_at = NULL, lease_token = NULL, sent_at = NULL
			 WHERE state = 'failed' AND op NOT LIKE 'entity.%'`)
		if err != nil {
			return fmt.Errorf("outbox retry failed: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		if registered != 0 && n != 0 {
			return bumpItemIdentityEpoch(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) FailedCreates(ctx context.Context) (int, error) {
	return countFailedCreates(ctx, s.db)
}

func countFailedCreates(ctx context.Context, q execer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox
		 WHERE state = 'failed' AND op = ? AND substr(task_id, 1, ?) = ?`,
		OpTaskCreate, len(LocalIDPrefix), LocalIDPrefix).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("outbox failed creates: %w", err)
	}
	return n, nil
}

type DroppedMutation struct {
	Seq       int64
	Op        string
	TaskID    string
	ProjectID string

	Title string

	QueuedAt time.Time

	ParkedAt time.Time

	Attempts int

	Requested bool

	TaskRemoved bool

	ChecklistRecoveryPending bool

	Reason string
}

func (s *Store) DropParked(ctx context.Context) ([]DroppedMutation, error) {
	return s.DropParkedConfirmed(ctx, nil)
}

func (s *Store) DropParkedConfirmed(ctx context.Context, confirm func(parkedCreates int) error) ([]DroppedMutation, error) {
	var out []DroppedMutation
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		out = nil
		if confirm != nil {
			creates, err := countFailedCreates(ctx, tx)
			if err != nil {
				return err
			}
			if err := confirm(creates); err != nil {
				return err
			}
		}

		var revs []int64
		var payloads [][]byte
		rows, err := tx.QueryContext(ctx,
			`SELECT o.seq, o.op, o.task_id, o.project_id, o.created_at, o.failed_at,
			        o.attempts, o.last_error, o.rev, coalesce(t.title, ''), o.payload
			 FROM outbox o LEFT JOIN tasks t ON t.id = o.task_id
			 WHERE o.state = 'failed' AND o.op NOT LIKE 'entity.%' AND o.op <> 'task.move_native'
			 AND NOT `+retainedColumnMutationSQL("o.")+`
			 ORDER BY o.seq`)
		if err != nil {
			return fmt.Errorf("outbox drop parked: %w", err)
		}
		for rows.Next() {
			var (
				m         DroppedMutation
				taskID    sql.NullString
				projectID sql.NullString
				createdAt int64
				failedAt  sql.NullInt64
				rev       sql.NullInt64
			)
			var payload []byte
			if err := rows.Scan(&m.Seq, &m.Op, &taskID, &projectID, &createdAt, &failedAt,
				&m.Attempts, &m.Reason, &rev, &m.Title, &payload); err != nil {
				rows.Close()
				return fmt.Errorf("outbox drop parked: %w", err)
			}
			m.TaskID = taskID.String
			m.ProjectID = projectID.String
			m.QueuedAt = time.Unix(createdAt, 0)
			m.ParkedAt = fromUnix(failedAt)

			m.Requested = m.Attempts > 0
			out = append(out, m)
			revs = append(revs, rev.Int64)
			payloads = append(payloads, append([]byte(nil), payload...))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("outbox drop parked: %w", err)
		}

		if err := rows.Close(); err != nil {
			return fmt.Errorf("outbox drop parked: %w", err)
		}
		identityChanged := false
		for i, m := range out {
			entry := OutboxItem{OutboxEntry: OutboxEntry{Op: m.Op, TaskID: m.TaskID, ProjectID: m.ProjectID, Payload: payloads[i]}}
			send, versioned, decodeErr := decodeFeatureSend(entry)
			registered, err := taskHasItemControl(ctx, tx, m.TaskID)
			if err != nil {
				return err
			}
			var taskExists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id = ?)`, m.TaskID).Scan(&taskExists); err != nil {
				return err
			}
			if decodeErr == nil && versioned && send.Metadata.Fields.Items && taskExists {
				out[i].ChecklistRecoveryPending = true
				identities, err := itemIdentities(ctx, tx, m.TaskID)
				if err != nil {
					return err
				}
				for _, identity := range identities {
					identity.State = ItemAbandoned
					if err := setItemIdentity(ctx, tx, identity); err != nil {
						return err
					}
				}
				if err := setItemIdentity(ctx, tx, ItemIdentity{TaskID: m.TaskID, State: RecoveryPending}); err != nil {
					return err
				}
				identityChanged = true
			}
			cleared, err := clearDirtyAt(ctx, tx, m.TaskID, revs[i])
			if err != nil {
				return fmt.Errorf("outbox drop parked %d: %w", m.Seq, err)
			}
			if registered && cleared {
				identityChanged = true
			}
		}
		if identityChanged {
			if err := bumpItemIdentityEpoch(ctx, tx); err != nil {
				return err
			}
		}
		n, removed, err := discardQueued(ctx, tx, `state = 'failed' AND op NOT LIKE 'entity.%' AND op <> 'task.move_native'
			AND NOT `+retainedColumnMutationSQL(""))
		if err != nil {
			return err
		}
		if n != int64(len(out)) {
			return fmt.Errorf("outbox drop parked: deleted %d entries but read %d", n, len(out))
		}

		gone := make(map[string]bool, len(removed))
		for _, id := range removed {
			gone[id] = true
		}
		for i := range out {
			out[i].TaskRemoved = out[i].Op == OpTaskCreate && gone[out[i].TaskID]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) OutboxCounts(ctx context.Context) (OutboxCounts, error) {
	var c OutboxCounts
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM outbox GROUP BY state`)
	if err != nil {
		return c, fmt.Errorf("outbox counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return c, fmt.Errorf("outbox counts: %w", err)
		}
		switch OutboxState(state) {
		case OutboxPending:
			c.Pending = n
		case OutboxInflight:
			c.Inflight = n
		case OutboxFailed:
			c.Failed = n
		default:
			c.Unknown += n
		}
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("outbox counts: %w", err)
	}
	return c, nil
}

func scanOutbox(rows *sql.Rows) (OutboxItem, error) {
	var (
		item       OutboxItem
		taskID     sql.NullString
		projectID  sql.NullString
		target     string
		state      string
		payload    []byte
		createdAt  int64
		inflightAt sql.NullInt64
		rev        sql.NullInt64
		token      sql.NullString
	)
	err := rows.Scan(&item.Seq, &target, &item.Op, &taskID, &projectID, &payload,
		&createdAt, &item.Attempts, &item.LastError, &state, &inflightAt, &rev, &token)
	if err != nil {
		return item, fmt.Errorf("outbox scan: %w", err)
	}
	item.Target = OutboxTarget(target)
	item.State = OutboxState(state)
	item.TaskID = taskID.String
	item.ProjectID = projectID.String
	item.Payload = json.RawMessage(payload)
	item.CreatedAt = time.Unix(createdAt, 0)
	item.InflightAt = fromUnix(inflightAt)
	item.Rev = rev.Int64
	item.LeaseToken = LeaseToken(token.String)
	return item, nil
}

func affectedOne(ctx context.Context, q execer, res sql.Result, seq int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return notAffected(ctx, q, seq)
}

func notAffected(ctx context.Context, q execer, seq int64) error {
	var found int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE seq = ?`, seq).Scan(&found); err != nil {
		return fmt.Errorf("outbox %d: %w", seq, err)
	}
	if found > 0 {
		return fmt.Errorf("%w: seq %d", ErrLeaseLost, seq)
	}
	return fmt.Errorf("%w: seq %d", ErrNoOutboxRow, seq)
}

func leaseSeconds(lease time.Duration) int64 {
	if lease <= 0 {
		return int64(lease.Seconds())
	}

	secs := int64(lease / time.Second)
	if lease%time.Second != 0 {
		secs++
	}
	return secs
}

func newLeaseToken() (LeaseToken, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("outbox claim: %w", err)
	}
	return LeaseToken(hex.EncodeToString(b[:])), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
