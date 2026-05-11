package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertEntity_RoundTripWithoutProvenance(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	want := &Entity{
		ID: "boardgame:upsert-only",
		Kind: "boardgame",
		Data: map[string]any{"title": "Up Inserted"},
		// Provenance intentionally non-empty: UpsertEntity must IGNORE it.
		// The persisted entity must come back with empty provenance.
		Provenance: []ProvenanceEntry{
			{Source: "ignored:upsert", FetchedAt: ts("2026-04-29T00:00:00Z"), OK: true},
		},
	}
	require.NoError(t, s.UpsertEntity(ctx, want), "UpsertEntity")

	got, err := s.GetEntity(ctx, want.ID)
	require.NoError(t, err, "GetEntity")
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, want.Data, got.Data)
	assert.Empty(t, got.Provenance,
		"provenance after UpsertEntity: want empty (UpsertEntity ignores Provenance)")
}

func TestUpsertEntity_PreservesExistingProvenance(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// First seed via SaveEntity (which writes provenance).
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "boardgame:keepprov",
		Kind: "boardgame",
		Data: map[string]any{"title": "Old Title"},
		Provenance: []ProvenanceEntry{
			{Source: "first:fetch", FetchedAt: ts("2026-01-01T00:00:00Z"), OK: true},
		},
	}), "seed")

	// UpsertEntity with new data — must update data, leave provenance.
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:keepprov",
		Kind: "boardgame",
		Data: map[string]any{"title": "New Title"},
	}), "UpsertEntity")

	got, err := s.GetEntity(ctx, "boardgame:keepprov")
	require.NoError(t, err, "GetEntity")
	assert.Equal(t, "New Title", got.Data["title"])
	require.Len(t, got.Provenance, 1, "provenance: want 1 (preserved through UpsertEntity)")
	assert.Equal(t, "first:fetch", got.Provenance[0].Source)
}

func TestAppendProvenance_AccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:append-acc",
		Kind: "boardgame",
		Data: map[string]any{"title": "Accumulator"},
	}), "UpsertEntity")

	// First append: one row.
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:append-acc", []ProvenanceEntry{
		{Source: "ingest:1", FetchedAt: ts("2026-04-01T00:00:00Z"), OK: true},
	}), "first AppendProvenance")

	// Second append: another row. The existing one stays.
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:append-acc", []ProvenanceEntry{
		{Source: "ingest:2", FetchedAt: ts("2026-04-02T00:00:00Z"), OK: true},
	}), "second AppendProvenance")

	// Third append: a failed entry plus an agent fill — multiple rows in
	// one call.
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:append-acc", []ProvenanceEntry{
		{
			Source: "ingest:3", FetchedAt: ts("2026-04-03T00:00:00Z"),
			OK: false, Error: "extractor_timeout", ErrorMessage: "timed out",
		},
		{Source: "agent:bob", FilledAt: ts("2026-04-03T00:01:00Z"), OK: true},
	}), "third AppendProvenance")

	got, err := s.GetEntity(ctx, "boardgame:append-acc")
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 4, "provenance: want 4 (1+1+2 across three appends)")
	gotSources := make([]string, len(got.Provenance))
	for i, p := range got.Provenance {
		gotSources[i] = p.Source
	}
	assert.Equal(t, []string{"ingest:1", "ingest:2", "ingest:3", "agent:bob"}, gotSources,
		"provenance sources: want insertion order")

	// The fill entry round-trips with FilledAt set, FetchedAt nil.
	last := got.Provenance[3]
	assert.NotNil(t, last.FilledAt, "provenance[3].filled_at: want non-nil on agent fill")
	assert.Nil(t, last.FetchedAt, "provenance[3].fetched_at: want nil on agent fill")

	// The failed entry round-trips with error fields populated.
	failed := got.Provenance[2]
	assert.False(t, failed.OK, "provenance[2].ok: want false on failed entry")
	assert.Equal(t, "extractor_timeout", failed.Error)
	assert.Equal(t, "timed out", failed.ErrorMessage)
}

// TestAppendProvenance_FetchAttachmentsRoundTrip exercises the
// ADR-0014 fetch_attachments JSON-column persistence: write a
// provenance row with a non-empty FetchAttachments slice, read it
// back, verify exact equality (ordering matters for the (role, uri)
// re-fetch comparison). Pre-ADR-0014 rows have NULL columns and read
// back as nil — same test verifies that fallback by writing one row
// without and one with attachments.
func TestAppendProvenance_FetchAttachmentsRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:adr-14-rt",
		Kind: "boardgame",
		Data: map[string]any{"title": "ADR-14 Round Trip"},
	}), "UpsertEntity")

	require.NoError(t, s.AppendProvenance(ctx, "boardgame:adr-14-rt", []ProvenanceEntry{
		// Pre-ADR-0014 shape — no attachments. Should round-trip as nil.
		{Source: "bgg:legacy", FetchedAt: ts("2026-05-05T00:00:00Z"), OK: true},
		// ADR-0014 shape — two attachments in input order.
		{
			Source: "bgg:130680",
			FetchedAt: ts("2026-05-06T00:00:00Z"),
			OK: true,
			FetchAttachments: []FetchAttachmentRef{
				{Role: "thumb", URI: "https://cf.geekdo-images.com/.../thumb.jpg"},
				{Role: "cover", URI: "file:///tmp/staging/cover-130680.png"},
			},
		},
	}), "AppendProvenance")

	got, err := s.GetEntity(ctx, "boardgame:adr-14-rt")
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 2)

	// Legacy row: FetchAttachments must be nil (NULL column).
	assert.Nil(t, got.Provenance[0].FetchAttachments,
		"legacy provenance row: FetchAttachments must read back as nil")

	// ADR-0014 row: exact equality, ordering preserved.
	want := []FetchAttachmentRef{
		{Role: "thumb", URI: "https://cf.geekdo-images.com/.../thumb.jpg"},
		{Role: "cover", URI: "file:///tmp/staging/cover-130680.png"},
	}
	assert.Equal(t, want, got.Provenance[1].FetchAttachments,
		"ADR-0014 provenance row: FetchAttachments round-trip")
}

// TestLoadEntityProvenance_CorruptFetchAttachmentsIsSoftFail pins
// the soft-fail contract the cold-reviewer flagged on a prior PR review: a corrupt
// fetch_attachments JSON column on ONE provenance row must NOT
// propagate up through GetEntity and break ALL reads of the
// entity. The whole-row blast radius is disproportionate for a
// column whose worst case is "the next ingest's re-fetch comparison
// re-fetches everything." Instead we WARN + treat as nil.
func TestLoadEntityProvenance_CorruptFetchAttachmentsIsSoftFail(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:corrupt-row",
		Kind: "boardgame",
		Data: map[string]any{"title": "Corrupt Row"},
	}), "UpsertEntity")

	// Insert a provenance row with malformed JSON in the
	// fetch_attachments column directly via the underlying DB —
	// AppendProvenance always marshals correctly so we can't
	// reproduce the corrupt state through the public API.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provenance (
			target_kind, target_entity_id, source,
			fetched_at, ok, fetch_attachments
		) VALUES ('entity', ?, ?, ?, 1, ?)
	`, "boardgame:corrupt-row", "bgg:corrupt", "2026-05-06T00:00:00Z", "{not valid json")
	require.NoError(t, err, "raw insert with corrupt fetch_attachments")

	// GetEntity must succeed — corrupt column logs WARN + reads as
	// nil, the rest of the entity is intact.
	got, err := s.GetEntity(ctx, "boardgame:corrupt-row")
	require.NoError(t, err, "GetEntity must NOT propagate the decode error")
	require.Len(t, got.Provenance, 1)
	assert.Nil(t, got.Provenance[0].FetchAttachments,
		"corrupt fetch_attachments column must read back as nil (degraded), not error")
	assert.Equal(t, "bgg:corrupt", got.Provenance[0].Source,
		"the rest of the row must round-trip intact")
}

func TestAppendProvenance_EmptyEntriesIsNoOp(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:empty-append",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}), "UpsertEntity")

	assert.NoError(t, s.AppendProvenance(ctx, "boardgame:empty-append", nil),
		"AppendProvenance(nil)")
	assert.NoError(t, s.AppendProvenance(ctx, "boardgame:empty-append", []ProvenanceEntry{}),
		"AppendProvenance([])")

	got, err := s.GetEntity(ctx, "boardgame:empty-append")
	require.NoError(t, err, "GetEntity")
	assert.Empty(t, got.Provenance, "provenance after no-op appends")
}

// TestLoadEntityProvenance_PreservesInsertionOrder pins the
// `ORDER BY id ASC` contract on `loadEntityProvenance` (`sqlite.go`).
// Many tests across the suite assert insertion-order on the returned
// provenance slice; that assumption is silently load-bearing on this
// one ORDER BY clause + the AUTOINCREMENT rowid → insertion-order
// invariant. A regression that removes the ORDER BY (or replaces it
// with ORDER BY source / fetched_at / etc.) would break those tests
// in confusing ways. This guard test fails fast and points at the
// right invariant.
//
// Strategy: insert sources in reverse-alphabetical order, verify
// they come back in insertion order (NOT alphabetical-by-source).
// Any "default" SQLite ordering by rowid still passes; an
// alphabetical-by-source ORDER BY would fail.
func TestLoadEntityProvenance_PreservesInsertionOrder(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:order-guard",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}))

	// Reverse-alphabetical sources, distinct fetched_at timestamps so
	// any timestamp-ordered SELECT would also fail. Multi-call appends
	// to exercise the "multiple txs of inserts" path.
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:order-guard",
		[]ProvenanceEntry{{Source: "z-source", FetchedAt: ts("2026-05-01T00:00:00Z"), OK: true}}))
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:order-guard",
		[]ProvenanceEntry{{Source: "m-source", FetchedAt: ts("2026-05-01T00:00:01Z"), OK: true}}))
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:order-guard",
		[]ProvenanceEntry{{Source: "a-source", FetchedAt: ts("2026-05-01T00:00:02Z"), OK: true}}))

	got, err := s.GetEntity(ctx, "boardgame:order-guard")
	require.NoError(t, err)
	require.Len(t, got.Provenance, 3)
	gotSources := []string{
		got.Provenance[0].Source,
		got.Provenance[1].Source,
		got.Provenance[2].Source,
	}
	assert.Equal(t, []string{"z-source", "m-source", "a-source"}, gotSources,
		"loadEntityProvenance must return rows in insertion order (ORDER BY id ASC + AUTOINCREMENT rowid). "+
			"Alphabetical-by-source would give [a, m, z]; chronological-by-fetched_at would also give [z, m, a] coincidentally; "+
			"this test catches the alphabetical-by-source regression specifically.")
}

// TestAppendProvenance_DuplicateFetchRowIsSilent pins ADR-0010's
// idempotency contract on the fetch path: inserting the same
// (entity_id, source, fetched_at) tuple a second time is a no-op —
// no error, no second row. Closes the AppendProvenance-after-
// ReplaceProvenance race window from ADR-0009 §Race / consistency.
func TestAppendProvenance_DuplicateFetchRowIsSilent(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:dup-fetch",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}))

	row := ProvenanceEntry{
		Source: "wikipedia:fetch",
		FetchedAt: ts("2026-05-01T00:00:00Z"),
		OK: true,
	}
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:dup-fetch",
		[]ProvenanceEntry{row}), "first AppendProvenance")
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:dup-fetch",
		[]ProvenanceEntry{row}), "second AppendProvenance with identical row should not error")

	got, err := s.GetEntity(ctx, "boardgame:dup-fetch")
	require.NoError(t, err)
	require.Len(t, got.Provenance, 1,
		"provenance count after duplicate Append: want 1 (ON CONFLICT DO NOTHING)")
	assert.Equal(t, "wikipedia:fetch", got.Provenance[0].Source)
}

// TestAppendProvenance_DuplicateFillRowIsSilent is the fill-path
// equivalent. Per ADR-0010 §"Why two partial indexes", the fill path
// has its own UNIQUE on (entity_id, source, filled_at) gated by
// `WHERE filled_at IS NOT NULL` — a separate index from the fetch
// path's so the SQLite NULL-distinct behaviour can't bypass either.
func TestAppendProvenance_DuplicateFillRowIsSilent(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:dup-fill",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}))

	row := ProvenanceEntry{
		Source: "agent:bob",
		FilledAt: ts("2026-05-01T00:00:00Z"),
		OK: true,
	}
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:dup-fill",
		[]ProvenanceEntry{row}), "first AppendProvenance")
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:dup-fill",
		[]ProvenanceEntry{row}), "second AppendProvenance with identical fill row should not error")

	got, err := s.GetEntity(ctx, "boardgame:dup-fill")
	require.NoError(t, err)
	require.Len(t, got.Provenance, 1,
		"provenance count after duplicate fill Append: want 1 (ON CONFLICT DO NOTHING)")
	assert.Equal(t, "agent:bob", got.Provenance[0].Source)
	assert.NotNil(t, got.Provenance[0].FilledAt)
	assert.Nil(t, got.Provenance[0].FetchedAt,
		"fill row: FetchedAt nil so the fetch-path UNIQUE doesn't claim it")
}

// TestAppendProvenance_FetchAndFillSameSourceCoexist guards against a
// regression that conflated the two indexes. A fetch row and a fill
// row with the same (entity, source) but disjoint timestamp shapes
// MUST coexist — they live under different partial UNIQUE indexes
// (each scoped by its own WHERE NOT NULL predicate). Conflating into
// one composite UNIQUE would make this test fail.
func TestAppendProvenance_FetchAndFillSameSourceCoexist(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:coexist",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}))

	require.NoError(t, s.AppendProvenance(ctx, "boardgame:coexist", []ProvenanceEntry{
		{Source: "wikipedia", FetchedAt: ts("2026-05-01T00:00:00Z"), OK: true},
		{Source: "wikipedia", FilledAt: ts("2026-05-01T00:00:01Z"), OK: true},
	}), "AppendProvenance with fetch + fill rows of same source")

	got, err := s.GetEntity(ctx, "boardgame:coexist")
	require.NoError(t, err)
	require.Len(t, got.Provenance, 2,
		"both fetch + fill rows of same source coexist (different partial UNIQUE indexes)")
}

// TestProvenanceUniqueIndexesPresent verifies that migration 006 has
// landed both partial UNIQUE indexes by name — sqlite_master is the
// authoritative source. A regression that drops or renames either
// index breaks the idempotency contract silently; this test catches
// it before consumers observe duplicate rows in the DB.
func TestProvenanceUniqueIndexesPresent(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	for _, idx := range []string{"idx_prov_unique_fetch", "idx_prov_unique_fill"} {
		var got string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name = ?`, idx,
		).Scan(&got)
		require.NoError(t, err, "lookup index %q", idx)
		assert.Equal(t, idx, got)
	}
}

// TestProvenanceMigrationDedupsExistingDuplicates pins the migration's
// pre-create dedup pass. Drops the indexes, inserts intentional
// duplicates via raw SQL (bypassing AppendProvenance's ON CONFLICT),
// re-runs JUST the dedup query from migration 006, and verifies only
// the earliest-rowid row survives per group + the indexes can re-
// create cleanly. Proves the migration is selective (preserves non-
// duplicates) and effective (removes duplicates) so `CREATE UNIQUE
// INDEX` can succeed on an existing DB with accumulated duplicates.
func TestProvenanceMigrationDedupsExistingDuplicates(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	// Drop the indexes from migration 006 so we can insert duplicates
	// without ON CONFLICT eating them. (Migration 006 dropped the
	// indexes after this dedup pass; we're simulating the pre-006
	// state to exercise the dedup query.)
	for _, idx := range []string{"idx_prov_unique_fetch", "idx_prov_unique_fill"} {
		_, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+idx)
		require.NoError(t, err, "drop %s for the test setup", idx)
	}

	// Seed three pairs of duplicates (fetch-shape, fill-shape, error-
	// fetch) plus a unique row that should survive untouched.
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:dedup",
		Kind: "boardgame",
		Data: map[string]any{"title": "x"},
	}))
	insert := func(source string, fetchedAt, filledAt *time.Time, ok bool) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO provenance (
				target_kind, target_entity_id, source,
				fetched_at, filled_at, ok, error, error_message
			) VALUES ('entity', ?, ?, ?, ?, ?, NULL, NULL)
		`, "boardgame:dedup", source,
			nullableTime(fetchedAt), nullableTime(filledAt), boolToInt(ok))
		require.NoError(t, err, "raw insert for %s", source)
	}
	t1 := ts("2026-05-01T00:00:00Z")
	t2 := ts("2026-05-01T00:00:01Z")
	insert("fetch:dup", t1, nil, true) // pair: row A
	insert("fetch:dup", t1, nil, true) // pair: row A duplicate
	insert("fill:dup", nil, t2, true) // pair: row B
	insert("fill:dup", nil, t2, true) // pair: row B duplicate
	insert("unique:keep", t2, nil, true)

	var preCount int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM provenance WHERE target_entity_id = ?`,
		"boardgame:dedup").Scan(&preCount))
	require.Equal(t, 5, preCount, "pre-dedup row count")

	// Re-run JUST the dedup query from migration 006.
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM provenance WHERE rowid NOT IN (
			SELECT MIN(rowid) FROM provenance
			GROUP BY target_entity_id, source, fetched_at, filled_at
		)
	`)
	require.NoError(t, err, "re-run migration 006 dedup query")

	var postCount int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM provenance WHERE target_entity_id = ?`,
		"boardgame:dedup").Scan(&postCount))
	assert.Equal(t, 3, postCount,
		"post-dedup row count: 1 fetch + 1 fill + 1 unique (each duplicate pair collapsed to MIN(rowid))")

	// Indexes can re-create successfully on the dedup'd state.
	_, err = s.db.ExecContext(ctx, `
		CREATE UNIQUE INDEX idx_prov_unique_fetch
			ON provenance(target_entity_id, source, fetched_at)
			WHERE fetched_at IS NOT NULL`)
	require.NoError(t, err, "recreate fetch index after dedup")
	_, err = s.db.ExecContext(ctx, `
		CREATE UNIQUE INDEX idx_prov_unique_fill
			ON provenance(target_entity_id, source, filled_at)
			WHERE filled_at IS NOT NULL`)
	require.NoError(t, err, "recreate fill index after dedup")
}

// TestReplaceProvenance_OverwritesExistingList pins the ADR-0009
// contract: ReplaceProvenance drops the entity's prior provenance rows
// and inserts the new list in one transaction. Reindex's vault-canonical
// re-derivation depends on this being the destructive shape (vs.
// AppendProvenance's accumulating shape).
func TestReplaceProvenance_OverwritesExistingList(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:replace-prov",
		Kind: "boardgame",
		Data: map[string]any{"title": "Replacer"},
	}), "UpsertEntity")

	// Seed three prior rows via AppendProvenance — exercises the
	// "prior set is non-trivial" path.
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:replace-prov", []ProvenanceEntry{
		{Source: "ingest:1", FetchedAt: ts("2026-04-01T00:00:00Z"), OK: true},
		{Source: "ingest:2", FetchedAt: ts("2026-04-02T00:00:00Z"), OK: true},
		{Source: "agent:bob", FilledAt: ts("2026-04-02T00:01:00Z"), OK: true},
	}), "seed AppendProvenance")

	// Replace with a different two-row list. Old rows should be gone,
	// new ones present in insertion order.
	require.NoError(t, s.ReplaceProvenance(ctx, "boardgame:replace-prov", []ProvenanceEntry{
		{Source: "vault:reindex", FetchedAt: ts("2026-05-01T00:00:00Z"), OK: true},
		{
			Source: "ingest:retry", FetchedAt: ts("2026-05-01T00:00:01Z"),
			OK: false, Error: "extractor_timeout", ErrorMessage: "timed out",
		},
	}), "ReplaceProvenance")

	got, err := s.GetEntity(ctx, "boardgame:replace-prov")
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 2,
		"provenance after Replace: want 2 (old 3 dropped, new 2 inserted)")
	gotSources := make([]string, len(got.Provenance))
	for i, p := range got.Provenance {
		gotSources[i] = p.Source
	}
	assert.Equal(t, []string{"vault:reindex", "ingest:retry"}, gotSources,
		"provenance sources: want exactly the new list, in insertion order")

	// Failure-shape round-trip on the second new row.
	assert.False(t, got.Provenance[1].OK)
	assert.Equal(t, "extractor_timeout", got.Provenance[1].Error)
}

// TestReplaceProvenance_EmptyEntriesDropsAll covers the "vault list
// became empty" case: ReplaceProvenance with nil/[] removes every prior
// row for the entity and inserts nothing. The entity row itself stays
// (provenance is independent state).
func TestReplaceProvenance_EmptyEntriesDropsAll(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertEntity(ctx, &Entity{
		ID: "boardgame:replace-empty",
		Kind: "boardgame",
		Data: map[string]any{"title": "Empty"},
	}), "UpsertEntity")
	require.NoError(t, s.AppendProvenance(ctx, "boardgame:replace-empty", []ProvenanceEntry{
		{Source: "ingest:1", FetchedAt: ts("2026-04-01T00:00:00Z"), OK: true},
	}), "seed AppendProvenance")

	require.NoError(t, s.ReplaceProvenance(ctx, "boardgame:replace-empty", nil),
		"ReplaceProvenance(nil)")

	got, err := s.GetEntity(ctx, "boardgame:replace-empty")
	require.NoError(t, err, "GetEntity")
	assert.Empty(t, got.Provenance, "provenance after Replace(nil): want empty")
	assert.Equal(t, "Empty", got.Data["title"],
		"entity data: want unchanged (Replace touches only provenance)")

	// And the empty-slice form is identical.
	require.NoError(t, s.ReplaceProvenance(ctx, "boardgame:replace-empty", []ProvenanceEntry{}),
		"ReplaceProvenance([])")
	got, err = s.GetEntity(ctx, "boardgame:replace-empty")
	require.NoError(t, err, "GetEntity")
	assert.Empty(t, got.Provenance, "provenance after Replace([]): want empty")
}

// TestReplaceProvenance_DoesNotTouchOtherEntities pins the per-entity
// scoping: Replace for entity A leaves entity B's provenance intact.
// A regression that DELETE-d without the WHERE would wipe the whole
// table and silently break other entities; the test catches it.
func TestReplaceProvenance_DoesNotTouchOtherEntities(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	for _, id := range []string{"boardgame:scope-a", "boardgame:scope-b"} {
		require.NoError(t, s.UpsertEntity(ctx, &Entity{
			ID: id,
			Kind: "boardgame",
			Data: map[string]any{"title": id},
		}), "UpsertEntity %s", id)
		require.NoError(t, s.AppendProvenance(ctx, id, []ProvenanceEntry{
			{Source: id + ":fetch", FetchedAt: ts("2026-04-01T00:00:00Z"), OK: true},
		}), "AppendProvenance %s", id)
	}

	// Replace ONLY entity A's provenance.
	require.NoError(t, s.ReplaceProvenance(ctx, "boardgame:scope-a", []ProvenanceEntry{
		{Source: "vault:reindex-a", FetchedAt: ts("2026-05-01T00:00:00Z"), OK: true},
	}), "ReplaceProvenance(A)")

	gotA, err := s.GetEntity(ctx, "boardgame:scope-a")
	require.NoError(t, err)
	require.Len(t, gotA.Provenance, 1)
	assert.Equal(t, "vault:reindex-a", gotA.Provenance[0].Source,
		"A's provenance: want replaced")

	gotB, err := s.GetEntity(ctx, "boardgame:scope-b")
	require.NoError(t, err)
	require.Len(t, gotB.Provenance, 1, "B's provenance: want unchanged (1 row)")
	assert.Equal(t, "boardgame:scope-b:fetch", gotB.Provenance[0].Source,
		"B's provenance: want untouched by A's Replace")
}

// TestReplaceProvenance_EmptyEntityIDRejected guards the precondition
// (mirrors AppendProvenance's empty-entityID check). Without this,
// a caller bug could DELETE FROM provenance with a tx-wide WHERE that
// matches every empty-entity row in the table — defense in depth.
func TestReplaceProvenance_EmptyEntityIDRejected(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()
	err := s.ReplaceProvenance(ctx, "", []ProvenanceEntry{
		{Source: "x", FetchedAt: ts("2026-05-01T00:00:00Z"), OK: true},
	})
	require.Error(t, err, "ReplaceProvenance with empty entityID")
	assert.Contains(t, err.Error(), "empty entityID")
}

// TestSaveEntity_StillWipes locks the historical SaveEntity contract so
// the divergence from UpsertEntity stays explicit. a prior PR's
// TestSaveEntity_OnReSaveReplacesProvenance covers the same rule; this
// is the parallel assertion phrased against the new naming so future
// readers see both methods together.
func TestSaveEntity_StillWipesProvenanceOnReSave(t *testing.T) {
	t.Parallel()

	s := newMemoryStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "boardgame:save-wipes",
		Kind: "boardgame",
		Data: map[string]any{"title": "X"},
		Provenance: []ProvenanceEntry{
			{Source: "first:fetch", FetchedAt: ts("2026-01-01T00:00:00Z"), OK: true},
		},
	}), "first SaveEntity")
	require.NoError(t, s.SaveEntity(ctx, &Entity{
		ID: "boardgame:save-wipes",
		Kind: "boardgame",
		Data: map[string]any{"title": "Y"},
		Provenance: []ProvenanceEntry{
			{Source: "second:fetch", FetchedAt: ts("2026-01-02T00:00:00Z"), OK: true},
		},
	}), "second SaveEntity")

	got, err := s.GetEntity(ctx, "boardgame:save-wipes")
	require.NoError(t, err, "GetEntity")
	require.Len(t, got.Provenance, 1, "provenance after re-SaveEntity: want 1 (wiped+rewritten)")
	assert.Equal(t, "second:fetch", got.Provenance[0].Source)
}
