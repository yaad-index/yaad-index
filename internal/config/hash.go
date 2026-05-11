// Deterministic hash over the canonical-vocabulary subset of the
// config (per ADR-0013 §3 / alice2-index a prior PR). Surfaced as the
// `config_hash` field on `/v1/cv-status` so operator tooling can
// detect when the canonical_kinds / canonical_edge_types config
// changed between calls — agents and CI can poll, diff the hash,
// and decide whether a reindex is needed.
//
// Distinct from the `version` hash on `/v1/structure`: that one
// covers the full structural signature (kinds + edge_types +
// plugins + discrim). cv-status only needs the canonical subset
// — a plugin upgrade or a new plugin loading doesn't change
// drift state if the canonical_kinds config is unchanged.

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// ConfigHash produces the canonical-vocabulary config hash
// surfaced by `/v1/cv-status` (per ADR-0013 §3 / alice2-index).
//
// SHA-256 over a canonical JSON serialization of:
//
// - canonical_kinds map (sorted-by-key Marshal — Go's
// encoding/json walks map keys in sorted order, deterministic
// across calls).
// - canonical_edge_types slice — caller-sorted before hashing
// so an operator reorder of the YAML list does NOT bump the
// hash. Matches `/v1/structure`'s `version` contract from
// a prior PR (alice2-index) so the two observability surfaces
// agree on what counts as a config change. The orchestrator
// already treats edge_types as a set (dedupe-via-map at
// lookup time), so the YAML order has no semantic load — the
// two-surface consistency is the right invariant.
// - A discrim sentinel binding the schema-version: "alice2-index/
// cv-config-v1". Hand-bumped when the canonForm layout
// changes.
//
// Output truncated to the first 16 hex chars matching the
// `/v1/structure` `version` field's truncation. Keeps both hashes
// the same width for operator-side string handling — callers that
// rely on `len(h) == 16` can do so safely on the success path.
//
// Returns `(string, error)` rather than a single sentinel-encoded
// string so a Marshal failure surfaces explicitly rather than
// hiding inside an error-shaped hex string that callers might
// `len`-check past (the cold-reviewer's a prior PR catch). In practice Marshal
// never fails on the canonForm shape — every field is plain
// `string`, `map[string]string`, or `[]string` — so the error
// path is defense-in-depth; a prior PR's handler propagates it as a
// 500 internal_error.
func ConfigHash(canonicalKinds map[string]CanonicalKindConfig, canonicalEdgeTypes []string) (string, error) {
	type canonForm struct {
		Kinds map[string]CanonicalKindConfig `json:"canonical_kinds"`
		EdgeTypes []string `json:"canonical_edge_types"`
		// Discrim is hand-bumped when the canonForm layout changes
		// so existing client-side caches deliberately invalidate
		// across the bump.
		Discrim string `json:"discrim"`
	}
	// Normalize nil → empty so absent-key and empty-block configs
	// hash identically (operator-tooling treats them as
	// observationally equivalent — a polled cv-status hash should
	// not flicker just because a YAML edit toggled `{}` vs the
	// key being commented out).
	if canonicalKinds == nil {
		canonicalKinds = map[string]CanonicalKindConfig{}
	}
	if canonicalEdgeTypes == nil {
		canonicalEdgeTypes = []string{}
	}
	// Sort a defensive copy so the caller's slice ordering is
	// preserved upstream; only the hashed view sees the canonical
	// (sorted) order. Matches `/v1/structure`'s pre-hash sort
	// (alice2's a prior PR review note + a prior PR contract).
	sortedEdges := make([]string, len(canonicalEdgeTypes))
	copy(sortedEdges, canonicalEdgeTypes)
	sort.Strings(sortedEdges)
	body, err := json.Marshal(canonForm{
		Kinds: canonicalKinds,
		EdgeTypes: sortedEdges,
		Discrim: "alice2-index/cv-config-v1",
	})
	if err != nil {
		return "", fmt.Errorf("ConfigHash: marshal canonical-vocabulary form: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16], nil
}
