package reindex

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// CheckReport is the per-run accounting returned by Check. It tallies
// the three vault↔store divergence classes #455 reports on, each
// mapping to a recently-fixed bug:
//
//   - StaleDayRefEdges (#446): the store carries a day-targeting edge
//     from an entity whose CURRENT vault frontmatter no longer declares
//     it.
//   - CascadeStrippedEdges (#447): the entity's vault `edges:` block
//     declares an edge the store is MISSING.
//   - AliasMismatches (#445): an alias's stored Kind differs from what
//     the vault frontmatter implies.
//
// The Details* slices carry a human-scannable `<entityID> -> <what>`
// line per counted divergence for the operator-facing report; the
// counts are the load-bearing fields (Total drives the CLI exit code).
type CheckReport struct {
	StaleDayRefEdges     int `json:"stale_day_ref_edges"`
	CascadeStrippedEdges int `json:"cascade_stripped_edges"`
	AliasMismatches      int `json:"alias_mismatches"`

	DetailsStaleDayRef     []string `json:"details_stale_day_ref,omitempty"`
	DetailsCascadeStripped []string `json:"details_cascade_stripped,omitempty"`
	DetailsAliasMismatch   []string `json:"details_alias_mismatch,omitempty"`

	// Scanned is the number of *.md vault files compared, for context
	// in the report (a zero-divergence report over zero files reads
	// differently from one over a populated vault).
	Scanned int `json:"scanned"`

	// Errors collects per-file read/parse/store-probe failures. Like
	// Run's Summary.Errors these are non-fatal — the walk continues —
	// but a non-empty list means the report's counts may be incomplete.
	Errors []string `json:"errors,omitempty"`
}

// Total is the sum of the three divergence counts. The CLI exits
// non-zero iff Total > 0.
func (c CheckReport) Total() int {
	return c.StaleDayRefEdges + c.CascadeStrippedEdges + c.AliasMismatches
}

// Check is the read-only dry-run companion to Run (#455). It walks the
// vault exactly as Run does — every `*.md` under vaultRoot, skipping the
// `_archive` subtree and hidden/temp files — but instead of writing the
// derived store it COMPARES each entity's current vault frontmatter
// against the store and tallies the three #455 divergence classes into a
// CheckReport. Check NEVER mutates the store: it issues only read calls
// (GetEdgesFor, ListAliasesForEntity), so an operator can run it on a
// live index to detect drift without rebuilding.
//
// Per entity the comparison is:
//
//  1. Stale day-ref edges (#446): the store's day-targeting edges
//     (those whose `.To` parses as a `day:` id) whose target is NOT in
//     the set ScanDayRefs derives from the entity's current frontmatter.
//  2. Cascade-stripped edges (#447): the entity's vault-declared edges
//     (`e.Edges`) absent from the store's edge set for the entity. With
//     a nil guard every vault edge should be present; a non-nil guard's
//     dropped edge types are excluded so the comparison matches what a
//     reindex would actually have written.
//  3. Alias mismatches (#445): aliases present in BOTH the vault-implied
//     set (vault.MergedAliasesFor + TypedAliasEntries, the same
//     derivation reindex/ingest use) and the store whose Kind differs.
func (r *Reindexer) Check(ctx context.Context) (CheckReport, error) {
	var report CheckReport

	// canonicalKinds selects the title field MergedAliasesFor's
	// synthesize step reads (source-shape → data.title, canonical-shape
	// → data.name). Source it from the guard's enabled kinds, the same
	// place canonical.MirrorAliases (#445) gets it; nil guard → nil set
	// (every kind treated source-shape), the permissive default.
	canonicalKinds := r.guard.EnabledKinds()

	walkErr := filepath.WalkDir(r.vaultRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("walk %s: %v", path, err))
			return nil
		}
		if d.IsDir() {
			if d.Name() == "_archive" && filepath.Dir(path) == r.vaultRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("read %s: %v", path, readErr))
			return nil
		}
		entity, parseErr := vault.Unmarshal(body)
		if parseErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("parse %s: %v", path, parseErr))
			return nil
		}
		report.Scanned++

		// Read the entity's full store edge set once; both edge-class
		// comparisons consume it.
		storeEdges, edgeErr := r.store.GetEdgesFor(ctx, entity.ID, nil)
		if edgeErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("get edges %s: %v", entity.ID, edgeErr))
			return nil
		}

		r.checkStaleDayRefs(entity, storeEdges, &report)
		r.checkCascadeStripped(entity, storeEdges, &report)

		if aliasErr := r.checkAliasMismatches(ctx, entity, canonicalKinds, &report); aliasErr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("list aliases %s: %v", entity.ID, aliasErr))
		}
		return nil
	})

	if walkErr != nil {
		return report, fmt.Errorf("walk %s: %w", r.vaultRoot, walkErr)
	}
	return report, nil
}

// checkStaleDayRefs counts store day-targeting edges (`.To` parses as a
// `day:` id) whose target is not in the entity's current frontmatter
// day-references (#446).
func (r *Reindexer) checkStaleDayRefs(entity *vault.Entity, storeEdges []store.Edge, report *CheckReport) {
	expected := make(map[string]struct{})
	for _, ref := range canonical.ScanDayRefs(entity.Data) {
		expected[ref.DayID] = struct{}{}
	}
	for _, e := range storeEdges {
		if _, ok := canonical.ParseDayID(e.To); !ok {
			continue
		}
		if _, want := expected[e.To]; want {
			continue
		}
		report.StaleDayRefEdges++
		report.DetailsStaleDayRef = append(report.DetailsStaleDayRef,
			fmt.Sprintf("%s -> %s:%s", entity.ID, e.Type, e.To))
	}
}

// checkCascadeStripped counts vault-declared edges (e.Edges) absent from
// the store's edge set for the entity (#447). A non-nil guard's
// dropped edge types are skipped so the comparison reflects what a
// reindex would have written, not the raw frontmatter.
func (r *Reindexer) checkCascadeStripped(entity *vault.Entity, storeEdges []store.Edge, report *CheckReport) {
	type edgeKey struct{ typ, to string }
	have := make(map[edgeKey]struct{}, len(storeEdges))
	for _, e := range storeEdges {
		have[edgeKey{e.Type, e.To}] = struct{}{}
	}
	for _, ve := range entity.Edges {
		if ve.Type == "" || ve.To == "" {
			continue
		}
		// Mirror applyVaultEdges' guard: an edge type the operator's
		// config drops would never have been written, so it's not a
		// stripped-edge divergence. nil guard → AllowEdgeType is moot
		// (every edge counted).
		if r.guard != nil && !r.guard.AllowEdgeType(ve.Type) {
			continue
		}
		if _, ok := have[edgeKey{ve.Type, ve.To}]; ok {
			continue
		}
		report.CascadeStrippedEdges++
		report.DetailsCascadeStripped = append(report.DetailsCascadeStripped,
			fmt.Sprintf("%s -> %s:%s", entity.ID, ve.Type, ve.To))
	}
}

// checkAliasMismatches counts aliases present in BOTH the vault-implied
// set and the store whose Kind differs (#445). The vault-implied set is
// MergedAliasesFor (title-synthesized + plugin entries) classified via
// TypedAliasEntries with the operator's canonicalEdgeTypes — the same
// derivation reindex's upsert and ingest use.
func (r *Reindexer) checkAliasMismatches(ctx context.Context, entity *vault.Entity, canonicalKinds []string, report *CheckReport) error {
	merged := vault.MergedAliasesFor(entity.ID, entity.Kind, entity.Data, entity.Aliases, canonicalKinds)
	expectedKind := make(map[string]string, len(merged))
	for _, a := range store.TypedAliasEntries(entity.ID, merged, r.canonicalEdgeTypes) {
		expectedKind[a.Alias] = a.Kind
	}

	actual, err := r.store.ListAliasesForEntity(ctx, entity.ID)
	if err != nil {
		return err
	}
	for _, a := range actual {
		want, both := expectedKind[a.Alias]
		if !both {
			continue
		}
		if a.Kind == want {
			continue
		}
		report.AliasMismatches++
		report.DetailsAliasMismatch = append(report.DetailsAliasMismatch,
			fmt.Sprintf("%s -> %s (store=%s want=%s)", entity.ID, a.Alias, a.Kind, want))
	}
	return nil
}
