// Via-section dual-storage helpers per #163. Every task_append
// implicitly records a breadcrumb naming the source workflow +
// triggering entity. The data lives in two synchronized places:
//
//  1. **Frontmatter** `via:` — a YAML list of `{workflow, entity}`
//     objects. The structured / parser-readable source of truth;
//     reads always come from here.
//  2. **Body** `## Via` section — a rendered view of the same
//     data, immediately below the frontmatter. Obsidian-readable;
//     each line wraps the workflow name + entity id in `[[ ]]`
//     so the rendered task surfaces clickable backlinks.
//
// Ordering: prepend (newest-first). Dedup: same (workflow,
// entity) pair never repeats — the existing entry stays in its
// original position.

package actions

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// viaUnknownEntity is the literal recorded in both storage
// surfaces when the triggering entity id is empty (e.g.
// manual trigger with no entity context).
const viaUnknownEntity = "unknown"

// viaSectionName is the body section header that mirrors the
// frontmatter via list. Held as a const so the writer + future
// readers (Phase 6 task.* surface) speak the same vocab.
const viaSectionName = "Via"

// viaEntry is one breadcrumb — a (workflow, entity) pair. The
// frontmatter list stores entries in this exact shape; the
// body section renders each as `- [[<workflow>]] from
// [[<entity>]]` (or `from unknown` when Entity is the
// `viaUnknownEntity` literal).
type viaEntry struct {
	Workflow string `yaml:"workflow"`
	Entity   string `yaml:"entity"`
}

// dedupAndPrepend returns the via list with `entry` inserted
// at position 0 unless an identical (workflow, entity) pair
// already exists. The existing entry stays at its original
// position — no re-sort, no time-shift. Matches the spec's
// "insertion order is read order" semantics.
func dedupAndPrepend(list []viaEntry, entry viaEntry) []viaEntry {
	for _, e := range list {
		if e.Workflow == entry.Workflow && e.Entity == entry.Entity {
			return list
		}
	}
	out := make([]viaEntry, 0, len(list)+1)
	out = append(out, entry)
	out = append(out, list...)
	return out
}

// renderViaBodySection produces the body lines for the
// `## Via` section. Each entry is one line of the shape
// `- [[<workflow>]] from [[<entity>]]`, or `- [[<workflow>]]
// from unknown` when the entity is the unknown literal
// (workflow names are always wrapped; "unknown" is a literal
// sentinel, not an entity slug).
//
// Caller controls entity wrapping: this helper takes the
// already-formatted entity reference (which is what the
// VaultTaskWriter computes via maybeWrapEntity for known
// kinds OR leaves as `unknown` for the sentinel). The helper
// itself only wraps workflow names — always — and emits the
// section verbatim.
//
// Returns the section body lines starting with the `## Via`
// header, an empty separator line, then one `- ...` line per
// entry, then a trailing empty line. Empty list returns an
// empty section (header + blank line) which is still a valid
// shape but signals "no breadcrumbs yet" — the writer never
// emits this case because the first call always populates
// the list with the firing breadcrumb.
func renderViaBodySection(list []viaEntry, formatEntity func(string) string) string {
	var b strings.Builder
	b.WriteString("## " + viaSectionName + "\n\n")
	for _, e := range list {
		entityRef := viaUnknownEntity
		if e.Entity != viaUnknownEntity {
			entityRef = formatEntity(e.Entity)
		}
		b.WriteString("- " + wrapWorkflow(e.Workflow) + " from " + entityRef + "\n")
	}
	return b.String()
}

// taskFrontmatter is the yaml-roundtrip shape for the task file
// frontmatter. The named fields capture every field
// `freshTaskBody` emits + the new `via:` list per #163; Extras
// preserves any operator hand-added fields via the inline
// catch-all tag.
//
// Marshal/unmarshal of this struct DOES lose YAML comments and
// re-orders any operator-added fields under `Extras`. v1 trade-
// off: task files are workflow-managed and operators rarely
// hand-edit the frontmatter. Documented in the file writer's
// package comment.
type taskFrontmatter struct {
	Kind      string         `yaml:"kind,omitempty"`
	Workflow  string         `yaml:"workflow,omitempty"`
	Subject   string         `yaml:"subject,omitempty"`
	DedupKey  string         `yaml:"dedup_key,omitempty"`
	CreatedAt string         `yaml:"created_at,omitempty"`
	Via       []viaEntry     `yaml:"via,omitempty"`
	Extras    map[string]any `yaml:",inline"`
}

// parseTaskFrontmatter splits a task-file body at the `---`
// fences and yaml-unmarshals the frontmatter block. Returns
// (frontmatter struct, body-without-frontmatter, error).
//
// An empty body or a body without a leading `---\n` is treated
// as a body-only shape (no frontmatter) — returns an empty
// taskFrontmatter and the body unchanged. The writer should
// have written valid frontmatter on first create, so this
// branch is defensive.
func parseTaskFrontmatter(content string) (taskFrontmatter, string, error) {
	var fm taskFrontmatter
	if !strings.HasPrefix(content, "---\n") {
		return fm, content, nil
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return fm, content, fmt.Errorf("task frontmatter: missing closing `---` fence")
	}
	fmBlock := rest[:end]
	body := rest[end+len("\n---\n"):]
	if err := yaml.Unmarshal([]byte(fmBlock), &fm); err != nil {
		return fm, content, fmt.Errorf("task frontmatter yaml: %w", err)
	}
	return fm, body, nil
}

// renderTaskFrontmatter marshals the struct back to the
// frontmatter block including the surrounding `---\n`
// fences + one trailing newline. Marshal of an empty
// taskFrontmatter returns just the bare fences.
func renderTaskFrontmatter(fm taskFrontmatter) (string, error) {
	body, err := yaml.Marshal(fm)
	if err != nil {
		return "", fmt.Errorf("marshal task frontmatter: %w", err)
	}
	return "---\n" + string(body) + "---\n", nil
}

// upsertViaBodySection inserts or replaces the `## Via`
// section at the top of the body (the canonical position
// per #163). If the body already has a `## Via` section,
// the old content is replaced with the rendered output.
// If not, the section is prepended above all existing body
// content.
//
// A leading blank line is inserted between the via section
// and the next section (or any non-empty body content) so
// the markdown rendering doesn't collapse them.
func upsertViaBodySection(body, viaSection string) string {
	body = strings.TrimLeft(body, "\n")
	header := "## " + viaSectionName
	if !strings.HasPrefix(body, header+"\n") {
		// No existing via section at top — prepend.
		if body == "" {
			return "\n" + viaSection
		}
		return "\n" + viaSection + "\n" + body
	}
	// Find end of the via section (next `## ` header or EOF).
	lines := splitLines(body)
	endIdx := len(lines)
	for i := 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			endIdx = i
			break
		}
	}
	// Trim trailing blank lines inside the section so the new
	// rendered section + the separator we add don't accumulate
	// extra blanks across re-writes.
	for endIdx > 0 && lines[endIdx-1] == "" {
		endIdx--
	}
	tail := strings.Join(lines[endIdx:], "\n")
	if tail != "" && !strings.HasPrefix(tail, "\n") {
		tail = "\n" + tail
	}
	return "\n" + viaSection + tail
}
