package vault

import (
	"errors"
	"strings"
)

// PluginBodyStartMarker / PluginBodyEndMarker delimit plugin-managed
// body content per ADR-0015 §1. HTML-comment shape so the markers
// are invisible in rendered Obsidian / GitHub / standard markdown
// views; operators see clean rendered output and the daemon-plugin
// contract stays vault-readable.
//
// Generic `yaad:plugin` (rather than per-plugin `yaad:bgg`,
// `yaad:wikipedia`) so the daemon implementation is plugin-agnostic
// — one merge implementation handles every plugin emitting body
// content (per ADR-0015 §"Alternative A: rejected").
const (
	PluginBodyStartMarker = "<!-- yaad:plugin start -->"
	PluginBodyEndMarker = "<!-- yaad:plugin end -->"
)

// NotesStartMarker / NotesEndMarker delimit the agent-emitted
// `## Notes` section (ADR-0015 marker-pair
// pattern extended to notes). Distinct prefix (`yaad:notes`,
// not `yaad:plugin`) so a plugin re-ingest that splices its body
// region doesn't touch the notes region — notes are append-
// only agent-emitted, plugin body is plugin-owned, the two regions
// have independent lifecycles.
//
// On read: parser detects the marker pair and parses the notes
// table from inside it; falls back to the legacy section-aware
// parser for un-marked entities. On write: writeNotesSection
// wraps the rendered table in the marker pair so the next read
// finds them deterministically (no ambiguity from operator-titled
// `## Notes` sections elsewhere in the body).
//
// Operator-hand-edits inside the marker region: notes stay
// structured ([]Note) — the table inside the markers is
// re-rendered from in-memory state on each write. Raw markdown
// between table rows is discarded on next agent-add (current
// behavior preserved, not a regression from this change).
const (
	NotesStartMarker = "<!-- yaad:notes start -->"
	NotesEndMarker = "<!-- yaad:notes end -->"
)

// DataviewStartMarker / DataviewEndMarker delimit the agent-
// appended dataview paragraphs. Each
// paragraph is a single line of Obsidian dataview-inline
// metadata (`key:: value  key:: value`) representing one
// canonical-type fill event on the target canonical entity.
// Distinct from the notes-pair so a plugin re-ingest or an
// operator-authored notes section can't smear data into the
// dataview region by accident.
//
// On read: parser detects the marker pair and splits the
// inner block into structured paragraphs. On write:
// writeDataviewSection wraps the rendered paragraphs in the
// marker pair so the next read finds them deterministically.
// Bare `key:: value` lines outside the marker pair stay in
// CleanContent verbatim — the marker is the activation
// signal, prose stays prose.
const (
	DataviewStartMarker = "<!-- yaad:dataview start -->"
	DataviewEndMarker = "<!-- yaad:dataview end -->"
)

// ErrPluginEmittedMarker is returned by MergePluginBody when the
// plugin's emitted body contains the literal start or end marker
// substring. Per ADR-0015 §4 last bullet: fail-fast surfaces the
// plugin bug rather than silently mangling content. The caller
// (the ingest tracker) propagates this as a tracker-failed
// transition; the operator sees a clear ingest_failed envelope
// naming the plugin.
var ErrPluginEmittedMarker = errors.New("plugin body contains reserved yaad:plugin marker substring")

// PluginBodyMerge is the result of merging plugin-emitted body
// content into an existing vault entity body per ADR-0015 §3.
//
// Body is the merged content the caller writes to vault.
//
// PriorMarkers names which path the merge took: "" on first-write
// (no prior markers — body either empty or operator-only), "clean"
// on re-ingest with well-formed markers (between-region replaced),
// or a non-empty malformed-reason string on fallback paths
// ("start_only_no_end", "end_only_no_start", "end_before_start").
// The malformed cases fall back to wholesale-replace per ADR-0015
// §4 (defensible safe-default that prefers data-correctness over
// preservation in a known-broken state). The caller logs WARN
// when PriorMarkers is a malformed reason.
type PluginBodyMerge struct {
	Body string
	PriorMarkers string
}

// MergePluginBody composes plugin-emitted body content with an
// existing vault entity body per ADR-0015 §3.
//
// Behaviors by input shape:
//
// - existingBody == "" — first ingest. Returns
// `<start>\n<plugin>\n<end>` with PriorMarkers="".
// - existingBody has markers (well-formed). Re-ingest happy path.
// Splits existingBody into before/between/after; replaces
// between with new plugin content + markers; preserves before
// and after. PriorMarkers="clean".
// - existingBody has no markers but is non-empty (operator
// hand-wrote content into the entity body before the plugin
// started emitting body content). Per ADR-0015 §4 first
// bullet: existing body becomes `before` (preserved verbatim),
// plugin region appended at the END so operator content stays
// at the top. PriorMarkers="".
// - existingBody has malformed markers (start without end, end
// without start, end before start). Falls back to wholesale-
// replace (the marker-aware path can't safely splice). The
// caller logs WARN; PriorMarkers names the reason.
//
// Plugin-emitted-marker detection: scans pluginContent for the
// literal start + end marker substrings. Detection returns
// ErrPluginEmittedMarker; per ADR-0015 §4 the daemon prefers
// fail-fast to silent escaping for v1.
func MergePluginBody(existingBody, pluginContent string) (PluginBodyMerge, error) {
	if strings.Contains(pluginContent, PluginBodyStartMarker) ||
		strings.Contains(pluginContent, PluginBodyEndMarker) {
		return PluginBodyMerge{}, ErrPluginEmittedMarker
	}

	wrapped := PluginBodyStartMarker + "\n" + pluginContent + "\n" + PluginBodyEndMarker

	// First-write empty body — straight wrap.
	if existingBody == "" {
		return PluginBodyMerge{Body: wrapped, PriorMarkers: ""}, nil
	}

	startIdx := strings.Index(existingBody, PluginBodyStartMarker)
	endIdx := strings.Index(existingBody, PluginBodyEndMarker)

	switch {
	case startIdx == -1 && endIdx == -1:
		// First-write with operator hand-written content. Plugin
		// region appears AT THE END so operator content stays
		// where they wrote it (top of body). Convention per
		// ADR-0015 §4 first bullet.
		sep := ""
		if !strings.HasSuffix(existingBody, "\n") {
			sep = "\n"
		}
		return PluginBodyMerge{
			Body: existingBody + sep + wrapped,
			PriorMarkers: "",
		}, nil
	case startIdx != -1 && endIdx == -1:
		// Malformed: start present without end. Fallback +
		// flag for caller WARN.
		return PluginBodyMerge{Body: wrapped, PriorMarkers: "start_only_no_end"}, nil
	case startIdx == -1 && endIdx != -1:
		return PluginBodyMerge{Body: wrapped, PriorMarkers: "end_only_no_start"}, nil
	case endIdx < startIdx:
		// Both present but out of order — the start-marker is
		// after a stray end-marker. Same fallback shape.
		return PluginBodyMerge{Body: wrapped, PriorMarkers: "end_before_start"}, nil
	}

	// Happy re-ingest path: splice. `before` is everything up to
	// the start marker; `after` is everything past the end marker.
	// The markers themselves are entirely contained in the
	// `between` region we replace — `wrapped` re-emits both.
	before := existingBody[:startIdx]
	afterStart := endIdx + len(PluginBodyEndMarker)
	after := existingBody[afterStart:]

	return PluginBodyMerge{
		Body: before + wrapped + after,
		PriorMarkers: "clean",
	}, nil
}
