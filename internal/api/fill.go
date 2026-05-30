package api

import (
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/vault"
)

// #355 Cut 2: the standalone fill handler that lived here is gone.
// /v1/entities/{id}/fill now routes through handleEntityOperatorFill
// (the unified handler per ADR-0029); /v1/entities/{id}/operator-fill
// returns 410 gone. The response shapes + the vault→DB projection
// helpers stay because they're consumed by operator_fill.go and the
// ingest tests.

// fillResponse is the 200 envelope on a successful fill. `gaps`
// surfaces the remaining unfilled gap field names from the vault
// frontmatter post-write — empty list when this call closed every
// open gap, otherwise the caller can chain another partial fill
// without first re-fetching via GET /v1/entities/{id}. Same shape
// as operatorFillResponse — the unified endpoint emits this envelope
// regardless of which case fired.
//
// Always non-nil — JSON-encodes as `[]` rather than `null` so
// callers have a stable schema (no presence vs. emptiness ambiguity).
type fillResponse struct {
	OK     bool     `json:"ok"`
	Entity entity   `json:"entity"`
	Gaps   []string `json:"gaps"`
}

// fillConflictResponse is the legacy 409 envelope for fields rejected
// as already-filled. Retained for tests that decode the old shape;
// the unified handler emits ADR-0029's already_filled (overwrite) /
// unknown_field (ad-hoc) / agent_only_field (strategy) shapes via
// writeError instead.
type fillConflictResponse struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
	Message  string   `json:"message"`
	Rejected []string `json:"rejected"`
}

// vaultEntityDataForDB projects a vault entity into the data map the
// store sees. Top-level vault fields that the DB tracks (summary,
// tags, notes) are folded into data so that GET /v1/entities/{id}
// returns them via the entity data field — preserving the existing
// wire shape.
//
// `notes_text` is a derived concatenation of every note's Text
// joined by newlines — stored under that key so the DB-side
// LIKE-on-data search can find note content. The actual note list
// (with date + author) stays in the vault file's frontmatter, which
// is the canonical source.
func vaultEntityDataForDB(e *vault.Entity) map[string]any {
	out := make(map[string]any, len(e.Data)+3)
	for k, v := range e.Data {
		out[k] = v
	}
	if e.Summary != "" {
		out["summary"] = e.Summary
	}
	if len(e.Tags) > 0 {
		out["tags"] = e.Tags
	}
	if len(e.Notes) > 0 {
		parts := make([]string, 0, len(e.Notes))
		for _, c := range e.Notes {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			out["notes_text"] = strings.Join(parts, "\n")
		}
	}
	return out
}

// fmt is referenced indirectly via callers; keep import alive for
// future helpers without a stray underscore. _ ensures the package
// builds even if no helper in this file uses it directly.
var _ = fmt.Sprintf