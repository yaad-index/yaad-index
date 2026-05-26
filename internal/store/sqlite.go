package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the pure-Go sqlite driver under "sqlite"
)

const sqliteTimeFormat = time.RFC3339Nano

// New opens (or creates) a SQLite database at path, sets the canonical
// connection pragmas, runs any pending migrations, and returns a Store
// backed by it.
//
// Two special path values are accepted as in-memory: ":memory:" and any
// path with the "file::memory:" prefix. For those, the parent-directory
// auto-create and WAL pragma are skipped (WAL is meaningless without a
// file).
func New(path string) (Store, error) {
	if !isMemoryPath(path) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create db parent dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %q: %w", path, err)
	}

	// SQLite serialises writes at the file level. With Go's connection
	// pool and WAL enabled, concurrent goroutines can each grab a
	// connection and stack write attempts — modernc surfaces the
	// resulting contention as SQLITE_BUSY rather than waiting. Cap the
	// pool to one connection so all writes funnel through a single
	// driver-side queue. (Reads serialize too at this cap; if read-side
	// throughput becomes a bottleneck we can split into a read-only
	// pool against the same file later.)
	db.SetMaxOpenConns(1)

	if err := applyPragmas(db, path); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &sqliteStore{db: db}, nil
}

func isMemoryPath(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:")
}

func applyPragmas(db *sql.DB, path string) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	}
	if !isMemoryPath(path) {
		// WAL requires a file-backed database; meaningless on :memory:.
		pragmas = append(pragmas, "PRAGMA journal_mode = WAL")
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// sqliteStore is the modernc-backed Store implementation. The methods
// return ErrNotImplemented today; subsequent PRs swap them in one at a
// time per ADR-0002's staged-rollout (handlers continue to serve their
// hardcoded fixtures until each method lands).
type sqliteStore struct {
	db *sql.DB
}

// SaveEntity writes an entity row plus its provenance entries. Existing
// provenance for the entity is replaced with what's on `e` — the simplest
// "this is the current full state" semantics. Real ingest will need
// upsert + provenance-append (ADR-0002 line 281); when that lands the
// behavior on re-save changes and this comment becomes the divergence
// note.
func (s *sqliteStore) SaveEntity(ctx context.Context, e *Entity) error {
	if e == nil {
		return errors.New("SaveEntity: nil entity")
	}
	if e.ID == "" {
		return errors.New("SaveEntity: empty id")
	}
	if e.Kind == "" {
		return errors.New("SaveEntity: empty kind")
	}

	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now

	dataJSON, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("marshal data for %s: %w", e.ID, err)
	}
	gapStateJSON, err := marshalGapState(e.GapState)
	if err != nil {
		return fmt.Errorf("marshal gap_state for %s: %w", e.ID, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entities (id, kind, data, gap_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			data = excluded.data,
			gap_state = excluded.gap_state,
			updated_at = excluded.updated_at
	`, e.ID, e.Kind, string(dataJSON), gapStateJSON,
		e.CreatedAt.Format(sqliteTimeFormat),
		e.UpdatedAt.Format(sqliteTimeFormat),
	); err != nil {
		return fmt.Errorf("upsert entity %s: %w", e.ID, err)
	}

	// Wipe + reinsert provenance for this entity. See the package comment
	// above — append-on-re-save is the real-ingest semantics, not what
	// this method does today.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM provenance WHERE target_kind = 'entity' AND target_entity_id = ?`,
		e.ID,
	); err != nil {
		return fmt.Errorf("clear provenance for %s: %w", e.ID, err)
	}
	for i, p := range e.Provenance {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provenance (
				target_kind, target_entity_id, source,
				fetched_at, filled_at, ok, error, error_message
			) VALUES ('entity', ?, ?, ?, ?, ?, ?, ?)
		`, e.ID, p.Source,
			nullableTime(p.FetchedAt),
			nullableTime(p.FilledAt),
			boolToInt(p.OK),
			nullIfEmpty(p.Error),
			nullIfEmpty(p.ErrorMessage),
		); err != nil {
			return fmt.Errorf("insert provenance[%d] for %s: %w", i, e.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// UpsertEntity writes the entity row only — kind, data, timestamps. Any
// existing provenance for this id is left untouched (this is the load-
// bearing divergence from SaveEntity, which wipes-and-rewrites). Used by
// the ingest write path so re-ingest of the same URL updates `data`
// while AppendProvenance records each attempt as a new row.
func (s *sqliteStore) UpsertEntity(ctx context.Context, e *Entity) error {
	if e == nil {
		return errors.New("UpsertEntity: nil entity")
	}
	if e.ID == "" {
		return errors.New("UpsertEntity: empty id")
	}
	if e.Kind == "" {
		return errors.New("UpsertEntity: empty kind")
	}

	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now

	dataJSON, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("marshal data for %s: %w", e.ID, err)
	}
	gapStateJSON, err := marshalGapState(e.GapState)
	if err != nil {
		return fmt.Errorf("marshal gap_state for %s: %w", e.ID, err)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO entities (id, kind, data, gap_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			data = excluded.data,
			gap_state = excluded.gap_state,
			updated_at = excluded.updated_at
	`, e.ID, e.Kind, string(dataJSON), gapStateJSON,
		e.CreatedAt.Format(sqliteTimeFormat),
		e.UpdatedAt.Format(sqliteTimeFormat),
	); err != nil {
		return fmt.Errorf("upsert entity %s: %w", e.ID, err)
	}
	return nil
}

// MarkGapCallDone stamps the entity's `gap_call_done_at` to the
// current UTC timestamp (per ADR-0013 §4 + §5). Returns ErrNotFound
// when the entity doesn't exist (the column lives ON `entities`,
// so a missing row means there's nothing to flag).
func (s *sqliteStore) MarkGapCallDone(ctx context.Context, entityID string) error {
	if entityID == "" {
		return errors.New("MarkGapCallDone: empty entityID")
	}
	now := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := s.db.ExecContext(ctx,
		`UPDATE entities SET gap_call_done_at = ? WHERE id = ?`,
		now, entityID,
	)
	if err != nil {
		return fmt.Errorf("mark gap_call_done_at for %s: %w", entityID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ArchiveEntity flips an entity into the archived state per
// ADR-0018 step 2. Idempotent: a row whose `archived_at` is already
// non-NULL keeps its original timestamp (a re-archive is a no-op,
// not a re-stamp — the original archive event is the source of truth).
// Returns ErrNotFound when no entity with the given id exists.
func (s *sqliteStore) ArchiveEntity(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("ArchiveEntity: empty id")
	}
	now := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := s.db.ExecContext(ctx,
		`UPDATE entities
			SET archived_at = COALESCE(archived_at, ?),
			 updated_at = ?
			WHERE id = ?`,
		now, now, id,
	)
	if err != nil {
		return fmt.Errorf("archive entity %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RestoreEntity is the inverse of ArchiveEntity: clears archived_at
// and bumps updated_at. Idempotent on already-active rows. Returns
// ErrNotFound when no entity with the given id exists.
func (s *sqliteStore) RestoreEntity(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("RestoreEntity: empty id")
	}
	now := time.Now().UTC().Format(sqliteTimeFormat)
	res, err := s.db.ExecContext(ctx,
		`UPDATE entities SET archived_at = NULL, updated_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("restore entity %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearGapCallDone sets the flag to NULL (per ADR-0013 §4: called
// on `force_refetch=true` and TTL-driven refetch). Idempotent —
// clearing an already-NULL flag returns success without an error.
// Returns ErrNotFound when the entity doesn't exist.
func (s *sqliteStore) ClearGapCallDone(ctx context.Context, entityID string) error {
	if entityID == "" {
		return errors.New("ClearGapCallDone: empty entityID")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE entities SET gap_call_done_at = NULL WHERE id = ?`,
		entityID,
	)
	if err != nil {
		return fmt.Errorf("clear gap_call_done_at for %s: %w", entityID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGapCallableCandidates is the DB-side filter for the
// `GET /v1/needs-fill` paginated endpoint (per ADR-0013 §6 /
// yaad-index). Returns entities whose `gap_call_done_at` is
// NULL — the lifecycle's "AI has not yet been gap-called this
// fetch-cycle" condition (per ADR-0013 §4). The handler layer
// vault-reads each result to confirm there are actually unfilled
// gaps before surfacing it on the wire.
//
// Ordering: `id ASC` for deterministic pagination. `afterID`,
// when non-empty, restricts to ids strictly greater (cursor
// resume); empty afterID returns the first page. `limit` caps the
// row count; callers clamp / default at the handler boundary.
func (s *sqliteStore) ListGapCallableCandidates(ctx context.Context, afterID string, limit int) ([]Entity, error) {
	if limit <= 0 {
		return nil, nil
	}
	query := `
		SELECT id, kind, data, created_at, updated_at, gap_call_done_at
		FROM entities
		WHERE gap_call_done_at IS NULL
	`
	args := make([]any, 0, 2)
	if afterID != "" {
		query += " AND id > ?"
		args = append(args, afterID)
	}
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query gap-callable candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Entity, 0, limit)
	for rows.Next() {
		var (
			e Entity
			dataJSON string
			createdStr string
			updatedStr string
			gapStr sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Kind, &dataJSON, &createdStr, &updatedStr, &gapStr); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		if err := json.Unmarshal([]byte(dataJSON), &e.Data); err != nil {
			return nil, fmt.Errorf("unmarshal data for %s: %w", e.ID, err)
		}
		if e.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
			return nil, fmt.Errorf("parse created_at for %s: %w", e.ID, err)
		}
		if e.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, fmt.Errorf("parse updated_at for %s: %w", e.ID, err)
		}
		// gap_call_done_at is filtered to NULL in the WHERE clause —
		// no need to populate the field.
		_ = gapStr
		e.Edges = []EdgeRef{}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return out, nil
}

// ReplaceProvenance overwrites an entity's provenance rows with the
// given list. Wraps the DELETE + INSERTs in a single transaction:
// either every input row replaces the prior set, or the prior set
// stays intact (rollback on any failure). Used by reindex to re-derive
// DB provenance from the canonical vault frontmatter list per ADR-0009;
// the live ingest / fill paths keep using AppendProvenance for
// incremental accumulation.
//
// Empty entries is permitted — the entity's existing provenance rows
// are deleted and nothing is inserted. Useful for entities whose vault
// frontmatter has had its `provenance:` list cleared.
//
// The entity's existence is not pre-checked (provenance has no FK to
// entities, matching AppendProvenance's contract). Operators can
// reset provenance for an id even before / after the entity row lands.
func (s *sqliteStore) ReplaceProvenance(ctx context.Context, entityID string, entries []ProvenanceEntry) error {
	if entityID == "" {
		return errors.New("ReplaceProvenance: empty entityID")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM provenance WHERE target_kind = 'entity' AND target_entity_id = ?`,
		entityID,
	); err != nil {
		return fmt.Errorf("delete prior provenance for %s: %w", entityID, err)
	}

	for i, p := range entries {
		fetchAttJSON, err := marshalFetchAttachments(p.FetchAttachments)
		if err != nil {
			return fmt.Errorf("marshal fetch_attachments[%d] for %s: %w", i, entityID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provenance (
				target_kind, target_entity_id, source,
				fetched_at, filled_at, ok, error, error_message,
				fetch_attachments
			) VALUES ('entity', ?, ?, ?, ?, ?, ?, ?, ?)
		`, entityID, p.Source,
			nullableTime(p.FetchedAt),
			nullableTime(p.FilledAt),
			boolToInt(p.OK),
			nullIfEmpty(p.Error),
			nullIfEmpty(p.ErrorMessage),
			fetchAttJSON,
		); err != nil {
			return fmt.Errorf("insert provenance[%d] for %s: %w", i, entityID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// AppendProvenance inserts new provenance rows for an entity without
// touching anything that's already there. Empty entries is a no-op.
//
// The entity's existence is not pre-checked — the provenance schema does
// not declare a foreign key to entities, so callers can record an
// ingest-attempt provenance row even before the entity row lands (or
// after the entity has been removed). Ingest's flow always pairs
// UpsertEntity with AppendProvenance to keep the two in sync.
func (s *sqliteStore) AppendProvenance(ctx context.Context, entityID string, entries []ProvenanceEntry) error {
	if entityID == "" {
		return errors.New("AppendProvenance: empty entityID")
	}
	if len(entries) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, p := range entries {
		fetchAttJSON, err := marshalFetchAttachments(p.FetchAttachments)
		if err != nil {
			return fmt.Errorf("marshal fetch_attachments[%d] for %s: %w", i, entityID, err)
		}
		// Two ON CONFLICT clauses target the two partial UNIQUE
		// indexes from migration 006_provenance_unique (per ADR-0010):
		// fetch-path rows (fetched_at non-NULL) and fill-path rows
		// (filled_at non-NULL). Either matches the inserting row's
		// shape; a duplicate becomes a silent no-op, so a concurrent
		// reindex `ReplaceProvenance` + live ingest `AppendProvenance`
		// can't produce a duplicate row. ReplaceProvenance is itself
		// unaffected — DELETE-then-INSERT in one tx, no conflict
		// possible internally.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provenance (
				target_kind, target_entity_id, source,
				fetched_at, filled_at, ok, error, error_message,
				fetch_attachments
			) VALUES ('entity', ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(target_entity_id, source, fetched_at) WHERE fetched_at IS NOT NULL DO NOTHING
			ON CONFLICT(target_entity_id, source, filled_at) WHERE filled_at IS NOT NULL DO NOTHING
		`, entityID, p.Source,
			nullableTime(p.FetchedAt),
			nullableTime(p.FilledAt),
			boolToInt(p.OK),
			nullIfEmpty(p.Error),
			nullIfEmpty(p.ErrorMessage),
			fetchAttJSON,
		); err != nil {
			return fmt.Errorf("insert provenance[%d] for %s: %w", i, entityID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetEntity reads an entity row + its provenance. Returns ErrNotFound when
// no row matches. Edges are intentionally not loaded today — ADR-0002's
// `with_edges` expansion lands with the edge-side cutover; until then
// every entity is returned with an empty edges array (matches the wire
// shape callers have always seen).
func (s *sqliteStore) GetEntity(ctx context.Context, id string) (*Entity, error) {
	if id == "" {
		return nil, errors.New("GetEntity: empty id")
	}
	e := &Entity{ID: id, Edges: []EdgeRef{}}
	var dataJSON, createdStr, updatedStr string
	var gapCallStr, archivedStr, gapStateStr sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, data, created_at, updated_at, gap_call_done_at, archived_at, gap_state
		FROM entities WHERE id = ?
	`, id).Scan(&e.Kind, &dataJSON, &createdStr, &updatedStr, &gapCallStr, &archivedStr, &gapStateStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get entity %s: %w", id, err)
	}

	if err := json.Unmarshal([]byte(dataJSON), &e.Data); err != nil {
		return nil, fmt.Errorf("unmarshal data for %s: %w", id, err)
	}
	if e.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
		return nil, fmt.Errorf("parse created_at for %s: %w", id, err)
	}
	if e.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
		return nil, fmt.Errorf("parse updated_at for %s: %w", id, err)
	}
	if gapCallStr.Valid && gapCallStr.String != "" {
		t, err := parseSQLiteTime(gapCallStr.String)
		if err != nil {
			return nil, fmt.Errorf("parse gap_call_done_at for %s: %w", id, err)
		}
		e.GapCallDoneAt = &t
	}
	// archived_at is nullable per ADR-0018: NULL = active, non-NULL
	// = archived at that timestamp.
	if archivedStr.Valid && archivedStr.String != "" {
		t, err := parseSQLiteTime(archivedStr.String)
		if err != nil {
			return nil, fmt.Errorf("parse archived_at for %s: %w", id, err)
		}
		e.ArchivedAt = &t
	}
	// gap_state per ADR-0019 §Storage. Pre-ADR-0019 rows are NULL;
	// post-ADR-0019 rows with no metadata still serialize as NULL
	// (marshalGapState collapses empty + nil to "" so the column
	// stays NULL, not an empty-object string).
	if gapStateStr.Valid && gapStateStr.String != "" {
		e.GapState, err = unmarshalGapState(gapStateStr.String)
		if err != nil {
			return nil, fmt.Errorf("parse gap_state for %s: %w", id, err)
		}
	}

	prov, err := s.loadEntityProvenance(ctx, id)
	if err != nil {
		return nil, err
	}
	e.Provenance = prov
	return e, nil
}

// GetEntities reads a batch of entities. Order of `matched` follows the
// order of `ids`; ids with no matching row land in `missing` (also in
// input order). An empty input yields empty outputs and a nil error.
//
// Implementation note: a single "WHERE id IN (...)" query would lose the
// caller's order; instead we issue per-id GetEntity calls behind the
// single-connection pool. For batches up to 100 (the API cap) this is
// 100 round-trips on a local DB — fine for v1. If the wire shape grows or
// batch sizes balloon, swap to one query + an order-preserving merge.
func (s *sqliteStore) GetEntities(ctx context.Context, ids []string) ([]Entity, []string, error) {
	matched := make([]Entity, 0, len(ids))
	missing := make([]string, 0)
	for _, id := range ids {
		e, err := s.GetEntity(ctx, id)
		if errors.Is(err, ErrNotFound) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		matched = append(matched, *e)
	}
	return matched, missing, nil
}

func (s *sqliteStore) loadEntityProvenance(ctx context.Context, entityID string) ([]ProvenanceEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, fetched_at, filled_at, ok, error, error_message, fetch_attachments
		FROM provenance
		WHERE target_kind = 'entity' AND target_entity_id = ?
		ORDER BY id ASC
	`, entityID)
	if err != nil {
		return nil, fmt.Errorf("query provenance for %s: %w", entityID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProvenanceEntry
	for rows.Next() {
		var (
			source string
			fetchedAt, filledAt sql.NullString
			okInt int
			errStr, errMsgStr sql.NullString
			fetchAttJSON sql.NullString
		)
		if err := rows.Scan(&source, &fetchedAt, &filledAt, &okInt, &errStr, &errMsgStr, &fetchAttJSON); err != nil {
			return nil, fmt.Errorf("scan provenance for %s: %w", entityID, err)
		}
		p := ProvenanceEntry{Source: source, OK: okInt != 0}
		if fetchedAt.Valid {
			t, err := parseSQLiteTime(fetchedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse fetched_at for %s: %w", entityID, err)
			}
			p.FetchedAt = &t
		}
		if filledAt.Valid {
			t, err := parseSQLiteTime(filledAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse filled_at for %s: %w", entityID, err)
			}
			p.FilledAt = &t
		}
		if errStr.Valid {
			p.Error = errStr.String
		}
		if fetchAttJSON.Valid && fetchAttJSON.String != "" {
			// Decode failure is non-fatal — a single corrupt
			// fetch_attachments column would otherwise propagate up
			// through loadEntityProvenance + GetEntity and block ALL
			// reads of an entity whose row is otherwise healthy.
			// Disproportionate blast radius for a column whose worst-
			// case effect is "the next ingest's re-fetch comparison
			// thinks no prior attachments existed and re-fetches
			// every URI" — same fail-soft posture as ADR §4 and the
			// dispatcher's vault-read-error path. Log at WARN, treat
			// as no-prior, continue (the cold-reviewer's a prior PR review note).
			refs, err := unmarshalFetchAttachments(fetchAttJSON.String)
			if err != nil {
				slog.Default().Warn("decode fetch_attachments column failed; treating as no-prior",
					"entity", entityID, "err", err)
			} else {
				p.FetchAttachments = refs
			}
		}
		if errMsgStr.Valid {
			p.ErrorMessage = errMsgStr.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provenance for %s: %w", entityID, err)
	}
	return out, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(sqliteTimeFormat)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// marshalGapState serializes the gap_state map for the SQLite TEXT
// column per ADR-0019 §Storage. Empty / nil maps return a literal
// nil interface so the database/sql driver binds it as SQL NULL on
// the column — preserves the "absent gap_state" shape that pre-
// ADR-0019 rows have and that callers branch on. Non-empty maps
// return the raw JSON object as a string; the driver writes it
// verbatim into the TEXT column.
func marshalGapState(state map[string]GapStateEntry) (any, error) {
	if len(state) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// unmarshalGapState parses the gap_state TEXT column. The empty-
// string + NULL cases are handled by the caller (only invokes this
// when the column is non-NULL non-empty) so this just runs JSON
// unmarshal. Returns nil map on the JSON `null` literal so callers
// see "no metadata" cleanly even if a buggy writer stored the
// literal "null" string.
func unmarshalGapState(raw string) (map[string]GapStateEntry, error) {
	if raw == "" || raw == "null" {
		return nil, nil
	}
	out := make(map[string]GapStateEntry)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseSQLiteTime accepts both RFC3339 and RFC3339Nano so a database
// written by an older code path (RFC3339 second-resolution) is read back
// without losing fidelity. Newer writes use RFC3339Nano (sqliteTimeFormat).
func parseSQLiteTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// GetEdgesFor reads edges originating at fromID. If types is non-empty,
// only edges whose .Type is in the set are returned. Output ordering is
// stable (created_at ASC) so callers can rely on deterministic output
// for tests and snapshotting. Provenance and metadata are populated;
// the implementation owns the SQL.
func (s *sqliteStore) GetEdgesFor(ctx context.Context, fromID string, types []string) ([]Edge, error) {
	if fromID == "" {
		return nil, errors.New("GetEdgesFor: empty fromID")
	}

	query := `
		SELECT type, from_id, to_id, metadata, created_at, updated_at
		FROM edges
		WHERE from_id = ?
	`
	args := []any{fromID}
	if len(types) > 0 {
		// Build a "type IN (?, ?, ...)" clause for the optional filter.
		// Per-call slice — no string-builder cost worth caring about at
		// the API's batch sizes.
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		query += " AND type IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY created_at ASC, type ASC, to_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges for %s: %w", fromID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Edge, 0)
	for rows.Next() {
		var (
			edge Edge
			metadataRaw sql.NullString
			createdStr string
			updatedStr string
		)
		if err := rows.Scan(&edge.Type, &edge.From, &edge.To, &metadataRaw, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan edge for %s: %w", fromID, err)
		}
		if metadataRaw.Valid && metadataRaw.String != "" {
			if err := json.Unmarshal([]byte(metadataRaw.String), &edge.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata for edge %s→%s: %w", edge.From, edge.To, err)
			}
		}
		if edge.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
			return nil, fmt.Errorf("parse edge created_at: %w", err)
		}
		if edge.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, fmt.Errorf("parse edge updated_at: %w", err)
		}
		// Edge provenance is not loaded today — no provenance rows are
		// written for edges yet either. When ingest starts producing
		// provenance for edges, the same loadProvenance pattern as
		// entities applies.
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edges for %s: %w", fromID, err)
	}
	return out, nil
}

// GetEdgesTo reads edges terminating at toID — the inbound mirror
// of GetEdgesFor. If types is non-empty, only edges whose .Type is
// in the set are returned. Stable ordering (created_at ASC, type
// ASC, from_id ASC) parallels the outbound query.
//
// Per yaad-index: the existing /v1/entities/{id}?with_edges=
// surface only emits outbound; this helper is the substrate for
// the new GET /v1/edges?direction=in path.
func (s *sqliteStore) GetEdgesTo(ctx context.Context, toID string, types []string) ([]Edge, error) {
	if toID == "" {
		return nil, errors.New("GetEdgesTo: empty toID")
	}

	query := `
		SELECT type, from_id, to_id, metadata, created_at, updated_at
		FROM edges
		WHERE to_id = ?
	`
	args := []any{toID}
	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		query += " AND type IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY created_at ASC, type ASC, from_id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query edges to %s: %w", toID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Edge, 0)
	for rows.Next() {
		var (
			edge Edge
			metadataRaw sql.NullString
			createdStr string
			updatedStr string
		)
		if err := rows.Scan(&edge.Type, &edge.From, &edge.To, &metadataRaw, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan edge to %s: %w", toID, err)
		}
		if metadataRaw.Valid && metadataRaw.String != "" {
			if err := json.Unmarshal([]byte(metadataRaw.String), &edge.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata for edge %s→%s: %w", edge.From, edge.To, err)
			}
		}
		if edge.CreatedAt, err = parseSQLiteTime(createdStr); err != nil {
			return nil, fmt.Errorf("parse edge created_at: %w", err)
		}
		if edge.UpdatedAt, err = parseSQLiteTime(updatedStr); err != nil {
			return nil, fmt.Errorf("parse edge updated_at: %w", err)
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edges to %s: %w", toID, err)
	}
	return out, nil
}

// CreateEdge upserts an edge keyed on (type, from, to) and rewrites its
// metadata + updated_at. If either endpoint references an entity id that
// doesn't exist, ErrMissingEntity is returned (wrapped with the offending
// id) so the handler can map it to a 422 missing_entity envelope.
//
// Implementation note: existence is pre-checked rather than relying on
// the schema-level FOREIGN KEY constraint to surface the violation.
// The pre-check produces a deterministic, driver-agnostic error path
// (modernc/sqlite's exact FK-error wording is an implementation detail
// we'd otherwise pin a string match against). The FK constraint stays
// in place as defense-in-depth — under SetMaxOpenConns(1) there's no
// race window worth worrying about.
func (s *sqliteStore) CreateEdge(ctx context.Context, e *Edge) error {
	if e == nil {
		return errors.New("CreateEdge: nil edge")
	}
	if e.Type == "" || e.From == "" || e.To == "" {
		return errors.New("CreateEdge: type, from, and to are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := assertEntityExists(ctx, tx, e.From, "from"); err != nil {
		return err
	}
	if err := assertEntityExists(ctx, tx, e.To, "to"); err != nil {
		return err
	}

	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now

	var metadataJSON any
	if e.Metadata == nil {
		metadataJSON = nil
	} else {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for %s→%s: %w", e.From, e.To, err)
		}
		metadataJSON = string(b)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges (type, from_id, to_id, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(type, from_id, to_id) DO UPDATE SET
			metadata = excluded.metadata,
			updated_at = excluded.updated_at
	`, e.Type, e.From, e.To, metadataJSON,
		e.CreatedAt.Format(sqliteTimeFormat),
		e.UpdatedAt.Format(sqliteTimeFormat),
	); err != nil {
		return fmt.Errorf("upsert edge %s %s→%s: %w", e.Type, e.From, e.To, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// DeleteEdgesByTypeFrom removes every edge of the given type
// originating at fromID. Used by the canonical_type fill path
// (yaad-index) to implement idempotent re-fill semantics:
// before creating new edges from a re-filled canonical_type
// gap, the prior fill's edges are deleted so the post-fill
// edge set is exactly the new fill's labels. No edge appending,
// no partial diff — full replacement.
//
// Returns the number of rows removed (purely informational; the
// caller treats zero as a normal first-fill). Any DB error is
// returned as-is.
func (s *sqliteStore) DeleteEdgesByTypeFrom(ctx context.Context, fromID, edgeType string) (int64, error) {
	if fromID == "" || edgeType == "" {
		return 0, errors.New("DeleteEdgesByTypeFrom: fromID and edgeType are required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM edges WHERE from_id = ? AND type = ?`,
		fromID, edgeType,
	)
	if err != nil {
		return 0, fmt.Errorf("delete edges from=%q type=%q: %w", fromID, edgeType, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// assertEntityExists checks that an entity row with the given id exists
// inside the supplied transaction. Returns ErrMissingEntity wrapped with
// the offending id (and which side — `from` / `to`) when the row is
// missing; any other DB error is returned as-is.
func assertEntityExists(ctx context.Context, tx *sql.Tx, id, side string) error {
	var dummy int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE id = ?`, id).Scan(&dummy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s=%q", ErrMissingEntity, side, id)
	}
	if err != nil {
		return fmt.Errorf("check entity %q (%s): %w", id, side, err)
	}
	return nil
}

// Search performs a v1 substring match against the entities table. The
// query is matched (case-sensitively) against entity.id and the JSON
// representation of entity.data — `LIKE '%q%'` semantics. An optional
// kind filter narrows by entity.kind. limit/offset paginate; totalCount
// is the unfiltered match count so callers can compute pagination
// independent of how many rows the LIMIT cut.
//
// v1 limitations (deliberate, called out in ADR-0002 Open Question 1 +
// the dispatch):
// - LIKE substring, not FTS5 — no relevance ranking, no tokenisation,
// no fuzzy. Real ranking + snippet generation lands when FTS5 (or
// an external engine) is wired.
// - No escaping of `%` / `_` in query text — a user searching for
// "100%" gets wildcard semantics. Acceptable for the in-house v1
// fleet; flagged here so a future PR can add a sanitiser.
// - Score is a fixed placeholder (1.0) — there's nothing to rank.
// - Snippet is empty — extracting a substring around the match is
// cheap to add later, but the wire shape today is the same one
// the stub produced (Snippet field present, content placeholder).
func (s *sqliteStore) Search(ctx context.Context, query, kind string, limit, offset int, archived ArchivedFilter, journalOnly bool) ([]Hit, int, error) {
	pattern := "%" + query + "%"

	// Match on id, data, OR any of the entity's aliases per #3.
	// The EXISTS subquery keeps the row count clean — a LEFT JOIN
	// would duplicate rows when an entity carries multiple aliases
	// that each match the pattern.
	whereParts := []string{"(id LIKE ? OR data LIKE ? OR EXISTS (SELECT 1 FROM entity_aliases ea WHERE ea.entity_id = entities.id AND ea.alias LIKE ?))"}
	whereArgs := []any{pattern, pattern, pattern}
	if kind != "" {
		whereParts = append(whereParts, "kind = ?")
		whereArgs = append(whereArgs, kind)
	}
	// Archive-state filter per ADR-0018 step 2. ArchivedExclude is
	// the default-hide ("active only"); ArchivedInclude returns the
	// full set; ArchivedOnly returns the complement of the default.
	switch archived {
	case ArchivedExclude:
		whereParts = append(whereParts, "archived_at IS NULL")
	case ArchivedOnly:
		whereParts = append(whereParts, "archived_at IS NOT NULL")
	case ArchivedInclude:
		// no clause — full set
	}
	// is_journal filter per ADR-0025 cut 3 (#222). The flag lives
	// in vault frontmatter `data:` (mirrored verbatim to the DB
	// data column via vaultEntityDataForDB). SQLite JSON1's
	// json_extract returns the underlying type: YAML `true`
	// round-trips through Go bool → JSON `true` → json_extract
	// returns 1. Both spellings handled for defense-in-depth in
	// case a future writer emits the alternate JSON shape.
	if journalOnly {
		whereParts = append(whereParts,
			"(json_extract(data, '$.is_journal') = 1 OR json_extract(data, '$.is_journal') = 'true')")
	}
	where := strings.Join(whereParts, " AND ")

	// Total count first — the dispatch's "totalCount independence" rule
	// requires this number to ignore LIMIT/OFFSET.
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM entities WHERE "+where,
		whereArgs...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search matches: %w", err)
	}

	// SELECTing `data` on every search hit so the API layer can derive
	// snippets without a per-result GetEntity roundtrip. With LIMIT
	// (default 20, max 100) the worst-case bytes are bounded by the
	// page size — acceptable for v1 against single-tenant deployments.
	// Future optimisation paths if this becomes a bottleneck: (a) only
	// SELECT data when the caller actually needs snippets (would
	// require plumbing a flag from the API layer), or (b) generate
	// the snippet inside the search backend so the data column never
	// crosses the store boundary. Tracked as a follow-up.
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, kind, data FROM entities WHERE "+where+
			" ORDER BY id ASC LIMIT ? OFFSET ?",
		append(whereArgs, limit, offset)...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("query search results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]Hit, 0)
	for rows.Next() {
		var h Hit
		var dataBytes []byte
		if err := rows.Scan(&h.ID, &h.Kind, &dataBytes); err != nil {
			return nil, 0, fmt.Errorf("scan search row: %w", err)
		}
		if len(dataBytes) > 0 {
			if err := json.Unmarshal(dataBytes, &h.Data); err != nil {
				return nil, 0, fmt.Errorf("unmarshal data column for %s: %w", h.ID, err)
			}
		}
		// Snippet + Score are placeholders today; FTS5 will populate
		// them. Field present in the wire shape so callers don't see
		// the cutover in their JSON parsers.
		h.Score = 1.0
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate search rows: %w", err)
	}
	return hits, total, nil
}

func (s *sqliteStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
