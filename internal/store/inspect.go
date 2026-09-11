package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrNoCache = errors.New("no cache")

var ErrNotAFile = errors.New("not a regular file")

var ErrCannotLook = errors.New("cannot look at the cache path")

var ErrBadMigrations = errors.New("the migrations built into this tt are not usable")

var ErrBadReferenceSchema = errors.New("the schema this tt compares a cache against could not be built")

type Inspection struct {
	Store        *Store
	SchemaLatest int
}

type ForeignKeyViolation struct {
	Table      string
	RowID      sql.NullInt64
	Parent     string
	Constraint int
}

type TaskProjectReference struct {
	TaskID    string
	ProjectID string
}

type ItemTaskReference struct {
	TaskID string
	ItemID string
}

func Inspect(ctx context.Context, path string) (*Inspection, error) {
	return inspect(ctx, path, loadMigrations)
}

func inspect(ctx context.Context, path string, load func() ([]migration, error)) (*Inspection, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("inspect %s: %w", path, ErrNoCache)
	case err != nil:

		return nil, fmt.Errorf("%w: %w", ErrCannotLook, err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("inspect %s: %w", path, ErrNotAFile)
	case fi.Size() == 0:
		return nil, fmt.Errorf("inspect %s: %w", path, ErrNoCache)
	}

	if err := preflightSidecars(path); err != nil {
		return nil, err
	}

	migrations, err := load()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadMigrations, err)
	}

	db, err := sql.Open("sqlite", inspectDSN(ctx, path))
	if err != nil {
		return nil, err
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()

		return nil, err
	}

	return &Inspection{
		Store:        &Store{db: db, path: path},
		SchemaLatest: migrations[len(migrations)-1].version,
	}, nil
}

func (i *Inspection) Close() error { return i.Store.Close() }

func (i *Inspection) Version(ctx context.Context) (found int, hasMeta, hasRow bool, err error) {
	var n int
	if err := i.Store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'meta'`).Scan(&n); err != nil {
		return 0, false, false, fmt.Errorf("look for the meta table: %w", err)
	}
	if n == 0 {
		return 0, false, false, nil
	}

	found, err = schemaVersion(ctx, i.Store.db)
	if err != nil {
		return 0, true, false, err
	}
	if found != 0 {
		return found, true, true, nil
	}

	_, hasRow, err = getMeta(ctx, i.Store.db, schemaVersionKey)
	if err != nil {
		return 0, true, false, err
	}
	return 0, true, hasRow, nil
}

func IsReadOnlyRefusal(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code()&0xff == sqlite3.SQLITE_READONLY
}

func (s *Store) ForeignKeyViolations(ctx context.Context) ([]ForeignKeyViolation, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	var out []ForeignKeyViolation
	for rows.Next() {
		var v ForeignKeyViolation
		if err := rows.Scan(&v.Table, &v.RowID, &v.Parent, &v.Constraint); err != nil {
			return nil, fmt.Errorf("foreign key check: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("foreign key check: %w", err)
	}
	return out, nil
}

func (s *Store) UncachedTaskProjects(ctx context.Context) ([]TaskProjectReference, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tasks.id, tasks.project_id
		FROM tasks
		LEFT JOIN projects ON projects.id = tasks.project_id
		WHERE tasks.project_id <> '' AND projects.id IS NULL
		ORDER BY tasks.id`)
	if err != nil {
		return nil, fmt.Errorf("read uncached task projects: %w", err)
	}
	defer rows.Close()
	var out []TaskProjectReference
	for rows.Next() {
		var v TaskProjectReference
		if err := rows.Scan(&v.TaskID, &v.ProjectID); err != nil {
			return nil, fmt.Errorf("read uncached task projects: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read uncached task projects: %w", err)
	}
	return out, nil
}

func (s *Store) DanglingItemTasks(ctx context.Context) ([]ItemTaskReference, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT items.task_id, items.id
		FROM items
		LEFT JOIN tasks ON tasks.id = items.task_id
		WHERE tasks.id IS NULL
		ORDER BY items.task_id, items.id`)
	if err != nil {
		return nil, fmt.Errorf("read dangling item tasks: %w", err)
	}
	defer rows.Close()
	var out []ItemTaskReference
	for rows.Next() {
		var v ItemTaskReference
		if err := rows.Scan(&v.TaskID, &v.ItemID); err != nil {
			return nil, fmt.Errorf("read dangling item tasks: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read dangling item tasks: %w", err)
	}
	return out, nil
}

func inspectDSN(ctx context.Context, path string) string {
	wait := busyTimeout
	if deadline, ok := ctx.Deadline(); ok {

		wait = max(0, min(wait, time.Until(deadline)))
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", wait.Milliseconds()))
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}

func createMetaTable(ctx context.Context, q execer) error {
	if _, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}
	return nil
}

type SchemaObject struct {
	Kind string
	Name string
}

type SchemaDifference struct {
	Missing []SchemaObject
	Extra   []SchemaObject
}

func (d SchemaDifference) Blocking() []SchemaObject {
	var out []SchemaObject
	for _, o := range d.Missing {
		switch o.Kind {
		case "table", "column", "unique index":
			out = append(out, o)
		}
	}
	return out
}

func (s *Store) CompareSchema(ctx context.Context, v int) (SchemaDifference, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return SchemaDifference{}, err
	}
	latest := migrations[len(migrations)-1].version
	if v < 1 || v > latest {
		return SchemaDifference{}, fmt.Errorf("compare against schema version %d: this build describes 1..%d", v, latest)
	}

	ref, err := referenceSchema(ctx, migrations, v)
	if err != nil {
		return SchemaDifference{}, fmt.Errorf("%w: %w", ErrBadReferenceSchema, err)
	}
	defer ref.Close()

	want, err := schemaObjects(ctx, ref)
	if err != nil {
		return SchemaDifference{}, fmt.Errorf("%w: schema version %d: %w", ErrBadReferenceSchema, v, err)
	}
	got, err := schemaObjects(ctx, s.db)
	if err != nil {
		return SchemaDifference{}, err
	}

	var d SchemaDifference
	for key, o := range want {
		if _, ok := got[key]; !ok {
			d.Missing = append(d.Missing, o)
		}
	}
	for key, o := range got {
		if _, ok := want[key]; !ok {
			d.Extra = append(d.Extra, o)
		}
	}
	for key, o := range want {
		if o.Kind != "table" {
			continue
		}
		if _, ok := got[key]; !ok {

			continue
		}
		wantColumns, err := columnNames(ctx, ref, o.Name)
		if err != nil {
			return SchemaDifference{}, fmt.Errorf("%w: schema version %d: %w", ErrBadReferenceSchema, v, err)
		}
		gotColumns, err := columnNames(ctx, s.db, o.Name)
		if err != nil {
			return SchemaDifference{}, err
		}
		for name := range wantColumns {
			if !gotColumns[name] {
				d.Missing = append(d.Missing, SchemaObject{Kind: "column", Name: o.Name + "." + name})
			}
		}
		for name := range gotColumns {
			if !wantColumns[name] {
				d.Extra = append(d.Extra, SchemaObject{Kind: "column", Name: o.Name + "." + name})
			}
		}
	}
	sortObjects(d.Missing)
	sortObjects(d.Extra)
	return d, nil
}

func referenceSchema(ctx context.Context, migrations []migration, v int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, fmt.Errorf("build the reference schema: %w", err)
	}

	db.SetMaxOpenConns(1)
	if err := createMetaTable(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	for _, m := range migrations {
		if m.version > v {
			break
		}
		if _, err := db.ExecContext(ctx, m.body); err != nil {
			db.Close()
			return nil, fmt.Errorf("build the reference schema: migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return db, nil
}

func schemaObjects(ctx context.Context, q execer) (map[string]SchemaObject, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, ifnull(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return nil, fmt.Errorf("read the schema: %w", err)
	}
	defer rows.Close()
	out := map[string]SchemaObject{}
	for rows.Next() {
		var o SchemaObject
		var definition string
		if err := rows.Scan(&o.Kind, &o.Name, &definition); err != nil {
			return nil, fmt.Errorf("read the schema: %w", err)
		}
		if o.Kind == "index" && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(definition)), "CREATE UNIQUE INDEX") {
			o.Kind = "unique index"
		}
		out[o.Kind+" "+o.Name] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the schema: %w", err)
	}
	return out, nil
}

func columnNames(ctx context.Context, q execer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read the columns of %s: %w", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	return out, nil
}

func sortObjects(objects []SchemaObject) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Kind != objects[j].Kind {
			return objects[i].Kind < objects[j].Kind
		}
		return objects[i].Name < objects[j].Name
	})
}
