package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// runMigrations applies any pending migrations from migrationsFS in
// version order. Each migration runs in its own transaction together with
// the schema_migrations bookkeeping insert, so a partial failure rolls
// back cleanly. Re-running on an up-to-date database is a no-op.
func runMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	pending, err := loadMigrationsFromFS(migrationsFS, migrationsDir)
	if err != nil {
		return err
	}

	for _, m := range pending {
		if applied[m.version] {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

func loadAppliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

type migration struct {
	version int
	filename string
}

// loadMigrationsFromFS reads migrations from the given filesystem and
// returns them sorted by version. Filenames must match `\d+_<name>.sql`;
// the leading numeric is the version key. Two files claiming the same
// version is a programming error — the second would silently skip after
// the first applies — so this function rejects collisions explicitly
// rather than letting them ship.
func loadMigrationsFromFS(fsys fs.ReadDirFS, dir string) ([]migration, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var migs []migration
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("migration filename %q must be <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("migration filename %q: version is not an integer: %w", e.Name(), err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", v, prev, e.Name())
		}
		seen[v] = e.Name()
		migs = append(migs, migration{version: v, filename: e.Name()})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

func applyMigration(db *sql.DB, m migration) error {
	body, err := migrationsFS.ReadFile(path.Join(migrationsDir, m.filename))
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.filename, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.filename, err)
	}
	if _, err := tx.Exec(string(body)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %s: %w", m.filename, err)
	}
	if _, err := tx.Exec(
		"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
		m.version, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration %s: %w", m.filename, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.filename, err)
	}
	return nil
}
