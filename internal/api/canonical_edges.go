// canonical_edges.go centralises the daemon-side machinery for the
// ADR-0021 canonical-label edge model: thin-row materialization,
// `{name, kind}` / pre-formed-label parsing, and the post-write
// edge create/replace step. All three fill paths (operator-fill
// Per the prior design, agent-fill Per the prior design, UGC frontmatter-edges Per the prior design,)
// import these helpers — the contract was set by and reused
// verbatim by the others, so the helpers live in one place.
//
// The "(per yaad-index)" / "" provenance comments
// stay because that's where the contract was set; the file split
// is a structural reorg, not a contract change.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/config"
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
func applyCanonicalTypeEdges(
	ctx context.Context,
	st store.Store,
	sourceID string,
	ops []operatorFillOp,
	gaps map[string]config.GapSpec,
	logger *slog.Logger,
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
		labels, ok := op.Value.([]string)
		if !ok {
			return fmt.Errorf("canonical_type op %q: expected []string value, got %T", op.Field, op.Value)
		}
		for _, label := range labels {
			if err := ensureCanonicalLabelRow(ctx, st, label, logger); err != nil {
				return fmt.Errorf("ensure label row %q: %w", label, err)
			}
			if err := st.CreateEdge(ctx, &store.Edge{
				Type: op.Field,
				From: sourceID,
				To: label,
			}); err != nil {
				return fmt.Errorf("create edge %s -[%s]-> %s: %w",
					sourceID, op.Field, label, err)
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

func ensureCanonicalLabelRow(ctx context.Context, st store.Store, label string, logger *slog.Logger) error {
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
) ([]string, *opError) {
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
	out := make([]string, 0, len(entries))
	for i, entry := range entries {
		label, perr := parseCanonicalLabelEntry(field, i, entry, resolution, allowPreformedLabels)
		if perr != nil {
			return nil, perr
		}
		out = append(out, label)
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
) (string, *opError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", &opError{
			status: http.StatusBadRequest,
			code: "type_mismatch",
			message: fmt.Sprintf("field %q[%d]: empty value", field, index),
		}
	}
	switch trimmed[0] {
	case '"':
		// Pre-formed canonical-label string. Operator/UGC paths only.
		if !allowPreformedLabels {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: pre-formed canonical-label string only accepted on operator-fill (got %s); use {name, kind} instead",
					field, index, stringEllipsis(string(raw), 40)),
			}
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: decode string: %v", field, index, err),
			}
		}
		kind, slugPart, ok := splitCanonicalLabelID(s)
		if !ok {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "invalid_canonical_label",
				message: fmt.Sprintf("field %q[%d]: %q is not a valid `<kind>:<slug>` canonical label", field, index, s),
			}
		}
		if _, ok := resolution[kind]; !ok {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "kind_not_allowed",
				message: fmt.Sprintf("field %q[%d]: kind %q not in the gap's resolution set", field, index, kind),
			}
		}
		return kind + ":" + slugPart, nil
	case '{':
		var ref canonicalRef
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ref); err != nil {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: %v", field, index, err),
			}
		}
		if ref.Name == "" || ref.Kind == "" {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "type_mismatch",
				message: fmt.Sprintf("field %q[%d]: object form requires non-empty `name` AND `kind`", field, index),
			}
		}
		if _, ok := resolution[ref.Kind]; !ok {
			return "", &opError{
				status: http.StatusBadRequest,
				code: "kind_not_allowed",
				message: fmt.Sprintf("field %q[%d]: kind %q not in the gap's resolution set", field, index, ref.Kind),
			}
		}
		return ref.Kind + ":" + slug.Slug(ref.Name), nil
	default:
		return "", &opError{
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
// of a canonical_type fill: `{"name": "...", "kind": "..."}`.
// DisallowUnknownFields rejects extra keys so typo-driven fills
// don't silently land malformed values. The same `{name, kind}`
// shape appears uniformly across plugin emissions (ADR-0021's
// universal-source-shape edges block) and fill paths.
type canonicalRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
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
