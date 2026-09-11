package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type EntityReference struct {
	Field string    `json:"field"`
	Ref   EntityRef `json:"ref"`
}

type EntityMutation struct {
	Ref        EntityRef         `json:"ref"`
	Action     string            `json:"action"`
	ProjectKey string            `json:"project_key,omitempty"`
	Patch      json.RawMessage   `json:"patch"`
	References []EntityReference `json:"references,omitempty"`
	DependsOn  []int64           `json:"depends_on,omitempty"`
}

type EntityMutationPreview struct {
	Mutation EntityMutation `json:"mutation"`
	Before   ResourceEntity `json:"before"`
	Exists   bool           `json:"exists"`
}

type EntityMutationOutcome struct {
	Entity       ResourceEntity `json:"entity"`
	OperationSeq int64          `json:"operation_seq"`
}

type entityEnvelope struct {
	Version  int             `json:"version"`
	Mutation EntityMutation  `json:"mutation"`
	Before   ResourceEntity  `json:"before"`
	Revision int64           `json:"revision"`
	Phase    string          `json:"phase"`
	Request  json.RawMessage `json:"request,omitempty"`
	RemoteID string          `json:"remote_id,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

type EntityOperation struct {
	Item     OutboxItem      `json:"item"`
	Mutation EntityMutation  `json:"mutation"`
	Before   ResourceEntity  `json:"before"`
	Revision int64           `json:"revision"`
	Phase    string          `json:"phase"`
	Request  json.RawMessage `json:"request,omitempty"`
	RemoteID string          `json:"remote_id,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

func validateEntityMutation(m EntityMutation) error {
	if err := validateEntityRef(m.Ref); err != nil {
		return err
	}
	if m.Action != "create" && m.Action != "update" && m.Action != "delete" {
		return errors.New("unsupported entity mutation")
	}
	if m.Ref.Kind == "countdown" {
		return errors.New("countdown is read-only")
	}
	fields, err := entityObject(m.Patch)
	if err != nil {
		return err
	}
	if m.Action == "delete" && (len(fields) != 0 || len(m.References) != 0) {
		return errors.New("delete cannot contain a patch")
	}
	seen := map[string]bool{}
	for _, reference := range m.References {
		if reference.Field == "" || strings.ContainsAny(reference.Field, ".\x00\r\n") || seen[reference.Field] {
			return errors.New("invalid entity reference field")
		}
		if err := validateEntityRef(reference.Ref); err != nil {
			return err
		}
		if reference.Ref == m.Ref {
			return errors.New("entity cannot reference itself")
		}
		if _, exists := fields[reference.Field]; exists {
			return errors.New("reference field is also present in patch")
		}
		seen[reference.Field] = true
	}
	for _, seq := range m.DependsOn {
		if seq <= 0 {
			return errors.New("invalid dependency sequence")
		}
	}
	return nil
}

func copyEntityMutation(m EntityMutation) EntityMutation {
	m.Patch = append(json.RawMessage(nil), m.Patch...)
	m.References = append([]EntityReference(nil), m.References...)
	m.DependsOn = append([]int64(nil), m.DependsOn...)
	return m
}

func (s *Store) PreviewEntityMutation(ctx context.Context, mutation EntityMutation) (EntityMutationPreview, error) {
	if mutation.Action == "create" && mutation.Ref.Key == "" {
		key, err := NewLocalID()
		if err != nil {
			return EntityMutationPreview{}, err
		}
		mutation.Ref.Key = key
	}
	if len(mutation.Patch) == 0 {
		mutation.Patch = json.RawMessage(`{}`)
	}
	if err := validateEntityMutation(mutation); err != nil {
		return EntityMutationPreview{}, err
	}
	before, err := s.Entity(ctx, mutation.Ref)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return EntityMutationPreview{}, err
	}
	if mutation.Action == "create" && exists {
		return EntityMutationPreview{}, ErrEntityChanged
	}
	if mutation.Action != "create" && (!exists || before.Deleted) {
		return EntityMutationPreview{}, ErrNotFound
	}
	return EntityMutationPreview{Mutation: copyEntityMutation(mutation), Before: before, Exists: exists}, nil
}

func applyEntityPatch(before json.RawMessage, mutation EntityMutation) (json.RawMessage, error) {
	fields, err := entityObject(before)
	if err != nil {
		return nil, err
	}
	patch, err := entityObject(mutation.Patch)
	if err != nil {
		return nil, err
	}
	for name, value := range patch {
		fields[name] = value
	}
	for _, reference := range mutation.References {
		fields[reference.Field], _ = json.Marshal(reference.Ref.Key)
	}
	return json.Marshal(fields)
}

func (s *Store) ApplyEntityMutation(ctx context.Context, preview EntityMutationPreview) (EntityMutationOutcome, error) {
	mutation := copyEntityMutation(preview.Mutation)
	if err := validateEntityMutation(mutation); err != nil {
		return EntityMutationOutcome{}, err
	}
	var outcome EntityMutationOutcome
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		current, err := readEntity(ctx, tx, mutation.Ref)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if preview.Exists != (err == nil) {
			return ErrEntityChanged
		}
		if preview.Exists && (current.Revision != preview.Before.Revision || current.ServerID != preview.Before.ServerID ||
			!entityJSONEqual(current.Data, preview.Before.Data) || !entityJSONEqual(current.Base, preview.Before.Base) || current.Deleted != preview.Before.Deleted) {
			return ErrEntityChanged
		}
		if mutation.Action == "create" {
			if preview.Exists {
				return ErrEntityChanged
			}
			current = ResourceEntity{Ref: mutation.Ref, ProjectKey: mutation.ProjectKey, Data: json.RawMessage(`{}`), Base: json.RawMessage(`{}`)}
		} else if !preview.Exists || current.Deleted {
			return ErrEntityChanged
		}
		before := current
		dependencies := map[int64]bool{}
		for _, seq := range mutation.DependsOn {
			dependencies[seq] = true
		}
		for _, reference := range mutation.References {
			target, err := readEntity(ctx, tx, reference.Ref)
			if err != nil {
				return err
			}
			if target.Deleted {
				return ErrEntityDependency
			}
			if target.ServerID == "" {
				var seq int64
				if err := tx.QueryRowContext(ctx, `SELECT max(seq) FROM entity_operation_targets WHERE kind=? AND entity_key=?`, reference.Ref.Kind, reference.Ref.Key).Scan(&seq); err != nil {
					return ErrEntityDependency
				}
				dependencies[seq] = true
			}
		}
		for seq := range dependencies {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entity_operation_targets WHERE seq=?`, seq).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return ErrEntityDependency
			}
		}
		if mutation.Action != "delete" {
			current.Data, err = applyEntityPatch(current.Data, mutation)
			if err != nil {
				return err
			}
		}
		current.Revision++
		current.Dirty = true
		current.Deleted = mutation.Action == "delete"
		if err := writeEntity(ctx, tx, current); err != nil {
			return err
		}
		envelope := entityEnvelope{Version: 1, Mutation: mutation, Before: before, Revision: current.Revision, Phase: "prepared"}
		payload, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		seq, err := enqueue(ctx, tx, OutboxEntry{Target: TargetOpenAPI, Op: "entity." + mutation.Action, Payload: payload, Rev: current.Revision})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entity_operation_targets(seq,kind,entity_key) VALUES (?,?,?)`, seq, mutation.Ref.Kind, mutation.Ref.Key); err != nil {
			return err
		}
		for dependency := range dependencies {
			if _, err := tx.ExecContext(ctx, `INSERT INTO entity_operation_dependencies(seq,depends_on) VALUES (?,?)`, seq, dependency); err != nil {
				return err
			}
		}
		outcome = EntityMutationOutcome{Entity: current, OperationSeq: seq}
		ref := mutation.Ref
		return pushUndo(ctx, tx, UndoAction{Op: OpEntityMutation, EntityRef: &ref, OperationSeq: seq})
	})
	return outcome, err
}

func decodeEntityOperation(item OutboxItem) (EntityOperation, error) {
	var envelope entityEnvelope
	if err := json.Unmarshal(item.Payload, &envelope); err != nil {
		return EntityOperation{}, err
	}
	if envelope.Version != 1 || item.Op != "entity."+envelope.Mutation.Action {
		return EntityOperation{}, errors.New("unsupported entity operation envelope")
	}
	if err := validateEntityMutation(envelope.Mutation); err != nil {
		return EntityOperation{}, err
	}
	switch envelope.Phase {
	case "prepared", "armed", "accepted", "rejected", "uncertain":
	default:
		return EntityOperation{}, errors.New("invalid entity operation phase")
	}
	return EntityOperation{Item: item, Mutation: envelope.Mutation, Before: envelope.Before, Revision: envelope.Revision,
		Phase: envelope.Phase, Request: envelope.Request, RemoteID: envelope.RemoteID, Response: envelope.Response}, nil
}

func readEntityOperation(ctx context.Context, q execer, seq int64) (EntityOperation, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE seq=? AND op LIKE 'entity.%'`, seq)
	if err != nil {
		return EntityOperation{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return EntityOperation{}, err
		}
		return EntityOperation{}, ErrNoOutboxRow
	}
	item, err := scanOutbox(rows)
	if err != nil {
		return EntityOperation{}, err
	}
	return decodeEntityOperation(item)
}

func writeEntityOperation(ctx context.Context, q execer, operation EntityOperation) error {
	envelope := entityEnvelope{Version: 1, Mutation: operation.Mutation, Before: operation.Before, Revision: operation.Revision,
		Phase: operation.Phase, Request: operation.Request, RemoteID: operation.RemoteID, Response: operation.Response}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE outbox SET payload=? WHERE seq=?`, string(payload), operation.Item.Seq)
	return err
}

func checkEntityLease(operation EntityOperation, token LeaseToken) error {
	if token == "" || operation.Item.State != OutboxInflight || operation.Item.LeaseToken != token {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) ClaimEntityOperations(ctx context.Context, limit int, lease time.Duration) ([]EntityOperation, error) {
	if limit <= 0 {
		return nil, nil
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	out := []EntityOperation{}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		cutoff := now - leaseSeconds(lease)
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state='failed',last_error=?,failed_at=?,inflight_at=NULL,lease_token=NULL
			WHERE op LIKE 'entity.%' AND state='inflight' AND (inflight_at IS NULL OR inflight_at<=?)
			AND (sent_at IS NOT NULL OR json_extract(payload,'$.phase') <> 'prepared')`, ErrEntityUncertain.Error(), now, cutoff); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `UPDATE outbox SET state='inflight',inflight_at=?,lease_token=?,attempts=attempts+1
			WHERE seq IN (SELECT q.seq FROM outbox q JOIN entity_operation_targets target ON target.seq=q.seq
			WHERE q.op LIKE 'entity.%' AND (q.state='pending' OR (q.state='inflight' AND q.inflight_at<=? AND q.sent_at IS NULL))
			AND NOT EXISTS(SELECT 1 FROM entity_operation_dependencies dependency WHERE dependency.seq=q.seq)
			AND NOT EXISTS(SELECT 1 FROM entity_operation_targets older WHERE older.kind=target.kind AND older.entity_key=target.entity_key AND older.seq<q.seq)
			ORDER BY q.seq LIMIT ?) RETURNING `+outboxColumns, now, string(token), cutoff, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanOutbox(rows)
			if err != nil {
				return err
			}
			operation, err := decodeEntityOperation(item)
			if err != nil {
				return err
			}
			out = append(out, operation)
		}
		return rows.Err()
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Item.Seq < out[j].Item.Seq })
	return out, err
}

func entityPatchMatches(ctx context.Context, q execer, data, expected json.RawMessage, mutation EntityMutation) bool {
	actualFields, err := entityObject(data)
	if err != nil {
		return false
	}
	expectedFields, err := entityObject(expected)
	if err != nil {
		return false
	}
	patchFields, err := entityObject(mutation.Patch)
	if err != nil {
		return false
	}
	for name := range patchFields {
		actual, actualPresent := actualFields[name]
		want, wantPresent := expectedFields[name]
		if actualPresent != wantPresent || actualPresent && !entityJSONEqual(actual, want) {
			return false
		}
	}
	for _, reference := range mutation.References {
		actual, actualPresent := actualFields[reference.Field]
		want, wantPresent := expectedFields[reference.Field]
		var key string
		if json.Unmarshal(want, &key) == nil {
			if bound, err := readEntity(ctx, q, EntityRef{Kind: reference.Ref.Kind, Key: key}); err == nil && bound.ServerID != "" {
				want, _ = json.Marshal(bound.ServerID)
			}
		}
		if actualPresent != wantPresent || actualPresent && !entityJSONEqual(actual, want) {
			return false
		}
	}
	return true
}

func (s *Store) ArmEntityOperation(ctx context.Context, seq int64, token LeaseToken) (EntityOperation, error) {
	var operation EntityOperation
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		operation, err = readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if err := checkEntityLease(operation, token); err != nil {
			return err
		}
		if operation.Phase != "prepared" {
			return ErrEntityUncertain
		}
		entity, err := readEntity(ctx, tx, operation.Mutation.Ref)
		if err != nil {
			return err
		}
		if operation.Mutation.Action == "create" {
			if entity.ServerID != "" {
				return ErrEntityChanged
			}
			if operation.Mutation.Ref.Kind == "checkin" {
				if err := validateResourceAddress("checkin", operation.Mutation.Patch, operation.Mutation.Ref.Key); err != nil {
					return err
				}
				operation.RemoteID = operation.Mutation.Ref.Key
			}
		} else {
			if entity.ServerID == "" {
				return ErrEntityDependency
			}
			if operation.Mutation.Action == "delete" {
				if !entityJSONEqual(entity.Base, operation.Before.Data) {
					return ErrEntityChanged
				}
			} else if !entityPatchMatches(ctx, tx, entity.Base, operation.Before.Data, operation.Mutation) {
				return ErrEntityChanged
			}
			operation.RemoteID = entity.ServerID
		}
		if len(operation.Request) == 0 {
			fields, err := entityObject(operation.Mutation.Patch)
			if err != nil {
				return err
			}
			for _, reference := range operation.Mutation.References {
				target, err := readEntity(ctx, tx, reference.Ref)
				if err != nil {
					return err
				}
				if target.ServerID == "" || target.Deleted {
					return ErrEntityDependency
				}
				fields[reference.Field], _ = json.Marshal(target.ServerID)
			}
			operation.Request, err = json.Marshal(fields)
			if err != nil {
				return err
			}
		}
		operation.Phase = "armed"
		if err := writeEntityOperation(ctx, tx, operation); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE outbox SET sent_at=? WHERE seq=?`, time.Now().Unix(), seq)
		return err
	})
	return operation, err
}

func (s *Store) AcceptEntityOperation(ctx context.Context, seq int64, token LeaseToken, remoteID string, data json.RawMessage) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		operation, err := readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if err := checkEntityLease(operation, token); err != nil {
			return err
		}
		if operation.Phase != "armed" && operation.Phase != "accepted" {
			return ErrEntityUncertain
		}
		if remoteID == "" || operation.RemoteID != "" && operation.RemoteID != remoteID {
			return errors.New("accepted entity has a different server ID")
		}
		if len(data) != 0 {
			if err := validateResourceAddress(operation.Mutation.Ref.Kind, data, remoteID); err != nil {
				return err
			}
		}
		operation.Phase, operation.RemoteID = "accepted", remoteID
		operation.Response = append(json.RawMessage(nil), data...)
		return writeEntityOperation(ctx, tx, operation)
	})
}

func (s *Store) ConfirmEntityOperation(ctx context.Context, seq int64, token LeaseToken, remoteID string, data json.RawMessage) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		operation, err := readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if err := checkEntityLease(operation, token); err != nil {
			return err
		}
		if operation.Phase != "armed" && operation.Phase != "accepted" && operation.Phase != "uncertain" {
			return errors.New("entity operation was not sent")
		}
		if remoteID == "" || operation.RemoteID != "" && operation.RemoteID != remoteID {
			return errors.New("entity confirmation ID mismatch")
		}
		entity, err := readEntity(ctx, tx, operation.Mutation.Ref)
		if err != nil {
			return err
		}
		if entity.ServerID != "" && entity.ServerID != remoteID {
			return errors.New("entity binding mismatch")
		}
		if operation.Mutation.Action != "delete" {
			if err := validateResourceAddress(operation.Mutation.Ref.Kind, data, remoteID); err != nil {
				return err
			}
			requestFields, err := entityObject(operation.Request)
			if err != nil {
				return err
			}
			actualFields, err := entityObject(data)
			if err != nil {
				return err
			}
			for name, want := range requestFields {
				if !entityJSONEqual(actualFields[name], want) {
					return fmt.Errorf("entity field %s was not confirmed", name)
				}
			}
			entity.Base = append(json.RawMessage(nil), data...)
		}
		entity.ServerID = remoteID
		if err := rebuildEntityOverlay(ctx, tx, &entity, seq); err != nil {
			return err
		}
		if operation.Mutation.Action == "delete" {
			entity.Deleted = true
		}
		entity.Revision++
		if err := writeEntity(ctx, tx, entity); err != nil {
			return err
		}
		return markDone(ctx, tx, seq, token)
	})
}

func (s *Store) FailEntityOperation(ctx context.Context, seq int64, token LeaseToken, message string, definitelyRejected bool) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		operation, err := readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if err := checkEntityLease(operation, token); err != nil {
			return err
		}
		if definitelyRejected && operation.Phase == "armed" {
			operation.Phase = "rejected"
		} else if operation.Phase != "prepared" && operation.Phase != "accepted" {
			operation.Phase = "uncertain"
		}
		if err := writeEntityOperation(ctx, tx, operation); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE outbox SET state='failed',last_error=?,failed_at=?,inflight_at=NULL,lease_token=NULL WHERE seq=?`, message, time.Now().Unix(), seq)
		return err
	})
}

// RetryEntityOperation schedules explicit recovery. Armed and uncertain operations remain read-only.
func (s *Store) RetryEntityOperation(ctx context.Context, seq, expectedRevision int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		operation, err := readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if operation.Revision != expectedRevision || operation.Item.State != OutboxFailed {
			return ErrEntityChanged
		}
		if operation.Phase == "rejected" {
			operation.Phase = "prepared"
		}
		if err := writeEntityOperation(ctx, tx, operation); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE outbox SET state='pending',inflight_at=NULL,lease_token=NULL WHERE seq=?`, seq)
		return err
	})
}

func (s *Store) CancelEntityOperation(ctx context.Context, seq, expectedRevision int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		operation, err := readEntityOperation(ctx, tx, seq)
		if err != nil {
			return err
		}
		if operation.Revision != expectedRevision || operation.Item.State == OutboxInflight {
			return ErrEntityChanged
		}
		if err := cancelEntityOperationTx(ctx, tx, operation); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM undo_log WHERE kind=? AND json_valid(payload)
			AND json_extract(payload,'$.operation_seq')=?
			AND json_extract(payload,'$.entity_ref.kind')=? AND json_extract(payload,'$.entity_ref.key')=?`, OpEntityMutation, seq, operation.Mutation.Ref.Kind, operation.Mutation.Ref.Key)
		return err
	})
}

func rebuildEntityOverlay(ctx context.Context, q execer, entity *ResourceEntity, excludeSeq int64) error {
	rows, err := q.QueryContext(ctx, `SELECT o.payload FROM outbox o JOIN entity_operation_targets target ON target.seq=o.seq
		WHERE target.kind=? AND target.entity_key=? AND o.seq<>? ORDER BY o.seq`, entity.Ref.Kind, entity.Ref.Key, excludeSeq)
	if err != nil {
		return err
	}
	var mutations []EntityMutation
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return err
		}
		var envelope entityEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			rows.Close()
			return err
		}
		if envelope.Version != 1 {
			rows.Close()
			return errors.New("unsupported entity overlay version")
		}
		mutations = append(mutations, envelope.Mutation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	entity.Data = append(json.RawMessage(nil), entity.Base...)
	entity.Dirty = len(mutations) != 0
	entity.Deleted = false
	for _, mutation := range mutations {
		if mutation.Action == "delete" {
			entity.Deleted = true
			continue
		}
		entity.Data, err = applyEntityPatch(entity.Data, mutation)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EntityOperationSummaries(ctx context.Context) ([]EntityOperation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE op LIKE 'entity.%' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := []EntityOperation{}
	for rows.Next() {
		item, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		operation, err := decodeEntityOperation(item)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}
