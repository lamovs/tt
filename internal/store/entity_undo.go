package store

import (
	"context"
	"database/sql"
	"errors"
)

const OpEntityMutation = "entity.mutation"

var ErrEntityUndoUnavailable = errors.New("resource operation is already settled or removed; no remote reversal is supported; history retained")

func entityUndoOperation(ctx context.Context, q execer, action UndoAction) (EntityOperation, error) {
	if action.Op != OpEntityMutation || action.EntityRef == nil || action.OperationSeq <= 0 || validateEntityRef(*action.EntityRef) != nil {
		return EntityOperation{}, ErrUndoIncomplete
	}
	operation, err := readEntityOperation(ctx, q, action.OperationSeq)
	if errors.Is(err, ErrNoOutboxRow) {
		return EntityOperation{}, ErrEntityUndoUnavailable
	}
	if err != nil {
		return EntityOperation{}, err
	}
	if operation.Mutation.Ref != *action.EntityRef {
		return EntityOperation{}, ErrUndoConflict
	}
	if err := checkEntityCancellation(ctx, q, operation); err != nil {
		return EntityOperation{}, err
	}
	return operation, nil
}

func (s *Store) PreviewEntityUndo(ctx context.Context, expected UndoEntry) (ResourceEntity, error) {
	var entity ResourceEntity
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := scanUndo(tx.QueryRowContext(ctx, `SELECT `+undoColumns+` FROM undo_log ORDER BY seq DESC LIMIT 1`))
		if err != nil {
			return err
		}
		if current.Seq != expected.Seq {
			return ErrUndoConflict
		}
		operation, err := entityUndoOperation(ctx, tx, current.Action)
		if err != nil {
			return err
		}
		entity, err = readEntity(ctx, tx, operation.Mutation.Ref)
		return err
	})
	return entity, err
}

func checkEntityCancellation(ctx context.Context, q execer, operation EntityOperation) error {
	if operation.Item.State == OutboxInflight {
		return ErrEntityChanged
	}
	if operation.Phase != "prepared" || len(operation.Request) != 0 {
		return ErrEntityUncertain
	}
	var dependents int
	if err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM entity_operation_dependencies WHERE depends_on=?)+
		(SELECT count(*) FROM entity_operation_targets WHERE kind=? AND entity_key=? AND seq>?)`, operation.Item.Seq, operation.Mutation.Ref.Kind, operation.Mutation.Ref.Key, operation.Item.Seq).Scan(&dependents); err != nil {
		return err
	}
	if dependents != 0 {
		return ErrEntityDependency
	}
	return nil
}

func cancelEntityOperationTx(ctx context.Context, tx *sql.Tx, operation EntityOperation) error {
	if err := checkEntityCancellation(ctx, tx, operation); err != nil {
		return err
	}
	entity, err := readEntity(ctx, tx, operation.Mutation.Ref)
	if err != nil {
		return err
	}
	if operation.Mutation.Action == "create" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_entities WHERE kind=? AND entity_key=?`, entity.Ref.Kind, entity.Ref.Key); err != nil {
			return err
		}
	} else {
		if err := rebuildEntityOverlay(ctx, tx, &entity, operation.Item.Seq); err != nil {
			return err
		}
		entity.Revision++
		if err := writeEntity(ctx, tx, entity); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE seq=?`, operation.Item.Seq)
	return err
}

func undoEntityMutation(ctx context.Context, tx *sql.Tx, action UndoAction) error {
	operation, err := entityUndoOperation(ctx, tx, action)
	if err != nil {
		return err
	}
	return cancelEntityOperationTx(ctx, tx, operation)
}
