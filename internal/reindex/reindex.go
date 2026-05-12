// Package reindex walks a markdown vault and (re)builds the derived
// SQLite index from its files. Implements the body of the
// `yaad-index reindex` CLI subcommand and the POST /v1/reindex HTTP
// endpoint declared in ADR-0008.
//
// Two modes:
//
// - **Incremental** (default): list every `*.md` under the vault root,
// compare each file's (mtime, content_hash) against the per-file
// bookkeeping in store.ListReindexFiles, and only re-parse files
// whose signal has changed. Files in bookkeeping that no longer
// exist on disk produce a cascading entity delete.
// - **Full**: drop every entity, edge, provenance, and reindex_files
// row in a single transaction (store.WipeDerivedState), then walk
// and parse every file as if it were new.
//
// The walker is single-goroutine and synchronous; vaults at personal
// scale (thousands of files) finish in well under a second on a warm
// filesystem and the file I/O dominates over CPU. A future PR can
// parallelize parsing if benchmarks justify it.
package reindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// Mode selects the reindex strategy. Zero value is Incremental.
type Mode int

const (
	// Incremental rebuilds only files whose mtime or content hash
	// changed since the last walk. New files are added; deleted files
	// trigger entity removal.
	Incremental Mode = iota

	// Full drops the derived state and rebuilds from scratch. Use
	// after a schema change or when bookkeeping has drifted.
	Full
)

func (m Mode) String() string {
	switch m {
	case Full:
		return "full"
	default:
		return "incremental"
	}
}

// Summary is the per-run accounting returned by Run. The HTTP handler
// JSON-encodes it directly; the CLI prints it line-by-line. Counts
// are non-negative; fields with no activity stay zero.
type Summary struct {
	Mode string `json:"mode"`
	Scanned int `json:"scanned"` // total *.md files seen on disk
	Skipped int `json:"skipped"` // unchanged since last walk
	Parsed int `json:"parsed"` // re-parsed and upserted
	EntitiesCreated int `json:"entities_created"` // new bookkeeping rows
	EntitiesUpdated int `json:"entities_updated"` // existing bookkeeping rows whose entity_id was touched
	EntitiesDeleted int `json:"entities_deleted"` // file-disappeared cascade deletes
	EdgeRowsWritten int `json:"edge_rows_written"` // CreateEdge invocations across all parsed files
	Errors []string `json:"errors,omitempty"` // parse or upsert errors; non-fatal — the walk continues
	StartedAt string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMillis int64 `json:"duration_ms"`
}

// Reindexer encapsulates the dependencies and config of one walk path.
// Construct via New; reuse across calls if desired (Run is safe for
// sequential reuse but does not hold internal mutable state across
// calls).
type Reindexer struct {
	store store.Store
	vaultRoot string
	reader *vault.Reader
	guard *config.CanonicalGuard
	logger *slog.Logger
}

// New constructs a Reindexer rooted at vaultRoot. The root must be an
// absolute path to an existing directory — same rules as
// vault.NewReader. The store interface is the destination for derived
// state.
//
// guard, when non-nil, is consulted at edge-write time to drop edges
// whose type is not in the operator's `canonical_edge_types:` list and
// to gate canonical-label thin-row materialization by `canonical_kinds:`.
// nil guard preserves the legacy permissive behavior — every edge in
// the vault file lands in the DB without filtering, which is the right
// shape for tests and dev binaries that don't load operator config.
//
// logger is consulted for the per-edge drop / auto-materialize debug
// lines. nil logger silences them; the reindex Summary still
// accumulates errors.
func New(st store.Store, vaultRoot string, guard *config.CanonicalGuard, logger *slog.Logger) (*Reindexer, error) {
	r, err := vault.NewReader(vaultRoot)
	if err != nil {
		return nil, fmt.Errorf("init vault reader: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Reindexer{store: st, vaultRoot: vaultRoot, reader: r, guard: guard, logger: logger}, nil
}

// Run executes one reindex pass and returns its Summary. Errors that
// halt the walk (filesystem unreachable, store wipe failed) are
// returned as a non-nil err; per-file parse / upsert errors are
// captured in Summary.Errors and the walk continues.
func (r *Reindexer) Run(ctx context.Context, mode Mode) (Summary, error) {
	start := time.Now()
	summary := Summary{
		Mode: mode.String(),
		StartedAt: start.UTC().Format(time.RFC3339Nano),
	}

	if mode == Full {
		if err := r.store.WipeDerivedState(ctx); err != nil {
			return summary, fmt.Errorf("wipe derived state: %w", err)
		}
	}

	priorRows, err := r.store.ListReindexFiles(ctx)
	if err != nil {
		return summary, fmt.Errorf("list reindex bookkeeping: %w", err)
	}
	prior := make(map[string]store.ReindexFile, len(priorRows))
	for _, f := range priorRows {
		prior[f.Path] = f
	}
	seen := make(map[string]struct{}, len(prior))

	walkErr := filepath.WalkDir(r.vaultRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("walk %s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			// Skip the archive subtree per ADR-0018 step 2 —
			// archived entities live in DB rows but their vault
			// files don't get re-walked (the archived state is
			// authoritative on the DB side; reindex incrementally
			// reconciles only the active set).
			if d.Name() == "_archive" && filepath.Dir(path) == r.vaultRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			// hidden / temp files (writer's `.<slug>.md.tmp-*`) — skip
			return nil
		}
		summary.Scanned++
		seen[path] = struct{}{}

		info, statErr := d.Info()
		if statErr != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("stat %s: %v", path, statErr))
			return nil
		}
		mtime := info.ModTime().UTC()

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("read %s: %v", path, readErr))
			return nil
		}
		hash := hashContent(body)

		// Incremental skip: same path, same mtime, same hash → no
		// parse, no upsert. Full mode wiped bookkeeping above so this
		// branch never matches there.
		if existing, ok := prior[path]; ok && existing.Mtime.Equal(mtime) && existing.ContentHash == hash {
			summary.Skipped++
			return nil
		}

		entity, parseErr := vault.Unmarshal(body)
		if parseErr != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("parse %s: %v", path, parseErr))
			return nil
		}

		if upErr := r.upsertEntity(ctx, entity); upErr != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("upsert %s: %v", path, upErr))
			return nil
		}

		// Edge writes use delete-then-create per (source, edge_type)
		// tuple so reindex is idempotent across re-runs. Mirrors the
		// canonical_type fill path's `applyCanonicalTypeEdges` shape.
		// Forward-reference edges (target row not yet upserted in this
		// walk) get auto-materialized as thin canonical-label rows
		// before CreateEdge so the FK on the edges table holds.
		// Operator-config gates apply: edge types not in
		// `canonical_edge_types:` and target kinds not in
		// `canonical_kinds:` (with source-type bypass) drop with a
		// debug log instead of a CreateEdge failure.
		written, edgeErrs := r.applyVaultEdges(ctx, entity)
		summary.EdgeRowsWritten += written
		summary.Errors = append(summary.Errors, edgeErrs...)

		row := store.ReindexFile{
			Path: path,
			Mtime: mtime,
			ContentHash: hash,
			LastIndexedAt: time.Now().UTC(),
			EntityID: entity.ID,
			EntityKind: entity.Kind,
		}
		if bookErr := r.store.UpsertReindexFile(ctx, row); bookErr != nil {
			summary.Errors = append(summary.Errors, fmt.Sprintf("bookkeep %s: %v", path, bookErr))
			return nil
		}
		summary.Parsed++
		if _, was := prior[path]; was {
			summary.EntitiesUpdated++
		} else {
			summary.EntitiesCreated++
		}
		return nil
	})

	// Disappeared-file pass: anything that was in bookkeeping last
	// time but didn't show up on disk this time gets a cascade delete.
	// Full mode skips this — bookkeeping is empty after the wipe.
	if mode != Full {
		for path, f := range prior {
			if _, ok := seen[path]; ok {
				continue
			}
			if err := r.store.DeleteEntityCascade(ctx, f.EntityID); err != nil && !errors.Is(err, store.ErrNotFound) {
				summary.Errors = append(summary.Errors, fmt.Sprintf("delete entity %s (path %s): %v", f.EntityID, path, err))
				continue
			}
			if _, err := r.store.DeleteReindexFile(ctx, path); err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("delete bookkeeping %s: %v", path, err))
				continue
			}
			summary.EntitiesDeleted++
		}
	}

	end := time.Now()
	summary.FinishedAt = end.UTC().Format(time.RFC3339Nano)
	summary.DurationMillis = end.Sub(start).Milliseconds()

	if walkErr != nil {
		return summary, fmt.Errorf("walk %s: %w", r.vaultRoot, walkErr)
	}
	return summary, nil
}

// applyVaultEdges reconstitutes the DB edge graph for one source
// entity from its vault `edges:` block. Idempotent: bucket the
// vault edges by edge_type and DeleteEdgesByTypeFrom before
// CreateEdge so re-running reindex on the same vault produces the
// same DB state instead of duplicating rows or surfacing the
// "edge already exists" path.
//
// Per (source, edge_type) tuple in the vault:
//
// 1. AllowEdgeType gate. Drop the whole bucket when the operator's
// `canonical_edge_types:` config doesn't include the type;
// bump the per-(plugin, edge_type) drop counter so /v1/cv-status
// surfaces the drop. Reindex doesn't know which plugin
// originally emitted the edge, so the counter rolls up under
// a synthetic "reindex" provenance bucket.
// 2. DeleteEdgesByTypeFrom on the (source, edge_type) — wipes any
// prior edges of this type from the DB before writing the
// vault-canonical set.
// 3. For each vault entry in the bucket: ensureCanonicalLabelRow
// to satisfy FK; CreateEdge.
//
// nil guard skips the gate (legacy permissive path).
func (r *Reindexer) applyVaultEdges(ctx context.Context, entity *vault.Entity) (written int, errs []string) {
	if len(entity.Edges) == 0 {
		// Idempotency guarantee: a vault file with no `edges:` block
		// reading as nil means the source has no plugin-emitted
		// edges. We do NOT delete prior DB edges here — the absence
		// of an edges block on a vault file legacy is back-compat
		// silence, NOT an authoritative "drop everything" signal.
		// Backfill mode is the explicit path to repopulate those.
		return 0, nil
	}

	buckets := make(map[string][]vault.Edge, len(entity.Edges))
	for _, e := range entity.Edges {
		if e.Type == "" || e.To == "" {
			continue
		}
		buckets[e.Type] = append(buckets[e.Type], e)
	}

	// Sort bucket keys so the apply order is deterministic — useful
	// for tests + for grep'ing the debug log when investigating drops.
	types := make([]string, 0, len(buckets))
	for t := range buckets {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, edgeType := range types {
		bucket := buckets[edgeType]

		if r.guard != nil && !r.guard.AllowEdgeType(edgeType) {
			r.logger.Debug("reindex: edge type not in operator config — bucket skipped",
				"id", entity.ID, "type", edgeType, "count", len(bucket))
			if err := r.store.IncDroppedCanonicalEdge(ctx, "reindex", edgeType); err != nil {
				r.logger.Warn("reindex: IncDroppedCanonicalEdge (best-effort)",
					"err", err, "type", edgeType)
			}
			continue
		}

		if _, err := r.store.DeleteEdgesByTypeFrom(ctx, entity.ID, edgeType); err != nil {
			errs = append(errs,
				fmt.Sprintf("delete prior edges %s [%s]: %v", entity.ID, edgeType, err))
			continue
		}

		for _, ve := range bucket {
			kind, _, ok := canonical.SplitLabelID(ve.To)
			if !ok {
				errs = append(errs,
					fmt.Sprintf("malformed edge target %s [%s] -> %q", entity.ID, edgeType, ve.To))
				continue
			}
			// Auto-materialize-thin-row + AllowKind gate only fire on
			// the guarded path. Without a guard we don't know which
			// `<prefix>:<slug>` ids are canonical-label endpoints
			// (eligible for thin-row materialize) vs source-shape ids
			// (which must already exist as full entity rows from their
			// own vault file). Permissive nil-guard mode preserves the
			// legacy behavior: surface ErrMissingEntity from
			// CreateEdge for forward references and let the next walk
			// resolve them once both endpoints have landed.
			if r.guard != nil {
				if kind != canonical.SourceTypeKind && !r.guard.AllowKind(kind) {
					r.logger.Debug("reindex: edge target kind not in operator config — edge skipped",
						"id", entity.ID, "type", edgeType, "to", ve.To, "kind", kind)
					if err := r.store.IncDroppedCanonicalKind(ctx, "reindex", kind); err != nil {
						r.logger.Warn("reindex: IncDroppedCanonicalKind (best-effort)",
							"err", err, "kind", kind)
					}
					continue
				}
				if err := canonical.EnsureLabelRow(ctx, r.store, ve.To, r.logger); err != nil {
					errs = append(errs,
						fmt.Sprintf("ensure label row %s [%s] -> %s: %v", entity.ID, edgeType, ve.To, err))
					continue
				}
			}
			se := &store.Edge{Type: edgeType, From: entity.ID, To: ve.To, Metadata: ve.Metadata}
			if err := r.store.CreateEdge(ctx, se); err != nil {
				errs = append(errs,
					fmt.Sprintf("edge %s -[%s]-> %s: %v", entity.ID, edgeType, ve.To, err))
				continue
			}
			written++
		}
	}
	return written, errs
}

// upsertEntity writes the parsed entity into the store. Edges are
// handled separately by the caller so a forward-reference edge
// failure doesn't block the entity-row write — the entity itself
// landed cleanly even when its outbound edges can't be resolved
// yet.
//
// Provenance: the vault file's frontmatter `provenance:` list is
// canonical (per ADR-0009). After upserting the entity row we
// reconcile the DB-side provenance to the vault list via
// ReplaceProvenance — DELETE prior + INSERT new in one tx, atomic
// per call. Reindex's two-step (UpsertEntity, ReplaceProvenance)
// is NOT wrapped in an enclosing tx; per ADR-0009's race section
// the sequential-failure window is tolerable because the next
// reindex pass re-runs both and the vault stays authoritative.
func (r *Reindexer) upsertEntity(ctx context.Context, e *vault.Entity) error {
	se := &store.Entity{
		ID: e.ID,
		Kind: e.Kind,
		Data: e.Data,
	}
	if err := r.store.UpsertEntity(ctx, se); err != nil {
		return fmt.Errorf("UpsertEntity %s: %w", e.ID, err)
	}
	if err := r.store.ReplaceProvenance(ctx, e.ID, vaultProvenanceToStore(e.Provenance)); err != nil {
		return fmt.Errorf("ReplaceProvenance %s: %w", e.ID, err)
	}
	// Notations cache (per yaad-index the source issue a prior PR). The vault is
	// the canonical source for the entity_notations table — reindex
	// reconciles the DB to the vault frontmatter `notations:` list,
	// dropping orphaned rows the vault no longer carries. Same
	// DELETE+INSERT shape as ReplaceProvenance per ADR-0009.
	if err := r.store.ReplaceNotations(ctx, e.ID, vaultNotationsToStore(e.ID, e.Notations)); err != nil {
		return fmt.Errorf("ReplaceNotations %s: %w", e.ID, err)
	}
	return nil
}

// vaultNotationsToStore converts the vault-frontmatter notations
// list ([]string) into the store-layer Notation slice. Each entry
// pins the entity_id to the vault file's owning entity; kind defaults
// to NotationKindURL — the vault frontmatter doesn't carry per-row
// kind metadata in v1, and the URL discriminator is the schema
// default. A future PR can grow the frontmatter to a list-of-objects
// shape when shorthand-vs-url distinction matters at reindex.
func vaultNotationsToStore(entityID string, in []string) []store.Notation {
	if len(in) == 0 {
		return nil
	}
	out := make([]store.Notation, len(in))
	for i, n := range in {
		out[i] = store.Notation{
			Notation: n,
			EntityID: entityID,
			Kind: store.NotationKindURL,
		}
	}
	return out
}

// vaultProvenanceToStore converts the vault-frontmatter provenance
// shape into the store-layer shape. The two structs are field-
// identical; only the package boundary differs (vault has YAML
// tags, store doesn't). A nil input returns nil so ReplaceProvenance
// receives the "drop all provenance for this entity" signal cleanly
// when a vault file has no `provenance:` block.
func vaultProvenanceToStore(in []vault.ProvenanceEntry) []store.ProvenanceEntry {
	if in == nil {
		return nil
	}
	out := make([]store.ProvenanceEntry, len(in))
	for i, p := range in {
		out[i] = store.ProvenanceEntry{
			Source: p.Source,
			FetchedAt: p.FetchedAt,
			FilledAt: p.FilledAt,
			OK: p.OK,
			Error: p.Error,
			ErrorMessage: p.ErrorMessage,
		}
	}
	return out
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
