// Day-reference shape-scan + reference-creation per ADR-0025 cut 2
// (#221). Walks an entity's frontmatter for `day:YYYY-MM-DD`-shaped
// canonical-ID strings, ensures the target day entity exists, and
// emits a canonical edge from the source entity to the day entity.
//
// The emitted edge type is `references_day` by default; plugins MAY
// override per frontmatter field via their --init Capabilities
// `date_fields` declaration (mapping field-name → canonical edge
// type, e.g. `due_on` / `occurred_on` / `is_about_day`).

package canonical

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"

	"github.com/yaad-index/yaad-index/internal/store"
)

// DayRefStore is the narrow store surface EmitDayRefs and
// EnsureDayEntity depend on. Decoupled from store.Store so
// workflow-action callers (whose backend uses a tighter
// EntityStore interface) can pass their store without picking
// up the full store.Store surface. The production
// implementation is *sqliteStore (which satisfies store.Store
// and therefore this interface trivially); test fakes
// implement just these three methods.
type DayRefStore interface {
	GetEntity(ctx context.Context, id string) (*store.Entity, error)
	UpsertEntity(ctx context.Context, e *store.Entity) error
	CreateEdge(ctx context.Context, e *store.Edge) error
}

// DayRefEdgeWriter is the narrow edge-create surface the
// EmitDayRefs helper uses per #304 Cut C1. Decoupled from
// DayRefStore so callers can pass the centralized
// edgewrite.Service (production) without dragging the broader
// store.Store interface in.
type DayRefEdgeWriter interface {
	CreateEdge(ctx context.Context, e *store.Edge) error
}

// DayIDPrefix is the canonical-ID prefix for day entities per
// ADR-0025. The full ID shape is `day:YYYY-MM-DD`.
const DayIDPrefix = "day:"

// dayIDPattern matches a canonical day id at the shape level.
// Strict: `day:` + exactly `YYYY-MM-DD`. Calendar validity (e.g.
// month in [01,12]) is NOT enforced here — that would create a
// silent-drop path for ingest, and the day entity is purely an
// anchor whose slug is the string the operator typed. Operators
// who type a malformed date get a malformed day entity; the
// anchor still works.
var dayIDPattern = regexp.MustCompile(`^day:(\d{4}-\d{2}-\d{2})$`)

// ParseDayID returns the date slug (YYYY-MM-DD) from a canonical
// day-id string and ok=true. Returns ok=false for any non-matching
// shape — empty string, missing prefix, malformed date.
func ParseDayID(s string) (slug string, ok bool) {
	m := dayIDPattern.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// DayRef pairs a frontmatter field name with the day-id it points
// to. The shape-scan returns a slice of these in deterministic
// (field-then-id) order so edge writes are reproducible across runs.
type DayRef struct {
	Field string
	DayID string
}

// ScanDayRefs walks the top-level keys of an entity's data map and
// returns every (field, day-id) pair whose value is a string
// matching the `day:YYYY-MM-DD` shape. Non-string values are
// skipped; nested maps / slices are NOT walked in cut 2 (deferred
// until a use case surfaces).
//
// Result is sorted by field-name then day-id for deterministic
// edge-emit order; callers downstream rely on this for
// reproducibility across re-ingests.
func ScanDayRefs(data map[string]any) []DayRef {
	if len(data) == 0 {
		return nil
	}
	var refs []DayRef
	for field, value := range data {
		s, ok := value.(string)
		if !ok {
			continue
		}
		if _, ok := ParseDayID(s); ok {
			refs = append(refs, DayRef{Field: field, DayID: s})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Field != refs[j].Field {
			return refs[i].Field < refs[j].Field
		}
		return refs[i].DayID < refs[j].DayID
	})
	return refs
}

// ResolveDayEdgeType returns the canonical edge type to emit for
// the (field, dateFields) pair. When the plugin has declared the
// field in its `date_fields` capability, the declared edge type
// wins. Otherwise the baseline `references_day` is returned.
//
// Empty declared edge type (a plugin author bug — should not
// happen if the capability schema rejects empty values) is
// treated as undeclared so we fall back to the baseline rather
// than emitting an empty-type edge.
func ResolveDayEdgeType(field string, dateFields map[string]string) string {
	if edgeType, ok := dateFields[field]; ok && edgeType != "" {
		return edgeType
	}
	return EdgeTypeReferencesDay
}

// EnsureDayEntity is the day-side wrapper over ensureLabelRowFor.
// The target row is created if missing; otherwise the call is a
// no-op (idempotent). Returns an error only on a malformed day-id
// or a store-layer failure.
func EnsureDayEntity(ctx context.Context, st DayRefStore, dayID string, logger *slog.Logger) error {
	if _, ok := ParseDayID(dayID); !ok {
		return fmt.Errorf("malformed day id %q", dayID)
	}
	return ensureLabelRowFor(ctx, st, dayID, logger)
}

// ensureLabelRowFor mirrors EnsureLabelRow's contract but takes
// the narrower DayRefStore interface. Kept private so callers
// outside this file route through EnsureDayEntity (which adds the
// shape gate) or the EnsureLabelRow public surface (full
// store.Store, used by the canonical-label paths).
func ensureLabelRowFor(ctx context.Context, st DayRefStore, label string, logger *slog.Logger) error {
	kind, _, ok := SplitLabelID(label)
	if !ok {
		return fmt.Errorf("malformed canonical-label id %q", label)
	}
	_, err := st.GetEntity(ctx, label)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("probe %q: %w", label, err)
	}
	if err := st.UpsertEntity(ctx, &store.Entity{ID: label, Kind: kind}); err != nil {
		return fmt.Errorf("upsert thin row %q: %w", label, err)
	}
	if logger != nil {
		logger.Debug("auto-materialized thin canonical-label row", "id", label, "kind", kind)
	}
	return nil
}

// EmitDayRefs scans the entity's data, ensures each referenced day
// entity exists, and emits the appropriate canonical edge (declared
// edge type when plugin overrides, baseline `references_day`
// otherwise). The source entity MUST already exist in the store
// before this is invoked — the edge FK constraint depends on it.
//
// Per-reference failures (ensure-day errors, edge-upsert errors)
// log at WARN and continue so a single malformed day reference or
// transient store hiccup doesn't block the rest of the entity's
// commit. The source entity itself is the caller's responsibility;
// this helper only sweeps the day side.
//
// Returns the number of edges emitted (created or no-op-on-existing)
// across all references — purely informational for operator
// debugging.
func EmitDayRefs(
	ctx context.Context,
	st DayRefStore,
	edgeWriter DayRefEdgeWriter,
	sourceID string,
	data map[string]any,
	dateFields map[string]string,
	logger *slog.Logger,
) int {
	if edgeWriter == nil {
		// Back-compat: default to the store's CreateEdge when no
		// explicit edge writer is supplied. Production paths pass
		// a centralized edgewrite.Service so Cut C2 + C3 routing
		// applies; tests that don't care about routing leave it
		// nil and fall through to the legacy direct-store shape.
		edgeWriter = st
	}
	refs := ScanDayRefs(data)
	emitted := 0
	for _, ref := range refs {
		if err := EnsureDayEntity(ctx, st, ref.DayID, logger); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "day shape-scan: ensure day entity",
					"source", sourceID, "field", ref.Field, "day_id", ref.DayID, "err", err)
			}
			continue
		}
		edgeType := ResolveDayEdgeType(ref.Field, dateFields)
		edge := &store.Edge{Type: edgeType, From: sourceID, To: ref.DayID}
		if err := edgeWriter.CreateEdge(ctx, edge); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "day shape-scan: emit edge",
					"source", sourceID, "field", ref.Field,
					"edge_type", edgeType, "day_id", ref.DayID, "err", err)
			}
			continue
		}
		emitted++
	}
	return emitted
}
