package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMemoryStore(t *testing.T) *sqliteStore {
	t.Helper()
	s, err := New(":memory:")
	require.NoError(t, err, "New(:memory:)")
	t.Cleanup(func() { _ = s.Close() })
	impl, ok := s.(*sqliteStore)
	require.True(t, ok, "expected *sqliteStore, got %T", s)
	return impl
}

func tableExists(t *testing.T, s *sqliteStore, name string) bool {
	t.Helper()
	var got string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, name,
	).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	require.NoError(t, err, "query sqlite_master for %s", name)
	return got == name
}

func TestNew_OpensInMemoryAndAppliesMigration(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	for _, name := range []string{"entities", "edges", "provenance", "schema_migrations"} {
		assert.True(t, tableExists(t, s, name), "table %q missing after New", name)
	}
	assert.False(t, tableExists(t, s, "fill_tokens"),
		"fill_tokens dropped by migration 005; see migrations/005_drop_fill_tokens.sql")
}

func TestNew_RecordsAllMigrationsApplied(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	var minVersion int
	require.NoError(t,
		s.db.QueryRow(`SELECT MIN(version) FROM schema_migrations`).Scan(&minVersion),
		"query MIN(schema_migrations.version)")
	assert.Equal(t, 1, minVersion, "schema_migrations min version: want 1 (001_init)")

	// Count grows by 1 per migration file added under
	// internal/store/migrations. The looser assertion (>= 1, contiguous)
	// keeps this test from breaking on every future migration.
	var count int
	require.NoError(t,
		s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count),
		"count schema_migrations")
	assert.GreaterOrEqual(t, count, 1, "schema_migrations row count: want at least 1")

	// Versions should form a contiguous sequence starting at 1.
	rows, err := s.db.Query(`SELECT version FROM schema_migrations ORDER BY version ASC`)
	require.NoError(t, err, "query schema_migrations versions")
	defer func() { _ = rows.Close() }()
	expected := 1
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v), "scan version")
		assert.Equal(t, expected, v, "schema_migrations versions: want contiguous from 1")
		expected++
	}
}

func TestNew_TablesHaveExpectedColumns(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	cases := map[string][]string{
		// `archived_at` lands in migration 011 per ADR-0018: nullable
		// timestamp marking entity-archived-at; NULL = active.
		"entities": {"id", "kind", "data", "created_at", "updated_at", "archived_at"},
		"edges": {"type", "from_id", "to_id", "metadata", "created_at", "updated_at"},
		"provenance": {
			"id", "target_kind", "target_entity_id", "target_edge_type",
			"target_edge_from", "target_edge_to", "source", "fetched_at",
			"filled_at", "ok", "error", "error_message",
		},
	}
	for table, want := range cases {
		got := columnNames(t, s, table)
		for _, c := range want {
			assert.Contains(t, got, c, "table %s missing column %q (got %v)", table, c, got)
		}
	}
}

func columnNames(t *testing.T, s *sqliteStore, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	require.NoError(t, err, "pragma_table_info(%s)", table)
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n), "scan column name for %s", table)
		names = append(names, n)
	}
	require.NoError(t, rows.Err(), "iterate columns for %s", table)
	return names
}

// TestEntities_ArchivedAtColumnIsNullableAndQueryable pins ADR-0018
// step 1's invariants: `archived_at` is nullable (defaults to NULL on
// insert without an explicit value), distinguishes active rows from
// archived ones via IS NULL / IS NOT NULL, and is index-covered for
// the default-filter scan path that lands in step 2.
func TestEntities_ArchivedAtColumnIsNullableAndQueryable(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)

	// Insert two rows: one without archived_at (active), one with.
	// Direct INSERTs (not the future Archive method) — this test
	// exercises the SCHEMA, not the API.
	_, err := s.db.Exec(`
		INSERT INTO entities (id, kind, data, created_at, updated_at)
		VALUES (?, ?, '{}', '2026-05-08T12:00:00Z', '2026-05-08T12:00:00Z')`,
		"person:active", "person")
	require.NoError(t, err, "insert active entity (no archived_at)")

	_, err = s.db.Exec(`
		INSERT INTO entities (id, kind, data, created_at, updated_at, archived_at)
		VALUES (?, ?, '{}', '2026-05-08T12:00:00Z', '2026-05-08T12:00:00Z', '2026-05-08T12:01:00Z')`,
		"person:archived", "person")
	require.NoError(t, err, "insert archived entity (archived_at=now)")

	// Active-set scan: `WHERE archived_at IS NULL` returns the active
	// row only. Mirrors step 2's default list/search filter.
	var activeID string
	require.NoError(t,
		s.db.QueryRow(`SELECT id FROM entities WHERE archived_at IS NULL`).Scan(&activeID),
		"select active entity")
	assert.Equal(t, "person:active", activeID)

	// Archived-only scan: complement of the default filter.
	var archivedID string
	require.NoError(t,
		s.db.QueryRow(`SELECT id FROM entities WHERE archived_at IS NOT NULL`).Scan(&archivedID),
		"select archived entity")
	assert.Equal(t, "person:archived", archivedID)
}

// TestEntities_ArchivedAtIndexExists pins the migration-011 index
// covering the default-filter scan path. SQLite's sqlite_master
// table is the introspection surface (same shape used elsewhere in
// these tests for table-existence checks).
func TestEntities_ArchivedAtIndexExists(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name = ?`,
		"idx_entities_archived_at",
	).Scan(&name)
	require.NoError(t, err, "idx_entities_archived_at: index missing post-migration")
	assert.Equal(t, "idx_entities_archived_at", name)
}

func TestNew_ReopeningSamePathIsIdempotent(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "yaad-test.db")

	s1, err := New(dbPath)
	require.NoError(t, err, "first New")
	require.NoError(t, s1.Close(), "first Close")

	// Re-opening the same file must succeed and not double-apply migrations.
	s2, err := New(dbPath)
	require.NoError(t, err, "second New")
	defer func() { _ = s2.Close() }()

	impl, ok := s2.(*sqliteStore)
	require.True(t, ok, "expected *sqliteStore, got %T", s2)
	// Idempotency check: reopening doubled the row count would mean a
	// migration was re-applied. Capture the row count from a single
	// fresh-open elsewhere and compare — same set of files yields the
	// same number of rows whether the DB is fresh or pre-existing.
	freshOnly := newMemoryStore(t)
	var fresh int
	require.NoError(t,
		freshOnly.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&fresh),
		"count fresh schema_migrations")
	var reopened int
	require.NoError(t,
		impl.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&reopened),
		"count schema_migrations after reopen")
	assert.Equal(t, fresh, reopened,
		"schema_migrations row count after reopen: want same as fresh (migrations re-applied?)")
}

func TestNew_AutoCreatesParentDirectory(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "nested", "deeper", "yaad.db")
	s, err := New(dbPath)
	require.NoError(t, err, "New on path with missing parents")
	assert.NoError(t, s.Close(), "Close")
}

// All Store methods are now implemented — the staged-rollout contract
// closes here. ErrNotImplemented stays in the package as a sentinel so
// any future method added to the interface starts with the same
// stub-pending pattern, and search_test.go / entity_methods_test.go /
// edge_methods_test.go exercise each method's real behaviour directly.

// TestEntities_GapStateColumnIsNullableAndStores covers the
// migration-012 column per ADR-0019 §Storage. Entities inserted
// pre-ADR-0019 (no gap_state) keep the column NULL — the active-
// fill state machine treats NULL as "no metadata for any field"
// which is the expected starting state. Direct INSERTs (not the
// future operator-fill methods) exercise the SCHEMA only.
func TestEntities_GapStateColumnIsNullableAndStores(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)

	// Pre-ADR-0019 row: no gap_state column passed → stays NULL.
	_, err := s.db.Exec(`
		INSERT INTO entities (id, kind, data, created_at, updated_at)
		VALUES (?, ?, '{}', '2026-05-08T12:00:00Z', '2026-05-08T12:00:00Z')`,
		"person:no-gap-state", "person")
	require.NoError(t, err, "insert pre-ADR-0019 entity (no gap_state)")

	var rawNull sql.NullString
	require.NoError(t,
		s.db.QueryRow(`SELECT gap_state FROM entities WHERE id = ?`, "person:no-gap-state").Scan(&rawNull),
		"read gap_state column from pre-ADR-0019 row")
	assert.False(t, rawNull.Valid, "pre-ADR-0019 row must have NULL gap_state")

	// Post-ADR-0019 row with a JSON gap_state document.
	const stateJSON = `{"summary":{"source":"agent","filled_at":"2026-05-07T12:00:00Z"},"played":{"deferred":true,"deferred_at":"2026-05-08T16:30:00Z"}}`
	_, err = s.db.Exec(`
		INSERT INTO entities (id, kind, data, created_at, updated_at, gap_state)
		VALUES (?, ?, '{}', '2026-05-08T12:00:00Z', '2026-05-08T12:00:00Z', ?)`,
		"boardgame:has-state", "boardgame", stateJSON)
	require.NoError(t, err, "insert ADR-0019 entity with gap_state")

	var stored string
	require.NoError(t,
		s.db.QueryRow(`SELECT gap_state FROM entities WHERE id = ?`, "boardgame:has-state").Scan(&stored),
		"read gap_state JSON")
	assert.Equal(t, stateJSON, stored, "gap_state stored verbatim as TEXT")
}
