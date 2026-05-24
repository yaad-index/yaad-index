// Dataview-paragraph-append on canonical-label targets per yaad-
// index #119 and the auto-materialize policy per ADR-0021 §3.
// Shared between the api-side canonical_type fill paths (agent +
// operator) and the workflow-action add_canonical_edge primitive
// (#132). Extracted to a leaf package so both call sites
// reuse one implementation; live in `canonical` next to the
// existing label-id + thin-row-ensure helpers.

package canonical

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// CanonicalLabelPlugin is the sentinel plugin name stamped on
// auto-materialized canonical-label vault files. Distinct from
// any real plugin name so the frontmatter signals
// "operator-authored canonical metadata" at a glance.
const CanonicalLabelPlugin = "operator-fill"

// DataviewAppendDeps bundles the dependencies
// AppendDataviewParagraph needs so the per-call signature stays
// narrow. Production wires this from the daemon's shared
// services bag; tests can substitute fakes for each field
// individually.
type DataviewAppendDeps struct {
	Store       store.Store
	VaultReader *vault.Reader
	VaultWriter *vault.Writer
	WriteLocks  *writelocks.Manager
	KindReg     map[string]config.CanonicalKindConfig
	Bus         eventbus.Bus
	Logger      *slog.Logger
}

// AppendDataviewParagraph appends one dataview-inline paragraph
// onto the target canonical-label's vault file body, between
// the `<!-- yaad:dataview start/end -->` markers per ADR-0015.
//
// Per-call flow:
//
//  1. Acquire write-lock on targetID.
//  2. Read targetID's vault file; if missing AND data is
//     non-empty, build a fresh canonical-label entity for the
//     target kind via NewCanonicalLabelEntity — the substantive
//     structured data is the "honest content to attach"
//     trigger per ADR-0021 §3.
//  3. Compute the candidate paragraph (sorted keys per
//     vault.RenderDataviewParagraph) and dedup by content-hash
//     against existing paragraphs. Skip silently if identical.
//  4. Write back via WriteCanonicalLabelWithCommit.
//  5. ensureLabelRow mirrors the thin DB row so cross-package
//     consumers (search, edge queries) see the entity.
//
// Returns appended=true when a new paragraph landed; false
// when the call deduped or skipped. err non-nil on any vault /
// store / write-lock failure.
//
// DB-only deploys (VaultReader/VaultWriter/WriteLocks nil) skip
// silently — same shape as the fill / operator-fill handlers
// in internal/api. The caller branches on appended to decide
// whether to publish downstream fill.completed events.
//
// sourceWorkflow names the originating workflow when this
// append fires from a workflow action; empty when fired from
// agent/operator fill (the api package callers). Surfaces in
// the commit message and write-lock holder for audit traces.
func AppendDataviewParagraph(
	ctx context.Context,
	deps DataviewAppendDeps,
	targetID string,
	data map[string]string,
	gapField, sourceWorkflow string,
) (appended bool, err error) {
	if len(data) == 0 {
		return false, nil
	}
	if deps.VaultReader == nil || deps.VaultWriter == nil || deps.WriteLocks == nil {
		return false, nil
	}

	holder := "dataview-append"
	if sourceWorkflow != "" {
		holder = "workflow:" + sourceWorkflow + " [dataview-append]"
	}
	release, err := deps.WriteLocks.Acquire(targetID, holder)
	if err != nil {
		return false, fmt.Errorf("acquire write-lock: %w", err)
	}
	defer release()

	kind, _, ok := SplitLabelID(targetID)
	if !ok {
		return false, fmt.Errorf("invalid canonical label id %q", targetID)
	}

	ve, readErr := deps.VaultReader.ReadByID(kind, targetID)
	if readErr != nil {
		if !vault.IsNotExist(readErr) {
			return false, fmt.Errorf("vault read: %w", readErr)
		}
		// Auto-materialize per ADR-0021 §3.
		kindCfg, kindKnown := deps.KindReg[kind]
		if !kindKnown {
			return false, fmt.Errorf("target kind %q not in canonical_kinds; cannot auto-materialize", kind)
		}
		ve = NewCanonicalLabelEntity(targetID, kind, kindCfg)
	}

	candidate := vault.DataviewParagraph{Fields: data}
	candidateWire := vault.RenderDataviewParagraph(candidate)
	for _, p := range ve.Dataview {
		if vault.RenderDataviewParagraph(p) == candidateWire {
			return false, nil
		}
	}
	ve.Dataview = append(ve.Dataview, candidate)

	commitMsg := fmt.Sprintf("dataview-append on %s (gap=%s)", targetID, gapField)
	commitAuthor := "agent"
	if sourceWorkflow != "" {
		commitAuthor = "workflow:" + sourceWorkflow
	}
	if writeErr := deps.VaultWriter.WriteCanonicalLabelWithCommit(ctx, ve, commitMsg, commitAuthor); writeErr != nil {
		return false, fmt.Errorf("vault write: %w", writeErr)
	}

	if _, ensureErr := EnsureLabelRow(ctx, deps.Store, targetID, deps.Logger); ensureErr != nil {
		return false, fmt.Errorf("ensure label row: %w", ensureErr)
	}
	return true, nil
}

// NewCanonicalLabelEntity builds a fresh vault.Entity for an
// auto-materialized canonical-label vault file. The frontmatter
// starts empty (no operator-set fields yet); the kind config's
// declared gaps populate the entity's Gaps slice so the file
// surfaces in the operator's needs-fill listings if any gaps
// are operator-strategy.
func NewCanonicalLabelEntity(id, kind string, kindCfg config.CanonicalKindConfig) *vault.Entity {
	gaps := make([]string, 0, len(kindCfg.Gaps))
	for name := range kindCfg.Gaps {
		gaps = append(gaps, name)
	}
	sort.Strings(gaps)
	return &vault.Entity{
		ID:     id,
		Kind:   kind,
		// Per ADR-0028 §5 slash-form: a canonical-label entity
		// sourced from operator-fill carries the sentinel
		// plugin under the implicit `default` instance — there
		// is no operator-config-side instance for the synthetic
		// operator-fill emitter.
		Source: []string{CanonicalLabelPlugin + "/default"},
		Data:   map[string]any{},
		Gaps:   gaps,
	}
}

// StringifyMap converts a json.Unmarshal-derived map[string]any
// (the wire shape for the per-entry `data` payload on
// canonical_type fills) into the flat map[string]string the
// dataview paragraph stores. Non-string values render via
// fmt.Sprintf("%v") so numerics + nested maps round-trip as
// their string form (best-effort stringification; agents emit
// string-typed values when render fidelity matters).
func StringifyMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}
