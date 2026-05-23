// Daemon-built-in canonical infrastructure per ADR-0025: the `day`
// entity kind and the five canonical edge types that describe
// time-bound relationships. These are always available; operators
// don't enable them via canonical_kinds: / canonical_edge_types: in
// config. The constants live in the leaf `canonical` package so
// every layer that needs them (config guard wiring, /v1/kinds
// surface, future shape-scan + workflow validation) can import
// without pulling in heavier dependencies.
//
// ADR-0025 cut 1 (foundation) introduces these as advertised
// vocabulary only — no code path emits the edges yet. Cut 2 wires
// the daemon shape-scan + plugin date_fields capability that
// actually materializes them.

package canonical

import "github.com/yaad-index/yaad-index/internal/config"

// DayKind is the canonical entity kind for date-anchored entities
// per ADR-0025. Slug shape is `day:<YYYY-MM-DD>` (e.g.
// `day:2026-11-11`). The `<YYYY-MM-DD>` portion is the day in the
// daemon's configured timezone (per ADR-0025 § Timezone).
const DayKind = "day"

// Canonical edge type names per ADR-0025 § Edge types. The
// vocabulary is fixed at the daemon level; plugins may use these
// for portability or declare their own edge types in their
// --init date_fields capability for richer domain semantics.
const (
	// EdgeTypeDueOn connects a task / deadline entity to the day
	// entity it is due on.
	EdgeTypeDueOn = "due_on"

	// EdgeTypeOccurredOn connects an event / meeting / shipment
	// entity to the day it happened or will happen.
	EdgeTypeOccurredOn = "occurred_on"

	// EdgeTypeIsAboutDay connects a newsletter / digest / journal
	// entry to the day it describes.
	EdgeTypeIsAboutDay = "is_about_day"

	// EdgeTypeReferencesDay is the fallback baseline emitted by the
	// daemon shape-scan for any `day:`-shaped frontmatter reference
	// whose field isn't declared in a plugin's `date_fields`. Semantically
	// generic — "this entity refers to this day for some reason."
	EdgeTypeReferencesDay = "references_day"

	// EdgeTypeIngestedOn connects an entity to the day yaad-index
	// first received it. Reserved in the canonical vocabulary for
	// the operator-wired `entity.created → add_canonical_edge`
	// workflow per ADR-0025 §"ingested_on auto-tag — deferred"; the
	// daemon itself never emits this edge in v1.x.
	EdgeTypeIngestedOn = "ingested_on"
)

// DaemonEntityKinds returns the canonical entity kinds the daemon
// always allows, regardless of operator config. Currently just the
// `day` kind per ADR-0025 cut 1; week / month / year are deferred.
// Caller-side guards (config.CanonicalGuard, /v1/kinds aggregator)
// fold this into their effective set.
//
// Returns a fresh slice; callers may mutate freely.
func DaemonEntityKinds() []string {
	return []string{DayKind}
}

// DaemonEdgeTypes returns the canonical edge type names the daemon
// always allows, regardless of operator config. The five names
// landed by ADR-0025 cut 1 cover the time-bound relationships the
// design space anticipates; plugins may emit additional edge type
// names via their --init date_fields capability for domain-specific
// vocabulary that doesn't map onto the canonical baseline.
//
// Returns a fresh slice; callers may mutate freely.
func DaemonEdgeTypes() []string {
	return []string{
		EdgeTypeDueOn,
		EdgeTypeOccurredOn,
		EdgeTypeIsAboutDay,
		EdgeTypeReferencesDay,
		EdgeTypeIngestedOn,
	}
}

// NewGuardWithDaemonDefaults wraps config.NewCanonicalGuard, folding
// the daemon-built-in entity kinds + edge types into the effective
// allow-set before construction. Use at every guard-construction
// site so daemon-managed canonical infrastructure (the `day` kind,
// the five canonical edge types) is always permitted — operators
// don't need to list them in canonical_kinds: / canonical_edge_types:
// to make them work.
//
// Operator-supplied entries that overlap with the daemon set are
// de-duped at the config.CanonicalGuard layer (which uses a set).
// Empty / nil operator slices are fine — the daemon entries still
// land.
func NewGuardWithDaemonDefaults(operatorKinds, operatorEdges []string) *config.CanonicalGuard {
	kinds := make([]string, 0, len(operatorKinds)+1)
	kinds = append(kinds, operatorKinds...)
	kinds = append(kinds, DaemonEntityKinds()...)
	edges := make([]string, 0, len(operatorEdges)+len(DaemonEdgeTypes()))
	edges = append(edges, operatorEdges...)
	edges = append(edges, DaemonEdgeTypes()...)
	return config.NewCanonicalGuard(kinds, edges)
}
