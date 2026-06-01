package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// provenanceEntry / edgeRef / entity mirror the wire shape locked in
// ADR-0002 (`GET /v1/entities/{id}`, lines 28–62).
//
// On a successful provenance entry, `error` and `error_message` are absent
// (`omitempty`); on a failed entry both are populated. `ok` is always
// emitted — false is a meaningful value, not a missing field.
//
// Plugin-fetch entries set FetchedAt; agent-fill entries set FilledAt
// (ADR-0002 lines 213–223). Each entry sets exactly one — both are
// `omitempty` so the wire shape stays minimal per entry. Callers
// reading provenance can distinguish source by which field is present.
type provenanceEntry struct {
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
	FilledAt string `json:"filled_at,omitempty"`
	OK bool `json:"ok"`
	Error string `json:"error,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// edgeRef is the inline single-hop body shape emitted on
// `?with_edges=` expansion. `Archived` is set when the endpoint
// (the `to` entity) is archived per ADR-0018 step 3, so consumers
// can decide whether to follow without an extra round-trip. Flag
// is `omitempty` — only present when true to keep payloads small.
type edgeRef struct {
	Type string `json:"type"`
	To string `json:"to"`
	Archived bool `json:"archived,omitempty"`
}

// attachmentRef is the wire shape for one entry in an entity's
// attachment manifest per ADR-0018 §Attachments. Mirrors
// vault.Attachment but encodes JSON keys (not YAML) and uses
// `omitempty` consistently for the optional fields.
type attachmentRef struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
	Path string `json:"path"`
	Bytes int64 `json:"bytes,omitempty"`
}

type entity struct {
	ID string `json:"id"`
	Kind string `json:"kind"`
	// Source carries the ADR-0028 §5 slash-form attribution
	// (`<plugin>/<instance>`) for each source that emitted this
	// entity. Single-source entities serialize as a 1-element
	// array; multi-source overlap as N-element. The pre-ADR-0028
	// `plugin: <name>` wire shape was removed in Cut 2 (#244); MCP
	// + API clients now read `source` and split on `/` to extract
	// the plugin name when they need the bare value.
	Source []string `json:"source,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Notations []string `json:"notations,omitempty"`
	Data map[string]any `json:"data"`
	Provenance []provenanceEntry `json:"provenance"`
	Edges []edgeRef `json:"edges"`
	Attachments []attachmentRef `json:"attachments,omitempty"`

	// ADR-0018 step 3 archived-state surface. `omitempty` so the
	// flag is only present when true on the wire — same compaction
	// rule as edgeRef.Archived.
	Archived bool `json:"archived,omitempty"`

	// Single-hop body fields per yaad-index the source issue a prior PR
	// addendum. The vault is the source of truth (ADR-0008) for
	// these — the handler vault-reads when WithVaultIO is wired and
	// merges the frontmatter values onto the wire entity. omitempty
	// across the board so DB-only deployments don't surface
	// dummy-empty fields.
	CleanContent string `json:"clean_content,omitempty"`
	Summary string `json:"summary,omitempty"`
	Tags []string `json:"tags,omitempty"`
	Gaps []string `json:"gaps,omitempty"`
	Notes []noteEntry `json:"notes,omitempty"`
}

func handleEntity(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		// `with_edges` per ADR-0002 §"GET /v1/entities/{id}":
		// comma-separated edge types to expand inline. Two
		// equivalent shapes carry the "expand all types" intent
		// (per yaad-index):
		//
		// - **Presence-based**: `?with_edges` or `?with_edges=`
		// (key present, value empty) → expand all edge types.
		// Reads the param via Query().Has so an explicit empty
		// value is distinguishable from absence.
		// - **`*` / `all` sentinel**: `?with_edges=*` or
		// `?with_edges=all` → expand all edge types. Both
		// spellings are accepted; `*` is the canonical form.
		//
		// Comma-separated type filter (`?with_edges=is_about,
		// designed_by`) is unchanged — the most-used path. Absent
		// `with_edges` key (no `?with_edges` at all) keeps the
		// legacy `edges: []` behavior.
		expandEdges := r.URL.Query().Has("with_edges")
		withEdgesRaw := r.URL.Query().Get("with_edges")
		edgeTypes := parseWithEdges(withEdgesRaw)

		got, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entity")
			return
		}

		if expandEdges {
			edges, err := st.GetEdgesFor(r.Context(), got.ID, edgeTypes)
			if err != nil {
				logger.ErrorContext(r.Context(), "store.GetEdgesFor", "err", err,
					"id", id, "types", edgeTypes)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to expand edges")
				return
			}
			got.Edges = edgeRefsFromStoreEdges(edges)
		}

		// Vault-merge for the single-hop body fields (per yaad-index
		// the source issue a prior PR addendum). When WithVaultIO is wired, read
		// the vault frontmatter and overlay clean_content, summary,
		// tags, gaps, aliases, plugin, notations, notes onto the
		// wire entity. A vault read failure downgrades to the DB-only
		// shape — the entity itself still resolves.
		out := toAPIEntity(got)

		// ADR-0018 step 3: when expanding edges, surface the archived
		// flag on each endpoint so consumers can decide whether to
		// follow. Batch-load the `to` entities and stamp Archived. A
		// lookup failure downgrades to "no flag" — the edges still
		// resolve, just without the archived hint.
		if expandEdges && len(out.Edges) > 0 {
			stampEdgeArchivedFlags(r.Context(), logger, st, out.Edges)
		}
		// `notes_kind` per #186 Cut 3: agent-feedback callers may
		// scope the returned `notes` array to a single Note.Kind so
		// they can fetch only `annotation` entries without paging
		// through everyday note traffic. Empty / absent → no filter
		// (legacy shape). Invalid value → 400 invalid_argument.
		notesKind, err := parseNotesKindFilter(r.URL.Query().Get("notes_kind"))
		if err != nil {
			writeFieldError(w, "notes_kind", err.Error())
			return
		}

		if vaultReader != nil {
			ve, err := vaultReader.ReadByID(got.Kind, got.ID)
			if err != nil {
				logger.WarnContext(r.Context(),
					"vault read for entity-surface enrichment errored; serving DB-only shape",
					"id", id, "err", err)
			} else {
				out = mergeVaultEntity(out, ve)
			}
		}
		out = filterNotesByKind(out, notesKind)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities response", "err", err, "id", id)
		}
	}
}

// parseNotesKindFilter validates the optional `?notes_kind=` query
// param per #186 Cut 3. Accepts empty (no filter), `note`, or
// `annotation`. Any other value returns an error suitable for
// surfacing through writeFieldError.
func parseNotesKindFilter(raw string) (string, error) {
	switch raw {
	case "", vault.NoteKindNote, vault.NoteKindAnnotation:
		return raw, nil
	default:
		return "", fmt.Errorf("must be one of: %s, %s",
			vault.NoteKindNote, vault.NoteKindAnnotation)
	}
}

// filterNotesByKind drops entries from out.Notes whose Kind does not
// match the requested filter. Empty filter is a no-op (legacy shape).
// A note with empty Kind is treated as `note` so legacy vault entries
// (written before #186) keep matching the `notes_kind=note` filter
// even though they don't carry the bracket-tag.
func filterNotesByKind(out entity, want string) entity {
	if want == "" || len(out.Notes) == 0 {
		return out
	}
	kept := make([]noteEntry, 0, len(out.Notes))
	for _, n := range out.Notes {
		k := n.Kind
		if k == "" {
			k = vault.NoteKindNote
		}
		if k == want {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		out.Notes = nil
	} else {
		out.Notes = kept
	}
	return out
}

// parseWithEdges splits a comma-separated `with_edges` value, trims
// whitespace, and drops empty entries. The output drives GetEdgesFor's
// type filter — len == 0 means "no filter, return all edges".
//
// Per yaad-index the explicit "all types" sentinels `*` and
// `all` collapse to the no-filter shape. Either spelling is
// accepted; `*` is the canonical form. Mixing a sentinel with
// concrete edge types in the same value (e.g. `?with_edges=*,
// is_about`) treats it as "all types" — the broader of the two
// wins, which keeps the agent's mental model linear.
func parseWithEdges(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if t == "*" || t == "all" {
			return nil
		}
		out = append(out, t)
	}
	return out
}

// stampEdgeArchivedFlags sets edgeRef.Archived=true for each endpoint
// (the `to` entity) that is currently archived. ADR-0018 step 3.
//
// Implementation: one batch GetEntities for all `to` ids — the edge
// table doesn't carry endpoint-archive state directly, but the
// caller's expansion list bounds N to a small number (a single
// entity's outbound edges, capped by the per-entity edge count).
//
// On a store-layer error this just leaves Archived=false on the
// wire; the agent sees no archived hint but the edge list itself is
// still served. The caller logs at WARN so the gap is visible
// without 500'ing the request.
func stampEdgeArchivedFlags(ctx context.Context, logger *slog.Logger, st store.Store, edges []edgeRef) {
	if len(edges) == 0 {
		return
	}
	toIDs := make([]string, 0, len(edges))
	seen := make(map[string]struct{}, len(edges))
	for _, er := range edges {
		if _, dup := seen[er.To]; dup {
			continue
		}
		seen[er.To] = struct{}{}
		toIDs = append(toIDs, er.To)
	}
	matched, _, err := st.GetEntities(ctx, toIDs)
	if err != nil {
		logger.WarnContext(ctx,
			"stamp archived flag on expanded edges errored; serving without archived hint",
			"err", err, "to_count", len(toIDs))
		return
	}
	archivedSet := make(map[string]struct{}, len(matched))
	for _, e := range matched {
		if e.ArchivedAt != nil {
			archivedSet[e.ID] = struct{}{}
		}
	}
	for i := range edges {
		if _, ok := archivedSet[edges[i].To]; ok {
			edges[i].Archived = true
		}
	}
}

func edgeRefsFromStoreEdges(in []store.Edge) []store.EdgeRef {
	if len(in) == 0 {
		return []store.EdgeRef{}
	}
	out := make([]store.EdgeRef, len(in))
	for i, e := range in {
		out[i] = store.EdgeRef{Type: e.Type, To: e.To}
	}
	return out
}

// toAPIEntity converts the persistence-layer entity into the wire-format
// entity. The two types are intentionally distinct: store.ProvenanceEntry
// uses *time.Time so the layer can represent "fetched_at present /
// filled_at absent" precisely, while the wire shape encodes either as an
// RFC3339 string (or omits it via `omitempty`).
func toAPIEntity(e *store.Entity) entity {
	out := entity{
		ID: e.ID,
		Kind: e.Kind,
		Data: e.Data,
		Provenance: toAPIProvenance(e.Provenance),
		Edges: toAPIEdges(e.Edges),
		Archived: e.ArchivedAt != nil,
	}
	if out.Edges == nil {
		out.Edges = []edgeRef{}
	}
	return out
}

// mergeVaultEntity overlays vault-only fields (CleanContent, Summary,
// Tags, Gaps, Aliases, Plugin, Notations, Notes) onto a wire
// entity. Per yaad-index the source issue a prior PR addendum: GetEntity (and
// the cache-hit ingest path) should be a single hop — the agent
// gets the body + gap state without re-fetching through the plugin.
//
// The vault is the source of truth (ADR-0008) for these fields;
// the store-layer entity (which toAPIEntity reads) carries only the
// DB-mirrored slice. When the handler isn't wired with vault IO, the
// fields stay empty and `omitempty` keeps them off the wire.
func mergeVaultEntity(out entity, ve *vault.Entity) entity {
	if ve == nil {
		return out
	}
	out.Source = ve.Source
	out.Aliases = ve.Aliases
	out.Notations = ve.Notations
	out.CleanContent = ve.CleanContent
	out.Summary = ve.Summary
	out.Tags = ve.Tags
	out.Gaps = ve.Gaps
	if len(ve.Notes) > 0 {
		out.Notes = make([]noteEntry, len(ve.Notes))
		for i, c := range ve.Notes {
			out.Notes[i] = noteEntry{
				ID:       c.ID,
				Date:     c.Date.UTC().Format(time.RFC3339),
				Text:     c.Text,
				Author:   c.Author,
				Operator: c.Operator,
				Field:    c.Field,
				Kind:     c.Kind,
			}
		}
	}
	if len(ve.Attachments) > 0 {
		out.Attachments = make([]attachmentRef, len(ve.Attachments))
		for i, a := range ve.Attachments {
			out.Attachments[i] = attachmentRef{
				Name: a.Name,
				Kind: a.Kind,
				Path: a.Path,
				Bytes: a.Bytes,
			}
		}
	}
	return out
}

func toAPIProvenance(in []store.ProvenanceEntry) []provenanceEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]provenanceEntry, len(in))
	for i, p := range in {
		out[i] = provenanceEntry{
			Source: p.Source,
			OK: p.OK,
			Error: p.Error,
			ErrorMessage: p.ErrorMessage,
		}
		if p.FetchedAt != nil {
			out[i].FetchedAt = p.FetchedAt.UTC().Format(time.RFC3339)
		}
		if p.FilledAt != nil {
			out[i].FilledAt = p.FilledAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func toAPIEdges(in []store.EdgeRef) []edgeRef {
	if len(in) == 0 {
		return []edgeRef{}
	}
	out := make([]edgeRef, len(in))
	for i, e := range in {
		out[i] = edgeRef{Type: e.Type, To: e.To}
	}
	return out
}
