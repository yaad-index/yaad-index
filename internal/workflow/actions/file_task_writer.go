// FileTaskWriter is the production TaskWriter — a thin
// vault-backed implementation that finds-or-creates the
// canonical task file at `<vault>/tasks/<workflow>-<subject>.md`
// + appends content lines to named sections with the
// configured if_already_present policy.
//
// **What this implementation does.**
//   - Path: `<vault>/tasks/<workflow>-<subject>.md`.
//     Both workflow and subject are slugified (lowercased,
//     non-alphanumeric → hyphens) before joining so the
//     path stays operator-readable + filesystem-safe.
//   - Find-or-create: missing file → fresh markdown with a
//     minimal frontmatter + the section header + the
//     content line. Existing file → parse the body into
//     section blocks (separated by `## <name>` headers),
//     locate the target section (or append a fresh one),
//     apply the if_already_present policy.
//   - Skip-if-line-exists: exact-byte match on each line
//     inside the section. ifAlreadyPresent=skip is the
//     default; replace overwrites the first matching line;
//     append-anyway appends a duplicate.
//   - Atomic write: temp file + rename per filesystem-
//     standard pattern.
//
// **What this implementation does NOT do (yet).**
//   - Git commit: the production main.go wires this writer
//     before the vault.Writer auto-commit pathway. Future
//     follow-up routes task writes through vault.Writer +
//     the auto-commit chain so task history shows in git.
//   - File locking: concurrent task_append on the same
//     task file could race the read-modify-write. The
//     engine's per-Evaluator evalMu serializes Eval calls
//     for a single workflow, but cross-workflow writes to
//     the SAME task file (unlikely in v1 — task paths
//     name the workflow) would race. Follow-up: route
//     through the writelocks.Manager keyed by task path.

package actions

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/edgewrite"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// FileTaskWriter writes task files under the given vault
// root. Construct via NewFileTaskWriter; safe for
// concurrent use across goroutines (an internal mutex
// serializes reads + writes per task file).
//
// **Frontmatter handling (per #163).** Every append re-marshals
// the frontmatter to maintain the `via:` breadcrumb list +
// regenerate the `## Via` body section. yaml.Marshal does NOT
// preserve YAML comments and re-orders any operator-added
// fields under the `Extras` inline catch-all. Task files are
// workflow-managed; operators rarely hand-edit the frontmatter,
// so this v1 trade-off is acceptable.
//
// **Entity promotion (#268).** First-create of a task file also
// upserts a `task:<slug>` row in the store with `kind: task` and
// (when entityID is non-empty) emits a `triggered_by` edge from
// the task to the triggering source entity. The store + edge
// surface makes `/v1/entities/task:<slug>` resolvable, lets
// workflow `set_property` target task ids, and lets
// `graph.in_neighbors(source_id, "triggered_by")` answer "which
// tasks did this source spawn?" queries. Subsequent appends to
// the same task file leave the store row alone (idempotent —
// the row's already in shape).
type FileTaskWriter struct {
	vaultRoot string
	// kinds is the operator's canonical-kinds registry, used
	// by maybeWrapEntity to wrap `<kind>:<id>`-shaped strings
	// in `[[ ]]` per #163. Nil-safe: a nil/empty registry
	// disables wikilink wrapping (every string passes through).
	kinds map[string]config.CanonicalKindConfig

	// store mirrors first-create task files into entity rows +
	// edges per #268. Nil-safe: a nil store skips the
	// materialization step so test fixtures that just want the
	// on-disk file shape don't need to wire a backing store.
	store store.Store

	// edgeWriter routes the `triggered_by` edge create per
	// #304 Cut C1 through the centralized edge-write service.
	// Defaulted in NewFileTaskWriter when nil so test fixtures
	// stay buildable without explicit wiring.
	edgeWriter edgewrite.EdgeWriter

	// logger surfaces non-fatal store/edge materialization
	// failures at WARN. The on-disk file write is the load-
	// bearing op; store errors degrade to "row will materialize
	// on next reindex" rather than failing the task spawn.
	logger *slog.Logger

	// mu serializes the read-modify-write cycle so a single
	// task_append on the same file path can't race. For
	// cross-file appends this is over-strict but the cost
	// is negligible at v1 scale.
	mu sync.Mutex
}

// NewFileTaskWriter constructs a writer rooted at the
// vault path. The `<vault>/tasks/` directory is created
// on first write; callers don't need to ensure it exists.
//
// kinds is the operator's canonical-kinds registry, threaded
// through to maybeWrapEntity per #163 — `<kind>:<id>`-shaped
// strings in task content + breadcrumb entity refs wrap into
// `[[ ]]` only when `kind` is in this registry. Pass nil to
// disable wikilink wrapping (test-friendly default).
//
// st is the entity store the writer mirrors first-create tasks
// into per #268. Nil disables the materialization step (file-
// only mode — useful for unit fixtures); production wires a
// real store so /v1/entities/task:id resolves and set_property
// can target task ids.
//
// logger surfaces non-fatal store materialization failures.
// Nil falls back to slog.Default().
func NewFileTaskWriter(vaultRoot string, kinds map[string]config.CanonicalKindConfig, st store.Store, edgeWriter edgewrite.EdgeWriter, logger *slog.Logger) *FileTaskWriter {
	if logger == nil {
		logger = slog.Default()
	}
	if edgeWriter == nil && st != nil {
		// Default-init the centralized edge-write service so the
		// triggered_by edge mirror flows through the same routing
		// layer the rest of the daemon uses per #304 Cut C1. nil
		// store stays nil-safe (mirror loop is skipped anyway).
		svc, err := edgewrite.New(st, nil)
		if err != nil {
			panic(fmt.Sprintf("NewFileTaskWriter: default edgewrite.Service construction failed: %v", err))
		}
		edgeWriter = svc
	}
	return &FileTaskWriter{
		vaultRoot:  vaultRoot,
		kinds:      kinds,
		store:      st,
		edgeWriter: edgeWriter,
		logger:     logger,
	}
}

// MissingRefsSectionName is the body section the task_append
// runner re-syncs each fire to reflect the current
// missing-reference id list per ADR-0024 §"Missing-reference
// handling". Held as a const so the writer + future
// readers (Phase 6 task.* surface) speak the same vocab.
const MissingRefsSectionName = "Missing references"

// AppendTaskSection finds-or-creates the task file at the
// canonical path + appends content to the named section
// per the if_already_present policy. See package doc for
// the full semantics. dedupKey is written to the
// frontmatter on first create per ADR-0024 §"Per-pattern
// de-duplication"; subsequent appends preserve the
// existing dedupKey (the original frontmatter value wins).
//
// entityID is the triggering entity for the workflow fire
// (per #163). When non-empty it's recorded as the `entity`
// half of the breadcrumb; empty input is stored + rendered
// as the literal `unknown`. The (workflow, entityID) pair
// dedups across multiple appends; a repeat fire leaves the
// breadcrumb list unchanged.
//
// content is wrapped via maybeWrapEntity before write — a
// CEL template that rendered to a `<kind>:<id>` shape with
// `kind` in the canonical-kinds registry becomes
// `[[<kind>:<id>]]` in the task body. Strings outside that
// shape pass through unchanged.
func (w *FileTaskWriter) AppendTaskSection(ctx context.Context, workflow, subject, dedupKey, entityID, section, content, ifAlreadyPresent string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if strings.TrimSpace(workflow) == "" {
		return fmt.Errorf("FileTaskWriter: workflow is empty")
	}
	if strings.TrimSpace(section) == "" {
		return fmt.Errorf("FileTaskWriter: section is empty")
	}

	path := w.taskPath(workflow, subject)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir tasks dir: %w", err)
	}

	wrappedContent := maybeWrapEntity(content, w.kinds)
	entry := viaEntry{Workflow: workflow, Entity: w.viaEntityValue(entityID)}

	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// First write — create the file with frontmatter +
		// `## Via` section + named section + content line.
		body, err := w.freshTaskBodyWithVia(workflow, subject, dedupKey, entry, section, wrappedContent)
		if err != nil {
			return err
		}
		if err := w.writeFile(path, body); err != nil {
			return err
		}
		// #268: mirror the new task into the store as a first-
		// class entity so /v1/entities/task:<slug>, set_property,
		// and graph walks reach it. Best-effort — file write is
		// the load-bearing step; store / edge errors degrade to
		// "row absent for this task" rather than failing the
		// task spawn. No automatic backfill — the next spawn
		// that touches a fresh task file gets a fresh
		// materialization; a row that failed to upsert here
		// stays absent until the operator recreates the task.
		w.materializeTaskEntity(ctx, workflow, subject, dedupKey, entityID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing task %q: %w", path, err)
	}

	body, err := w.appendWithVia(string(existing), entry, section, wrappedContent, ifAlreadyPresent)
	if err != nil {
		return err
	}
	return w.writeFile(path, []byte(body))
}

// materializeTaskEntity upserts the task row + `triggered_by`
// edge for #268. Called only on the first-create path of
// AppendTaskSection (subsequent appends leave the row alone —
// it's already in shape). entityID may be empty (manual trigger
// without a target); the triggered_by edge is skipped in that
// case rather than emitted against an empty endpoint.
//
// Errors are logged at WARN and swallowed: the on-disk file is
// the source of truth per ADR-0008, so a transient store
// failure leaves the file behind without a backing row. There
// is no automatic backfill in v1.x — a row that failed here
// stays absent until the task is recreated.
func (w *FileTaskWriter) materializeTaskEntity(ctx context.Context, workflow, subject, dedupKey, entityID string) {
	if w.store == nil {
		return
	}
	taskID := TaskEntityID(workflow, subject)
	if taskID == "" {
		return
	}
	data := map[string]any{
		"workflow":   workflow,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if subject != "" {
		data["subject"] = subject
	}
	if dedupKey != "" {
		data["dedup_key"] = dedupKey
	}
	if err := w.store.UpsertEntity(ctx, &store.Entity{
		ID:   taskID,
		Kind: canonical.TaskKind,
		Data: data,
	}); err != nil {
		w.logger.WarnContext(ctx, "task entity store upsert failed (vault file landed)",
			"task_id", taskID, "err", err)
		return
	}
	if entityID == "" || entityID == viaUnknownEntity {
		return
	}
	edge := &store.Edge{Type: canonical.EdgeTypeTriggeredBy, From: taskID, To: entityID}
	if err := w.edgeWriter.CreateEdge(ctx, edge); err != nil {
		// ErrMissingEntity here means the source entity isn't
		// in the store — common for manual-trigger inputs that
		// reference an unknown id. Don't WARN on that shape;
		// the missing-ref path already covers it from the CEL
		// side.
		if !errors.Is(err, store.ErrMissingEntity) {
			w.logger.WarnContext(ctx, "task triggered_by edge create failed",
				"task_id", taskID, "source_id", entityID, "err", err)
		}
	}
}

// TaskEntityID returns the canonical entity id for the task
// produced by (workflow, subject) per ADR-0024 §"Task" /
// #268. Matches the on-disk filename convention so the vault
// path resolver (vault.KindDir-aware) can reach the file from
// the id without a separate mapping table.
func TaskEntityID(workflow, subject string) string {
	wfSlug := slugify(workflow)
	subSlug := slugify(subject)
	name := wfSlug
	if subSlug != "" {
		name = wfSlug + "-" + subSlug
	}
	if name == "" {
		return ""
	}
	return canonical.TaskKind + ":" + name
}


// viaEntityValue normalizes the entity id into the value the
// via list stores: the raw id when non-empty, else the
// `unknown` sentinel literal.
func (w *FileTaskWriter) viaEntityValue(entityID string) string {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return viaUnknownEntity
	}
	return entityID
}

// freshTaskBodyWithVia renders the first-create task body —
// frontmatter (with the seeded via entry) + body `## Via`
// section + the named section header + content line. The
// via section is the canonical body-top placement per #163.
func (w *FileTaskWriter) freshTaskBodyWithVia(workflow, subject, dedupKey string, entry viaEntry, section, content string) ([]byte, error) {
	fm := taskFrontmatter{
		Kind:      "task",
		Workflow:  workflow,
		Subject:   subject,
		DedupKey:  dedupKey,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Via:       []viaEntry{entry},
	}
	fmStr, err := renderTaskFrontmatter(fm)
	if err != nil {
		return nil, err
	}
	viaSection := renderViaBodySection(fm.Via, w.formatViaEntity)
	body := fmStr + "\n" + viaSection + "\n## " + section + "\n\n" + content + "\n"
	return []byte(body), nil
}

// appendWithVia is the existing-file path: parse the
// frontmatter, dedup-and-prepend the new via entry, re-
// render frontmatter + body via section, then apply the
// named-section merge for the new content line.
func (w *FileTaskWriter) appendWithVia(existing string, entry viaEntry, section, content, ifAlreadyPresent string) (string, error) {
	fm, body, err := parseTaskFrontmatter(existing)
	if err != nil {
		return "", err
	}
	fm.Via = dedupAndPrepend(fm.Via, entry)
	fmStr, err := renderTaskFrontmatter(fm)
	if err != nil {
		return "", err
	}
	viaSection := renderViaBodySection(fm.Via, w.formatViaEntity)
	body = upsertViaBodySection(body, viaSection)
	body, err = mergeSection(body, section, content, ifAlreadyPresent)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(body, "\n") {
		body = "\n" + body
	}
	return fmStr + body, nil
}

// formatViaEntity is the per-writer formatter passed to
// renderViaBodySection — wraps `<kind>:<id>` shapes where
// `kind` is in the configured registry; leaves other strings
// (slugs, names) bare so the renderer can hand them straight
// to Obsidian as plain text.
func (w *FileTaskWriter) formatViaEntity(id string) string {
	return maybeWrapEntity(id, w.kinds)
}

// ResolveTaskLine flips or removes the first line within
// `section` of `<vault>/tasks/<workflow>-<subject>.md` whose
// content prefix matches `matchKey` per #266. Modes:
//
//   - TaskResolveModeCheck — flip `- [ ] <rest>` →
//     `- [x] <rest>` on the matched line. Already-checked
//     lines are left in place (idempotent end-state).
//   - TaskResolveModeRemove — strip the matched line from the
//     section entirely.
//
// Missing file → no-op (returns nil). The caller may log a
// WARN; the writer itself doesn't because the cross-workflow
// resolve target may not exist if the originating workflow
// never fired. No-match within an existing file → no-op.
//
// Match is strict content-prefix: a line's content (everything
// after the markdown bullet/checkbox prefix) must START with
// matchKey. The first matching line wins; later matches stay
// untouched. Workflow authors use a discriminating prefix
// shape (e.g. `<owner>/<repo>#<n>`) to avoid accidental
// collisions.
func (w *FileTaskWriter) ResolveTaskLine(_ context.Context, workflow, subject, section, matchKey, mode string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if strings.TrimSpace(workflow) == "" {
		return fmt.Errorf("FileTaskWriter: workflow is empty")
	}
	if strings.TrimSpace(section) == "" {
		return fmt.Errorf("FileTaskWriter: section is empty")
	}
	if matchKey == "" {
		return fmt.Errorf("FileTaskWriter: match_key is empty")
	}
	switch mode {
	case parser.TaskResolveModeCheck, parser.TaskResolveModeRemove:
		// accepted
	default:
		return fmt.Errorf("FileTaskWriter: unknown task_resolve mode %q (expected check or remove)", mode)
	}

	path := w.taskPath(workflow, subject)
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Cross-workflow target task file doesn't exist — the
		// originating workflow never fired, or the file was
		// archived. No-op per the issue's idempotence rule;
		// the caller WARNs.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing task %q: %w", path, err)
	}

	body, err := resolveTaskLineInBody(string(existing), section, matchKey, mode)
	if err != nil {
		return err
	}
	if body == string(existing) {
		// No-match or already-resolved — leave the file alone.
		return nil
	}
	return w.writeFile(path, []byte(body))
}

// resolveTaskLineInBody is the pure-string transform under
// ResolveTaskLine — separated for unit-testability without
// filesystem fixtures. Walks the named section line-by-line;
// the first line whose checkbox/bullet content starts with
// matchKey gets the mode transform applied. Returns the
// (possibly mutated) body. Returns the original body verbatim
// when no line matches OR the matched line is already in the
// target state (idempotent end-state semantics).
func resolveTaskLineInBody(body, section, matchKey, mode string) (string, error) {
	lines := strings.Split(body, "\n")
	sectionHeader := "## " + section
	inSection := false
	mutated := false
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !mutated && inSection && !strings.HasPrefix(line, "## ") {
			content, prefix, ok := splitBulletContent(line)
			if ok && strings.HasPrefix(content, matchKey) {
				switch mode {
				case parser.TaskResolveModeCheck:
					if strings.HasPrefix(prefix, "- [ ]") {
						line = "- [x]" + strings.TrimPrefix(prefix, "- [ ]") + content
						mutated = true
					}
					// already `- [x]` or bare bullet — no-op
				case parser.TaskResolveModeRemove:
					mutated = true
					continue
				}
			}
		}
		if strings.HasPrefix(line, "## ") {
			inSection = line == sectionHeader
		}
		out = append(out, line)
	}
	if !mutated {
		return body, nil
	}
	return strings.Join(out, "\n"), nil
}

// splitBulletContent splits a markdown bullet/checkbox line
// into (content, prefix, ok). prefix carries the bullet shape
// + any trailing space up to the content start; content is
// everything after. Returns ok=false for non-bullet lines.
//
// Recognized prefixes:
//   - `- [ ] ` (unchecked checkbox)
//   - `- [x] ` / `- [X] ` (checked checkbox)
//   - `- ` (bare bullet)
//
// Tab + multi-space-indented variants pass through unchanged
// (the workflow-emitted lines come through the canonical
// task_append shape — flat, no leading whitespace).
func splitBulletContent(line string) (content, prefix string, ok bool) {
	switch {
	case strings.HasPrefix(line, "- [ ] "):
		return line[len("- [ ] "):], "- [ ] ", true
	case strings.HasPrefix(line, "- [x] "):
		return line[len("- [x] "):], "- [x] ", true
	case strings.HasPrefix(line, "- [X] "):
		return line[len("- [X] "):], "- [X] ", true
	case strings.HasPrefix(line, "- "):
		return line[len("- "):], "- ", true
	}
	return "", "", false
}

// EnsureMissingRefsSection idempotently rewrites the task
// file's `## Missing references` section to reflect refs.
// See TaskWriter docstring for the four-case semantics
// (refs empty / non-empty × section present / absent +
// file-absent no-op).
func (w *FileTaskWriter) EnsureMissingRefsSection(_ context.Context, workflow, subject string, refs []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	path := w.taskPath(workflow, subject)
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// No task file to annotate yet — task_append hasn't
		// run for this (workflow, subject). Silent no-op
		// per the docstring; the missing-refs section is
		// strictly a task-body annotation, not a task
		// creator.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing task %q: %w", path, err)
	}

	body, err := rewriteMissingRefsSection(string(existing), refs)
	if err != nil {
		return err
	}
	if body == string(existing) {
		return nil
	}
	return w.writeFile(path, []byte(body))
}

// rewriteMissingRefsSection produces a new task-file body
// with the `## Missing references` section in sync with
// refs. Pure helper — no I/O. Handles the four shapes:
//   - refs empty + section absent → return existing.
//   - refs empty + section present → drop the section.
//   - refs non-empty + section absent → append the section
//     at end of body.
//   - refs non-empty + section present → replace the
//     section's body with the new refs list.
//
// Section body shape: one ref per line, formatted as
// "- <id>". Trailing newline preserved.
func rewriteMissingRefsSection(existing string, refs []string) (string, error) {
	lines := splitLines(existing)
	header := "## " + MissingRefsSectionName
	startIdx := -1
	endIdx := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == header {
			startIdx = i
			// Section runs until the next `## ` header or EOF.
			endIdx = len(lines)
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "## ") {
					endIdx = j
					break
				}
			}
			break
		}
	}

	if len(refs) == 0 {
		if startIdx == -1 {
			return existing, nil
		}
		// Drop the section + the blank line typically before
		// it (so two adjacent sections don't collapse to no
		// gap).
		drop := startIdx
		if drop > 0 && lines[drop-1] == "" {
			drop--
		}
		out := append([]string(nil), lines[:drop]...)
		out = append(out, lines[endIdx:]...)
		return strings.Join(out, "\n"), nil
	}

	refLines := make([]string, 0, len(refs)+3)
	refLines = append(refLines, header, "")
	for _, r := range refs {
		refLines = append(refLines, "- "+r)
	}
	refLines = append(refLines, "")

	if startIdx == -1 {
		// Append at end. Strip trailing blanks before
		// inserting + re-add one blank as separator.
		body := strings.TrimRight(existing, "\n")
		if body != "" {
			body += "\n\n"
		}
		body += strings.Join(refLines, "\n")
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return body, nil
	}

	// Replace [startIdx, endIdx).
	out := append([]string(nil), lines[:startIdx]...)
	out = append(out, refLines...)
	out = append(out, lines[endIdx:]...)
	return strings.Join(out, "\n"), nil
}

// taskPath computes the canonical task file path. workflow
// + subject are slugified so the path is filesystem-safe.
// Routes through vault.KindDir so the on-disk task directory
// (operator-facing `tasks/`) stays aligned with the kind-
// keyed entity path the vault reader/writer derives — same
// physical location either way.
func (w *FileTaskWriter) taskPath(workflow, subject string) string {
	return TaskVaultPath(w.vaultRoot, workflow, subject)
}

// TaskVaultPath is the public-facing version of taskPath —
// allows callers to compute the canonical vault path for a
// (workflow, subject) pair without instantiating a
// FileTaskWriter.
func TaskVaultPath(vaultRoot, workflow, subject string) string {
	wfSlug := slugify(workflow)
	subSlug := slugify(subject)
	name := wfSlug
	if subSlug != "" {
		name = wfSlug + "-" + subSlug
	}
	return filepath.Join(vaultRoot, vault.KindDir(canonical.TaskKind), name+".md")
}

// slugify converts an arbitrary string to a filesystem-
// safe slug: lowercase, non-alphanumeric runs collapsed
// to single hyphens, trim leading/trailing hyphens.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// writeFile writes body to path atomically (temp file +
// rename). Permissions match a standard markdown file.
func (w *FileTaskWriter) writeFile(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write temp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q → %q: %w", tmp, path, err)
	}
	return nil
}

// mergeSection takes the existing file body + finds the
// target section's content + applies the
// if_already_present policy on a content-line match.
// Returns the updated body string.
//
// Section parsing rules:
//   - A section starts at a `## <name>` line (markdown
//     H2 header) and ends at the next `## ` header or
//     EOF.
//   - Section content is every line between the header
//     and the next-header / EOF, with surrounding blank
//     lines preserved.
//   - The target section header is `## <section>`. Match
//     is exact-byte on the trimmed section name.
//
// Policy:
//   - skip: if any line in the section equals content
//     verbatim → no-op (the body is returned unchanged).
//   - replace: if any line matches → overwrite the FIRST
//     matching line with content. Other lines preserved.
//     No match → append (same as skip's no-match path).
//   - append-anyway: always append regardless of
//     duplicate-line presence.
//
// When the section is missing from the file: a fresh
// section is appended to the end with the content line.
func mergeSection(existing, section, content, ifAlreadyPresent string) (string, error) {
	lines := splitLines(existing)
	header := "## " + section
	startIdx := -1
	endIdx := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == header {
			startIdx = i + 1
			// Find end of this section.
			for j := startIdx; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "## ") {
					endIdx = j
					break
				}
			}
			if endIdx == -1 {
				endIdx = len(lines)
			}
			break
		}
	}

	if startIdx == -1 {
		// Section missing — append a fresh one with the
		// content line. Preserve the trailing newline shape.
		body := strings.TrimRight(existing, "\n")
		if body != "" {
			body += "\n\n"
		}
		body += header + "\n\n" + content + "\n"
		return body, nil
	}

	// Section found — scan lines[startIdx:endIdx] for an
	// exact-byte match on content.
	matchIdx := -1
	for j := startIdx; j < endIdx; j++ {
		if lines[j] == content {
			matchIdx = j
			break
		}
	}

	switch ifAlreadyPresent {
	case "skip", "":
		if matchIdx != -1 {
			return existing, nil
		}
		// Append before the next-section divider (or at EOF).
		return insertLine(lines, endIdx, content), nil
	case "replace":
		if matchIdx != -1 {
			lines[matchIdx] = content
			return strings.Join(lines, "\n"), nil
		}
		// No existing match — fall through to append.
		return insertLine(lines, endIdx, content), nil
	case "append-anyway":
		return insertLine(lines, endIdx, content), nil
	default:
		return "", fmt.Errorf("FileTaskWriter: if_already_present %q is not one of {skip, replace, append-anyway}", ifAlreadyPresent)
	}
}

// splitLines splits the body into lines, preserving the
// original trailing-newline shape via the empty-last-
// element pattern.
func splitLines(s string) []string {
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []string
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	// Preserve trailing newline so the joined output round-
	// trips identically when no insertions happen.
	if strings.HasSuffix(s, "\n") {
		out = append(out, "")
	}
	return out
}

// insertLine inserts `content` at index `at` in lines.
// Used to append a content line just before a section
// boundary (the next `## ` line or EOF). Inserts an
// adjacent blank line above when the prior line is non-
// blank so the section's line-density stays readable.
func insertLine(lines []string, at int, content string) string {
	// Trim trailing blank lines within the section to
	// keep the insertion close to existing content.
	insertAt := at
	for insertAt > 0 && lines[insertAt-1] == "" {
		insertAt--
	}
	prefix := append([]string(nil), lines[:insertAt]...)
	suffix := append([]string(nil), lines[insertAt:]...)
	inserted := append(prefix, content)
	inserted = append(inserted, suffix...)
	return strings.Join(inserted, "\n")
}
