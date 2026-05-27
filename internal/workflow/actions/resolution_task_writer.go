// Resolution-task primitive per #304 Cut C3.1 — a typed
// structured-task shape distinct from the legacy text-only
// `task_append` path. The shape carries the full
// edgewrite.ResolutionDeferred payload (source-edge tuple,
// resolver plugin, normalized raw target, options list) so
// Cut C3.3's resolve handler can pick an option and route
// through Cut B's update_edge_target / store.CreateEdge.
//
// Idempotency: the file path is derived deterministically
// from the 5-tuple
// (from_id, edge_type, target_kind, normalized_raw_target,
// resolver_plugin) — same workflow retry → same path → an
// os.Stat probe collapses to one task. No new dedup table
// per the locked design (Cut C3 design pass, #304).
//
// Schema versioning: every resolution-task carries
// `schema_version: 1` in its frontmatter. Reserved for the
// future when the typed payload shape evolves; readers gate
// on the integer rather than parsing by structural shape.
// Legacy `kind: task` files do NOT carry this field.

package actions

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/slug"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// ResolutionTaskKind is the `kind:` value on the new typed
// resolution-task frontmatter. Distinct from canonical.TaskKind
// (`task`) so readers + filters can branch on the shape without
// schema_version sniffing.
const ResolutionTaskKind = "resolution-task"

// ResolutionTaskSchemaVersion pins the integer schema version
// stamped onto every resolution-task frontmatter at write time.
// Bump when the typed-payload shape changes in a way readers
// must detect. Stays at 1 for the lifetime of Cut C3.
const ResolutionTaskSchemaVersion = 1

// resolutionTaskFrontmatter is the typed frontmatter shape
// FileTaskWriter.WriteResolutionTask renders. Field order is
// the operator-readable order yaml.Marshal emits (Go struct
// declaration order); `options` lands last because it's the
// largest block.
type resolutionTaskFrontmatter struct {
	Kind                string             `yaml:"kind"`
	SchemaVersion       int                `yaml:"schema_version"`
	IdempotencyKey      string             `yaml:"idempotency_key"`
	CreatedAt           string             `yaml:"created_at"`
	FromID              string             `yaml:"from_id"`
	EdgeType            string             `yaml:"edge_type"`
	TargetKind          string             `yaml:"target_kind"`
	ResolverPlugin      string             `yaml:"resolver_plugin"`
	NormalizedRawTarget string             `yaml:"normalized_raw_target"`
	RawTarget           string             `yaml:"raw_target"`
	Options             []resolutionOption `yaml:"options"`
}

// resolutionOption mirrors plugins.DisambiguationOption with
// the candidate's canonical id surfaced as a named field. The
// upstream map's key (the entity id) becomes the `id` field
// here so the operator can read a flat list of candidates
// from the rendered yaml + the resolve handler can drive on
// `id` directly.
type resolutionOption struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label,omitempty"`
	Summary string `yaml:"summary,omitempty"`
}

// ResolutionTaskKey derives the deterministic idempotency
// key for a ResolutionDeferred. Length-prefixed SHA-256 over
// the 5-tuple fields preserves field boundaries — slug-and-
// join would let `|` separators normalize to the same `-` as
// embedded punctuation (e.g. `email:m1`'s colon → hyphen),
// collapsing distinct 5-tuples like `(from="x", edge="a-b")`
// and `(from="x-a", edge="b")` to the same path. Each
// component is length-prefixed with an 8-byte big-endian
// uint64 before hashing so structurally-different tuples
// produce different digests regardless of which component
// embeds the bytes another component happens to contain.
//
// The 5-tuple per the locked design:
// (from_id, edge_type, target_kind, normalized_raw_target,
// resolver_plugin). RawTarget is normalized via slug.Slug
// before hashing so plugin-side casing / whitespace
// differences collapse to the same key.
//
// Output shape: `<target_kind_slug>-<16-hex-chars>`. The
// target-kind prefix keeps `ls tasks/` operator-readable;
// the 16 hex chars (64 bits) give collision resistance to
// >10^19 distinct tuples.
func ResolutionTaskKey(d *edgewrite.ResolutionDeferred) string {
	if d == nil {
		return ""
	}
	h := sha256.New()
	writeLenPrefixed := func(s string) {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(len(s)))
		h.Write(buf[:])
		h.Write([]byte(s))
	}
	writeLenPrefixed(d.From)
	writeLenPrefixed(d.EdgeType)
	writeLenPrefixed(d.TargetKind)
	writeLenPrefixed(slug.Slug(d.RawTarget))
	writeLenPrefixed(d.ResolverPlugin)
	sum := h.Sum(nil)
	return slug.Slug(d.TargetKind) + "-" + hex.EncodeToString(sum[:8])
}

// ResolutionTaskID returns the canonical entity id for the
// resolution-task — `task:<idempotency-key>`. The `task:`
// kind prefix matches the legacy text-task shape so the
// existing /v1/entities/task:<id> resolver, list filters,
// and store mirror keep working uniformly across both task
// flavors.
func ResolutionTaskID(d *edgewrite.ResolutionDeferred) string {
	key := ResolutionTaskKey(d)
	if key == "" {
		return ""
	}
	return canonical.TaskKind + ":" + key
}

// ResolutionTaskVaultPath returns the on-disk file path for
// the resolution-task. Lives under the same `<vault>/tasks/`
// directory as legacy text tasks so listing-by-prefix
// (existing `tasks.Reader.List`) returns both shapes.
func ResolutionTaskVaultPath(vaultRoot string, d *edgewrite.ResolutionDeferred) string {
	key := ResolutionTaskKey(d)
	if key == "" {
		return ""
	}
	return filepath.Join(vaultRoot, vault.KindDir(canonical.TaskKind), key+".md")
}

// WriteResolutionTask creates a structured resolution-task
// file from the supplied ResolutionDeferred, idempotent on
// the 5-tuple. Returns (taskID, created, err):
//
//   - (taskID, true, nil) — fresh file written + entity row
//     mirrored. The workflow caller should treat this as "task
//     spawned, workflow paused on operator resolve".
//   - (taskID, false, nil) — file already existed at the
//     idempotency-probe path; no write performed. Mirrors the
//     workflow-retry-collapse contract: a second fire of the
//     same workflow over the same source / target / plugin
//     adds nothing.
//   - ("", false, err) — input validation failure or fs error.
//
// nil ResolutionDeferred, or one missing any of the 5 required
// fields (From / EdgeType / TargetKind / RawTarget /
// ResolverPlugin) errors out — the idempotency key would be
// meaningless without all of them.
func (w *FileTaskWriter) WriteResolutionTask(ctx context.Context, d *edgewrite.ResolutionDeferred) (string, bool, error) {
	if d == nil {
		return "", false, fmt.Errorf("WriteResolutionTask: ResolutionDeferred is nil")
	}
	if err := validateResolutionDeferred(d); err != nil {
		return "", false, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	taskID := ResolutionTaskID(d)
	path := ResolutionTaskVaultPath(w.vaultRoot, d)

	// Idempotency probe per the locked filesystem-path-encodes-
	// key design. Stat-before-write is the cheap collapse for
	// workflow-retry-storm: same tuple → same path → second fire
	// returns the existing taskID without touching the file.
	if _, err := os.Stat(path); err == nil {
		return taskID, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("probe resolution-task %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir tasks dir: %w", err)
	}

	body, err := renderResolutionTaskBody(d)
	if err != nil {
		return "", false, err
	}
	if err := w.writeFile(path, body); err != nil {
		return "", false, err
	}

	// Mirror into the store as a `task:<key>` entity so
	// /v1/entities/task:<id>, set_property, and the resolve
	// flow's lookup all resolve uniformly. Best-effort
	// (matches legacy text-task materialization): file write
	// is load-bearing; store errors degrade to "row absent
	// until next reindex / recreate".
	w.materializeResolutionTaskEntity(ctx, d, taskID)

	return taskID, true, nil
}

// validateResolutionDeferred rejects payloads missing any
// idempotency-key component. Empty Options is allowed (the
// resolve handler would return "no candidates" — Cut C3.3's
// concern, not C3.1's).
func validateResolutionDeferred(d *edgewrite.ResolutionDeferred) error {
	switch {
	case strings.TrimSpace(d.From) == "":
		return fmt.Errorf("WriteResolutionTask: From is required")
	case strings.TrimSpace(d.EdgeType) == "":
		return fmt.Errorf("WriteResolutionTask: EdgeType is required")
	case strings.TrimSpace(d.TargetKind) == "":
		return fmt.Errorf("WriteResolutionTask: TargetKind is required")
	case strings.TrimSpace(d.RawTarget) == "":
		return fmt.Errorf("WriteResolutionTask: RawTarget is required")
	case strings.TrimSpace(d.ResolverPlugin) == "":
		return fmt.Errorf("WriteResolutionTask: ResolverPlugin is required")
	}
	return nil
}

// renderResolutionTaskBody serializes the typed frontmatter
// + a short operator-readable body section. The body's
// "Pick one" checklist mirrors the option list so an
// operator scanning the file sees the candidates without
// re-reading the yaml — Cut C3.3's resolve handler reads
// from the frontmatter, not the body, so the checklist is
// presentational.
func renderResolutionTaskBody(d *edgewrite.ResolutionDeferred) ([]byte, error) {
	fm := resolutionTaskFrontmatter{
		Kind:                ResolutionTaskKind,
		SchemaVersion:       ResolutionTaskSchemaVersion,
		IdempotencyKey:      ResolutionTaskKey(d),
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
		FromID:              d.From,
		EdgeType:            d.EdgeType,
		TargetKind:          d.TargetKind,
		ResolverPlugin:      d.ResolverPlugin,
		NormalizedRawTarget: slug.Slug(d.RawTarget),
		RawTarget:           d.RawTarget,
		Options:             optionsFromPluginMap(d.Options),
	}
	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("marshal resolution-task frontmatter: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBytes)
	b.WriteString("---\n\n")
	b.WriteString("## Resolution\n\n")
	fmt.Fprintf(&b,
		"Workflow paused on ambiguous resolve of %q via plugin %s.\n"+
			"Pick one option below and resolve via `task_resolve` (Cut C3.3).\n\n",
		d.RawTarget, d.ResolverPlugin,
	)
	for _, opt := range fm.Options {
		line := "- [ ] " + opt.ID
		if opt.Label != "" {
			line += " — " + opt.Label
		}
		if opt.Summary != "" {
			line += " — " + opt.Summary
		}
		b.WriteString(line + "\n")
	}
	return []byte(b.String()), nil
}

// optionsFromPluginMap projects the plugin's options map
// (keyed by candidate id) into the slice shape stored on the
// frontmatter. Sorting by id keeps the rendered yaml stable
// across writes — workflows that retry shouldn't see option-
// order churn on the on-disk file even when an os-level
// re-read shows a different map iteration order.
func optionsFromPluginMap(m map[string]plugins.DisambiguationOption) []resolutionOption {
	if len(m) == 0 {
		return nil
	}
	out := make([]resolutionOption, 0, len(m))
	for id, opt := range m {
		out = append(out, resolutionOption{ID: id, Label: opt.Label, Summary: opt.Summary})
	}
	sortResolutionOptions(out)
	return out
}

// sortResolutionOptions sorts in-place by id (ascending).
// Inlined to keep the package's `sort` import surface tight
// and avoid touching unrelated files.
func sortResolutionOptions(opts []resolutionOption) {
	for i := 1; i < len(opts); i++ {
		for j := i; j > 0 && opts[j-1].ID > opts[j].ID; j-- {
			opts[j-1], opts[j] = opts[j], opts[j-1]
		}
	}
}

// materializeResolutionTaskEntity upserts the task row for
// the new resolution-task. Mirrors the legacy text-task
// materialization shape (`task:<id>` entity with workflow
// metadata) plus the resolution-specific fields so the
// resolve handler can read everything it needs from the
// store row without re-parsing the on-disk frontmatter.
func (w *FileTaskWriter) materializeResolutionTaskEntity(ctx context.Context, d *edgewrite.ResolutionDeferred, taskID string) {
	if w.store == nil {
		return
	}
	data := map[string]any{
		"kind_extra":            ResolutionTaskKind,
		"schema_version":        ResolutionTaskSchemaVersion,
		"created_at":            time.Now().UTC().Format(time.RFC3339),
		"idempotency_key":       ResolutionTaskKey(d),
		"from_id":               d.From,
		"edge_type":             d.EdgeType,
		"target_kind":           d.TargetKind,
		"resolver_plugin":       d.ResolverPlugin,
		"normalized_raw_target": slug.Slug(d.RawTarget),
		"raw_target":            d.RawTarget,
	}
	if err := w.store.UpsertEntity(ctx, &store.Entity{
		ID:   taskID,
		Kind: canonical.TaskKind,
		Data: data,
	}); err != nil {
		w.logger.WarnContext(ctx, "resolution-task entity store upsert failed (vault file landed)",
			"task_id", taskID, "err", err)
	}
}
