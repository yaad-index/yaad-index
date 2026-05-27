// canonical_edges.go centralises the daemon-side machinery for the
// ADR-0021 canonical-label edge model: thin-row materialization,
// `{name, kind}` / pre-formed-label parsing, and the post-write
// edge create/replace step. All three fill paths (operator-fill
// Per the prior design, agent-fill Per the prior design, UGC frontmatter-edges Per the prior design,)
// import these helpers — the contract was set by and reused
// verbatim by the others, so the helpers live in one place.
//
// The "(per yaad-index)" / "" provenance notes
// stay because that's where the contract was set; the file split
// is a structural reorg, not a contract change.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/slug"
	"github.com/yaad-index/yaad-index/internal/store"
)

// applyCanonicalTypeEdges runs the post-write edge create/replace
// step for canonical_type gap fills per yaad-index. Each
// canonical_type op produces a deterministic set of edges from
// the source entity to one canonical label per fill entry; the
// edge type is the gap-name (e.g. `subjects → boardgame:brass-…`,
// `subjects → person:martin-…`).
//
// For each canonical_type op:
//
// 1. DeleteEdgesByTypeFrom(source, gap-name) — wipes the prior
// fill's edges. Idempotent on first fill (zero rows removed).
// 2. For each label in the new fill:
// a. UpsertEntity (thin row: kind + id) when the label has
// no existing row, so CreateEdge's FK is satisfied. Skips
// when a row already exists (preserves prior data, e.g.
// operator-fill values from a different ct/-routed flow).
// b. CreateEdge with type=gap-name, from=source, to=label.
//
// Op kinds other than opSet (clear, defer, undefer) are no-ops
// here. opClear on a canonical_type gap is treated as "drop the
// list and any edges" — the prior-edge delete fires; no new
// edges are created. Defer / undefer are state-only on
// canonical_type gaps; no edge work.
//
// Errors propagate. CreateEdge's ErrMissingEntity should not
// fire because we pre-create thin rows; if it does anyway the
// caller surfaces 500 since the edge layer is partway-applied
// and the caller has no clean rollback.
//
// Eventbus emissions (ADR-0024 Phase 2): when bus is non-nil, this
// helper emits:
//
//   - entity.created on each thin label row that's materialized for
//     the first time (gated on the `created` return from
//     ensureCanonicalLabelRow so existing rows don't re-emit).
//   - entity.edge_added on each canonical-type edge created.
//
// The `source` tag flows through to both event types so subscribers
// can branch on agent-fill vs operator-fill (and future
// workflow-injected fills via Phase 4+). When bus is nil the helper
// is silent on emission — used by the UGC paths until UGC eventbus
// wiring lands as a follow-up.
//
// Per #154, when `pending` is non-nil the helper appends events
// to the queue rather than calling bus.Publish synchronously; the
// caller drains the queue AFTER releasing its per-entity write-
// lock. This avoids the synchronous-handler-takes-same-lock
// deadlock that the prior in-lock Publish shape produced. When
// pending is nil the helper falls back to immediate publish
// (callers outside any locked scope).
func applyCanonicalTypeEdges(
	ctx context.Context,
	st store.Store,
	edgeWriter edgewrite.EdgeWriter,
	sourceID string,
	ops []operatorFillOp,
	gaps map[string]config.GapSpec,
	logger *slog.Logger,
	bus eventbus.Bus,
	source eventbus.Source,
	pending *eventbus.PendingEvents,
) error {
	for _, op := range ops {
		spec, ok := gaps[op.Field]
		if !ok || spec.Type != config.CanonicalTypeName {
			continue
		}
		if op.Kind != opSet && op.Kind != opClear {
			// Defer / undefer ops don't touch edges; the gap-
			// state stamp in vault frontmatter records the
			// transition.
			continue
		}
		// Always wipe prior edges first — applies to opClear,
		// opSet (including the empty-list case per spec).
		if _, err := st.DeleteEdgesByTypeFrom(ctx, sourceID, op.Field); err != nil {
			return fmt.Errorf("delete prior edges (gap=%q): %w", op.Field, err)
		}
		if op.Kind == opClear {
			continue
		}
		entries, ok := op.Value.([]canonicalLabelEntry)
		if !ok {
			return fmt.Errorf("canonical_type op %q: expected []canonicalLabelEntry value, got %T", op.Field, op.Value)
		}
		for _, e := range entries {
			label := e.ID
			created, err := ensureCanonicalLabelRow(ctx, st, label, logger)
			if err != nil {
				return fmt.Errorf("ensure label row %q: %w", label, err)
			}
			if created && bus != nil {
				// Thin canonical-label row materialized for the
				// first time on this fill — emit per ADR-0024
				// Phase 2. SplitLabelID was already validated
				// inside EnsureLabelRow, so the kind extraction
				// here can't fail. CausedByEntityID = sourceID:
				// the entity being filled is the actor whose
				// fill drove the materialization.
				kind, _, _ := splitCanonicalLabelID(label)
				eventbus.QueueOrPublish(ctx, bus, pending, eventbus.EntityCreatedEvent{
					ID:               label,
					Kind:             kind,
					SourceTag:        source,
					At:               time.Now().UTC(),
					Chain:            eventbus.WorkflowChainFromContext(ctx),
					CausedByEntityID: sourceID,
				})
			}
			if err := edgeWriter.CreateEdge(ctx, &store.Edge{
				Type: op.Field,
				From: sourceID,
				To:   label,
			}); err != nil {
				return fmt.Errorf("create edge %s -[%s]-> %s: %w",
					sourceID, op.Field, label, err)
			}
			if bus != nil {
				// One entity.edge_added per canonical-label edge
				// created — workflow subscribers commonly trigger
				// on these to surface "this entity got tagged as
				// X" downstream effects.
				eventbus.QueueOrPublish(ctx, bus, pending, eventbus.EntityEdgeAddedEvent{
					FromID:           sourceID,
					ToID:             label,
					EdgeType:         op.Field,
					SourceTag:        source,
					At:               time.Now().UTC(),
					Chain:            eventbus.WorkflowChainFromContext(ctx),
					CausedByEntityID: sourceID,
				})
			}
		}
	}
	return nil
}

// canonicalLabelEntryIDs extracts just the IDs from a slice of
// canonicalLabelEntry. Frontmatter `data[<field>]` stores label
// IDs only — the per-entry `Data` payload lands as a dataview
// paragraph on the target canonical entity, not in the source
// entity's frontmatter, per yaad-index #119.
func canonicalLabelEntryIDs(v any) []string {
	entries, ok := v.([]canonicalLabelEntry)
	if !ok {
		return nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// appendDataviewParagraphs records each canonical_type entry's
// `data` map as a dataview-inline paragraph on the target
// canonical entity's body per yaad-index #119. Iterates the api-
// internal operatorFillOp slice and delegates the per-entry
// vault read-merge-write to canonical.AppendDataviewParagraph
// so the same auto-materialize + dedup logic serves the agent /
// operator fill paths here AND the workflow-action
// add_canonical_edge primitive (#132).
//
// Errors on any single paragraph fail the whole call so the
// caller surfaces a consistent state — partial-success would
// leave the source entity claiming edges to labels whose
// dataview never recorded.
//
// Per-paragraph fill.completed event publication stays here
// (rather than in canonical.AppendDataviewParagraph) because
// the SourceTag varies by caller: SourceAgent for fill,
// SourceOperator for operator-fill. The workflow action
// surfaces a workflow:<name> source tag through its own
// publish path.
//
// Per #154 the helper accepts a `*eventbus.PendingEvents`
// queue: when non-nil the fill.completed event is appended for
// publish-after-unlock; when nil it falls through to immediate
// bus.Publish (pre-#154 callers outside any locked scope).
func appendDataviewParagraphs(
	ctx context.Context,
	deps canonical.DataviewAppendDeps,
	ops []operatorFillOp,
	source eventbus.Source,
	sourceWorkflow string,
	pending *eventbus.PendingEvents,
) error {
	if deps.VaultReader == nil || deps.VaultWriter == nil || deps.WriteLocks == nil {
		return nil
	}
	now := time.Now().UTC()
	for _, op := range ops {
		if op.Kind != opSet {
			continue
		}
		entries, ok := op.Value.([]canonicalLabelEntry)
		if !ok {
			continue
		}
		for _, e := range entries {
			if len(e.Data) == 0 {
				continue
			}
			appended, err := canonical.AppendDataviewParagraph(
				ctx, deps, e.ID,
				canonical.StringifyMap(e.Data),
				op.Field, sourceWorkflow,
			)
			if err != nil {
				return fmt.Errorf("append dataview paragraph on %s (gap=%q): %w", e.ID, op.Field, err)
			}
			if appended && deps.Bus != nil {
				eventbus.QueueOrPublish(ctx, deps.Bus, pending, eventbus.FillCompletedEvent{
					EntityID:  e.ID,
					Gap:       op.Field,
					SourceTag: source,
					At:        now,
					Chain:     eventbus.WorkflowChainFromContext(ctx),
				})
			}
		}
	}
	return nil
}

// ensureCanonicalLabelRow / splitCanonicalLabelID delegate to
// internal/canonical so reindex (which can't import internal/api
// without a cycle) shares the same implementation as the fill
// paths. The local thin wrappers keep call sites inside this
// package readable while making the cross-package extract obvious.

func ensureCanonicalLabelRow(ctx context.Context, st store.Store, label string, logger *slog.Logger) (bool, error) {
	return canonical.EnsureLabelRow(ctx, st, label, logger)
}

func splitCanonicalLabelID(id string) (kind, slug string, ok bool) {
	return canonical.SplitLabelID(id)
}

// parseCanonicalLabelList decodes a `canonical_type` gap fill per
// yaad-index. Two element shapes coexist:
//
// - Object `{"name": "Brass Pittsburgh", "kind": "boardgame"}` —
// daemon slugifies via `slug.Slug(name)` to produce the
// canonical-label id `<kind>:<slug>`. Accepted on every fill
// path (operator, agent, UGC).
// - String `"boardgame:brass-pittsburgh"` — pre-formed canonical
// label. Daemon extracts the kind from the prefix and accepts
// the slug as-is. Accepted on operator-fill and UGC
// create/edit — both operator-authored. Rejected on
// agent-fill since the agent is not the operator.
//
// Each element's kind is validated against the resolution set:
// when `gap.Kinds == ["*"]`, the registry's full canonical_kinds;
// otherwise the explicit gap.Kinds list. Unknown kinds reject
// with 400.
//
// Empty list `[]` is a valid fill — transitions the gap to
// filled state with no edges (per spec §Edge cases).
//
// Returns the canonical-label ids in the order they appeared in
// the request. The caller (apply phase) stores the list in
// `ve.Data[field]` and the post-write phase creates edges.
func parseCanonicalLabelList(
	field string,
	raw json.RawMessage,
	gap config.GapSpec,
	operatorAllKinds []string,
	allowPreformedLabels bool,
) ([]canonicalLabelEntry, *opError) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, &opError{
			status: http.StatusBadRequest,
			code: "type_mismatch",
			message: fmt.Sprintf("field %q: expected canonical_type list (`[{name, kind}, ...]`%s), got %v",
				field,
				preformedHint(allowPreformedLabels),
				stringEllipsis(string(raw), 60),
			),
		}
	}
	resolution := canonicalKindResolution(gap, operatorAllKinds)
	out := make([]canonicalLabelEntry, 0, len(entries))
	for i, entry := range entries {
		labelEntry, perr := parseCanonicalLabelEntry(field, i, entry, resolution, allowPreformedLabels)
		if perr != nil {
			return nil, perr
		}
		out = append(out, labelEntry)
	}
	return out, nil
}

// preformedHint produces the informational suffix for the
// "expected canonical_type list" error message — names the second
// accepted shape when the path accepts pre-formed labels.
func preformedHint(allowPreformedLabels bool) string {
	if allowPreformedLabels {
		return ` or ["<kind>:<slug>", ...]`
	}
	return ""
}

// canonicalKindResolution returns the set of canonical kinds an
// individual fill entry's `kind` must belong to. When the gap
// declared `kinds: "*"` the resolution is the operator's full
// `canonical_kinds` registry; otherwise it's the explicit gap
// allowlist.
func canonicalKindResolution(gap config.GapSpec, operatorAllKinds []string) map[string]struct{} {
	src := gap.Kinds
	if len(gap.Kinds) == 1 && gap.Kinds[0] == config.CanonicalTypeWildcard {
		src = operatorAllKinds
	}
	out := make(map[string]struct{}, len(src))
	for _, k := range src {
		out[k] = struct{}{}
	}
	return out
}

// parseCanonicalLabelEntry resolves one element of a
// canonical_type fill list to a canonical-label id. Branches on
// the JSON shape (object vs string) and runs the appropriate
// validation for each.
func parseCanonicalLabelEntry(
	field string,
	index int,
	raw json.RawMessage,
	resolution map[string]struct{},
	allowPreformedLabels bool,
) (canonicalLabelEntry, *opError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return canonicalLabelEntry{}, &opError{
			status: http.StatusBadRequest,
			code: "type_mismatch",
			message: fmt.Sprintf("field %q[%d]: empty value", field, index),
		}
	}
	switch trimmed[0] {
	case '"':
		// Pre-formed canonical-label string. Operator/UGC paths only.
		// String form does NOT carry data — only the object form
		// `{name, kind, data}` accepts the dataview payload.
		if !allowPreformedLabels {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: pre-formed canonical-label string only accepted on operator-fill (got %s); use {name, kind} instead",
					field, index, stringEllipsis(string(raw), 40)),
			}
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: decode string: %v", field, index, err),
			}
		}
		kind, slugPart, ok := splitCanonicalLabelID(s)
		if !ok {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "invalid_canonical_label",
				message: fmt.Sprintf("field %q[%d]: %q is not a valid `<kind>:<slug>` canonical label", field, index, s),
			}
		}
		if _, ok := resolution[kind]; !ok {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "kind_not_allowed",
				message: fmt.Sprintf("field %q[%d]: kind %q not in the gap's resolution set", field, index, kind),
			}
		}
		return canonicalLabelEntry{ID: kind + ":" + slugPart}, nil
	case '{':
		var ref canonicalRef
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ref); err != nil {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: %v", field, index, err),
			}
		}
		if ref.Name == "" || ref.Kind == "" {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: object form requires non-empty `name` AND `kind`", field, index),
			}
		}
		if _, ok := resolution[ref.Kind]; !ok {
			return canonicalLabelEntry{}, &opError{
				status: http.StatusBadRequest,
				code: "kind_not_allowed",
				message: fmt.Sprintf("field %q[%d]: kind %q not in the gap's resolution set", field, index, ref.Kind),
			}
		}
		return canonicalLabelEntry{
			ID:   ref.Kind + ":" + slug.Slug(ref.Name),
			Data: ref.Data,
		}, nil
	default:
		return canonicalLabelEntry{}, &opError{
			status: http.StatusBadRequest,
			code: "type_mismatch",
			message: fmt.Sprintf("field %q[%d]: expected {name, kind} object%s, got %s",
				field, index,
				preformedHint(allowPreformedLabels),
				stringEllipsis(string(raw), 40)),
		}
	}
}

// canonicalRef is the wire shape for one entry in the object form
// of a canonical_type fill: `{"name": "...", "kind": "...",
// "data": {...}}`. The `data` field per yaad-index #119 is the
// optional free-form metadata map appended as a dataview-inline
// paragraph on the target canonical entity's body.
// DisallowUnknownFields rejects extra keys so typo-driven fills
// don't silently land malformed values.
type canonicalRef struct {
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data,omitempty"`
}

// canonicalLabelEntry is the per-element output of
// parseCanonicalLabelList. The ID is the resolved canonical-
// label id (`<kind>:<slug>`); Data is the optional free-form
// payload to record as a dataview paragraph on the target
// per yaad-index #119. Pre-formed-label string entries
// (operator-fill / UGC paths) always emit empty Data — only
// the object form `{name, kind, data}` carries it.
type canonicalLabelEntry struct {
	ID   string
	Data map[string]any
}

// stringEllipsis returns s with a `…` appended after `max` runes
// when the string exceeds that length. Used in error messages to
// keep request-body fragments short while preserving enough
// surface for operators to recognize the offending input.
func stringEllipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// checkCanonicalTypeResolverPlugins walks the canonical_type
// fill ops and, for each entry whose target kind has a
// `resolver_plugin:` config set, requires the canonical id to
// already exist in the store. Returns a non-nil opError on the
// first unresolved-target violation with HTTP 422
// `unresolved_target` + a suggested-action hint naming the
// resolver plugin.
//
// Per #276:
//
//   - Kinds without `resolver_plugin:` set fall through to the
//     existing auto-materialize path — agents / operators can
//     introduce new entries freely.
//   - Kinds with `resolver_plugin:` set require pre-existence:
//     the agent should have ingested through the named plugin
//     first; only the resolver plugin's emit path can introduce
//     new canonical-id values for the kind.
//   - The plugin-emit edge path (bgg → is_about → boardgame:X)
//     is unaffected — this helper runs only on fill paths
//     (POST /v1/entities/{id}/fill, /operator-fill).
//
// Caller-side opt-out: operator-fill can pass
// `allowUnresolved = true` to short-circuit the check (audit-
// stamped at the caller). Agent-fill never opts out.
func checkCanonicalTypeResolverPlugins(
	ctx context.Context,
	st store.Store,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	ops []operatorFillOp,
	allowUnresolved bool,
) *opError {
	if allowUnresolved {
		return nil
	}
	for _, op := range ops {
		if op.Kind != opSet {
			continue
		}
		entries, ok := op.Value.([]canonicalLabelEntry)
		if !ok {
			continue
		}
		for _, e := range entries {
			kind, _, ok := splitCanonicalLabelID(e.ID)
			if !ok {
				continue
			}
			cfg, ok := canonicalKindReg[kind]
			if !ok {
				continue
			}
			if cfg.ResolverPlugin == "" {
				continue
			}
			if _, err := st.GetEntity(ctx, e.ID); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return &opError{
					status:  http.StatusInternalServerError,
					code:    "internal_error",
					message: fmt.Sprintf("probe target %q: %v", e.ID, err),
				}
			}
			return &opError{
				status: http.StatusUnprocessableEntity,
				code:   "unresolved_target",
				message: fmt.Sprintf(
					"field %q: target %q has resolver_plugin=%q but no entity exists; ingest via the %q plugin first then re-run the fill",
					op.Field, e.ID, cfg.ResolverPlugin, cfg.ResolverPlugin,
				),
			}
		}
	}
	return nil
}
