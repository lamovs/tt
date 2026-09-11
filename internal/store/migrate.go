package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const schemaVersionKey = "schema_version"

type migration struct {
	version int
	name    string
	body    string
}

type VersionError struct {
	Found     int
	Supported int
	Path      string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("cache %s has schema version %d, this build supports %d: update tt to the version that wrote it. "+
		"Do not delete the cache to get past this - it holds the changes the server has not been told about yet, "+
		"and nothing else has a copy; move the file aside if you have to start over",
		e.Path, e.Found, e.Supported)
}

func migrate(ctx context.Context, db *sql.DB, dbPath string) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	if err := createMetaTable(ctx, db); err != nil {
		return err
	}
	current, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	latest := migrations[len(migrations)-1].version
	if current > latest {
		return &VersionError{Found: current, Supported: latest, Path: dbPath}
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m, latest, dbPath); err != nil {
			var ve *VersionError
			if errors.As(err, &ve) {
				return err
			}
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, latest int, dbPath string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > latest {

		return &VersionError{Found: current, Supported: latest, Path: dbPath}
	}
	if m.version <= current {

		return nil
	}
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return err
	}
	if err := setMeta(ctx, tx, schemaVersionKey, strconv.Itoa(m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

func schemaVersion(ctx context.Context, q execer) (int, error) {
	value, ok, err := getMeta(ctx, q, schemaVersionKey)
	if err != nil || !ok {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("meta %s is not a number: %q", schemaVersionKey, value)
	}
	return v, nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return schemaVersion(ctx, s.db)
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %s: bad version prefix", e.Name())
		}
		body, err := migrationsFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: name, body: string(body)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations embedded")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must run 1..N without gaps, got %d at position %d", m.version, i+1)
		}
	}
	return out, nil
}
