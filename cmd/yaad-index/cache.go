// Cache management CLI subcommands per alice2-index.
//
// Cache lifecycle is vault-driven (per ADR-0008): past-expiry only
// gates the lookup path, NOT the on-disk state. An expired entity
// sits in vault + DB until either an ingest call falls through OR
// an operator runs one of these CLI commands.
//
// Subcommands:
// - `list-expired`: read-only probe (operator's "what's stale?"
// view).
// - `purge`: delete vault files for matched entries; reindex
// reconciles DB rows.
// - `refetch`: force-refetch via the running daemon's HTTP API.
//
// Per alice2-index PR-B: TTL resolution at ingest produces an
// absolute-date `cache_expires:` stamp; the CLI reads that stamp
// directly. Legacy entries that still carry `cache_ttl_seconds:`
// don't participate in the predicate — operators bulk-migrate
// them via `cache refetch` (which re-ingests and re-stamps).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/reindex"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// CacheCmd is the parent subcommand grouping cache management
// operations (per alice2-index). The kong tree mounts it under
// `alice2-index cache <subcommand>`.
type CacheCmd struct {
	ListExpired CacheListExpiredCmd `cmd:"list-expired" help:"List vault entries whose cache TTL has expired."`
	Purge CachePurgeCmd `cmd:"purge" help:"Delete expired vault entries; reindex reconciles DB rows."`
	Refetch CacheRefetchCmd `cmd:"refetch" help:"Force-refetch expired entries via a running alice2-index daemon (preserves user-added comments / UGC Per the prior design,/)."`
}

// CacheListExpiredCmd implements `alice2-index cache list-expired`.
// Read-only walk of the vault root: prints every entity whose
// frontmatter `cache_expires:` is in the past (per alice2-index).
//
// Filters:
//
// - --plugin restricts to entities whose `plugin:` field matches.
// - --kind restricts to entities of the given kind.
// - --include-infinite includes entities with the `never` sentinel
// (which never expire); excluded by default since they can't be
// expired.
//
// Output is tab-separated, suitable for piping through `column -t`
// for visual alignment OR for awk/cut-style processing.
type CacheListExpiredCmd struct {
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/alice2-index/config.yaml" help:"path to the alice2-index config file. vault.path is read from here unless --vault-path overrides."`
	VaultPath string `name:"vault-path" env:"YAAD_INDEX_VAULT_PATH" help:"override vault root (otherwise read from config.vault.path). Must be absolute."`
	Plugin string `name:"plugin" help:"filter to entities whose Plugin frontmatter field matches this value."`
	Kind string `name:"kind" help:"filter to entities of the given kind."`
	IncludeInfinite bool `name:"include-infinite" help:"include entries with the 'never' cache_expires sentinel (which never expire); default excluded."`
}

// expiredEntry is one row of the list-expired output. Captured as a
// slice (rather than streaming directly to stdout) so tests can
// assert on the discovered set without parsing tab-formatted text.
//
// Per alice2-index PR-B: the row carries the absolute expiry
// date (or the Never sentinel) instead of's duration field.
// Legacy entries with cache_ttl_seconds only never reach this
// row — they don't participate in the list-expired predicate
// post-PR-B.
type expiredEntry struct {
	ID string
	Kind string
	Plugin string
	FetchedAt time.Time // zero if no qualifying provenance entry
	ExpiresAt time.Time // zero when ExpiresNever; otherwise the cache_expires stamp
	Never bool // true when CacheExpires.Never is set
	Age time.Duration
}

// Run executes the list-expired CLI subcommand.
func (c *CacheListExpiredCmd) Run() error {
	levelVar := new(slog.LevelVar)
	levelVar.Set(config.DefaultLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))

	vaultPath, err := resolveVaultPath(logger, c.ConfigPath, c.VaultPath)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	rows, err := findExpiredCacheEntries(logger, vaultPath, c.Plugin, c.Kind, c.IncludeInfinite, now)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	// tabwriter.Writer buffers; per-write errors before Flush are
	// transient and surface on Flush below. Discarding the per-write
	// returns keeps the loop clean.
	_, _ = fmt.Fprintln(w, "ID\tKIND\tPLUGIN\tFETCHED_AT\tEXPIRES\tAGE")
	for _, r := range rows {
		fetchedField, ageField := formatFetchedAge(r)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Kind, r.Plugin, fetchedField, formatExpires(r), ageField)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}
	if len(rows) == 0 {
		// stderr-side helpful message; a write failure here is
		// non-fatal (the empty-stdout result is the load-bearing
		// signal for callers).
		_, _ = fmt.Fprintln(os.Stderr, "no expired entries.")
	}
	return nil
}

// findExpiredCacheEntries walks vaultPath and returns one row per
// vault file that matches the filter set AND meets the expired
// predicate (cacheEntryExpired) at `now`. Read-only; doesn't touch
// the store. Errors only on filesystem traversal failures; per-file
// read errors log + skip so a single bad file doesn't block the
// whole listing.
//
// Exposed (lowercase) so the test file in the same package can
// drive it without going through the kong CLI surface.
func findExpiredCacheEntries(logger *slog.Logger, vaultPath, pluginFilter, kindFilter string, includeInfinite bool, now time.Time) ([]expiredEntry, error) {
	reader, err := vault.NewReader(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("init vault reader at %s: %w", vaultPath, err)
	}
	var out []expiredEntry
	walkErr := filepath.WalkDir(vaultPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			logger.Warn("walk error", "path", path, "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		ent, err := reader.ReadFile(path)
		if err != nil {
			logger.Warn("vault read failed", "path", path, "err", err)
			return nil
		}
		if pluginFilter != "" && ent.Plugin != pluginFilter {
			return nil
		}
		if kindFilter != "" && ent.Kind != kindFilter {
			return nil
		}
		if !cacheEntryExpired(ent, now, includeInfinite) {
			return nil
		}
		row := expiredEntry{
			ID: ent.ID,
			Kind: ent.Kind,
			Plugin: ent.Plugin,
		}
		if ent.CacheExpires != nil {
			row.Never = ent.CacheExpires.Never
			row.ExpiresAt = ent.CacheExpires.Time
		}
		freshest := freshestPersistentFetchVault(ent)
		if !freshest.IsZero() {
			row.FetchedAt = freshest
			row.Age = now.Sub(freshest)
		}
		out = append(out, row)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk vault root %s: %w", vaultPath, walkErr)
	}
	return out, nil
}

// cacheEntryExpired returns true when the entity should appear on
// the `list-expired` output per alice2-index's cache_expires
// stamp:
//
// - nil CacheExpires → no opinion; cache forever; NOT expired.
// - CacheExpires.Never → "infinite" sentinel; never expires.
// Surfaced only when --include-infinite is set.
// - CacheExpires.Time → expired iff now > Time.
//
// Legacy entries (cache_ttl_seconds only) cache forever — they
// don't participate in the gate until force_refetch re-ingests
// and stamps cache_expires. Operators bulk-migrate via's
// `cache refetch` CLI.
func cacheEntryExpired(e *vault.Entity, now time.Time, includeInfinite bool) bool {
	if e.CacheExpires == nil {
		return false
	}
	if e.CacheExpires.Never {
		return includeInfinite
	}
	return now.After(e.CacheExpires.Time)
}

// freshestPersistentFetchVault is the vault.Entity counterpart to
// internal/api's freshestPersistentFetch (which works on
// store.Entity). Walks the entity's provenance, picks the latest
// fetched_at across OK=true rows whose source is NOT
// `cache:notations` — same exclusion the lookup-first path uses so
// a cache hit's ephemeral provenance can't extend the entity's life.
func freshestPersistentFetchVault(e *vault.Entity) time.Time {
	var latest time.Time
	for _, p := range e.Provenance {
		if !p.OK {
			continue
		}
		if p.Source == cacheNotationsSource {
			continue
		}
		if p.FetchedAt == nil {
			continue
		}
		t := *p.FetchedAt
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// cacheNotationsSource is the provenance.source string used by the
// lookup-first cache path to mark its ephemeral cache-hit response
// (per alice2-index a prior PR). Defined here to avoid pulling
// internal/api into the cmd binary's import graph.
const cacheNotationsSource = "cache:notations"

// CachePurgeCmd implements `alice2-index cache purge`. Deletes vault
// files for expired entries (per alice2-index's sentinel rules)
// and runs reindex to drop the corresponding store rows via the
// disappear-pass reconcile.
//
// Defaults to --dry-run for safety: a fresh invocation prints what
// would be deleted without touching disk. The operator re-runs with
// --dry-run=false to actually purge.
//
// Filters narrow the predicate. --older-than DURATION OVERRIDES the
// TTL-expired predicate: instead of "expired per cache_ttl_seconds",
// the predicate becomes "freshest fetched_at older than DURATION"
// regardless of stamped TTL. Useful for bulk-clearing entries that
// aren't TTL-stamped (e.g. legacy deployments).
type CachePurgeCmd struct {
	DBPath string `name:"db-path" env:"YAAD_INDEX_DB_PATH" default:"~/.local/share/alice2-index/alice2.db" help:"path to the SQLite database file (matches the serve subcommand)."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/alice2-index/config.yaml" help:"path to the alice2-index config file. vault.path is read from here unless --vault-path overrides."`
	VaultPath string `name:"vault-path" env:"YAAD_INDEX_VAULT_PATH" help:"override vault root (otherwise read from config.vault.path). Must be absolute."`
	Plugin string `name:"plugin" help:"filter to entities whose Plugin frontmatter field matches this value."`
	Kind string `name:"kind" help:"filter to entities of the given kind."`
	OlderThan string `name:"older-than" help:"override the TTL-expired predicate: purge entries whose freshest fetched_at is older than this duration (e.g. 720h, 30d-equivalent). Empty → use cache_ttl_seconds expiry."`
	DryRun bool `name:"dry-run" default:"true" help:"print what would be deleted without touching disk (default true for safety; pass --dry-run=false to actually purge)."`
}

// Run executes the purge CLI subcommand.
func (c *CachePurgeCmd) Run() error {
	levelVar := new(slog.LevelVar)
	levelVar.Set(config.DefaultLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))

	vaultPath, err := resolveVaultPath(logger, c.ConfigPath, c.VaultPath)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	var rows []expiredEntry
	if c.OlderThan != "" {
		dur, err := time.ParseDuration(c.OlderThan)
		if err != nil {
			return fmt.Errorf("parse --older-than %q: %w", c.OlderThan, err)
		}
		if dur <= 0 {
			return fmt.Errorf("--older-than must be positive; got %s", dur)
		}
		rows, err = findOlderThanCacheEntries(logger, vaultPath, c.Plugin, c.Kind, dur, now)
		if err != nil {
			return err
		}
	} else {
		rows, err = findExpiredCacheEntries(logger, vaultPath, c.Plugin, c.Kind, false, now)
		if err != nil {
			return err
		}
	}

	if len(rows) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "no entries match the purge predicate.")
		return nil
	}

	if c.DryRun {
		_, _ = fmt.Fprintln(os.Stderr, "DRY RUN — pass --dry-run=false to actually purge. Matched entries:")
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tKIND\tPLUGIN\tFETCHED_AT\tEXPIRES\tAGE")
		for _, r := range rows {
			fetchedField, ageField := formatFetchedAge(r)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.ID, r.Kind, r.Plugin, fetchedField, formatExpires(r), ageField)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("flush dry-run output: %w", err)
		}
		return nil
	}

	// Real purge path: delete vault file per match. Use vault.Writer's
	// DeleteWithCommit so a wired Committer (when present) lands a
	// `delete:` audit commit. The CLI doesn't construct a committer
	// here — operator can git-commit manually after, OR re-run the
	// daemon's auto-commit pipeline catches up on next write.
	vaultWriter, err := vault.NewWriter(vaultPath, vault.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("init vault writer: %w", err)
	}

	var purged int
	for _, r := range rows {
		if err := vaultWriter.DeleteWithCommit(context.Background(), r.Kind, r.ID, "", ""); err != nil {
			logger.Warn("delete vault file failed; continuing", "id", r.ID, "kind", r.Kind, "err", err)
			continue
		}
		_, _ = fmt.Fprintf(os.Stdout, "purged %s\n", r.ID)
		purged++
	}

	if purged == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "no entries purged (all delete attempts failed; see logs).")
		return nil
	}

	// Reconcile: reindex's incremental pass picks up the disappeared
	// files and drops the corresponding store rows + edges + provenance
	// + notations via DeleteEntityCascade. Same disappear-pass that
	// handles hand-edits.
	dbPath, err := expandPath(c.DBPath)
	if err != nil {
		return fmt.Errorf("expand --db-path %q: %w", c.DBPath, err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	// nil guard is safe here: the post-purge reconcile is incremental,
	// so unchanged vault files are skip-counted (no re-parse, no
	// edge re-write). Only files whose mtime/hash changed get re-
	// parsed, and the cache purge path doesn't touch source files —
	// it only deletes them — so this loop only ever exercises the
	// disappear-pass (DeleteEntityCascade), not the apply-edges path.
	reindexer, err := reindex.New(st, vaultPath, nil, logger)
	if err != nil {
		return fmt.Errorf("init reindex: %w", err)
	}
	summary, err := reindexer.Run(context.Background(), reindex.Incremental)
	if err != nil {
		return fmt.Errorf("post-purge reindex: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stderr,
		"purged %d entries; reindex deleted %d store rows.\n",
		purged, summary.EntitiesDeleted)
	return nil
}

// findOlderThanCacheEntries is the --older-than variant of the
// expired-predicate walk: ignores cache_ttl_seconds entirely and
// matches entries whose freshest qualifying fetched_at is older
// than `dur`. Used by `purge --older-than DURATION`.
//
// Entries with no qualifying fetched_at at all (corrupt state)
// match unconditionally — same defensive shape as the TTL path.
// Negative-TTL entries (sentinel: never expire) ALSO match: when
// the operator passes --older-than they're saying "ignore the
// stamped policy, age trumps".
func findOlderThanCacheEntries(logger *slog.Logger, vaultPath, pluginFilter, kindFilter string, dur time.Duration, now time.Time) ([]expiredEntry, error) {
	reader, err := vault.NewReader(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("init vault reader at %s: %w", vaultPath, err)
	}
	var out []expiredEntry
	walkErr := filepath.WalkDir(vaultPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			logger.Warn("walk error", "path", path, "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		ent, err := reader.ReadFile(path)
		if err != nil {
			logger.Warn("vault read failed", "path", path, "err", err)
			return nil
		}
		if pluginFilter != "" && ent.Plugin != pluginFilter {
			return nil
		}
		if kindFilter != "" && ent.Kind != kindFilter {
			return nil
		}
		freshest := freshestPersistentFetchVault(ent)
		if !freshest.IsZero() && now.Sub(freshest) <= dur {
			return nil
		}
		row := expiredEntry{
			ID: ent.ID,
			Kind: ent.Kind,
			Plugin: ent.Plugin,
		}
		if ent.CacheExpires != nil {
			row.Never = ent.CacheExpires.Never
			row.ExpiresAt = ent.CacheExpires.Time
		}
		if !freshest.IsZero() {
			row.FetchedAt = freshest
			row.Age = now.Sub(freshest)
		}
		out = append(out, row)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk vault root %s: %w", vaultPath, walkErr)
	}
	return out, nil
}

// CacheRefetchCmd implements `alice2-index cache refetch`. Force-
// refetches expired vault entries by issuing `POST /v1/ingest` with
// `force_refetch=true` against a RUNNING alice2-index daemon. The
// daemon's existing ingest path (post- +/) preserves
// user-added comments and UGC sections by design (the
// buildVaultEntity merge inherits Comments/Tags/Summary/Edges from
// the existing vault file before stamping fresh plugin-provided
// fields) — refetch via this CLI inherits that contract for free.
//
// CLI mode (vs daemon-direct): refetch requires an active daemon
// for two reasons:
//
// 1. The plugin registry needs to be live (subprocess plugins
// can't be cheaply spun up + torn down per CLI call).
// 2. The vault writer + store must be the SAME instance the daemon
// is using; concurrent writers to the same vault/store would
// race on the auto-commit pipeline.
//
// Operators with no running daemon: start serve, then run refetch.
type CacheRefetchCmd struct {
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/alice2-index/config.yaml" help:"path to the alice2-index config file. vault.path is read from here unless --vault-path overrides."`
	VaultPath string `name:"vault-path" env:"YAAD_INDEX_VAULT_PATH" help:"override vault root (otherwise read from config.vault.path). Must be absolute."`
	Server string `name:"server" env:"YAAD_INDEX_SERVER" default:"http://127.0.0.1:7433" help:"URL of the running alice2-index daemon."`
	Token string `name:"token" env:"YAAD_INDEX_AUTH_TOKEN" help:"Bearer JWT for the daemon's auth.required=true mode (omit for anonymous bypass)."`
	Plugin string `name:"plugin" help:"filter to entities whose Plugin frontmatter field matches."`
	Kind string `name:"kind" help:"filter to entities of the given kind."`
	Limit int `name:"limit" default:"0" help:"maximum number of refetches in this invocation; 0 means no limit. Useful for rate-bounded operator workflows."`
}

// Run executes the refetch CLI subcommand.
func (c *CacheRefetchCmd) Run() error {
	levelVar := new(slog.LevelVar)
	levelVar.Set(config.DefaultLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar, ReplaceAttr: clock.LogTimeAttr}))

	vaultPath, err := resolveVaultPath(logger, c.ConfigPath, c.VaultPath)
	if err != nil {
		return err
	}
	if c.Server == "" {
		return errors.New("--server is required (refetch invokes the daemon's /v1/ingest with force_refetch=true)")
	}

	now := time.Now().UTC()
	rows, err := findExpiredCacheEntries(logger, vaultPath, c.Plugin, c.Kind, false, now)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "no expired entries to refetch.")
		return nil
	}

	// Need each entity's notations list to pick a URL that the
	// daemon's plugin registry recognizes. Re-read the vault entity
	// via vault.Reader for the notations slice (findExpiredCacheEntries
	// returned id/kind/plugin only).
	reader, err := vault.NewReader(vaultPath)
	if err != nil {
		return fmt.Errorf("init vault reader at %s: %w", vaultPath, err)
	}

	limit := c.Limit
	if limit <= 0 {
		limit = len(rows)
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	ingestURL := strings.TrimRight(c.Server, "/") + "/v1/ingest"

	var refetched, failed int
	for i, r := range rows {
		if i >= limit {
			break
		}
		ent, err := reader.ReadByID(r.Kind, r.ID)
		if err != nil {
			logger.Warn("re-read vault entity failed; skipping", "id", r.ID, "err", err)
			failed++
			continue
		}
		notation := pickRefetchNotation(ent.Notations)
		if notation == "" {
			logger.Warn("entity has no notations; cannot refetch", "id", r.ID)
			failed++
			continue
		}
		if err := postForceRefetch(httpClient, ingestURL, c.Token, notation); err != nil {
			logger.Warn("refetch POST failed", "id", r.ID, "url", notation, "err", err)
			failed++
			continue
		}
		_, _ = fmt.Fprintf(os.Stdout, "refetched %s (via %s)\n", r.ID, notation)
		refetched++
	}

	_, _ = fmt.Fprintf(os.Stderr,
		"refetched %d entries; %d failed (see logs); %d remaining over --limit.\n",
		refetched, failed, max(0, len(rows)-limit))
	return nil
}

// pickRefetchNotation chooses a notation to use for the refetch
// POST. Prefers an http(s) URL since plugin Match() patterns
// usually match URL forms; falls back to the first non-empty
// entry. Empty notations slice returns empty string.
func pickRefetchNotation(notations []string) string {
	for _, n := range notations {
		if strings.HasPrefix(n, "http://") || strings.HasPrefix(n, "https://") {
			return n
		}
	}
	for _, n := range notations {
		if n != "" {
			return n
		}
	}
	return ""
}

// postForceRefetch issues the actual HTTP request to the daemon.
// Synchronous: waits for the daemon's response so per-entity
// failures surface immediately (logged + counted; the loop
// continues with the next entity).
func postForceRefetch(client *http.Client, ingestURL, token, url string) error {
	body := map[string]any{
		"url": url,
		"force_refetch": true,
		"wait_seconds": 30,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, ingestURL, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// formatFetchedAge centralizes the dry-run / list-expired output
// formatting used by both purge and list-expired. Renders the
// fetched-at time in the operator-configured location (per alice2-
// index) so CLI output reads in the operator's local TZ.
func formatFetchedAge(r expiredEntry) (string, string) {
	if r.FetchedAt.IsZero() {
		return "<none>", "<unknown>"
	}
	return clock.In(r.FetchedAt).Format(time.RFC3339), r.Age.Truncate(time.Second).String()
}

// formatExpires renders the cache_expires column for list-expired
// + purge-dry-run output (per alice2-index PR-B). Three shapes:
//
// - r.Never → "never" (sentinel)
// - r.ExpiresAt non-zero → ISO date in operator-TZ
// - both unset → "<none>" (defensive — entry that
// somehow reached the row without a stamp)
func formatExpires(r expiredEntry) string {
	switch {
	case r.Never:
		return "never"
	case !r.ExpiresAt.IsZero():
		return clock.In(r.ExpiresAt).Format(time.RFC3339)
	default:
		return "<none>"
	}
}

// resolveVaultPath consolidates the config-load + vault-path-override
// dance shared with ReindexCmd. Returns the absolute, expanded vault
// root path. Errors when the path can't be resolved (no config + no
// override).
//
// Also applies cfg.Timezone to the clock package as a side effect
// (per alice2-index PR-C) so subsequent log lines + CLI output
// use the operator-configured location.
func resolveVaultPath(logger *slog.Logger, configPath, override string) (string, error) {
	vaultPath := override
	cfg, err := loadConfigOptional(logger, configPath)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	if cfg != nil {
		if vaultPath == "" {
			vaultPath = cfg.Vault.Path
		}
		if cfg.Timezone != "" {
			tzLoc, perr := time.LoadLocation(cfg.Timezone)
			if perr != nil {
				return "", fmt.Errorf("parse timezone %q: %w", cfg.Timezone, perr)
			}
			clock.SetLocation(tzLoc)
		}
	}
	if vaultPath == "" {
		return "", errors.New("vault path is required (set vault.path in config or pass --vault-path)")
	}
	expanded, err := expandPath(vaultPath)
	if err != nil {
		return "", fmt.Errorf("expand vault path %q: %w", vaultPath, err)
	}
	return expanded, nil
}
