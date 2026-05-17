// set_property runner — writes static/CEL-templated values
// directly into a target entity's frontmatter `data` map per
// ADR-0024 §"Output surface". Delegates to a PropertyWriter
// interface so the production vault-backed implementation
// (read-merge-write into vault.Entity.Data + DB upsert)
// stays out of this package and tests use an in-memory fake.
//
// Per-field write semantics: overwrite-on-collision, preserve
// other keys. The runner publishes one fill.completed event
// per field that lands so downstream workflows can subscribe
// per-field. Empty Bus is OK (test/dev path) — emission is
// skipped silently.

package actions

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// PropertyWriter is the frontmatter-data surface the
// set_property runner depends on. Production wires a vault-
// backed implementation that does the read-merge-write dance
// + mirrors into the store; tests wire an in-memory fake.
type PropertyWriter interface {
	// SetProperties merges the given fields into the target
	// entity's frontmatter `data` map. Existing keys are
	// overwritten; absent keys are added; keys not in fields
	// are left unchanged. workflow names the originating
	// workflow for commit-author + lock-holder attribution.
	SetProperties(ctx context.Context, workflow, entityID string, fields map[string]any) error
}

// runSetProperty executes one set_property action by resolving
// the target entity id + each field value from the engine's
// pre-rendered template map (or the raw action fields when no
// renderer is wired), invoking the PropertyWriter, then
// publishing one fill.completed event per field that landed.
func (d *dispatcher) runSetProperty(ctx context.Context, idx int, _ *parser.Workflow, a *parser.SetPropertyAction, dec Decision, act Activation) ActionResult {
	if d.propertyWriter == nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "set_property",
			Err:       fmt.Errorf("set_property: no PropertyWriter wired (engine constructed without actions.Options.PropertyWriter)"),
		}
	}
	if len(a.Fields) == 0 {
		return ActionResult{
			ActionIdx: idx,
			Type:      "set_property",
			Err:       fmt.Errorf("%w: set_property.fields is empty", ErrActionAuthorBug),
		}
	}

	// Target resolution: prefer the engine's rendered Entity
	// (or the raw action.Entity as a fallback when no
	// renderer is wired), then default to the triggering
	// entity's id when neither is set.
	target := strings.TrimSpace(d.rendered(act, idx, "entity", a.Entity))
	if target == "" {
		target = dec.EntityID
	}
	if target == "" {
		return ActionResult{
			ActionIdx: idx,
			Type:      "set_property",
			Err:       fmt.Errorf("%w: set_property has no target (action.entity empty + decision.entity_id empty)", ErrActionAuthorBug),
		}
	}

	// Render each field's value via the engine's pre-rendered
	// map (or fall back to the raw expression). Stable iteration
	// order so the fill.completed sequence is deterministic
	// across runs — subscribers that key off event order get
	// reproducible behavior.
	fieldNames := make([]string, 0, len(a.Fields))
	for name := range a.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)

	resolved := make(map[string]any, len(fieldNames))
	for _, name := range fieldNames {
		value := d.rendered(act, idx, "field:"+name, a.Fields[name])
		resolved[name] = value
	}

	if err := d.propertyWriter.SetProperties(ctx, dec.Workflow, target, resolved); err != nil {
		return ActionResult{
			ActionIdx: idx,
			Type:      "set_property",
			Err:       fmt.Errorf("set_property: %w", err),
		}
	}

	// Publish one fill.completed per field that landed. Source
	// is `workflow:<name>` matching ADR-0024 vocabulary; the
	// Phase 5 self-loop backstop reads this to skip re-firing
	// workflow X on a property X itself injected.
	if d.bus != nil && dec.Workflow != "" {
		source := eventbus.WorkflowSource(dec.Workflow)
		chain := eventbus.WorkflowChainFromContext(ctx)
		for _, name := range fieldNames {
			d.bus.Publish(ctx, eventbus.FillCompletedEvent{
				EntityID:  target,
				Gap:       name,
				SourceTag: source,
				At:        dec.At,
				Chain:     chain,
			})
		}
	}
	return ActionResult{ActionIdx: idx, Type: "set_property"}
}
