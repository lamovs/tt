package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type EntityRef struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

type ResourceEntity struct {
	Ref        EntityRef       `json:"ref"`
	ServerID   string          `json:"server_id"`
	ProjectKey string          `json:"project_key,omitempty"`
	Data       json.RawMessage `json:"data"`
	Base       json.RawMessage `json:"base"`
	Revision   int64           `json:"revision"`
	Dirty      bool            `json:"dirty"`
	Deleted    bool            `json:"deleted"`
}

var ErrEntityChanged = errors.New("entity changed; prepare a new preview")
var ErrEntityDependency = errors.New("entity has unresolved operation dependencies")
var ErrEntityUncertain = errors.New("entity operation may have reached the server; read-only recovery is required")

func validEntityKind(kind string) bool {
	switch kind {
	case "project", "folder", "column", "tag", "habit", "checkin", "comment", "focus", "countdown":
		return true
	}
	return false
}

func validateEntityRef(ref EntityRef) error {
	if !validEntityKind(ref.Kind) || strings.TrimSpace(ref.Key) == "" || strings.ContainsAny(ref.Key, "\x00\r\n") {
		return errors.New("invalid entity reference")
	}
	return nil
}

func entityObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("entity data must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid entity field")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate entity field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing entity JSON data")
	}
	return fields, nil
}

func entityJSONEqual(a, b json.RawMessage) bool {
	var left, right any
	decode := func(raw json.RawMessage, out *any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(out)
	}
	return decode(a, &left) == nil && decode(b, &right) == nil && entityValuesEqual(left, right)
}

func entityValuesEqual(left, right any) bool {
	switch l := left.(type) {
	case json.Number:
		r, ok := right.(json.Number)
		if !ok {
			return false
		}
		if l == r {
			return true
		}
		// Bound exponent expansion before exact decimal comparison.
		valid := func(n json.Number) bool {
			if len(n.String()) > 1024 {
				return false
			}
			if _, exp, found := strings.Cut(strings.ToLower(n.String()), "e"); found {
				v, err := strconv.Atoi(exp)
				return err == nil && v >= -4096 && v <= 4096
			}
			return true
		}
		if !valid(l) || !valid(r) {
			return false
		}
		a, aok := new(big.Rat).SetString(l.String())
		b, bok := new(big.Rat).SetString(r.String())
		return aok && bok && a.Cmp(b) == 0
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, value := range l {
			other, exists := r[key]
			if !exists || !entityValuesEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !entityValuesEqual(l[i], r[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

func validateResourceAddress(kind string, data json.RawMessage, serverID string) error {
	if kind != "focus" && kind != "checkin" {
		return validateEntityAddress(data, serverID)
	}
	if _, err := entityObject(data); err != nil {
		return err
	}
	parent, key, ok := strings.Cut(serverID, "/")
	if !ok || parent == "" || key == "" {
		return errors.New("invalid compound resource identity")
	}
	var value struct {
		ID    string `json:"id"`
		Type  *int   `json:"type"`
		Stamp int    `json:"stamp"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if kind == "focus" {
		if value.Type == nil || (*value.Type != 0 && *value.Type != 1) || strconv.Itoa(*value.Type) != parent || value.ID != key {
			return errors.New("remote focus identity mismatch")
		}
	} else {
		if _, err := time.Parse("20060102", key); err != nil || strconv.Itoa(value.Stamp) != key {
			return errors.New("remote check-in date mismatch")
		}
	}
	return nil
}

func validateEntityAddress(data json.RawMessage, serverID string) error {
	fields, err := entityObject(data)
	if err != nil {
		return err
	}
	if raw, present := fields["id"]; present {
		var id string
		if json.Unmarshal(raw, &id) != nil || id != serverID {
			return errors.New("remote entity address mismatch")
		}
	}
	return nil
}

const entityColumns = `kind, entity_key, server_id, project_key, data, base, revision, dirty, deleted`

func scanEntity(row interface{ Scan(...any) error }) (ResourceEntity, error) {
	var entity ResourceEntity
	var data, base string
	err := row.Scan(&entity.Ref.Kind, &entity.Ref.Key, &entity.ServerID, &entity.ProjectKey, &data, &base,
		&entity.Revision, &entity.Dirty, &entity.Deleted)
	entity.Data, entity.Base = json.RawMessage(data), json.RawMessage(base)
	if errors.Is(err, sql.ErrNoRows) {
		return ResourceEntity{}, ErrNotFound
	}
	return entity, err
}

func readEntity(ctx context.Context, q execer, ref EntityRef) (ResourceEntity, error) {
	return scanEntity(q.QueryRowContext(ctx, `SELECT `+entityColumns+` FROM resource_entities WHERE kind=? AND entity_key=?`, ref.Kind, ref.Key))
}

func (s *Store) Entity(ctx context.Context, ref EntityRef) (ResourceEntity, error) {
	if err := validateEntityRef(ref); err != nil {
		return ResourceEntity{}, err
	}
	return readEntity(ctx, s.db, ref)
}

func (s *Store) Entities(ctx context.Context, kind, projectKey string, includeDeleted bool) ([]ResourceEntity, error) {
	if !validEntityKind(kind) {
		return nil, errors.New("unsupported entity kind")
	}
	query := `SELECT ` + entityColumns + ` FROM resource_entities WHERE kind=?`
	args := []any{kind}
	if projectKey != "" {
		query += ` AND project_key=?`
		args = append(args, projectKey)
	}
	if !includeDeleted {
		query += ` AND deleted=0`
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY entity_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceEntity{}
	for rows.Next() {
		entity, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entity)
	}
	return out, rows.Err()
}

func writeEntity(ctx context.Context, q execer, entity ResourceEntity) error {
	_, err := q.ExecContext(ctx, `INSERT INTO resource_entities (`+entityColumns+`) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(kind,entity_key) DO UPDATE SET server_id=excluded.server_id, project_key=excluded.project_key,
		data=excluded.data, base=excluded.base, revision=excluded.revision, dirty=excluded.dirty, deleted=excluded.deleted`,
		entity.Ref.Kind, entity.Ref.Key, entity.ServerID, entity.ProjectKey, string(entity.Data), string(entity.Base),
		entity.Revision, entity.Dirty, entity.Deleted)
	return err
}

// EntityCollectionFence captures local revisions before fetching a remote collection.
type EntityCollectionFence struct {
	Kind       string
	ProjectKey string
	Revisions  map[string]int64
}

func (s *Store) CaptureEntityCollectionFence(ctx context.Context, kind, projectKey string) (EntityCollectionFence, error) {
	entities, err := s.Entities(ctx, kind, projectKey, true)
	fence := EntityCollectionFence{Kind: kind, ProjectKey: projectKey, Revisions: map[string]int64{}}
	for _, entity := range entities {
		fence.Revisions[entity.Ref.Key] = entity.Revision
	}
	return fence, err
}

// MergeEntities preserves missing rows. Completeness alone is insufficient for pruning without a pre-fetch fence.
func (s *Store) MergeEntities(ctx context.Context, kind, projectKey string, incoming []ResourceEntity, complete bool) error {
	return s.mergeEntities(ctx, kind, projectKey, incoming, false, nil)
}

func (s *Store) MergeEntitiesFenced(ctx context.Context, incoming []ResourceEntity, complete bool, fence EntityCollectionFence) error {
	return s.mergeEntities(ctx, fence.Kind, fence.ProjectKey, incoming, complete, &fence)
}

func (s *Store) mergeEntities(ctx context.Context, kind, projectKey string, incoming []ResourceEntity, complete bool, fence *EntityCollectionFence) error {
	if !validEntityKind(kind) {
		return errors.New("unsupported entity kind")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		seen := map[string]bool{}
		for _, remote := range incoming {
			if remote.Ref.Kind != "" && remote.Ref.Kind != kind {
				return errors.New("remote entity kind mismatch")
			}
			if remote.ServerID == "" {
				return errors.New("remote entity has no server ID")
			}
			if seen[remote.ServerID] {
				return errors.New("duplicate remote entity ID")
			}
			seen[remote.ServerID] = true
			if err := validateResourceAddress(kind, remote.Data, remote.ServerID); err != nil {
				return err
			}
			current, err := scanEntity(tx.QueryRowContext(ctx, `SELECT `+entityColumns+` FROM resource_entities WHERE kind=? AND server_id=?`, kind, remote.ServerID))
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if errors.Is(err, ErrNotFound) {
				if remote.Ref.Key == "" {
					remote.Ref.Key = remote.ServerID
				}
				remote.Ref.Kind = kind
				if err := validateEntityRef(remote.Ref); err != nil {
					return err
				}
				if _, existing := readEntity(ctx, tx, remote.Ref); !errors.Is(existing, ErrNotFound) {
					if existing != nil {
						return existing
					}
					return ErrEntityChanged
				}
				remote.Base = append(json.RawMessage(nil), remote.Data...)
				remote.Revision, remote.Dirty, remote.Deleted = 1, false, false
				if projectKey != "" {
					remote.ProjectKey = projectKey
				}
				if err := writeEntity(ctx, tx, remote); err != nil {
					return err
				}
				continue
			}
			if fence != nil && fence.Revisions[current.Ref.Key] != current.Revision {
				continue
			}
			if entityJSONEqual(current.Base, remote.Data) && (current.Dirty || entityJSONEqual(current.Data, remote.Data)) && !current.Deleted {
				continue
			}
			current.Base = append(json.RawMessage(nil), remote.Data...)
			if !current.Dirty {
				current.Data = append(json.RawMessage(nil), remote.Data...)
				current.Deleted = false
			} else if err := rebuildEntityOverlay(ctx, tx, &current, 0); err != nil {
				return err
			}
			current.Revision++
			if err := writeEntity(ctx, tx, current); err != nil {
				return err
			}
		}
		if !complete || fence == nil {
			return nil
		}
		for key, revision := range fence.Revisions {
			entity, err := readEntity(ctx, tx, EntityRef{Kind: kind, Key: key})
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if entity.Revision != revision || entity.Dirty || entity.ServerID == "" || seen[entity.ServerID] {
				continue
			}
			var queued int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entity_operation_targets WHERE kind=? AND entity_key=?`, kind, key).Scan(&queued); err != nil {
				return err
			}
			if queued != 0 {
				continue
			}
			entity.Deleted = true
			entity.Revision++
			if err := writeEntity(ctx, tx, entity); err != nil {
				return err
			}
		}
		return nil
	})
}
