// POST /v1/search/upstream — plugin-federated search per
// yaad-index #2.
//
// Local search (DB-only) lives at GET /v1/search; this endpoint
// fans the operator/agent query out across plugins that have
// opted in via Capabilities.SupportsSearch=true. Per-plugin
// timeout + partial-results semantic — federation is best-effort.

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/yaad-index/internal/plugins"
)

const (
	// upstreamDefaultLimit caps the per-plugin candidate request
	// when the caller doesn't specify one. Tuned to be enough for
	// the disambiguation UX without flooding the wire.
	upstreamDefaultLimit = 10
	// upstreamMaxLimit is the per-plugin upper bound the daemon
	// enforces regardless of caller intent — guards against an
	// agent sending limit=10000 and exhausting an upstream API's
	// quota across plugins.
	upstreamMaxLimit = 50
	// upstreamDefaultPerPluginTimeout is the default per-plugin
	// wall-clock budget. 5s matches the median network-search
	// latency of representative plugins (yaad-wikipedia search
	// is ~200ms-1s under good conditions; the budget gives
	// laggards room before federated cancellation).
	upstreamDefaultPerPluginTimeout = 5 * time.Second
	// upstreamMaxPerPluginTimeout is the per-plugin upper bound.
	// Federations beyond this should run as a separate operator
	// workflow, not a single HTTP request.
	upstreamMaxPerPluginTimeout = 30 * time.Second
)

// searchUpstreamRequest is the POST /v1/search/upstream wire shape
// per yaad-index #2.
type searchUpstreamRequest struct {
	// Query is the operator/agent search string. Whitespace-
	// trimmed; empty rejects with 400.
	Query string `json:"query"`
	// Plugins is the explicit plugin-name allowlist. Empty / nil →
	// federate to every opted-in plugin (SupportsSearch=true). Any
	// name in the slice that isn't a registered plugin → 400. Any
	// name that IS registered but has SupportsSearch=false → 422.
	Plugins []string `json:"plugins,omitempty"`
	// Limit is the per-plugin candidate cap the daemon forwards to
	// each plugin. Bounded [1, upstreamMaxLimit]; absent / 0 → default.
	Limit int `json:"limit,omitempty"`
	// PerPluginTimeoutSeconds is the per-plugin wall-clock budget.
	// Bounded [1, upstreamMaxPerPluginTimeout]; absent / 0 → default.
	PerPluginTimeoutSeconds int `json:"per_plugin_timeout_seconds,omitempty"`
}

// searchUpstreamCandidate is one merged candidate on the federated
// response. Carries the plugin attribution alongside the plugin-
// emitted SearchCandidate fields.
type searchUpstreamCandidate struct {
	Plugin  string `json:"plugin"`
	ID      string `json:"id"`
	Label   string `json:"label"`
	Summary string `json:"summary,omitempty"`
}

// searchUpstreamPluginStatus is the per-plugin outcome surfaced on
// the federated response so callers see exactly which plugins
// returned vs timed out vs errored.
type searchUpstreamPluginStatus struct {
	Plugin       string `json:"plugin"`
	OK           bool   `json:"ok"`
	Candidates   int    `json:"candidates"`
	DurationMs   int64  `json:"duration_ms"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type searchUpstreamResponse struct {
	OK bool `json:"ok"`
	// Total is the sum of raw candidate counts each plugin
	// returned, BEFORE the per-plugin Limit truncation per
	// #338. Equals sum(PerPlugin[i].Candidates) and reflects
	// upstream availability — callers see "X hits were
	// available across all plugins; Y surfaced in results"
	// without having to sum the per_plugin_status block.
	Total      int                          `json:"total"`
	Results    []searchUpstreamCandidate    `json:"results"`
	PerPlugin  []searchUpstreamPluginStatus `json:"per_plugin_status"`
	Query      string                       `json:"query"`
	Limit      int                          `json:"limit"`
	TimeoutSec int                          `json:"per_plugin_timeout_seconds"`
}

// handleSearchUpstream implements POST /v1/search/upstream per
// yaad-index #2. Dispatches in parallel across opted-in plugins
// with per-plugin timeout; merges results in plugin-declaration
// order; surfaces per-plugin status so callers can debug.
func handleSearchUpstream(logger *slog.Logger, registry pluginRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req searchUpstreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"malformed JSON body: "+err.Error())
			return
		}
		query := strings.TrimSpace(req.Query)
		if query == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "query is required")
			return
		}
		limit, err := boundInt(req.Limit, 1, upstreamMaxLimit)
		if err != nil {
			if req.Limit == 0 {
				limit = upstreamDefaultLimit
			} else {
				writeError(w, http.StatusBadRequest, "invalid_argument",
					"limit: "+err.Error())
				return
			}
		}
		timeoutSec, err := boundInt(req.PerPluginTimeoutSeconds, 1, int(upstreamMaxPerPluginTimeout/time.Second))
		if err != nil {
			if req.PerPluginTimeoutSeconds == 0 {
				timeoutSec = int(upstreamDefaultPerPluginTimeout / time.Second)
			} else {
				writeError(w, http.StatusBadRequest, "invalid_argument",
					"per_plugin_timeout_seconds: "+err.Error())
				return
			}
		}

		targets, errKind, errMsg := resolveUpstreamTargets(registry, req.Plugins)
		if errMsg != "" {
			status := http.StatusBadRequest
			if errKind == "unsupported_plugin" {
				status = http.StatusUnprocessableEntity
			}
			writeError(w, status, errKind, errMsg)
			return
		}

		perPluginTimeout := time.Duration(timeoutSec) * time.Second
		merged, statuses := federateSearch(r.Context(), logger, targets, query, limit, perPluginTimeout)

		// #338: sum raw per-plugin candidate counts. Reflects
		// the pre-limit-truncation availability so callers know
		// how many hits exist upstream vs how many surfaced in
		// the merged results.
		total := 0
		for _, s := range statuses {
			total += s.Candidates
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(searchUpstreamResponse{
			OK:         true,
			Total:      total,
			Results:    merged,
			PerPlugin:  statuses,
			Query:      query,
			Limit:      limit,
			TimeoutSec: timeoutSec,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/search/upstream response", "err", err)
		}
	}
}

// pluginRegistry is the narrow subset of *plugins.Registry the
// handler needs. Defined here as an interface so tests can pass a
// fixture-registry stand-in without lifting heavy daemon plumbing.
type pluginRegistry interface {
	Plugins() []plugins.Plugin
	LookupByName(name string) (plugins.Plugin, bool)
}

// resolveUpstreamTargets resolves the operator-supplied plugin
// allowlist into a slice of dispatch targets. Empty allowlist →
// every opted-in plugin in registry order. Non-empty: every name
// must be a registered plugin with SupportsSearch=true.
//
// Returns (targets, errKind, errMsg). errMsg=="" on success;
// errKind names the failure class ("invalid_argument" for unknown
// plugin, "unsupported_plugin" for SupportsSearch=false target).
func resolveUpstreamTargets(registry pluginRegistry, allowlist []string) ([]plugins.Plugin, string, string) {
	if len(allowlist) == 0 {
		// Federate to every opted-in plugin. Preserve registry
		// (i.e. operator yaml) order so the merged results are
		// stable across calls.
		var out []plugins.Plugin
		for _, p := range registry.Plugins() {
			if p.Capabilities().SupportsSearch {
				out = append(out, p)
			}
		}
		return out, "", ""
	}
	out := make([]plugins.Plugin, 0, len(allowlist))
	for _, name := range allowlist {
		p, ok := registry.LookupByName(name)
		if !ok {
			return nil, "invalid_argument",
				"plugin \"" + name + "\" is not registered"
		}
		if !p.Capabilities().SupportsSearch {
			return nil, "unsupported_plugin",
				"plugin \"" + name + "\" does not support search (Capabilities.SupportsSearch=false)"
		}
		out = append(out, p)
	}
	return out, "", ""
}

// federateSearch fans the query out across targets in parallel
// (one goroutine per plugin) with a per-plugin timeout context.
// Returns the merged candidate list (plugin-declaration order;
// no relevance sort in v1 per yaad-index #2 scoping) + per-plugin
// status block.
//
// Partial-results semantic: timeouts + errors mark the plugin's
// status block but don't fail the call. All-failed federations
// still return 200 with empty results + populated per-plugin
// errors so callers can debug.
func federateSearch(
	parent context.Context,
	logger *slog.Logger,
	targets []plugins.Plugin,
	query string,
	limit int,
	perPluginTimeout time.Duration,
) ([]searchUpstreamCandidate, []searchUpstreamPluginStatus) {
	type result struct {
		index      int
		plugin     string
		candidates []plugins.SearchCandidate
		err        error
		duration   time.Duration
	}

	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Add(1)
		go func(idx int, plug plugins.Plugin) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, perPluginTimeout)
			defer cancel()
			start := time.Now()
			candidates, err := plug.Search(ctx, query, limit)
			results[idx] = result{
				index:      idx,
				plugin:     plug.Name(),
				candidates: candidates,
				err:        err,
				duration:   time.Since(start),
			}
		}(i, p)
	}
	wg.Wait()

	// Sort by index to restore declaration order — goroutine
	// completion order is non-deterministic.
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })

	merged := make([]searchUpstreamCandidate, 0)
	statuses := make([]searchUpstreamPluginStatus, 0, len(results))
	for _, r := range results {
		status := searchUpstreamPluginStatus{
			Plugin:     r.plugin,
			OK:         r.err == nil,
			Candidates: len(r.candidates),
			DurationMs: r.duration.Milliseconds(),
		}
		if r.err != nil {
			status.ErrorMessage = r.err.Error()
			logger.WarnContext(parent, "upstream search plugin failure",
				"plugin", r.plugin, "err", r.err, "duration_ms", status.DurationMs)
		}
		statuses = append(statuses, status)
		// Cap per-plugin candidates at the limit even when the
		// plugin returned more — the daemon's contract is to
		// respect the operator's limit regardless of plugin
		// adherence.
		for i, c := range r.candidates {
			if i >= limit {
				break
			}
			merged = append(merged, searchUpstreamCandidate{
				Plugin:  r.plugin,
				ID:      c.ID,
				Label:   c.Label,
				Summary: c.Summary,
			})
		}
	}
	return merged, statuses
}
