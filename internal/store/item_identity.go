package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type ItemIdentityState string

const (
	ItemUnbound     ItemIdentityState = "unbound"
	ItemBound       ItemIdentityState = "bound"
	ItemUncertain   ItemIdentityState = "uncertain"
	ItemAbandoned   ItemIdentityState = "abandoned"
	RecoveryIdle    ItemIdentityState = "recovery_idle"
	RecoveryPending ItemIdentityState = "recovery_pending"
)

type ItemIdentity struct {
	TaskID   string
	ItemKey  string
	ServerID string
	State    ItemIdentityState
}

func itemIdentities(ctx context.Context, q execer, taskID string) ([]ItemIdentity, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT task_id, item_key, ifnull(server_id, ''), state
		FROM item_identities
		WHERE task_id = ? AND item_key <> ''
		ORDER BY item_key`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read item identities of %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []ItemIdentity
	for rows.Next() {
		var identity ItemIdentity
		if err := rows.Scan(&identity.TaskID, &identity.ItemKey, &identity.ServerID, &identity.State); err != nil {
			return nil, fmt.Errorf("read item identities of %s: %w", taskID, err)
		}
		out = append(out, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read item identities of %s: %w", taskID, err)
	}
	return out, nil
}

func itemIdentity(ctx context.Context, q execer, taskID, itemKey string) (ItemIdentity, bool, error) {
	var identity ItemIdentity
	var serverID sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT task_id, item_key, server_id, state
		FROM item_identities
		WHERE task_id = ? AND item_key = ?`, taskID, itemKey).
		Scan(&identity.TaskID, &identity.ItemKey, &serverID, &identity.State)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ItemIdentity{}, false, nil
	case err != nil:
		return ItemIdentity{}, false, fmt.Errorf("read item identity %s of %s: %w", itemKey, taskID, err)
	}
	if serverID.Valid {
		identity.ServerID = serverID.String
	}
	return identity, true, nil
}

func setItemIdentity(ctx context.Context, q execer, identity ItemIdentity) error {
	var serverID any
	if identity.ServerID != "" {
		serverID = identity.ServerID
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO item_identities (task_id, item_key, server_id, state)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(task_id, item_key) DO UPDATE SET
			server_id = excluded.server_id,
			state = excluded.state`,
		identity.TaskID, identity.ItemKey, serverID, string(identity.State))
	if err != nil {
		return fmt.Errorf("write item identity %s of %s: %w", identity.ItemKey, identity.TaskID, err)
	}
	return nil
}

func ensureItemControl(ctx context.Context, q execer, taskID string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO item_identities (task_id, item_key, server_id, state)
		VALUES (?, '', NULL, ?)
		ON CONFLICT(task_id, item_key) DO NOTHING`, taskID, string(RecoveryIdle))
	if err != nil {
		return fmt.Errorf("register item identity control for %s: %w", taskID, err)
	}
	return nil
}

func newItemKey(ctx context.Context, q execer, taskID string) (string, error) {
	return newItemKeyWith(ctx, q, taskID, NewLocalID)
}

func newItemKeyWith(ctx context.Context, q execer, taskID string, next func() (string, error)) (string, error) {
	for {
		key, err := next()
		if err != nil {
			return "", fmt.Errorf("generate item key: %w", err)
		}
		if key == "" {
			return "", errors.New("generate item key: empty key")
		}
		if !IsLocalID(key) {
			return "", fmt.Errorf("generate item key: %q is not a local key", key)
		}
		var used int
		if err := q.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM items WHERE task_id = ? AND id = ?
				UNION ALL
				SELECT 1 FROM item_identities WHERE task_id = ? AND item_key = ?
			)`, taskID, key, taskID, key).Scan(&used); err != nil {
			return "", fmt.Errorf("check item key %s of %s: %w", key, taskID, err)
		}
		if used == 0 {
			return key, nil
		}
	}
}
