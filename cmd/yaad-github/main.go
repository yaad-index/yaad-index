// Command yaad-github is the GitHub PR + issue extractor binary
// for yaad-index, implementing the subprocess plugin protocol
// from ADR-0006 + ADR-0022 + ADR-0023 per the ADR-0026 design.
//
// CLI modes:
//
//   - `yaad-github --version` — write the version string and
//     exit. Daemon startup cache-key probe.
//
//   - `yaad-github --init` — write the capabilities JSON
//     (name, version, url_patterns, entity_kinds,
//     canonical_kinds_emitted, canonical_edge_types_emitted,
//     commands, source_namespace, cache_ttl_seconds) and exit.
//     Daemon-driven; URL patterns are interpolated from the
//     plugin's name (operator may override per instance) and
//     YAAD_GITHUB_BASE_URL so multi-instance setups per
//     ADR-0026 §7 get correct dispatch routing.
//
//   - `yaad-github` (no args) — read a JSON request from stdin
//     (URL-shape ingest per ADR-0021); fetch the single PR or
//     issue; emit one source-shape envelope per ADR-0023.
//     **Cut 1: stubbed — returns "not implemented yet"; lands
//     in Cut 2.**
//
//   - `yaad-github --command fetch` — bulk pass across the
//     configured `repos:` list per ADR-0026 §1. Emits NDJSON
//     envelopes per ADR-0023 + a trailing `_summary` packet.
//     **Cut 1: stubbed — returns "not implemented yet"; lands
//     in Cut 3.**
//
// Cut 1 scope: scaffold + capabilities + version + auth wiring
// + base-URL-interpolated URL patterns. Fetch + command paths
// follow in Cuts 2-3 per the issue #187 breakdown.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/yaad-index/yaad-index/internal/github"
)

// instanceName resolves the operator-side plugin name from env.
// Operators set this to the plugin's `name:` config entry per
// ADR-0026 §7 (e.g. `github-personal`, `github-work`) so the
// shorthand-URL pattern + the command-shape sigil match the
// operator's invocation surface.
//
// Defaults to github.PluginName ("github") when unset — keeps
// single-instance setups + tests working without ceremony.
const EnvInstanceName = "YAAD_GITHUB_INSTANCE_NAME"

// authTimeout caps the wall-clock budget for the startup
// `GET /user` call that resolves the operator login. Kept short:
// the daemon's plugin-startup window doesn't tolerate a stuck
// network round-trip, and ResolveUserLogin is the only point
// where we wait on github.com.
const authTimeout = 10 * time.Second

func main() {
	exit := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(exit)
}

// run is the testable entry point. Returns the exit code rather
// than calling os.Exit so tests can drive runInit / runVersion
// directly with a buffer pair. Exit codes: 0 success, 1 runtime
// error, 2 bad flags.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yaad-github", flag.ContinueOnError)
	fs.SetOutput(stderr)
	initMode := fs.Bool("init", false,
		"emit the capabilities document on stdout and exit (called by yaad-index at startup)")
	versionMode := fs.Bool("version", false,
		"print the plugin version and exit (called by yaad-index's cache-key probe)")
	commandName := fs.String("command", "",
		"named command to run (per ADR-0022 §1). Today the only declared command is `fetch` — bulk pass across configured repos.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *versionMode {
		_, _ = fmt.Fprintln(stdout, github.PluginVersion)
		return 0
	}

	if *initMode {
		if err := runInit(stdout); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-github --init: %v\n", err)
			return 1
		}
		return 0
	}

	if *commandName != "" {
		// Command-shape dispatch per ADR-0022 + ADR-0026 §1.
		// Unknown commands return exit 2 (flag-error) so an
		// operator who mistypes sees a clear error rather
		// than silently falling through.
		if *commandName != github.CommandFetch {
			_, _ = fmt.Fprintf(stderr,
				"yaad-github: unknown --command %q (declared: %v)\n",
				*commandName, github.DeclaredCommands)
			return 2
		}
		if err := runCommandFetch(stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "yaad-github --command fetch: %v\n", err)
			return 1
		}
		return 0
	}

	// URL-shape stdin ingest. Reads the `{operation, url}`
	// request body the daemon writes per ADR-0005, parses
	// the URL into a target, fetches the PR/issue, and emits
	// one source-shape envelope per ADR-0023 on stdout.
	if err := runURLShapeFetch(stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "yaad-github: %v\n", err)
		return 1
	}
	return 0
}

// capabilitiesDoc mirrors the wire shape yaad-index's
// subprocess.Capabilities decodes. Field tags match the
// daemon-side struct so a future schema change shows up here as
// a compile-time mismatch via the structural-equivalence tests.
type capabilitiesDoc struct {
	Name                      string         `json:"name"`
	Version                   string         `json:"version"`
	URLPatterns               []string       `json:"url_patterns"`
	EntityKinds               []kindSpecJSON `json:"entity_kinds"`
	EdgeKinds                 []kindSpecJSON `json:"edge_kinds"`
	CanonicalKindsEmitted     []string       `json:"canonical_kinds_emitted,omitempty"`
	CanonicalEdgeTypesEmitted []string       `json:"canonical_edge_types_emitted,omitempty"`
	SupportsSearch            bool           `json:"supports_search,omitempty"`
	SourceNamespace           string         `json:"source_namespace,omitempty"`
	CacheTTLSeconds           int            `json:"cache_ttl_seconds,omitempty"`
	Commands                  []string       `json:"commands,omitempty"`
}

type kindSpecJSON struct {
	Name           string `json:"name"`
	DefaultTTLDays int    `json:"default_ttl_days,omitempty"`
}

// runInit emits the capabilities document per ADR-0026 §1. URL
// patterns interpolate from EnvInstanceName + EnvBaseURL so
// multi-instance setups get correctly-scoped dispatch routing
// per ADR-0026 §7.
func runInit(stdout io.Writer) error {
	instance := os.Getenv(EnvInstanceName)
	baseURL := os.Getenv(github.EnvBaseURL)

	doc := capabilitiesDoc{
		Name:                      github.PluginName,
		Version:                   github.PluginVersion,
		URLPatterns:               github.BuildURLPatterns(instance, baseURL),
		EntityKinds:               []kindSpecJSON{{Name: github.UniversalSourceKind}},
		EdgeKinds:                 []kindSpecJSON{},
		CanonicalKindsEmitted:     github.KnownCanonicalKinds,
		CanonicalEdgeTypesEmitted: github.KnownCanonicalEdgeTypes,
		SupportsSearch:            false,
		SourceNamespace:           github.SourceNamespace,
		CacheTTLSeconds:           github.DefaultCacheTTLSeconds,
		Commands:                  github.DeclaredCommands,
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// resolveOperatorLogin is the startup-side helper Cut 3
// (`--command fetch`) calls to derive the `<operator-login>`
// token it splices into the GitHub search query
// (`is:open involves:<login>` per ADR-0026 §4). Defined here
// so the auth-resolve path stays in one place; the bulk
// fetch path calls this once per invocation (login is
// stable for the process lifetime).
//
// Returns ErrTokenMissing when the operator hasn't wired the
// PAT.
func resolveOperatorLogin(ctx context.Context) (string, error) {
	token := os.Getenv(github.EnvToken)
	baseURL := os.Getenv(github.EnvBaseURL)
	client := &http.Client{Timeout: authTimeout}
	return github.ResolveUserLogin(ctx, client, baseURL, token)
}

// commandFetchTimeout caps the whole `--command fetch` run
// via the outer context. Generous enough to walk a few dozen
// repos with several items each. Operators with large
// backlogs override via the daemon-side `fetch_timeout:`
// config knob (per the subprocess wrapper).
const commandFetchTimeout = 10 * time.Minute

// commandFetchItemTimeout caps each individual HTTP round-trip
// (search page, per-item GET) so a stuck connection fails
// fast instead of swallowing the broader run budget. The
// outer ctx still enforces the overall ceiling; this lives
// on the shared `*http.Client.Timeout` so net/http enforces
// it per request.
const commandFetchItemTimeout = 30 * time.Second

// summaryPacket is the ADR-0023 control packet emitted at
// end-of-stream. Trailing `_summary` mirrors what yaad-gmail
// emits — same shape so the daemon's NDJSON consumer treats
// both plugins uniformly.
type summaryPacket struct {
	Summary summaryFields `json:"_summary"`
}

type summaryFields struct {
	Repos          int   `json:"repos"`
	Emitted        int   `json:"emitted"`
	Errors         int   `json:"errors"`
	DurationMillis int64 `json:"duration_ms"`
}

// errorPacket is the ADR-0023 per-envelope error shape — used
// when the bulk-fetch run encounters an unrecoverable
// pre-condition (missing token, missing repo list). The
// daemon's NDJSON consumer logs the message; the binary
// also exits non-zero.
type errorPacket struct {
	Error        string `json:"_error"`
	ErrorMessage string `json:"_error_message,omitempty"`
}

// runCommandFetch is the bulk-fetch path per ADR-0026 §1 +
// §4. Resolves the operator login from the PAT (single
// `GET /user` call per process invocation), reads the repo
// list from EnvRepos, runs `is:open involves:<login>` per
// repo, and streams one source-shape envelope per matched
// item via the existing WriteEnvelope. Terminates with a
// `_summary` control packet.
//
// Pre-conditions enforced via `_error` envelopes + non-zero
// exit: missing token, missing repo list. Operator-JWT
// validation happens daemon-side per ADR-0005 before
// dispatch; this binary trusts the invocation as authorized.
//
// Closed-recent sweep per ADR-0026 §6 (2026-05-21 amendment)
// runs alongside the open search using a stateless N-day rolling
// window via GitHub Search's native `updated:>=` operator;
// `YAAD_GITHUB_RECENT_DAYS` (default 7) tunes the window.
// Archive lifecycle itself lives in operator-authored workflows
// per ADR-0024's `entity_updated` + `archive_entity` pair.
func runCommandFetch(stdout, stderr io.Writer) error {
	startedAt := time.Now()

	repos, err := github.ParseRepoList(os.Getenv(github.EnvRepos))
	if err != nil {
		emitError(stdout, "config_missing", err.Error())
		return err
	}

	recentDays, err := github.ParseRecentDays(os.Getenv(github.EnvRecentDays))
	if err != nil {
		emitError(stdout, "config_invalid", err.Error())
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandFetchTimeout)
	defer cancel()

	login, err := resolveOperatorLogin(ctx)
	if err != nil {
		emitError(stdout, "auth_failed", err.Error())
		return err
	}

	instance := os.Getenv(EnvInstanceName)
	baseURL := os.Getenv(github.EnvBaseURL)
	token := os.Getenv(github.EnvToken)

	// One shared client across every search + per-item fetch
	// this invocation. http.Client.Timeout enforces the
	// per-request ceiling; the outer ctx enforces the run
	// budget.
	client, err := github.NewClient(
		&http.Client{Timeout: commandFetchItemTimeout},
		baseURL,
		token,
	)
	if err != nil {
		emitError(stdout, "client_init", err.Error())
		return err
	}

	emitted := 0
	errCount := 0
	closedWindowAnchor := time.Now()

	for _, repo := range repos {
		openTargets, err := client.SearchInvolvedOpen(ctx, repo, login)
		if err != nil {
			errCount++
			_, _ = fmt.Fprintf(stderr, "yaad-github: search %s [open]: %v\n", repo.Slash(), err)
			// fall through to the closed search — one failed
			// query on a repo doesn't suppress the other.
		}

		closedTargets, err := client.SearchInvolvedClosedRecent(ctx, repo, login, closedWindowAnchor, recentDays)
		if err != nil {
			errCount++
			_, _ = fmt.Fprintf(stderr, "yaad-github: search %s [closed-recent]: %v\n", repo.Slash(), err)
		}

		// Union the two result sets, dedup by (owner, repo,
		// kind, number) — the closed sweep may surface items
		// that flipped state mid-sweep, double-counting on the
		// open side.
		targets := dedupTargets(openTargets, closedTargets)

		for _, target := range targets {
			item, err := client.FetchTarget(ctx, target)
			if err != nil {
				errCount++
				_, _ = fmt.Fprintf(stderr, "yaad-github: fetch %s/%s#%d: %v\n",
					target.Owner, target.Repo, target.Number, err)
				continue
			}
			fetchedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
			if err := github.WriteEnvelope(stdout, item, instance, baseURL, "", fetchedAt); err != nil {
				errCount++
				_, _ = fmt.Fprintf(stderr, "yaad-github: write envelope %s/%s#%d: %v\n",
					target.Owner, target.Repo, target.Number, err)
				continue
			}
			emitted++
		}
	}

	return emitSummary(stdout, summaryFields{
		Repos:          len(repos),
		Emitted:        emitted,
		Errors:         errCount,
		DurationMillis: time.Since(startedAt).Milliseconds(),
	})
}

// dedupTargets unions the open + closed-recent result sets and
// drops duplicates. The same (owner, repo, kind, number) appears
// at most once even when an item flipped state between the two
// queries — the second query's hit wins (later query, more
// recent state observation).
func dedupTargets(open, closed []github.Target) []github.Target {
	out := make([]github.Target, 0, len(open)+len(closed))
	seen := make(map[github.Target]struct{}, len(open)+len(closed))
	for _, t := range open {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range closed {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// emitError writes a single `_error` control packet to
// stdout. Best-effort: a write failure on stderr is
// non-fatal (the binary's exit code carries the failure
// signal).
func emitError(stdout io.Writer, code, message string) {
	enc := json.NewEncoder(stdout)
	_ = enc.Encode(errorPacket{Error: code, ErrorMessage: message})
}

// emitSummary writes the trailing `_summary` control
// packet. Returns the encoder error so the caller can
// surface a write-failure on a busy stdout.
func emitSummary(stdout io.Writer, fields summaryFields) error {
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(summaryPacket{Summary: fields}); err != nil {
		return fmt.Errorf("write _summary: %w", err)
	}
	return nil
}

// fetchTimeout caps the wall-clock budget for one URL-shape
// fetch (parse + GET + emit). Generous enough that a slow
// upstream doesn't trip on the median round-trip; tight
// enough that a stuck connection doesn't hang the daemon's
// per-plugin timeout per ADR-0005.
const fetchTimeout = 30 * time.Second

// fetchRequest mirrors the wire shape yaad-index writes to
// the plugin's stdin per ADR-0005 — the operation +
// originating input the daemon parsed from `/v1/ingest`.
// Decode-only; the plugin doesn't write this shape.
type fetchRequest struct {
	Operation string `json:"operation"`
	URL       string `json:"url"`
}

// runURLShapeFetch is the URL-shape ingest path per
// ADR-0026 §1 + ADR-0021. Reads the request, parses the
// target out of the input URL, fetches the PR or issue, and
// emits one source-shape envelope on stdout per ADR-0023.
//
// The token lookup is best-effort: an unauthenticated call
// is permitted (GitHub allows ~60 anonymous requests/hour),
// but the operator's intended path is to wire
// YAAD_GITHUB_TOKEN. We surface the unauthenticated path so
// the plugin works for one-off public-repo dispatches
// without forcing a PAT.
func runURLShapeFetch(stdin io.Reader, stdout io.Writer) error {
	var req fetchRequest
	dec := json.NewDecoder(stdin)
	if err := dec.Decode(&req); err != nil {
		if err == io.EOF {
			return fmt.Errorf("yaad-github: empty stdin (expected ingest request JSON)")
		}
		return fmt.Errorf("yaad-github: decode ingest request: %w", err)
	}
	if req.Operation != "" && req.Operation != "ingest" {
		return fmt.Errorf("yaad-github: unsupported operation %q (only \"ingest\" is implemented)", req.Operation)
	}

	target, err := github.ParseTarget(req.URL)
	if err != nil {
		return fmt.Errorf("yaad-github: parse target: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	opts := github.FetchOptions{
		Client:  &http.Client{Timeout: fetchTimeout},
		BaseURL: os.Getenv(github.EnvBaseURL),
		Token:   os.Getenv(github.EnvToken),
	}
	item, err := github.FetchTarget(ctx, opts, *target)
	if err != nil {
		return fmt.Errorf("yaad-github: fetch %s: %w", req.URL, err)
	}

	fetchedAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	instance := os.Getenv(EnvInstanceName)
	baseURL := os.Getenv(github.EnvBaseURL)
	if err := github.WriteEnvelope(stdout, item, instance, baseURL, req.URL, fetchedAt); err != nil {
		return fmt.Errorf("yaad-github: write envelope: %w", err)
	}
	return nil
}
