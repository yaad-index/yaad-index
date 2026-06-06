package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/plugins"
)

// daemonEntityKindDescriptions names the human-readable description
// surfaced on /v1/kinds for each daemon-built-in canonical entity
// kind. The set today: `day` per ADR-0025 cut 1, `task` per the
// ADR-0024 alignment landed in #268, and the gmail-emitted
// `email` / `email-address` / `label` kinds per #272. Everything
// else comes from registered plugins.
var daemonEntityKindDescriptions = map[string]string{
	canonical.DayKind: "Date anchor entity per ADR-0025 — slug shape `day:<YYYY-MM-DD>`. " +
		"Always available; operators don't enable via canonical_kinds: config.",
	canonical.TaskKind: "Workflow-spawned task entity per ADR-0024 §Task — slug shape " +
		"`task:<workflow>-<subject>` (or `task:<workflow>-err` for err tasks). " +
		"Always available; operators don't enable via canonical_kinds: config.",
	canonical.EmailKind: "Per-message gmail anchor entity (the `is_about` target of a gmail source) — " +
		"slug shape `email:<message-id-slug>`. Daemon-managed per #272.",
	canonical.EmailAddressKind: "Per-address gmail entity (the `from`/`to`/`cc`/`bcc` target of a gmail source) — " +
		"slug shape `email-address:<addr-slug>`. Daemon-managed per #272.",
	canonical.LabelKind: "Per-Gmail-label entity (the `tagged_as` target of a gmail source) — " +
		"slug shape `label:<label-slug>`. Daemon-managed per #272.",
}

// daemonEdgeKindDescriptions names the canonical edge type
// vocabulary. Cut-1 set per ADR-0025 § Edge types (the five
// time-bound relationships, all targeting `day`) plus
// `triggered_by` per #268 (task → source-entity attribution; the
// source kind is open since any entity can trigger a workflow)
// plus the gmail-emitted address-role + label edges per #272
// (`from`/`to`/`cc`/`bcc`/`tagged_as`).
var daemonEdgeKindDescriptions = map[string]string{
	canonical.EdgeTypeDueOn:         "Task / deadline entity is due on this day.",
	canonical.EdgeTypeOccurredOn:    "Event / meeting / shipment happened or will happen on this day.",
	canonical.EdgeTypeIsAboutDay:    "Newsletter / digest / journal entry describes this day.",
	canonical.EdgeTypeReferencesDay: "Generic reference to this day from any entity (daemon shape-scan fallback).",
	canonical.EdgeTypeIngestedOn:    "Entity was first received on this day. Reserved for operator-wired workflow; daemon never emits in v1.x.",
	canonical.EdgeTypeTriggeredBy:   "Workflow-spawned task points at the source entity whose firing produced it.",
	canonical.EdgeTypeFrom:          "Gmail source points at the email-address that sent the message.",
	canonical.EdgeTypeTo:            "Gmail source points at an email-address listed in the To header.",
	canonical.EdgeTypeCc:            "Gmail source points at an email-address listed in the Cc header.",
	canonical.EdgeTypeBcc:           "Gmail source points at an email-address listed in the Bcc header (sent-folder messages only).",
	canonical.EdgeTypeTaggedAs:      "Gmail source points at a label entity surfaced via the X-GM-LABELS header.",
}

// daemonEdgeKindEndpoints names the (from_kind, to_kind) pair the
// /v1/kinds aggregator stamps for each daemon-built-in edge type.
// Empty from_kind means "any entity can serve as the source"; same
// shape applies to to_kind. Day-anchored edges land on
// `to_kind=day` with open from_kind; the triggered_by edge has
// `from_kind=task` with open to_kind (the source side carries any
// triggering entity, per the per-firing source attribution from
// #264).
var daemonEdgeKindEndpoints = map[string]struct{ FromKind, ToKind string }{
	canonical.EdgeTypeDueOn:         {ToKind: canonical.DayKind},
	canonical.EdgeTypeOccurredOn:    {ToKind: canonical.DayKind},
	canonical.EdgeTypeIsAboutDay:    {ToKind: canonical.DayKind},
	canonical.EdgeTypeReferencesDay: {ToKind: canonical.DayKind},
	canonical.EdgeTypeIngestedOn:    {ToKind: canonical.DayKind},
	canonical.EdgeTypeTriggeredBy:   {FromKind: canonical.TaskKind},
	canonical.EdgeTypeFrom:          {ToKind: canonical.EmailAddressKind},
	canonical.EdgeTypeTo:            {ToKind: canonical.EmailAddressKind},
	canonical.EdgeTypeCc:            {ToKind: canonical.EmailAddressKind},
	canonical.EdgeTypeBcc:           {ToKind: canonical.EmailAddressKind},
	canonical.EdgeTypeTaggedAs:      {ToKind: canonical.LabelKind},
}

// daemonSourcePlugin is the synthetic source_plugins value the
// /v1/kinds aggregator stamps on daemon-built-in entries so
// consumers can tell daemon-managed kinds apart from plugin-emitted
// ones at a glance.
const daemonSourcePlugin = "yaad-index"

// entityKind / edgeKind / kindsResponse mirror the wire shape locked in
// ADR-0002 (`GET /v1/kinds`, lines 298–331). Field names map to JSON via
// snake_case tags.
type entityKind struct {
	Name string `json:"name"`
	Description string `json:"description"`
	SourcePlugins []string `json:"source_plugins"`
}

type edgeKind struct {
	Name string `json:"name"`
	Description string `json:"description"`
	FromKind string `json:"from_kind"`
	ToKind string `json:"to_kind"`
	SourcePlugins []string `json:"source_plugins"`
}

type kindsResponse struct {
	OK bool `json:"ok"`
	EntityKinds []entityKind `json:"entity_kinds"`
	EdgeKinds []edgeKind `json:"edge_kinds"`
}

// handleKinds aggregates entity / edge kinds across every registered
// plugin's capabilities and emits the union, deduped by name with
// source_plugins unioned across plugins emitting the same kind. Empty
// registry → empty arrays + ok=true.
//
// Sort order is alphabetical by name on both arrays so successive
// calls produce byte-identical responses with a stable plugin set.
// Description is taken from the first plugin to advertise the kind;
// from_kind / to_kind are taken from the same source. If two plugins
// disagree on description / from_kind / to_kind for the same kind
// name that's a config issue the operator should fix — the handler
// is deterministic but doesn't try to merge conflicting metadata.
func handleKinds(logger *slog.Logger, registry *plugins.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := aggregateKinds(registry)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/kinds response", "err", err)
		}
	}
}

func aggregateKinds(registry *plugins.Registry) kindsResponse {
	entityIdx := make(map[string]*entityKind)
	edgeIdx := make(map[string]*edgeKind)

	// Seed daemon-built-in canonical entity kinds per ADR-0025
	// before walking plugins so a plugin that also advertises the
	// same name (unexpected but defensible) joins the daemon entry
	// via the SourcePlugins union path below.
	for _, kind := range canonical.DaemonEntityKinds() {
		entityIdx[kind] = &entityKind{
			Name: kind,
			Description: daemonEntityKindDescriptions[kind],
			SourcePlugins: []string{daemonSourcePlugin},
		}
	}

	// Same shape for the canonical edge type vocabulary.
	// daemonEdgeKindEndpoints supplies the (from_kind, to_kind)
	// per edge — day-anchored edges land on `to_kind=day`; the
	// triggered_by edge has `from_kind=task`. Unknown edge names
	// (e.g. test fixtures with no endpoint entry) fall through
	// to empty endpoints.
	for _, edge := range canonical.DaemonEdgeTypes() {
		ep := daemonEdgeKindEndpoints[edge]
		edgeIdx[edge] = &edgeKind{
			Name:          edge,
			Description:   daemonEdgeKindDescriptions[edge],
			FromKind:      ep.FromKind,
			ToKind:        ep.ToKind,
			SourcePlugins: []string{daemonSourcePlugin},
		}
	}

	for _, p := range registry.Plugins() {
		caps := p.Capabilities()
		pluginName := caps.Name
		if pluginName == "" {
			pluginName = p.Name()
		}
		for _, ks := range caps.EntityKinds {
			if existing, ok := entityIdx[ks.Name]; ok {
				existing.SourcePlugins = appendUnique(existing.SourcePlugins, pluginName)
				continue
			}
			entityIdx[ks.Name] = &entityKind{
				Name: ks.Name,
				Description: ks.Description,
				SourcePlugins: []string{pluginName},
			}
		}
		for _, ks := range caps.EdgeKinds {
			if existing, ok := edgeIdx[ks.Name]; ok {
				existing.SourcePlugins = appendUnique(existing.SourcePlugins, pluginName)
				continue
			}
			edgeIdx[ks.Name] = &edgeKind{
				Name: ks.Name,
				Description: ks.Description,
				FromKind: ks.FromKind,
				ToKind: ks.ToKind,
				SourcePlugins: []string{pluginName},
			}
		}
	}

	resp := kindsResponse{
		OK: true,
		EntityKinds: make([]entityKind, 0, len(entityIdx)),
		EdgeKinds: make([]edgeKind, 0, len(edgeIdx)),
	}
	for _, e := range entityIdx {
		sort.Strings(e.SourcePlugins)
		resp.EntityKinds = append(resp.EntityKinds, *e)
	}
	for _, e := range edgeIdx {
		sort.Strings(e.SourcePlugins)
		resp.EdgeKinds = append(resp.EdgeKinds, *e)
	}
	sort.Slice(resp.EntityKinds, func(i, j int) bool {
		return resp.EntityKinds[i].Name < resp.EntityKinds[j].Name
	})
	sort.Slice(resp.EdgeKinds, func(i, j int) bool {
		return resp.EdgeKinds[i].Name < resp.EdgeKinds[j].Name
	})
	return resp
}

func appendUnique(slice []string, s string) []string {
	for _, x := range slice {
		if x == s {
			return slice
		}
	}
	return append(slice, s)
}
