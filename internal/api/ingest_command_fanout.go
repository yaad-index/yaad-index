package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

// ingestFanOutResponse is the ADR-0028 §4 (Cut 4) aggregate
// response for the bare-plugin command form when the plugin has
// 2+ configured instances. Each per-instance result carries the
// same shape ingest reports for single-attempt invocations —
// state + (entity_id | error) — plus the instance name so the
// caller can attribute results.
//
// Single-instance plugins (1 configured instance OR
// `<plugin>/<instance>: !<cmd>` instance-scoped form) skip this
// response shape entirely and use the existing single-attempt
// shapes (ingestQueuedResponse, ingestCompleteResponse, etc.).
type ingestFanOutResponse struct {
	OK     bool             `json:"ok"`
	State  string           `json:"state"` // "fan_out"
	Status string           `json:"status"`
	Plugin string           `json:"plugin"`
	Result []fanOutInstance `json:"result"`
}

// fanOutInstance is one per-instance result entry in
// ingestFanOutResponse.Result. State is the same vocabulary the
// single-attempt response shapes use (`complete`, `queued`,
// `needs_fill`, `disambiguation`, `failed`). EntityID is set
// when the underlying tracker reached a terminal-with-entity
// state; Error is set when the per-instance run failed (the
// fan-out continues to the next instance per ADR-0028 §4 —
// errors don't abort the walk).
type fanOutInstance struct {
	Instance string `json:"instance"`
	State    string `json:"state"`
	EntityID string `json:"entity_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// handleCommandFanOut dispatches an ADR-0028 §4 command-shape
// invocation. The single-instance code path collapses to the
// existing single-attempt response shapes for back-compat; the
// multi-instance bare-plugin form emits the aggregate fan-out
// response.
//
// Routing:
//
//   - inv.Instance set + name found in operator config → single
//     attempt against that instance. Unknown instance name →
//     404-equivalent error with the operator-config instance
//     list named for diagnostics.
//   - inv.Instance empty + 0 configured instances → reject
//     (Cut 1 invariant: every plugin has at least one configured
//     instance, even if synthesized; reaching this branch with
//     zero is a daemon-startup bug).
//   - inv.Instance empty + 1 configured instance → single attempt
//     against that instance; response shape is the regular
//     single-attempt shape.
//   - inv.Instance empty + 2+ configured instances → serial
//     fan-out, aggregate response per the ADR.
func handleCommandFanOut(
	w http.ResponseWriter,
	r *http.Request,
	logger *slog.Logger,
	st store.Store,
	tracker *ingestTracker,
	plugin plugins.Plugin,
	req ingestRequest,
	inv plugins.Invocation,
	instances []config.InstanceEntry,
	waitSeconds int,
	fillInstruction string,
	canonicalKindReg map[string]config.CanonicalKindConfig,
) {
	if inv.Instance != "" {
		// Instance-scoped form: validate against the operator's
		// configured instance list. Unknown instance → 404.
		target, ok := findInstanceByName(instances, inv.Instance)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown_instance",
				fmt.Sprintf("plugin %q has no instance named %q (configured: %v)",
					plugin.Name(), inv.Instance, instanceNames(instances)))
			return
		}
		// ADR-0028 §7 Cut 5: a disabled instance is invisible
		// to dispatch. Operator-scoped invocation against a
		// disabled instance rejects with a clear error so the
		// caller knows the configured target was deliberately
		// turned off (vs typo'd / missing).
		//
		// 400 Bad Request, not 503: `enabled: false` is operator
		// config state, not a transient server outage. 5xx
		// would invite retries / proxy backoff that can't help
		// — only an operator config change unblocks the request.
		// 400 matches the existing ADR-0028 §3 `unrouted_url`
		// shape from Cut 3: same class of "request well-formed
		// but the routing config doesn't accept it" rather than
		// "transient server failure."
		if !target.IsEnabled() {
			writeError(w, http.StatusBadRequest, "instance_disabled",
				fmt.Sprintf("plugin %q instance %q is disabled (enabled: false in operator config)",
					plugin.Name(), inv.Instance))
			return
		}
		extraEnv, envErr := buildInstanceEnv(plugin.Name(), target)
		if envErr != nil {
			writeError(w, http.StatusInternalServerError, "instance_env_failed", envErr.Error())
			return
		}
		dispatchSingleCommand(w, r, logger, st, tracker, plugin, req, target.Name, extraEnv, waitSeconds, fillInstruction, canonicalKindReg)
		return
	}

	// Bare-plugin form. Filter to enabled instances per ADR-0028
	// §7 — disabled instances are invisible to fan-out. The
	// pre-filter is on `instances` (the configured set);
	// `enabled` carries the dispatch-visible subset.
	enabled := enabledInstances(instances)

	// 0 configured instances should be impossible post-Cut-1
	// (Load synthesizes the implicit `default` instance when the
	// operator omits the block), so reaching here with zero is a
	// daemon-startup bug we surface rather than crash. 0 enabled
	// (but >=1 configured) means the operator disabled every
	// instance — reject with a clear error.
	if len(instances) == 0 {
		// Fall back to single-attempt with empty instance name so
		// the tracker's resolveInstanceName picks `default`. This
		// matches the pre-Cut-4 behavior for plugins that were
		// loaded without operator config (test paths, dev
		// binaries) and preserves the existing response shape.
		dispatchSingleCommand(w, r, logger, st, tracker, plugin, req, "", nil, waitSeconds, fillInstruction, canonicalKindReg)
		return
	}
	if len(enabled) == 0 {
		// 400, not 503: same operator-config-state reasoning as
		// the instance_disabled branch above. Retries can't
		// help — only the operator re-enabling at least one
		// instance unblocks the bare-plugin invocation surface.
		writeError(w, http.StatusBadRequest, "no_enabled_instances",
			fmt.Sprintf("plugin %q has no enabled instances (all %d configured instances have enabled: false)",
				plugin.Name(), len(instances)))
		return
	}
	if len(enabled) == 1 {
		extraEnv, envErr := buildInstanceEnv(plugin.Name(), enabled[0])
		if envErr != nil {
			writeError(w, http.StatusInternalServerError, "instance_env_failed", envErr.Error())
			return
		}
		dispatchSingleCommand(w, r, logger, st, tracker, plugin, req, enabled[0].Name, extraEnv, waitSeconds, fillInstruction, canonicalKindReg)
		return
	}

	// Multi-instance fan-out. Per ADR-0028 §4 serial in
	// declaration order; per-instance error logged + reported
	// but doesn't abort the walk. Logs are linear and
	// instance-attributed via the logger context. Disabled
	// instances are filtered out via `enabled` so the walk only
	// considers operator-active runtime contexts.
	dispatchFanOut(w, r, logger, tracker, plugin, req, enabled, waitSeconds)
}

// dispatchSingleCommand runs a single command-shape attempt
// (instance pre-resolved) through the tracker and emits the
// regular single-attempt response shape. Mirrors the post-
// pickInstance flow in handleIngest for URL-shape.
func dispatchSingleCommand(
	w http.ResponseWriter,
	r *http.Request,
	logger *slog.Logger,
	st store.Store,
	tracker *ingestTracker,
	plugin plugins.Plugin,
	req ingestRequest,
	instanceName string,
	extraEnv []string,
	waitSeconds int,
	fillInstruction string,
	canonicalKindReg map[string]config.CanonicalKindConfig,
) {
	att := ingestAttemptForPlugin(plugin, req.URL, instanceName)
	att.forceRefetch = req.ForceRefetch
	att.simulation.extraEnv = extraEnv
	rec := tracker.beginAttempt(att)

	if waitSeconds == 0 {
		respondIngestQueued(w, r, logger, att.plannedEntityID)
		return
	}

	timeout := time.Duration(waitSeconds) * time.Second
	snap, err := tracker.wait(r.Context(), rec, timeout)
	switch {
	case errors.Is(err, errIngestTimeout):
		respondIngestQueued(w, r, logger, att.plannedEntityID)
		return
	case err != nil:
		logger.WarnContext(r.Context(), "ingest long-poll abandoned",
			"err", err, "plugin", plugin.Name(), "instance", instanceName)
		return
	}

	switch snap.state {
	case ingestStateComplete:
		respondIngestComplete(w, r, logger, st, snap.entityID)
	case ingestStateNeedsFill:
		respondIngestNeedsFill(w, r, logger, st, snap, fillInstruction, canonicalKindReg)
	case ingestStateDisambiguation:
		respondIngestDisambiguation(w, r, logger, snap.options)
	case ingestStateFailed:
		writeError(w, http.StatusInternalServerError, snap.failureCode, snap.failureMessage)
	default:
		writeError(w, http.StatusInternalServerError, "unknown_terminal_state",
			fmt.Sprintf("tracker reached unknown terminal state %v", snap.state))
	}
}

// dispatchFanOut walks every enabled instance serially and
// aggregates the per-instance results into ingestFanOutResponse
// per ADR-0028 §4. Each per-instance attempt gets up to
// waitSeconds budget (the operator's cap is applied per instance
// — total wall clock can reach N × waitSeconds for an N-instance
// fan-out where every attempt long-polls to the cap). Per-
// instance errors are recorded + logged but don't abort the walk.
//
// Async case (waitSeconds == 0): each per-instance attempt is
// queued; the aggregate response carries the queued planned IDs.
func dispatchFanOut(
	w http.ResponseWriter,
	r *http.Request,
	logger *slog.Logger,
	tracker *ingestTracker,
	plugin plugins.Plugin,
	req ingestRequest,
	instances []config.InstanceEntry,
	waitSeconds int,
) {
	results := make([]fanOutInstance, 0, len(instances))
	for _, inst := range instances {
		extraEnv, envErr := buildInstanceEnv(plugin.Name(), inst)
		if envErr != nil {
			// Surface the per-instance env-build error as the
			// per-instance result rather than aborting the
			// walk — matches the ADR-0028 §4 "per-instance
			// error continues" contract.
			logger.WarnContext(r.Context(), "fan-out instance env build failed",
				"err", envErr, "plugin", plugin.Name(), "instance", inst.Name)
			results = append(results, fanOutInstance{
				Instance: inst.Name,
				State:    "failed",
				Error:    envErr.Error(),
			})
			continue
		}
		entry := runFanOutInstance(r, logger, tracker, plugin, req, inst.Name, extraEnv, waitSeconds)
		results = append(results, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(ingestFanOutResponse{
		OK:     true,
		State:  "fan_out",
		Status: "fan_out",
		Plugin: plugin.Name(),
		Result: results,
	}); err != nil {
		logger.ErrorContext(r.Context(), "encode /v1/ingest fan_out response",
			"err", err, "plugin", plugin.Name())
	}
}

// runFanOutInstance runs one per-instance attempt for the
// dispatchFanOut walk. Returns the result entry; errors are
// captured in the entry rather than aborting the caller's walk
// per ADR-0028 §4.
func runFanOutInstance(
	r *http.Request,
	logger *slog.Logger,
	tracker *ingestTracker,
	plugin plugins.Plugin,
	req ingestRequest,
	instanceName string,
	extraEnv []string,
	waitSeconds int,
) fanOutInstance {
	att := ingestAttemptForPlugin(plugin, req.URL, instanceName)
	att.forceRefetch = req.ForceRefetch
	att.simulation.extraEnv = extraEnv
	rec := tracker.beginAttempt(att)

	if waitSeconds == 0 {
		// Async: every per-instance attempt is queued; return
		// state=queued for each. plannedEntityID is empty on the
		// command-shape (data-derived ids), matching the
		// single-attempt async response shape.
		return fanOutInstance{Instance: instanceName, State: "queued", EntityID: att.plannedEntityID}
	}

	timeout := time.Duration(waitSeconds) * time.Second
	snap, err := tracker.wait(r.Context(), rec, timeout)
	switch {
	case errors.Is(err, errIngestTimeout):
		return fanOutInstance{Instance: instanceName, State: "queued", EntityID: att.plannedEntityID}
	case err != nil:
		logger.WarnContext(r.Context(), "fan-out instance abandoned",
			"err", err, "plugin", plugin.Name(), "instance", instanceName)
		return fanOutInstance{Instance: instanceName, State: "failed", Error: err.Error()}
	}

	switch snap.state {
	case ingestStateComplete:
		return fanOutInstance{Instance: instanceName, State: "complete", EntityID: snap.entityID}
	case ingestStateNeedsFill:
		return fanOutInstance{Instance: instanceName, State: "needs_fill", EntityID: snap.entityID}
	case ingestStateDisambiguation:
		return fanOutInstance{Instance: instanceName, State: "disambiguation"}
	case ingestStateFailed:
		return fanOutInstance{Instance: instanceName, State: "failed", Error: snap.failureMessage}
	default:
		return fanOutInstance{Instance: instanceName, State: "failed", Error: fmt.Sprintf("unknown terminal state %v", snap.state)}
	}
}

// findInstanceByName scans instances for an entry whose Name
// equals target. Returns (entry, true) on match.
func findInstanceByName(instances []config.InstanceEntry, target string) (config.InstanceEntry, bool) {
	for _, inst := range instances {
		if inst.Name == target {
			return inst, true
		}
	}
	return config.InstanceEntry{}, false
}

// instanceNames projects the instance list to a slice of names
// for diagnostic error messages.
func instanceNames(instances []config.InstanceEntry) []string {
	out := make([]string, 0, len(instances))
	for _, inst := range instances {
		out = append(out, inst.Name)
	}
	return out
}
