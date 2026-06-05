package store

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entityFK describes one column that holds a foreign key to
// entities(id), discovered live from the migrated schema.
type entityFK struct {
	Table           string
	Column          string
	OnDeleteCascade bool
}

// discoverEntityFKs walks every table in the migrated schema and
// collects the columns whose FK references entities(id). The set is
// read from SQLite's own PRAGMA foreign_key_list output, so a new
// migration that adds a REFERENCES entities(id) column is picked up
// automatically — no hardcoded list to keep in sync.
func discoverEntityFKs(t *testing.T, s *sqliteStore) []entityFK {
	t.Helper()

	tableRows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type='table'`)
	require.NoError(t, err, "list tables")
	var tables []string
	for tableRows.Next() {
		var name string
		require.NoError(t, tableRows.Scan(&name), "scan table name")
		tables = append(tables, name)
	}
	require.NoError(t, tableRows.Err(), "iterate tables")
	require.NoError(t, tableRows.Close(), "close table rows")

	var out []entityFK
	for _, table := range tables {
		// PRAGMA foreign_key_list(<table>) columns:
		//   id, seq, table, from, to, on_update, on_delete, match
		// `table` is the referenced (parent) table; `from` is the
		// child column on <table>; `to` is the referenced column.
		fkRows, err := s.db.Query(`SELECT "table", "from", "to", "on_delete" FROM pragma_foreign_key_list(?)`, table)
		require.NoError(t, err, "pragma_foreign_key_list(%s)", table)
		for fkRows.Next() {
			var parentTable, fromCol, toCol, onDelete string
			require.NoError(t, fkRows.Scan(&parentTable, &fromCol, &toCol, &onDelete),
				"scan fk row for %s", table)
			if parentTable == "entities" && toCol == "id" {
				out = append(out, entityFK{
					Table:           table,
					Column:          fromCol,
					OnDeleteCascade: strings.EqualFold(onDelete, "CASCADE"),
				})
			}
		}
		require.NoError(t, fkRows.Err(), "iterate fk rows for %s", table)
		require.NoError(t, fkRows.Close(), "close fk rows for %s", table)
	}
	return out
}

// funcBody slices the source of a single method out of a package file.
// It scans from the method's `func (s *sqliteStore) <name>(` header to
// the next top-level `\nfunc ` (or end of file). Tests run with CWD set
// to the package directory, so the relative path resolves.
func funcBody(t *testing.T, file, method string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	require.NoError(t, err, "read %s", file)
	text := string(src)

	marker := "func (s *sqliteStore) " + method + "("
	start := strings.Index(text, marker)
	require.GreaterOrEqual(t, start, 0, "method %s not found in %s", method, file)

	rest := text[start+len(marker):]
	if idx := strings.Index(rest, "\nfunc "); idx >= 0 {
		return text[start : start+len(marker)+idx]
	}
	return text[start:]
}

// re-key check: RenameEntity must always re-key an entity FK column
// explicitly (no entity FK carries ON UPDATE CASCADE), so the function
// must mention the table.
func renameHandles(body string, fk entityFK) bool {
	return strings.Contains(body, fk.Table)
}

// delete check: DeleteEntityCascade handles a column either with an
// explicit DELETE in the function or via the column's ON DELETE CASCADE.
func deleteHandles(body string, fk entityFK) bool {
	return fk.OnDeleteCascade || strings.Contains(body, fk.Table)
}

// wipe check: WipeDerivedState handles a column either by listing the
// table in derivedTables or via ON DELETE CASCADE (auto-removed when
// DELETE FROM entities runs).
func wipeHandles(body string, fk entityFK) bool {
	return fk.OnDeleteCascade || strings.Contains(body, fk.Table)
}

// TestSchemaDrift_EntityFKsHandledByLifecycleOps fails loudly when a
// future migration adds a REFERENCES entities(id) column but forgets to
// wire one of the three entity-lifecycle ops. The discovered FK set is
// live (PRAGMA-driven); each of the three function bodies is read from
// source and asserted to handle every column.
func TestSchemaDrift_EntityFKsHandledByLifecycleOps(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	fks := discoverEntityFKs(t, s)

	require.NotEmpty(t, fks, "no entity-id FKs discovered — schema introspection broke")
	for _, fk := range fks {
		t.Logf("discovered entity FK: %s.%s (on_delete_cascade=%v)", fk.Table, fk.Column, fk.OnDeleteCascade)
	}

	renameBody := funcBody(t, "rename.go", "RenameEntity")
	deleteBody := funcBody(t, "reindex.go", "DeleteEntityCascade")
	wipeBody := funcBody(t, "reindex.go", "WipeDerivedState")

	for _, fk := range fks {
		assert.True(t, renameHandles(renameBody, fk),
			"entity-FK column %s.%s (REFERENCES entities.id) is not re-keyed in RenameEntity (internal/store/rename.go) — a rename would orphan its rows; add an UPDATE %s SET %s = ?",
			fk.Table, fk.Column, fk.Table, fk.Column)

		assert.True(t, deleteHandles(deleteBody, fk),
			"entity-FK column %s.%s (REFERENCES entities.id) is not handled in DeleteEntityCascade (internal/store/reindex.go) — a delete would leave dangling references; add an explicit DELETE or an ON DELETE CASCADE FK",
			fk.Table, fk.Column)

		assert.True(t, wipeHandles(wipeBody, fk),
			"entity-FK column %s.%s (REFERENCES entities.id) is not handled in WipeDerivedState (internal/store/reindex.go) — a full wipe would leave orphaned rows; add the table to derivedTables or rely on ON DELETE CASCADE",
			fk.Table, fk.Column)
	}
}

// TestSchemaDrift_ChecksBiteOnUnhandledColumn is the negative control:
// it runs the same three boolean checks against a synthetic FK column
// that no lifecycle op handles, over the real function bodies, and
// asserts each check returns false. This proves the assertions in the
// positive test are load-bearing without mutating the real schema.
func TestSchemaDrift_ChecksBiteOnUnhandledColumn(t *testing.T) {
	t.Parallel()

	renameBody := funcBody(t, "rename.go", "RenameEntity")
	deleteBody := funcBody(t, "reindex.go", "DeleteEntityCascade")
	wipeBody := funcBody(t, "reindex.go", "WipeDerivedState")

	unhandled := entityFK{Table: "future_table", Column: "entity_id", OnDeleteCascade: false}

	assert.False(t, renameHandles(renameBody, unhandled),
		"RenameEntity check should fail for an unhandled FK column (negative control)")
	assert.False(t, deleteHandles(deleteBody, unhandled),
		"DeleteEntityCascade check should fail for an unhandled FK column (negative control)")
	assert.False(t, wipeHandles(wipeBody, unhandled),
		"WipeDerivedState check should fail for an unhandled FK column (negative control)")
}
