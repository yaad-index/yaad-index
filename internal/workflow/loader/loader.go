// Package loader implements the workflow discovery + hot-reload
// layer for the workflow engine per ADR-0024. It scans the
// configured workflow directories (vault-side
// `vault/workflows/` for operator-extensible workflows + a
// daemon-side path reserved for future system workflows),
// parses each `*.md` file via internal/workflow/parser,
// validates `allowed_plugins` against the live plugin registry,
// and maintains an in-memory registry keyed by Workflow.Name.
//
// The loader runs a polling ticker (default 15s, configurable)
// that re-scans paths on each tick:
//   - new files → parsed + registered.
//   - mtime-bumped files → re-parsed + re-registered (overwriting
//     the prior entry).
//   - removed files → unregistered.
//
// Rejected files (parse error, schema-validation failure,
// missing allowed_plugins) log a structured WARN line per file
// and DO NOT enter the registry. A subsequent edit that fixes
// the file lands on the next tick without daemon restart.
//
// **Out of scope for this package:**
//   - Subscribing parsed workflows to the event bus (Phase 3
//     decision-pipeline layer).
//   - Trigger/condition CEL evaluation (Phase 3).
//   - Action execution (Phase 4).

package loader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// DefaultPollInterval is the default mtime-polling cadence per
// the operator-suggested 10-30s range. 15s gives operators
// editing workflow files in the vault a snappy reload without
// hammering the filesystem.
const DefaultPollInterval = 15 * time.Second

// PluginRegistry is the load-time validation surface for
// `allowed_plugins`. The production plugin registry
// (internal/plugins.Registry) satisfies this via its
// LookupByName method; tests substitute a fake.
type PluginRegistry interface {
	// LookupByName returns (plugin, true) when a plugin with the
	// given name is currently loaded; (nil, false) otherwise.
	// The plugin value is opaque to the loader — it only cares
	// about presence — but matching the production registry
	// shape keeps the wiring trivial.
	LookupByName(name string) (any, bool)
}

// CanonicalRegistry is the load-time validation surface for
// add_canonical_edge action primitives (#132). The action names
// a literal edge_type + target.kind; both must be in the
// daemon's operator-configured registries at workflow-load
// time. A workflow referencing an unknown edge_type or
// target.kind is rejected at registration so the operator sees
// the typo before any fire.
type CanonicalRegistry interface {
	// KindExists reports whether the given canonical kind is
	// declared in the operator's canonical_kinds config.
	KindExists(kind string) bool

	// EdgeTypeExists reports whether the given edge type is
	// declared in the operator's canonical_edge_types config.
	EdgeTypeExists(edgeType string) bool
}

// Options configures a Loader. Paths is the list of directories
// to scan; PluginRegistry is the load-time validator; the
// remaining fields default sensibly when zero.
type Options struct {
	// Paths is the list of directories to scan for workflow
	// files. Each path is scanned non-recursively; only `*.md`
	// files in the top-level of the path are considered.
	// Empty paths are skipped (no error) so the loader is
	// happy in dev mode without vault configuration.
	Paths []string

	// PluginRegistry is consulted at load time to validate
	// every parsed workflow's `allowed_plugins` list. A nil
	// registry skips the check — useful for tests but should
	// not be used in production (workflow files with unloaded
	// plugins would parse + register without surfacing the
	// drift).
	PluginRegistry PluginRegistry

	// CanonicalRegistry is consulted at load time to validate
	// every parsed workflow's add_canonical_edge actions: the
	// literal edge_type must be in the daemon's
	// canonical_edge_types config, and target.kind in
	// canonical_kinds. Nil → skip the check (tests + dev
	// binaries without the operator config can still load
	// workflows).
	CanonicalRegistry CanonicalRegistry

	// PollInterval is the cadence of the polling reload loop.
	// Zero (the default) → DefaultPollInterval.
	PollInterval time.Duration

	// Logger receives the per-file accept/reject lines. Nil →
	// a discarding handler.
	Logger *slog.Logger
}

// Loader is the workflow discovery + registry surface.
// Construct via New; use Load for a one-shot scan or Run for
// the polling loop. Lookups + Snapshots are safe under
// concurrent reads.
type Loader struct {
	paths             []string
	pluginRegistry    PluginRegistry
	canonicalRegistry CanonicalRegistry
	pollInterval      time.Duration
	logger         *slog.Logger

	mu        sync.RWMutex
	workflows map[string]*parser.Workflow // by Workflow.Name
	perFile   map[string]string           // file path → workflow Name
	mtimes    map[string]time.Time        // file path → last-parsed mtime
	// collisionLogged tracks paths currently rejected for
	// name-collision so the loader doesn't spam WARN on every
	// poll while the collision persists. Collision-rejected
	// paths intentionally skip the mtimes cache so subsequent
	// polls re-attempt registration — a rejected file whose
	// colliding sibling later disappears has unchanged mtime
	// and would never re-register without the retry path.
	// Cleared when the rejection no longer applies (success /
	// parser-fail / missing-plugins) so re-collision after
	// recovery re-logs once.
	collisionLogged map[string]struct{}
}

// New constructs a Loader with the given options. Logger nil →
// discarding handler; PollInterval zero → DefaultPollInterval.
func New(opts Options) *Loader {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	interval := opts.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Loader{
		paths:             append([]string(nil), opts.Paths...),
		pluginRegistry:    opts.PluginRegistry,
		canonicalRegistry: opts.CanonicalRegistry,
		pollInterval:      interval,
		logger:            logger,
		workflows:       make(map[string]*parser.Workflow),
		perFile:         make(map[string]string),
		mtimes:          make(map[string]time.Time),
		collisionLogged: make(map[string]struct{}),
	}
}

// Load performs a single scan across all configured paths and
// rebuilds the in-memory registry. Returns nil on success;
// any I/O error walking a path returns immediately. Per-file
// parse / validate failures log + are excluded from the
// registry without failing the overall Load — operators see
// the structured rejection lines and can fix the offending
// files without daemon restart.
//
// Three-phase shape:
//
//  1. Enumerate `*.md` files across all configured paths
//     (no per-file parsing yet).
//  2. Drop perFile entries whose paths disappeared since the
//     last scan — must happen BEFORE per-file processing so
//     the collision-detection in maybeReloadFile sees a clean
//     state. (Without this, a collision-rejected file whose
//     prior-registrant file was removed between scans would
//     still see the stale prior entry and re-reject.)
//  3. Per-file parse + register.
func (l *Loader) Load(ctx context.Context) error {
	seen := make(map[string]struct{})
	var filesToScan []string
	for _, dir := range l.paths {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		paths, err := l.enumerateMarkdownFiles(ctx, dir)
		if err != nil {
			return err
		}
		for _, p := range paths {
			seen[p] = struct{}{}
			filesToScan = append(filesToScan, p)
		}
	}

	// Phase 2 — drop removed files BEFORE phase 3 per the
	// docstring rationale (avoid stale state on the collision
	// re-resolution path).
	l.mu.Lock()
	for path, name := range l.perFile {
		if _, kept := seen[path]; kept {
			continue
		}
		delete(l.workflows, name)
		delete(l.perFile, path)
		delete(l.mtimes, path)
		delete(l.collisionLogged, path)
		l.logger.InfoContext(ctx, "workflow file removed from registry",
			"path", path, "workflow_name", name)
	}
	// Clear collision-logged entries for paths that
	// disappeared between scans (they never registered, so
	// they're not in perFile; the loop above misses them).
	// Future reappearance re-logs once.
	for path := range l.collisionLogged {
		if _, kept := seen[path]; !kept {
			delete(l.collisionLogged, path)
		}
	}
	l.mu.Unlock()

	// Phase 3 — parse + register each enumerated file.
	for _, p := range filesToScan {
		l.maybeReloadFile(ctx, p)
	}
	return nil
}

// enumerateMarkdownFiles walks one directory non-recursively
// and returns the absolute paths of every `*.md` file (skip
// dotfiles + subdirectories). Used by Load's phase-1 sweep
// before per-file parsing runs; the file-removal sweep in
// phase 2 reads the returned paths against perFile.
func (l *Loader) enumerateMarkdownFiles(ctx context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Empty dir is acceptable: operator may have a
			// vault configured but no workflows authored yet.
			// Logged at INFO so it's visible without being
			// noisy.
			l.logger.InfoContext(ctx, "workflow directory missing; no workflows loaded from this path",
				"path", dir)
			return nil, nil
		}
		return nil, fmt.Errorf("workflow loader: read dir %q: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// maybeReloadFile checks the file's mtime against the
// last-parsed cache; if unchanged, the existing registry entry
// stays. Otherwise re-parse + re-register (or reject + drop
// the entry if it was previously registered).
func (l *Loader) maybeReloadFile(ctx context.Context, path string) {
	info, err := os.Stat(path)
	if err != nil {
		l.logger.WarnContext(ctx, "workflow file stat failed; skipping",
			"path", path, "err", err)
		return
	}
	mtime := info.ModTime().UTC()

	l.mu.RLock()
	prevMtime, hasPrev := l.mtimes[path]
	l.mu.RUnlock()
	if hasPrev && prevMtime.Equal(mtime) {
		// Unchanged since last successful parse — keep
		// existing registry entry.
		return
	}

	wf, err := parser.ParseFile(path)
	if err != nil {
		// Parse / validate failure — drop any previous
		// registration for this path so a previously-good
		// workflow file that the operator broke surfaces as
		// "removed from registry" rather than silently
		// keeping a stale version.
		l.mu.Lock()
		if prevName, was := l.perFile[path]; was {
			delete(l.workflows, prevName)
			delete(l.perFile, path)
		}
		// Track the mtime even on failure so we don't spam
		// the log re-parsing the same broken file every tick.
		l.mtimes[path] = mtime
		// Clear collision-logged flag: a parser-rejected file
		// is no longer collision-state, so a recovery + re-
		// collision should re-log once.
		delete(l.collisionLogged, path)
		l.mu.Unlock()
		l.logger.WarnContext(ctx, "workflow file rejected",
			"path", path, "err", err)
		return
	}

	// Allowed_plugins live-registry check. Per ADR-0024 §
	// "Workflow declares its plugin scope": missing plugins
	// reject the workflow at load time.
	if l.pluginRegistry != nil {
		var missing []string
		for _, name := range wf.AllowedPlugins {
			if _, ok := l.pluginRegistry.LookupByName(name); !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			l.mu.Lock()
			if prevName, was := l.perFile[path]; was {
				delete(l.workflows, prevName)
				delete(l.perFile, path)
			}
			l.mtimes[path] = mtime
			delete(l.collisionLogged, path)
			l.mu.Unlock()
			l.logger.WarnContext(ctx, "workflow file rejected: allowed_plugins not loaded",
				"path", path,
				"workflow_name", wf.Name,
				"missing_plugins", missing)
			return
		}
	}

	// add_canonical_edge: literal edge_type + target.kind
	// validation against the daemon's canonical_edge_types +
	// canonical_kinds registries (#132). Defended against the
	// typo case at workflow-load time so the operator sees the
	// rejection before any action fires.
	if l.canonicalRegistry != nil {
		if unknownEdge, unknownKind := canonicalEdgeAuthorBugs(wf, l.canonicalRegistry); len(unknownEdge) > 0 || len(unknownKind) > 0 {
			l.mu.Lock()
			if prevName, was := l.perFile[path]; was {
				delete(l.workflows, prevName)
				delete(l.perFile, path)
			}
			l.mtimes[path] = mtime
			delete(l.collisionLogged, path)
			l.mu.Unlock()
			l.logger.WarnContext(ctx, "workflow file rejected: add_canonical_edge references unknown edge_type or target.kind",
				"path", path,
				"workflow_name", wf.Name,
				"unknown_edge_types", unknownEdge,
				"unknown_target_kinds", unknownKind)
			return
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Name collision check: two files declaring the same
	// workflow name is an operator-side authoring error.
	// Reject the LATER one (alphabetical by path, since
	// scanPath visits in directory order); the first one
	// keeps the registry slot.
	//
	// Collision-rejected paths intentionally SKIP the mtimes
	// cache so subsequent polls re-attempt registration —
	// without this, a colliding file keeps its rejection
	// forever even after the prior registrant file is removed
	// (its mtime hasn't changed). The collisionLogged set
	// suppresses re-logging the rejection on every poll while
	// the collision persists.
	if priorPath, exists := nameRegisteredElsewhere(l.perFile, wf.Name, path); exists {
		if _, alreadyLogged := l.collisionLogged[path]; !alreadyLogged {
			l.logger.WarnContext(ctx, "workflow file rejected: name collision",
				"path", path,
				"workflow_name", wf.Name,
				"prior_path", priorPath)
			l.collisionLogged[path] = struct{}{}
		}
		return
	}
	// If this path previously mapped to a different name (operator
	// renamed the workflow), drop the prior registration.
	if prevName, was := l.perFile[path]; was && prevName != wf.Name {
		delete(l.workflows, prevName)
	}
	l.workflows[wf.Name] = wf
	l.perFile[path] = wf.Name
	l.mtimes[path] = mtime
	// Clear any prior collision-logged flag so a future
	// collision (e.g., operator re-adds a colliding file)
	// re-logs once.
	delete(l.collisionLogged, path)
	l.logger.InfoContext(ctx, "workflow file registered",
		"path", path,
		"workflow_name", wf.Name,
		"trigger_type", wf.Trigger.Type,
		"status", wf.Status)
}

// nameRegisteredElsewhere checks whether the given workflow
// name is already mapped to a different path in perFile.
// Returns (priorPath, true) on collision; ("", false)
// otherwise. The skipPath argument is the path currently
// being registered — it's excluded from the collision check
// since re-registering the same path under the same name is
// the normal mtime-bump path.
func nameRegisteredElsewhere(perFile map[string]string, name, skipPath string) (string, bool) {
	for path, n := range perFile {
		if path == skipPath {
			continue
		}
		if n == name {
			return path, true
		}
	}
	return "", false
}

// Run starts the polling reload loop. Performs an initial Load
// + ticks on PollInterval until ctx is cancelled. Returns the
// context's err on cancel.
func (l *Loader) Run(ctx context.Context) error {
	if err := l.Load(ctx); err != nil {
		return fmt.Errorf("workflow loader: initial load: %w", err)
	}
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := l.Load(ctx); err != nil {
				l.logger.WarnContext(ctx, "workflow loader: reload failed; will retry on next tick",
					"err", err)
			}
		}
	}
}

// Workflows returns a snapshot of every currently-registered
// workflow, sorted by Name for deterministic iteration. Safe
// to call from any goroutine; the returned slice is freshly
// allocated and not aliased to internal state.
func (l *Loader) Workflows() []*parser.Workflow {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*parser.Workflow, 0, len(l.workflows))
	for _, wf := range l.workflows {
		out = append(out, wf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the workflow with the given name. (nil, false)
// when no workflow by that name is currently registered.
func (l *Loader) Lookup(name string) (*parser.Workflow, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	wf, ok := l.workflows[name]
	return wf, ok
}

// canonicalEdgeAuthorBugs walks the workflow's add_canonical_edge
// actions and returns the literal edge_type + target.kind values
// that don't appear in the canonical registry. Empty slices when
// every reference checks out (or the workflow has no
// add_canonical_edge actions). Used by maybeReloadFile to reject
// workflows referencing unknown registry entries at load time so
// the operator sees the rejection before any action fires.
func canonicalEdgeAuthorBugs(wf *parser.Workflow, reg CanonicalRegistry) (unknownEdge, unknownKind []string) {
	for _, a := range wf.Actions {
		if a.AddCanonicalEdge == nil {
			continue
		}
		if !reg.EdgeTypeExists(a.AddCanonicalEdge.EdgeType) {
			unknownEdge = append(unknownEdge, a.AddCanonicalEdge.EdgeType)
		}
		if !reg.KindExists(a.AddCanonicalEdge.TargetKind) {
			unknownKind = append(unknownKind, a.AddCanonicalEdge.TargetKind)
		}
	}
	return unknownEdge, unknownKind
}
