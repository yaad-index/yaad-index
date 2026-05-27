// NameResolver wiring + ResolutionDeferred sentinel for #304
// Cut C2. The Service consumes a NameResolver to invoke a
// plugin's name-search primitive (the daemon's existing
// `<plugin>: <name>` shorthand ingest path); main.go shims
// api.SyncIngester into this interface so the edgewrite
// package doesn't import api (which already imports
// edgewrite — cycle risk).

package edgewrite

import (
	"context"
	"errors"
	"fmt"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

// NameResolver is the narrow plugin-resolution surface the
// centralized edge-write service depends on. Implementations
// invoke the named resolver plugin's `<plugin>: <name>`
// shorthand ingest path, returning either a single resolved
// canonical-kind entity id (the happy path) or the plugin's
// disambiguation Options map (the deferred path).
//
// targetKind is the canonical kind the workflow declared for
// the edge target (e.g. `boardgame`, `person`). The resolver
// MUST return an entity id of that kind, not a plugin-source
// id. Universal-source plugins (yaad-bgg, yaad-wikipedia, ...)
// materialize a source row alongside one or more canonical-
// edge targets; the implementation traverses the persisted
// canonical edges to surface the right `<targetKind>:...`
// entity for the requested kind. Plugins emitting an
// already-canonical-shape row return it directly.
//
// Returns:
//
//   - (entityID, nil, nil) on single-match resolution.
//     entityID has the `<targetKind>:` prefix; callers may rely
//     on the shape without re-checking.
//   - ("", options, nil) on disambiguation — Options is the
//     plugin's response map; the caller composes the
//     ResolutionDeferred sentinel from it.
//   - ("", nil, err) on transport / unresolvable failures.
//     Failures include "plugin ingest succeeded but produced
//     no canonical <targetKind> target" (source-shape plugin
//     that doesn't cover the requested kind).
//
// nil NameResolver wired into the Service degrades auto-mode
// to fall-through (every kind passes through to legacy
// CreateEdge regardless of resolver-map state); test fixtures
// don't have to wire a fake resolver unless they exercise
// auto-mode.
type NameResolver interface {
	ResolveCanonicalEntity(ctx context.Context, pluginName, targetKind, name string) (entityID string, options map[string]plugins.DisambiguationOption, err error)
}

// ResolutionDeferred is the sentinel error Service.
// CreateCanonicalEdgeByName returns when auto-mode resolution
// finds multiple candidates and Cut C3 should create a
// structured resolution-task. The struct carries every field
// Cut C3's task-creation path needs (edge tuple + resolver
// plugin name + raw target text + the plugin's options list)
// so the task builder doesn't have to re-derive any of it.
//
// Cut C2 only DEFINES the sentinel + wires its return path on
// the ambiguous branch. Cut C3 type-asserts via errors.As(err,
// &ResolutionDeferred{}) at the call site to extract the
// payload and create the task.
type ResolutionDeferred struct {
	// From is the source-edge entity id (the workflow's
	// triggering entity).
	From string

	// EdgeType is the edge type being written
	// (`mentions`, `designed_by`, etc.).
	EdgeType string

	// TargetKind is the canonical kind the workflow declared
	// for the target (`boardgame`, `person`, etc.).
	TargetKind string

	// RawTarget is the unresolved name the workflow passed
	// (`Brass`, `Susanna Clarke`, etc.). Cut C3's
	// idempotency key is computed from this string after
	// normalization.
	RawTarget string

	// ResolverPlugin is the plugin the Service routed
	// resolution to (the plugin that declared TargetKind in
	// its `resolves_canonical_kinds` Capability per Cut A).
	ResolverPlugin string

	// Options is the plugin's disambiguation map: key is the
	// candidate's canonical entity id, value is the
	// DisambiguationOption metadata (label + summary). Cut
	// C3 materializes one task line per entry.
	Options map[string]plugins.DisambiguationOption
}

// Error returns a one-line summary for logs + WARN paths.
// Callers shouldn't substring-match on this — type-assert via
// errors.As(err, &ResolutionDeferred{}) for structured access.
func (e *ResolutionDeferred) Error() string {
	return fmt.Sprintf(
		"edge resolution deferred: %s -[%s]-> %s:?%s (%d options via %s plugin)",
		e.From, e.EdgeType, e.TargetKind, e.RawTarget, len(e.Options), e.ResolverPlugin,
	)
}

// IsResolutionDeferred is a small helper that type-asserts
// err to *ResolutionDeferred. Returns (ptr, true) on match,
// (nil, false) otherwise. Cut C3's task builder uses this
// instead of `errors.As` boilerplate at each call site.
func IsResolutionDeferred(err error) (*ResolutionDeferred, bool) {
	var d *ResolutionDeferred
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}
