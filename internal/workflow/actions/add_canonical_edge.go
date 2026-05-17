// add_canonical_edge runner — the deterministic-fill counterpart
// to add_gap for canonical_type gaps per #132. The workflow
// declares the canonical-label target inline (CEL-rendered name +
// literal kind); the runner resolves the target id, hands it to
// the EdgeWriter, and the writer creates the canonical-label edge
// + (when data is non-empty) appends a dataview-inline paragraph
// on the target canonical entity per #119. No agent round-trip,
// no fill-gap detour.
//
// **Constraint enforcement.** The parser validates non-empty
// fields + non-empty data keys/values at workflow-load time
// (validateAddCanonicalEdge). The loader additionally validates
// EdgeType against the daemon's canonical_edge_types allowlist
// and TargetKind against canonical_kinds; both are operator-
// configured registries available at load time. Defense in depth:
// the runner re-checks empty fields at fire time so a future
// dynamically-constructed action list can't bypass the parser
// path.

package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// EdgeWriter is the canonical-edge creation surface the
// add_canonical_edge runner depends on. Production wires a
// vault-backed implementation (VaultEdgeWriter) that slugifies
// the target name, creates the canonical-label edge from the
// source entity, and (when data is non-empty) auto-materializes
// the target canonical entity + appends a dataview-inline
// paragraph per #119. Tests wire an in-memory fake.
//
// Idempotency contract:
//
//   - Same (sourceID, edgeType, targetKind, targetName) tuple
//     fired twice does NOT duplicate the edge.
//   - Same tuple with identical `data` is a no-op on the
//     dataview-append side (sorted-key content-hash dedup per
//     #119); different `data` accumulates as a new paragraph
//     (history-as-event-log).
type EdgeWriter interface {
	AddCanonicalEdge(
		ctx context.Context,
		workflow, sourceID, edgeType, targetKind, targetName string,
		data map[string]string,
	) error
}

// runAddCanonicalEdge executes one add_canonical_edge action:
// resolves the source entity id + rendered target name + each
// rendered data value, then invokes the EdgeWriter.
func (d *dispatcher) runAddCanonicalEdge(ctx context.Context, idx int, _ *parser.Workflow, a *parser.AddCanonicalEdgeAction, dec Decision, act Activation) ActionResult {
	if d.edgeWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("add_canonical_edge: no EdgeWriter wired (engine constructed without actions.Options.EdgeWriter)"),
		}
	}

	edgeType := strings.TrimSpace(a.EdgeType)
	if edgeType == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("%w: add_canonical_edge.edge_type is empty", ErrActionAuthorBug),
		}
	}
	targetKind := strings.TrimSpace(a.TargetKind)
	if targetKind == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("%w: add_canonical_edge.target.kind is empty", ErrActionAuthorBug),
		}
	}

	// Source resolution: prefer the engine's rendered Source
	// (or the raw action.Source as a fallback when no renderer
	// is wired), then default to the triggering entity's id
	// when neither is set.
	source := strings.TrimSpace(d.rendered(act, idx, "source", a.Source))
	if source == "" {
		source = dec.EntityID
	}
	if source == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("%w: add_canonical_edge has no source (action.source empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	// Target name renders from CEL; empty after render is an
	// author bug (the source data didn't carry what the workflow
	// claimed it would). The parser already enforces non-empty
	// expr text at load time; this catches the runtime case.
	targetName := strings.TrimSpace(d.rendered(act, idx, "target.name", a.TargetName))
	if targetName == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("%w: add_canonical_edge.target.name rendered empty (CEL expr produced no value)", ErrActionAuthorBug),
		}
	}

	// Render each data value via the engine's pre-rendered map.
	// Empty rendered values DROP from the map — a per-event
	// field like "salary" rendered to "" means the source didn't
	// carry the field, not that it's an empty string. Same shape
	// as set_property's per-field handling, except set_property
	// preserves empties because the workflow author asked for the
	// literal "".
	var data map[string]string
	if len(a.Data) > 0 {
		data = make(map[string]string, len(a.Data))
		for name, expr := range a.Data {
			val := strings.TrimSpace(d.rendered(act, idx, "data:"+name, expr))
			if val == "" {
				continue
			}
			data[name] = val
		}
	}

	if err := d.edgeWriter.AddCanonicalEdge(ctx, dec.Workflow, source, edgeType, targetKind, targetName, data); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "add_canonical_edge",
			Err:       fmt.Errorf("add_canonical_edge: %w", err),
		}
	}
	return ActionResult{ActionIdx: idx, Type: "add_canonical_edge"}
}
