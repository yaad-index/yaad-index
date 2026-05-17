package loader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// fakePluginRegistry is the test-side PluginRegistry that
// satisfies the loader's load-time validation interface
// without pulling in the production registry's dependencies.
type fakePluginRegistry struct {
	names map[string]struct{}
}

func newFakeRegistry(names ...string) *fakePluginRegistry {
	r := &fakePluginRegistry{names: make(map[string]struct{}, len(names))}
	for _, n := range names {
		r.names[n] = struct{}{}
	}
	return r
}

func (r *fakePluginRegistry) LookupByName(name string) (any, bool) {
	_, ok := r.names[name]
	return nil, ok
}

// fakeCanonicalRegistry satisfies CanonicalRegistry for the
// #132 + #142 loader-time validation tests.
type fakeCanonicalRegistry struct {
	kinds     map[string]struct{}
	edgeTypes map[string]struct{}
	gapTypes  map[string][]string
}

func newFakeCanonicalRegistry(kinds []string, edgeTypes []string) *fakeCanonicalRegistry {
	r := &fakeCanonicalRegistry{
		kinds:     make(map[string]struct{}, len(kinds)),
		edgeTypes: make(map[string]struct{}, len(edgeTypes)),
		gapTypes:  make(map[string][]string),
	}
	for _, k := range kinds {
		r.kinds[k] = struct{}{}
	}
	for _, e := range edgeTypes {
		r.edgeTypes[e] = struct{}{}
	}
	return r
}

// withGapType seeds a registered (gap, type) pair so #142
// loader tests can drive the inline-type-conflict path.
func (r *fakeCanonicalRegistry) withGapType(gap, typ string) *fakeCanonicalRegistry {
	r.gapTypes[gap] = append(r.gapTypes[gap], typ)
	return r
}

func (r *fakeCanonicalRegistry) KindExists(kind string) bool {
	_, ok := r.kinds[kind]
	return ok
}

func (r *fakeCanonicalRegistry) EdgeTypeExists(edgeType string) bool {
	_, ok := r.edgeTypes[edgeType]
	return ok
}

func (r *fakeCanonicalRegistry) RegisteredGapTypes(gap string) []string {
	return r.gapTypes[gap]
}

// addCanonicalEdgeWorkflowMarkdown returns a workflow file with a
// single add_canonical_edge action. Used by #132 loader-time
// validation tests.
func addCanonicalEdgeWorkflowMarkdown(name, edgeType, targetKind, targetName string) string {
	return fmt.Sprintf(
		"---\nname: %s\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\ntrigger:\n  type: manual\nactions:\n  - add_canonical_edge:\n      edge_type: '%s'\n      target:\n        kind: '%s'\n        name: '%s'\n```\n",
		name, edgeType, targetKind, targetName,
	)
}

// minimalWorkflowMarkdown returns a valid workflow file with
// the given name + allowed_plugins. Tests use this to seed
// the loader's scan directory.
func minimalWorkflowMarkdown(name string, allowedPlugins ...string) string {
	plugins := ""
	for _, p := range allowedPlugins {
		plugins += "  - " + p + "\n"
	}
	return fmt.Sprintf("---\nname: %s\n---\n\n```yaml\nallowed_plugins:\n%strigger:\n  type: manual\nactions:\n  - add_note:\n      content: 'hi'\n```\n", name, plugins)
}

// writeWorkflow writes content to dir/name.md with the given
// mtime. Returns the file path.
func writeWorkflow(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name+".md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// setMtime forces a deterministic mtime on the file so polling
// tests don't depend on filesystem clock resolution.
func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

// TestLoader_HappyPath_RegistersValidWorkflows: a directory
// with two valid workflow files yields a registry with both,
// keyed by name, sorted on Workflows().
func TestLoader_HappyPath_RegistersValidWorkflows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "alpha", minimalWorkflowMarkdown("alpha", "yaad-gmail"))
	writeWorkflow(t, dir, "beta", minimalWorkflowMarkdown("beta", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, l.Load(context.Background()))

	wfs := l.Workflows()
	require.Len(t, wfs, 2)
	assert.Equal(t, "alpha", wfs[0].Name)
	assert.Equal(t, "beta", wfs[1].Name)

	got, ok := l.Lookup("alpha")
	require.True(t, ok)
	assert.Equal(t, "alpha", got.Name)

	_, ok = l.Lookup("missing")
	assert.False(t, ok)
}

// TestLoader_RejectsParseError: a malformed workflow file
// (missing YAML body) is rejected. Other valid files in the
// same directory still register.
func TestLoader_RejectsParseError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "good", minimalWorkflowMarkdown("good", "yaad-gmail"))
	writeWorkflow(t, dir, "broken", "---\nname: broken\n---\n\nno yaml fence here\n")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		Logger:         logger,
	})
	require.NoError(t, l.Load(context.Background()))

	wfs := l.Workflows()
	require.Len(t, wfs, 1)
	assert.Equal(t, "good", wfs[0].Name)
	assert.Contains(t, logBuf.String(), "workflow file rejected")
	assert.Contains(t, logBuf.String(), "broken.md")
}

// TestLoader_AllowedPluginsValidation: a workflow declaring a
// plugin not in the live registry is rejected at load time.
func TestLoader_AllowedPluginsValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "wrong-plugin", minimalWorkflowMarkdown("wrong-plugin", "yaad-bgg"))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"), // yaad-bgg NOT loaded
		Logger:         logger,
	})
	require.NoError(t, l.Load(context.Background()))

	assert.Empty(t, l.Workflows(), "workflow with missing plugin should be rejected")
	assert.Contains(t, logBuf.String(), "allowed_plugins not loaded")
	assert.Contains(t, logBuf.String(), "yaad-bgg")
}

// TestLoader_NilPluginRegistry_SkipsValidation: when registry
// is nil the loader still parses + registers (useful for dev /
// test setups).
func TestLoader_NilPluginRegistry_SkipsValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "any", minimalWorkflowMarkdown("any", "made-up-plugin"))

	l := New(Options{
		Paths: []string{dir},
		// PluginRegistry omitted on purpose.
	})
	require.NoError(t, l.Load(context.Background()))
	assert.Len(t, l.Workflows(), 1, "nil registry skips allowed_plugins check")
}

// TestLoader_ReloadOnMtimeBump: editing a workflow file
// updates the registered version on the next Load call. The
// loader uses mtime to skip unchanged files, so the mtime
// has to advance.
func TestLoader_ReloadOnMtimeBump(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeWorkflow(t, dir, "iter", minimalWorkflowMarkdown("iter", "yaad-gmail"))
	setMtime(t, path, time.Now().Add(-time.Hour))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1)
	wf, ok := l.Lookup("iter")
	require.True(t, ok)
	assert.Equal(t, "manual", wf.Trigger.Type)

	// Operator edits: switch trigger to entity_created.
	updated := strings.Replace(minimalWorkflowMarkdown("iter", "yaad-gmail"),
		"type: manual", "type: entity_created", 1)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	setMtime(t, path, time.Now())

	require.NoError(t, l.Load(context.Background()))
	wf, ok = l.Lookup("iter")
	require.True(t, ok)
	assert.Equal(t, "entity_created", wf.Trigger.Type,
		"reload picks up the new trigger type")
}

// TestLoader_SameMtimeSkipsReparse: a file whose mtime hasn't
// changed between Load calls isn't re-parsed (the loader
// trusts the cached entry). We assert by mutating the file
// content WITHOUT bumping the mtime and confirming the
// registry still reports the old shape.
func TestLoader_SameMtimeSkipsReparse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeWorkflow(t, dir, "frozen", minimalWorkflowMarkdown("frozen", "yaad-gmail"))
	fixedMtime := time.Now().Add(-time.Hour)
	setMtime(t, path, fixedMtime)

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	wf, _ := l.Lookup("frozen")
	require.NotNil(t, wf)
	assert.Equal(t, "manual", wf.Trigger.Type)

	// Edit content but pin mtime to the previous value: the
	// loader should skip re-parsing on the next Load.
	updated := strings.Replace(minimalWorkflowMarkdown("frozen", "yaad-gmail"),
		"type: manual", "type: entity_created", 1)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
	setMtime(t, path, fixedMtime)

	require.NoError(t, l.Load(context.Background()))
	wf, _ = l.Lookup("frozen")
	require.NotNil(t, wf)
	assert.Equal(t, "manual", wf.Trigger.Type,
		"mtime unchanged → loader skips re-parse, cached entry preserved")
}

// TestLoader_FileDeletion_RemovesFromRegistry: a workflow file
// that disappears between Load calls is unregistered.
func TestLoader_FileDeletion_RemovesFromRegistry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keep := writeWorkflow(t, dir, "keep", minimalWorkflowMarkdown("keep", "yaad-gmail"))
	gone := writeWorkflow(t, dir, "gone", minimalWorkflowMarkdown("gone", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 2)

	require.NoError(t, os.Remove(gone))
	require.NoError(t, l.Load(context.Background()))

	wfs := l.Workflows()
	require.Len(t, wfs, 1)
	assert.Equal(t, "keep", wfs[0].Name)
	_, ok := l.Lookup("gone")
	assert.False(t, ok, "deleted file → workflow unregistered")
	_ = keep
}

// TestLoader_PreviouslyValidThenBroken: a workflow that was
// registered then edited into a broken state drops out of
// the registry (operator should see the rejection log).
func TestLoader_PreviouslyValidThenBroken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeWorkflow(t, dir, "fragile", minimalWorkflowMarkdown("fragile", "yaad-gmail"))
	setMtime(t, path, time.Now().Add(-time.Hour))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	_, ok := l.Lookup("fragile")
	require.True(t, ok)

	// Break the file: drop the YAML fence so the parser
	// rejects it.
	require.NoError(t, os.WriteFile(path, []byte("---\nname: fragile\n---\nno yaml\n"), 0o644))
	setMtime(t, path, time.Now())

	require.NoError(t, l.Load(context.Background()))
	_, ok = l.Lookup("fragile")
	assert.False(t, ok, "broken edit → workflow dropped from registry")
}

// TestLoader_NameCollision: two files declaring the same
// workflow name → second one rejected; first kept.
func TestLoader_NameCollision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "first", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))
	writeWorkflow(t, dir, "second", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		Logger:         logger,
	})
	require.NoError(t, l.Load(context.Background()))

	wfs := l.Workflows()
	require.Len(t, wfs, 1, "only one workflow registered under the colliding name")
	assert.Equal(t, "shared-name", wfs[0].Name)
	assert.Contains(t, logBuf.String(), "name collision")
}

// TestLoader_CollisionRecoversAfterPriorRemoved: a
// collision-rejected file's mtime is not cached, so when the
// prior-registrant file is removed the next Load re-attempts
// registration on the rejected file and succeeds. Without
// this behavior, the rejected file's unchanged mtime would
// short-circuit re-parse and the file would never recover.
func TestLoader_CollisionRecoversAfterPriorRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	priorPath := writeWorkflow(t, dir, "first", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))
	writeWorkflow(t, dir, "second", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		Logger:         slog.New(slog.DiscardHandler),
	})

	// First scan: collision rejected on the second file.
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1)

	// Remove the prior-registrant file. The second file's
	// mtime is unchanged on disk — but the loader didn't
	// cache it on rejection, so the next scan re-parses.
	require.NoError(t, os.Remove(priorPath))
	require.NoError(t, l.Load(context.Background()))
	wfs := l.Workflows()
	require.Len(t, wfs, 1, "previously-collision-rejected file now registered")
	assert.Equal(t, "shared-name", wfs[0].Name)
}

// TestLoader_CollisionWarnsOnce: while the collision persists
// (both files present), repeated polls don't re-log the WARN.
// First poll logs once; subsequent polls stay silent.
func TestLoader_CollisionWarnsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "first", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))
	writeWorkflow(t, dir, "second", minimalWorkflowMarkdown("shared-name", "yaad-gmail"))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		Logger:         logger,
	})

	require.NoError(t, l.Load(context.Background()))
	require.NoError(t, l.Load(context.Background()))
	require.NoError(t, l.Load(context.Background()))

	// Count the collision-warning lines: should be exactly 1
	// across the three Load calls.
	count := strings.Count(logBuf.String(), "name collision")
	assert.Equal(t, 1, count, "collision logged once, not per-poll")
}

// TestLoader_NonMarkdownFilesIgnored: only `*.md` files in the
// scan dir are considered. README, dotfiles, subdirectories
// pass through silently.
func TestLoader_NonMarkdownFilesIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "yes", minimalWorkflowMarkdown("yes", "yaad-gmail"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a workflow"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("dotfile"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1, "only the .md workflow file counted")
}

// TestLoader_MissingDirectory_Silent: a configured path that
// doesn't exist on disk logs INFO but returns no error. The
// operator may have a daemon-side path declared in config but
// the directory not yet created.
func TestLoader_MissingDirectory_Silent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "real", minimalWorkflowMarkdown("real", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{dir, "/path/that/does/not/exist/anywhere"},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	assert.Len(t, l.Workflows(), 1)
}

// TestLoader_EmptyPaths_LoadIsNoop: zero configured paths is
// not an error; the loader stays empty.
func TestLoader_EmptyPaths_LoadIsNoop(t *testing.T) {
	t.Parallel()
	l := New(Options{})
	require.NoError(t, l.Load(context.Background()))
	assert.Empty(t, l.Workflows())
}

// TestLoader_MultiplePaths_BothScanned: configured paths are
// scanned independently; workflows from each appear in the
// registry.
func TestLoader_MultiplePaths_BothScanned(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	daemon := t.TempDir()
	writeWorkflow(t, vault, "vault-wf", minimalWorkflowMarkdown("vault-wf", "yaad-gmail"))
	writeWorkflow(t, daemon, "daemon-wf", minimalWorkflowMarkdown("daemon-wf", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{vault, daemon},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))

	names := make([]string, 0, 2)
	for _, wf := range l.Workflows() {
		names = append(names, wf.Name)
	}
	assert.ElementsMatch(t, []string{"daemon-wf", "vault-wf"}, names)
}

// TestLoader_DefaultPollInterval: opts.PollInterval = 0 →
// DefaultPollInterval.
func TestLoader_DefaultPollInterval(t *testing.T) {
	t.Parallel()
	l := New(Options{})
	assert.Equal(t, DefaultPollInterval, l.pollInterval)

	l = New(Options{PollInterval: 5 * time.Second})
	assert.Equal(t, 5*time.Second, l.pollInterval)
}

// TestLoader_Run_PollsAndCancels: the polling loop picks up a
// new file added between ticks, and returns ctx.Err on cancel.
func TestLoader_Run_PollsAndCancels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		PollInterval:   50 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = l.Run(ctx)
	}()

	// Initial Load: no files yet.
	require.Eventually(t, func() bool {
		return len(l.Workflows()) == 0
	}, time.Second, 10*time.Millisecond)

	writeWorkflow(t, dir, "added-mid-run", minimalWorkflowMarkdown("added-mid-run", "yaad-gmail"))

	// Next tick should pick it up.
	require.Eventually(t, func() bool {
		return len(l.Workflows()) == 1
	}, 2*time.Second, 25*time.Millisecond,
		"polling loop should register file added between ticks")

	cancel()
	wg.Wait()
	assert.ErrorIs(t, runErr, context.Canceled)
}

// TestLoader_WorkflowsSnapshot_IsCopy: mutating the returned
// slice doesn't affect the next Workflows() call. Defensive
// against callers that try to filter in place.
func TestLoader_WorkflowsSnapshot_IsCopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "a", minimalWorkflowMarkdown("a", "yaad-gmail"))
	writeWorkflow(t, dir, "b", minimalWorkflowMarkdown("b", "yaad-gmail"))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
	})
	require.NoError(t, l.Load(context.Background()))
	got := l.Workflows()
	require.Len(t, got, 2)

	// Mutate the caller's slice.
	got[0] = &parser.Workflow{Name: "tampered"}
	got = append(got, &parser.Workflow{Name: "extra"})
	_ = got

	// Fresh snapshot — should be intact.
	again := l.Workflows()
	require.Len(t, again, 2)
	assert.Equal(t, "a", again[0].Name)
	assert.Equal(t, "b", again[1].Name)
}

// TestLoader_AddCanonicalEdge_UnknownEdgeTypeRejected: a
// workflow declaring an add_canonical_edge with edge_type not in
// canonical_edge_types is rejected at load time per #132.
func TestLoader_AddCanonicalEdge_UnknownEdgeTypeRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "bad-edge",
		addCanonicalEdgeWorkflowMarkdown("bad-edge", "unknown_edge", "person", "Uwe"))

	var logBuf bytes.Buffer
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: newFakeCanonicalRegistry([]string{"person"}, []string{"is_about"}),
		Logger:            slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	require.NoError(t, l.Load(context.Background()))
	assert.Empty(t, l.Workflows(), "rejected workflow stays out of registry")
	assert.Contains(t, logBuf.String(), "unknown_edge")
}

// TestLoader_AddCanonicalEdge_UnknownTargetKindRejected: target
// kind not in canonical_kinds is rejected at load time per #132.
func TestLoader_AddCanonicalEdge_UnknownTargetKindRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "bad-kind",
		addCanonicalEdgeWorkflowMarkdown("bad-kind", "is_about", "unknown_kind", "Whatever"))

	var logBuf bytes.Buffer
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: newFakeCanonicalRegistry([]string{"person"}, []string{"is_about"}),
		Logger:            slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	require.NoError(t, l.Load(context.Background()))
	assert.Empty(t, l.Workflows(), "rejected workflow stays out of registry")
	assert.Contains(t, logBuf.String(), "unknown_kind")
}

// TestLoader_AddCanonicalEdge_KnownRegistryAccepts: edge_type +
// target.kind both in their respective registries → workflow
// registers cleanly.
func TestLoader_AddCanonicalEdge_KnownRegistryAccepts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "good",
		addCanonicalEdgeWorkflowMarkdown("good", "is_about", "person", "Uwe"))

	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: newFakeCanonicalRegistry([]string{"person"}, []string{"is_about"}),
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1, "valid workflow registers")
	assert.Equal(t, "good", l.Workflows()[0].Name)
}

// TestLoader_AddCanonicalEdge_NilCanonicalRegistrySkips: when
// the loader is constructed without a CanonicalRegistry (test +
// dev path), add_canonical_edge actions register without
// validation. Plugins-side allowed_plugins validation still
// runs unaffected.
func TestLoader_AddCanonicalEdge_NilCanonicalRegistrySkips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "no-reg",
		addCanonicalEdgeWorkflowMarkdown("no-reg", "any_edge", "any_kind", "Whatever"))

	l := New(Options{
		Paths:          []string{dir},
		PluginRegistry: newFakeRegistry("yaad-gmail"),
		// CanonicalRegistry intentionally nil.
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1)
}

// addGapInlineSpecWorkflow returns a workflow markdown file with
// a single add_gap action that declares an inline GapSpec.
// Used by #142 loader-time validation tests.
func addGapInlineSpecWorkflow(name, gap, gapType string) string {
	return fmt.Sprintf(
		"---\nname: %s\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\naddable_gaps:\n  - %s\ntrigger:\n  type: manual\nactions:\n  - add_gap:\n      gap: %s\n      type: %s\n      kinds: [company]\n```\n",
		name, gap, gap, gapType,
	)
}

// TestLoader_AddGap_InlineTypeMatchesRegistry: workflow's
// inline Type matches the operator-config registered Type for
// the same gap name → workflow registers cleanly.
func TestLoader_AddGap_InlineTypeMatchesRegistry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "match",
		addGapInlineSpecWorkflow("match", "hiring_alert_for", "canonical_type"))

	reg := newFakeCanonicalRegistry(nil, nil).withGapType("hiring_alert_for", "canonical_type")
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: reg,
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1)
}

// TestLoader_AddGap_InlineTypeConflictRejected: workflow's
// inline Type contradicts the operator-config registered Type
// → reject at load time per #142.
func TestLoader_AddGap_InlineTypeConflictRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "conflict",
		addGapInlineSpecWorkflow("conflict", "hiring_alert_for", "canonical_type"))

	// Operator config has the gap registered as `int`; workflow
	// claims it's `canonical_type`. Reject.
	reg := newFakeCanonicalRegistry(nil, nil).withGapType("hiring_alert_for", "int")
	var logBuf bytes.Buffer
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: reg,
		Logger:            slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	require.NoError(t, l.Load(context.Background()))
	assert.Empty(t, l.Workflows(), "type conflict rejects the workflow file")
	assert.Contains(t, logBuf.String(), "hiring_alert_for")
	assert.Contains(t, logBuf.String(), "canonical_type")
	assert.Contains(t, logBuf.String(), "int")
}

// TestLoader_AddGap_InlineTypeNoRegistryEntry: workflow
// declares Type for a gap NOT in canonical_kinds → workflow
// stands alone, no conflict.
func TestLoader_AddGap_InlineTypeNoRegistryEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkflow(t, dir, "standalone",
		addGapInlineSpecWorkflow("standalone", "novel_gap", "canonical_type"))

	reg := newFakeCanonicalRegistry(nil, nil) // novel_gap unregistered
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: reg,
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1, "workflow with inline Type for unregistered gap stands alone")
}

// TestLoader_AddGap_NoInlineType_RegistryHasGap: workflow
// adds a gap WITHOUT declaring a Type → no conflict possible
// even when registry registers a Type for that gap name. The
// workflow simply inherits the registry's shape.
func TestLoader_AddGap_NoInlineType_RegistryHasGap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Workflow file without inline type.
	writeWorkflow(t, dir, "inherit",
		"---\nname: inherit\n---\n\n```yaml\nallowed_plugins:\n  - yaad-gmail\naddable_gaps:\n  - rating\ntrigger:\n  type: manual\nactions:\n  - add_gap:\n      gap: rating\n```\n")

	reg := newFakeCanonicalRegistry(nil, nil).withGapType("rating", "int")
	l := New(Options{
		Paths:             []string{dir},
		PluginRegistry:    newFakeRegistry("yaad-gmail"),
		CanonicalRegistry: reg,
	})
	require.NoError(t, l.Load(context.Background()))
	require.Len(t, l.Workflows(), 1)
}
