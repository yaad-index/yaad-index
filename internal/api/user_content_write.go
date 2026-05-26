// User-content (UGC) write endpoints per yaad-index PR-C of 3.
//
// Endpoints:
//
// POST /v1/user-content — create new UGC entity
// PUT /v1/user-content/{id}/sections/{sec} — replace one section's body
// DELETE /v1/user-content/{id} — delete entity
//
// Auth contract (mirrors a prior PR note author validation):
//
// - All endpoints require Bearer JWT (via the protect() middleware
// from yaad-index). Dev-mode AnonymousAuth bypasses identity-
// derived enforcement so existing tests + non-auth deploys keep
// working — the synthetic anon claim returns from IsAnonymousClaim
// and short-circuits the author/operator check.
//
// - **Create** stamps `data.author` from the JWT subject and
// `data.operator` from the pair-claim. The first provenance entry
// is `source: user`, fetched_at=now, ok=true.
//
// - **Edit / delete** is allowed when (a) the JWT subject matches
// the entity's stored author OR (b) the JWT operator matches the
// entity's stored operator (operator-on-behalf-of-any-agent path
// per ADR-0012's amended "Auth" section). Otherwise the call
// returns 403 author_mismatch.
//
// Concurrency:
//
// - PUT requires `If-Match: <etag>` where etag is sha256(CleanContent)[:8]
// hex, quoted (RFC 7232). The server hashes the current body and
// compares; mismatch returns 412 precondition_failed. Successful
// writes set ETag in the response so the agent can chain edits
// without a re-GET.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// userContentCreateRequest is the POST /v1/user-content body.
//
// `tags` is required and must be non-empty per ADR-0012. The body
// can be empty (a UGC entity with just a title is valid; the agent
// can fill content via PUT later).
//
// `data` is optional UGC frontmatter. Fields declared in the
// operator's `user_content_frontmatter_edges:` config trigger
// canonical-edge derivation per yaad-index (re-implementation
// of on the ADR-0021 contract). Each declared field's value
// is a `{name, kind}` object (or list of), or a pre-formed
// `<kind>:<slug>` canonical-label string (or list of) — UGC is
// operator-authored, so pre-formed labels are accepted just like
// on operator-fill. Other fields land verbatim under
// vault.Entity.Data without edge derivation.
type userContentCreateRequest struct {
	Title string `json:"title"`
	Body string `json:"body"`
	Tags []string `json:"tags"`
	Data map[string]json.RawMessage `json:"data,omitempty"`
}

// userContentSectionReplaceRequest is the PUT
// /v1/user-content/{id}/sections/{sec} body. `body` is taken
// verbatim — the server doesn't trim whitespace or add trailing
// newlines, so the agent owns whatever shape lands.
type userContentSectionReplaceRequest struct {
	Body string `json:"body"`
}

// userContentFrontmatterEditRequest is the PUT
// /v1/user-content/{id}/frontmatter body per yaad-index.
//
// `data` is the replacement frontmatter map: per-field semantics
// match POST /v1/user-content's `data` field — operator-config-
// declared frontmatter-edge fields trigger canonical-label edge
// re-derivation; undeclared fields land on `vault.Entity.Data`
// verbatim. The replacement is full (not patch): the new `data`
// REPLACES the prior data map. Edge re-derivation is idempotent
// per ADR-0021's canonical-label contract — applyCanonicalTypeEdges'
// DeleteEdgesByTypeFrom wipes prior edges for each declared
// edge_type before CreateEdge lands the new fill's edges.
//
// Title, tags, body are NOT editable here. Title is set-once
// (drives the slug → entity ID); body lives at /sections/{sec};
// tags are out of scope for this endpoint (separate follow-up if
// needed).
type userContentFrontmatterEditRequest struct {
	Data map[string]json.RawMessage `json:"data"`
}

// userContentDeleteResponse is the 200 envelope on a successful
// DELETE — small enough that JSON-on-success is preferable to 204
// since clients can branch on `ok` uniformly across the surface.
type userContentDeleteResponse struct {
	OK bool `json:"ok"`
	ID string `json:"id"`
	Deleted bool `json:"deleted"`
}

// handleUserContentCreate implements POST /v1/user-content. The agent
// sends `{title, body, tags}`; the server slugifies the title to
// derive `id = "user-content:" + slug`, stamps author + operator
// from the auth claim, and writes both the vault file and the DB
// row in one shot.
//
// Slug collision returns 409 conflict — the agent picks a new title
// (auto-suffix is deferred to a future PR per ADR-0012's open
// follow-ups).
func handleUserContentCreate(
	logger *slog.Logger,
	st store.Store,
	vaultReader *vault.Reader,
	vaultWriter *vault.Writer,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	frontmatterEdges map[string]config.UserContentFrontmatterEdgeMapping,
	writeLocks *writelocks.Manager,
	bus eventbus.Bus,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultWriter == nil || vaultReader == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}

		var req userContentCreateRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		title := strings.TrimSpace(req.Title)
		if title == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"title is required and must be non-empty after whitespace trim")
			return
		}
		if len(req.Tags) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"tags is required and must be a non-empty list (per ADR-0012)")
			return
		}
		slug, err := vault.SlugFromTitle(title)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("title slugifies to empty: %v", err))
			return
		}
		id := userContentIDPrefix + slug

		// Per-entity write-lock (yaad-index #23 + ADR-0024). The
		// slug is operator-supplied via the title; a concurrent
		// create on the same slug surfaces as a write conflict
		// rather than racing on os.Rename. Lock acquired after
		// slug derivation so the artifact key reflects the
		// resolved ID.
		release, lockOK := acquireWriteLock(w, r, writeLocks, id)
		if !lockOK {
			return
		}
		// #154: release first, then publish — Drain defer
		// (declared first) runs LAST via LIFO, after release
		// defer (declared second). Bus may be nil in test wiring.
		var pending eventbus.PendingEvents
		defer pending.Drain(r.Context(), bus)
		defer release()

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content create reached without an auth claim — middleware misconfigured",
				"id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		var author, operator string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
			operator = claim.Operator
		}

		// Slug-collision check: 409 if an entity with this id already
		// exists. The store is the cheap probe; a follow-up vault read
		// would be more authoritative but says auto-commit is the
		// version history, so the store/vault should be in sync.
		if _, err := st.GetEntity(r.Context(), id); err == nil {
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("a user-content entity with id %s already exists; pick a different title", id))
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			logger.ErrorContext(r.Context(), "store.GetEntity probe from user-content create", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to probe for existing entity")
			return
		}

		now := clock.Now().Truncate(time.Second)
		entityData := map[string]any{
			"id": id,
			"title": title,
		}
		if author != "" {
			entityData["author"] = author
		}
		if operator != "" {
			entityData["operator"] = operator
		}

		// Parse the operator-config-declared frontmatter-edge
		// fields up front: failures reject the whole request
		// before any vault/db write so the create call stays
		// transactional from the agent's perspective. UGC is
		// operator-authored content (the operator IS the writer),
		// so pre-formed canonical-label strings are accepted same
		// as operator-fill Per the prior design,.
		operatorAllKinds := make([]string, 0, len(canonicalKindReg))
		for k := range canonicalKindReg {
			operatorAllKinds = append(operatorAllKinds, k)
		}
		sort.Strings(operatorAllKinds)

		ucEdgeOps, opErr := parseUserContentFrontmatterEdges(req.Data, frontmatterEdges, operatorAllKinds)
		if opErr != nil {
			writeError(w, opErr.status, opErr.code, opErr.message)
			return
		}

		// Non-edge fields under `data` flow through to the vault
		// entity verbatim. Operator-config-declared edge fields
		// also land in Data — as a `[]string` of canonical-label
		// ids — so the frontmatter mirrors what's in the edge
		// graph. Edge ops drive the edges-table writes
		// post-vault.
		for fieldName, raw := range req.Data {
			if _, isEdgeField := frontmatterEdges[fieldName]; isEdgeField {
				continue // canonical-label list is set below from ucEdgeOps
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("data.%s: invalid JSON value: %v", fieldName, err))
				return
			}
			entityData[fieldName] = v
		}
		for _, op := range ucEdgeOps {
			entityData[op.Field] = canonicalLabelEntryIDs(op.Value)
		}

		ve := &vault.Entity{
			ID: id,
			Kind: userContentKind,
			// Per ADR-0028 §5 slash-form: user-generated content
			// (ADR-0012) is sourced from the synthetic `user`
			// plugin under the implicit `default` instance — there
			// is no operator-config instance for the UGC emitter.
			Source: []string{"user/default"},
			Data: entityData,
			Tags: append([]string(nil), req.Tags...),
			CleanContent: req.Body,
			Provenance: []vault.ProvenanceEntry{
				{Source: "user", FetchedAt: &now, OK: true},
			},
		}

		commitMsg := userContentCreateCommitMessage(id, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content create",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: id,
			Kind: userContentKind,
			Data: ve.Data,
			CreatedAt: now,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content create (vault already written)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		// Phase 2.2.C: emit entity.created on UGC create. UGC
		// IS a fresh-ingest equivalent — a new entity authored
		// by the operator. The earlier slug-collision check
		// returns 409 before reaching here, so the row is
		// guaranteed fresh on this code path (no cache-hit
		// pre-upsert probe needed). SourceOperator per
		// ADR-0012 (UGC is operator-authored).
		if bus != nil {
			// #154: queue for publish-after-unlock.
			// CausedByEntityID = id (self-cause: UGC create
			// is the operator-authored equivalent of a source-
			// plugin self-ingest).
			eventbus.QueueOrPublish(r.Context(), bus, &pending, eventbus.EntityCreatedEvent{
				ID:               id,
				Kind:             userContentKind,
				SourceTag:        eventbus.SourceOperator,
				At:               time.Now().UTC(),
				Chain:            eventbus.WorkflowChainFromContext(r.Context()),
				CausedByEntityID: id,
			})
		}

		// Frontmatter-edge derivation per yaad-index (re-impl
		// of on the ADR-0021 contract). Walks the parsed
		// canonical-label ops and creates edges from the UGC
		// entity to each label. Re-uses the shared
		// applyCanonicalTypeEdges helper (extracted to
		// internal/api/canonical_edges.go alongside): the
		// edge type is the gap-name, ensureCanonicalLabelRow
		// auto-materializes thin label rows so the FK is
		// satisfied, idempotent re-fill semantics inherited.
		//
		// This is create-side only. Re-edit (PUT-shape endpoint
		// that re-derives edges on existing UGC) is deferred to
		// a follow-up issue per yaad's scope direction.
		if len(ucEdgeOps) > 0 {
			ucGaps := userContentEdgeGapsFromMappings(frontmatterEdges)
			// Phase 2.2.C wires the bus + SourceOperator
			// (UGC is operator-authored per ADR-0012). The
			// helper publishes entity.created on each thin
			// canonical-label row materialized for the first
			// time + entity.edge_added on each derived edge.
			if err := applyCanonicalTypeEdges(r.Context(), st, id, ucEdgeOps, ucGaps, logger, bus, eventbus.SourceOperator, &pending); err != nil {
				logger.ErrorContext(r.Context(), "user-content create: canonical-edge derivation",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to materialize canonical-edges")
				return
			}
		}

		sections := vault.ParseSections(ve.CleanContent)
		page := buildSectionsPage(sections, 0, sectionsDefaultLimit)
		out := userContentEntityResponse{
			OK: true,
			ID: ve.ID,
			Kind: ve.Kind,
			Data: ve.Data,
			Tags: ve.Tags,
			Provenance: vaultProvenanceToAPI(ve.Provenance),
			Sections: page,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.Header().Set("Location", "/v1/user-content/"+id)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content (create) response", "err", err, "id", id)
		}
	}
}

// handleUserContentSectionReplace implements PUT
// /v1/user-content/{id}/sections/{sec}. Validates author/operator,
// honors If-Match, applies vault.ReplaceSectionBody, persists.
func handleUserContentSectionReplace(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content section-replace reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may edit this user-content entity")
			return
		}

		// If-Match concurrency gate — required on PUT Per the prior design,. Compare
		// the current body's etag against the supplied one; mismatch =
		// 412 so the agent re-GETs and retries with the fresh content.
		ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
		if ifMatch == "" {
			writeError(w, http.StatusPreconditionRequired, "precondition_required",
				"If-Match header is required on user-content section edits (per yaad-index)")
			return
		}
		current := userContentEtag(ve.CleanContent)
		if ifMatch != current {
			w.Header().Set("ETag", current)
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match etag does not match current entity body; re-GET and retry with the fresh etag")
			return
		}

		var req userContentSectionReplaceRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		addr := r.PathValue("sec")
		if addr == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"section address is required")
			return
		}
		sections := vault.ParseSections(ve.CleanContent)
		idx, ok := vault.ResolveSectionAddr(sections, addr)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no section %q on %s (or duplicate-slug, in which case use positional index)", addr, id))
			return
		}

		// Section-scoped write-lock (yaad-index #23 + ADR-0024).
		// Key on the resolved index (not the raw addr) so two writers
		// targeting the same section via slug + positional forms
		// collide correctly. Two writers on DIFFERENT sections of the
		// same UGC file proceed concurrently — the OS-rename layer
		// serializes the final disk write per ADR-0008.
		artifactKey := fmt.Sprintf("%s#%d", id, idx)
		release, lockOK := acquireWriteLock(w, r, writeLocks, artifactKey)
		if !lockOK {
			return
		}
		defer release()

		newBody, err := vault.ReplaceSectionBody(ve.CleanContent, sections, idx, req.Body)
		if err != nil {
			logger.ErrorContext(r.Context(), "vault.ReplaceSectionBody", "err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to assemble new entity body")
			return
		}
		ve.CleanContent = newBody

		// Provenance: append a `user` row marking this section edit.
		// Mirrors fill's provenance pattern — each agent-side mutation
		// shows up in the audit log.
		now := clock.Now().Truncate(time.Second)
		ve.Provenance = append(ve.Provenance, vault.ProvenanceEntry{
			Source: "user",
			FilledAt: &now,
			OK: true,
		})

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := userContentEditCommitMessage(id, addr, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content section-replace",
				"err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: id,
			Kind: userContentKind,
			Data: ve.Data,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content section-replace",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		// Re-parse so the response carries the post-edit section list
		// (the section we just wrote may have shifted positions if
		// the agent's new body added/removed nested headings).
		newSections := vault.ParseSections(ve.CleanContent)
		// Best-effort: re-resolve by addr so the response echoes the
		// CURRENT section under the same address. If the agent
		// rewrote a parent section in a way that removes the addressed
		// sub-heading, the re-resolve fails and we fall back to
		// echoing the section by its prior index.
		newIdx, found := vault.ResolveSectionAddr(newSections, addr)
		if !found {
			newIdx = idx
			if newIdx >= len(newSections) {
				newIdx = len(newSections) - 1
			}
			if newIdx < 0 {
				newIdx = 0
			}
		}
		var sectionEcho userContentSection
		if len(newSections) > 0 {
			sectionEcho = vaultSectionToAPI(newSections[newIdx])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(userContentSectionResponse{
			OK: true,
			ID: id,
			Section: sectionEcho,
		}); err != nil {
			logger.ErrorContext(r.Context(),
				"encode /v1/user-content/{id}/sections/{sec} (PUT) response",
				"err", err, "id", id, "sec", addr)
		}
	}
}

// userContentSectionAddRequest is the POST
// /v1/user-content/{id}/sections body per #299.
//
// after_sec is the address to insert AFTER:
//   - heading slug or positional index → new section lands as the
//     next sibling of that section.
//   - "-1" or "" → prepend at document start (after any pre-heading
//     body section).
//   - omitted/null → append at end (vault.InsertSection's
//     len(sections) shape).
//
// depth (1..6) overrides the heading depth; 0/omitted falls back to
// the after-section's depth (or 1 if appending to an empty doc).
type userContentSectionAddRequest struct {
	AfterSec *string `json:"after_sec,omitempty"`
	Depth    int     `json:"depth,omitempty"`
	Heading  string  `json:"heading"`
	Body     string  `json:"body"`
}

// userContentSectionRenameRequest is the PATCH
// /v1/user-content/{id}/sections/{sec}/heading body per #299.
// `new_heading` replaces the addressed section's heading text;
// body + nested headings are preserved verbatim.
type userContentSectionRenameRequest struct {
	NewHeading string `json:"new_heading"`
}

// handleUserContentSectionAdd implements POST
// /v1/user-content/{id}/sections per #299. Inserts a new section
// at the address resolved from `after_sec`. Etag-gated (same
// If-Match contract as section-replace); author/operator gated.
//
// Slug-collision (409 conflict): if the new heading's slug
// matches an existing same-depth sibling's slug at the same
// containment level, reject before write so the agent picks a
// different heading rather than addressing-by-slug-now-ambiguous.
func handleUserContentSectionAdd(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content section-add reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may edit this user-content entity")
			return
		}

		ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
		if ifMatch == "" {
			writeError(w, http.StatusPreconditionRequired, "precondition_required",
				"If-Match header is required on user-content section edits (per yaad-index)")
			return
		}
		current := userContentEtag(ve.CleanContent)
		if ifMatch != current {
			w.Header().Set("ETag", current)
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match etag does not match current entity body; re-GET and retry with the fresh etag")
			return
		}

		var req userContentSectionAddRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		if req.Heading == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"`heading` is required")
			return
		}

		sections := vault.ParseSections(ve.CleanContent)
		afterIdx := len(sections) // default: append at end
		if req.AfterSec != nil {
			addr := *req.AfterSec
			switch {
			case addr == "" || addr == "-1":
				afterIdx = -1
			default:
				idx, ok := vault.ResolveSectionAddr(sections, addr)
				if !ok {
					writeError(w, http.StatusNotFound, "not_found",
						fmt.Sprintf("no section %q on %s (or duplicate-slug, in which case use positional index)", addr, id))
					return
				}
				afterIdx = idx
			}
		}

		// Slug-collision pre-check restricted to the SAME containment
		// parent as the insertion slot. Two `### Notes` under
		// different `## A` and `## B` parents are legal — they're not
		// siblings of each other under the containment model. Mirror
		// vault.InsertSection's depth default so the check uses the
		// same depth the writer will pick.
		depth := req.Depth
		if depth <= 0 {
			depth = vault.DefaultInsertDepth(sections, afterIdx)
		}
		newSlug := vault.SlugifyHeading(req.Heading)
		if newSlug != "" && vault.SectionSlugConflictsAtInsertion(sections, afterIdx, depth, newSlug) {
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("section heading %q would slugify to %q which already exists as a same-depth sibling under the same parent", req.Heading, newSlug))
			return
		}

		// Section-scoped write-lock keyed on a synthetic "add" marker so
		// concurrent adds to the same entity serialize. Other section
		// edits on different sections still proceed in parallel.
		artifactKey := fmt.Sprintf("%s#add", id)
		release, lockOK := acquireWriteLock(w, r, writeLocks, artifactKey)
		if !lockOK {
			return
		}
		defer release()

		newBody, insertedOffset, err := vault.InsertSection(ve.CleanContent, sections, afterIdx, depth, req.Heading, req.Body)
		if err != nil {
			logger.ErrorContext(r.Context(), "vault.InsertSection", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to assemble new entity body")
			return
		}
		ve.CleanContent = newBody

		now := clock.Now().Truncate(time.Second)
		ve.Provenance = append(ve.Provenance, vault.ProvenanceEntry{
			Source:   "user",
			FilledAt: &now,
			OK:       true,
		})

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := userContentSectionAddCommitMessage(id, req.Heading, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content section-add",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:   id,
			Kind: userContentKind,
			Data: ve.Data,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content section-add",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		// Re-parse so the response echoes the section's final
		// post-insert index. Slug + depth alone aren't unique under
		// the containment model — a same-slug sibling can legally
		// live under a different parent — so we locate the new
		// section by the byte offset returned from InsertSection.
		newSections := vault.ParseSections(ve.CleanContent)
		newIdx := 0
		for i, s := range newSections {
			if s.ByteOffset == insertedOffset && s.Depth == depth {
				newIdx = i
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(userContentSectionResponse{
			OK:      true,
			ID:      id,
			Section: vaultSectionToAPI(newSections[newIdx]),
		}); err != nil {
			logger.ErrorContext(r.Context(),
				"encode /v1/user-content/{id}/sections (POST) response",
				"err", err, "id", id)
		}
	}
}

// handleUserContentSectionRenameHeading implements PATCH
// /v1/user-content/{id}/sections/{sec}/heading per #299.
// Rewrites only the heading line of the addressed section; body
// + nested headings preserved verbatim. Etag-gated. 409 conflict
// when the new slug collides with a sibling.
func handleUserContentSectionRenameHeading(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content section-rename reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may edit this user-content entity")
			return
		}

		ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
		if ifMatch == "" {
			writeError(w, http.StatusPreconditionRequired, "precondition_required",
				"If-Match header is required on user-content section edits (per yaad-index)")
			return
		}
		current := userContentEtag(ve.CleanContent)
		if ifMatch != current {
			w.Header().Set("ETag", current)
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match etag does not match current entity body; re-GET and retry with the fresh etag")
			return
		}

		var req userContentSectionRenameRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}
		if req.NewHeading == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"`new_heading` is required")
			return
		}

		addr := r.PathValue("sec")
		if addr == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"section address is required")
			return
		}
		sections := vault.ParseSections(ve.CleanContent)
		idx, ok := vault.ResolveSectionAddr(sections, addr)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no section %q on %s (or duplicate-slug, in which case use positional index)", addr, id))
			return
		}
		if sections[idx].Depth == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"the pre-heading section (index 0) has no heading to rename; use add+delete to introduce one")
			return
		}

		// Slug-collision pre-check restricted to same-parent
		// siblings of the renamed section. `## A / ### Notes` and
		// `## B / ### Notes` are legal — only colliding with
		// sections sharing the same containment-parent rejects.
		newSlug := vault.SlugifyHeading(req.NewHeading)
		if newSlug != "" && vault.SectionSlugConflicts(sections, idx, newSlug) {
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("new heading %q would slugify to %q which already exists as a same-depth sibling under the same parent", req.NewHeading, newSlug))
			return
		}

		artifactKey := fmt.Sprintf("%s#%d", id, idx)
		release, lockOK := acquireWriteLock(w, r, writeLocks, artifactKey)
		if !lockOK {
			return
		}
		defer release()

		oldHeading := sections[idx].Heading
		newBody, err := vault.RenameSectionHeading(ve.CleanContent, sections, idx, req.NewHeading)
		if err != nil {
			logger.ErrorContext(r.Context(), "vault.RenameSectionHeading", "err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to assemble new entity body")
			return
		}
		ve.CleanContent = newBody

		now := clock.Now().Truncate(time.Second)
		ve.Provenance = append(ve.Provenance, vault.ProvenanceEntry{
			Source:   "user",
			FilledAt: &now,
			OK:       true,
		})

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := userContentSectionRenameCommitMessage(id, oldHeading, req.NewHeading, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content section-rename",
				"err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:   id,
			Kind: userContentKind,
			Data: ve.Data,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content section-rename",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		newSections := vault.ParseSections(ve.CleanContent)
		if idx >= len(newSections) {
			idx = len(newSections) - 1
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(userContentSectionResponse{
			OK:      true,
			ID:      id,
			Section: vaultSectionToAPI(newSections[idx]),
		}); err != nil {
			logger.ErrorContext(r.Context(),
				"encode /v1/user-content/{id}/sections/{sec}/heading (PATCH) response",
				"err", err, "id", id, "sec", addr)
		}
	}
}

// handleUserContentSectionDelete implements DELETE
// /v1/user-content/{id}/sections/{sec} per #299. Removes the
// addressed section and every textually contained nested section
// (containment model). Etag-gated. Returns the entity's new etag
// + the removed section's old index.
func handleUserContentSectionDelete(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content section-delete reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may edit this user-content entity")
			return
		}

		ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
		if ifMatch == "" {
			writeError(w, http.StatusPreconditionRequired, "precondition_required",
				"If-Match header is required on user-content section edits (per yaad-index)")
			return
		}
		current := userContentEtag(ve.CleanContent)
		if ifMatch != current {
			w.Header().Set("ETag", current)
			writeError(w, http.StatusPreconditionFailed, "precondition_failed",
				"If-Match etag does not match current entity body; re-GET and retry with the fresh etag")
			return
		}

		addr := r.PathValue("sec")
		if addr == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"section address is required")
			return
		}
		sections := vault.ParseSections(ve.CleanContent)
		idx, ok := vault.ResolveSectionAddr(sections, addr)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no section %q on %s (or duplicate-slug, in which case use positional index)", addr, id))
			return
		}
		if sections[idx].Depth == 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"the pre-heading section (index 0) cannot be deleted; use PUT /sections/0 with body \"\" to clear it instead")
			return
		}

		artifactKey := fmt.Sprintf("%s#%d", id, idx)
		release, lockOK := acquireWriteLock(w, r, writeLocks, artifactKey)
		if !lockOK {
			return
		}
		defer release()

		removedHeading := sections[idx].Heading
		removedIdx := sections[idx].Index
		newBody, err := vault.DeleteSection(ve.CleanContent, sections, idx)
		if err != nil {
			logger.ErrorContext(r.Context(), "vault.DeleteSection", "err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to assemble new entity body")
			return
		}
		ve.CleanContent = newBody

		now := clock.Now().Truncate(time.Second)
		ve.Provenance = append(ve.Provenance, vault.ProvenanceEntry{
			Source:   "user",
			FilledAt: &now,
			OK:       true,
		})

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := userContentSectionDeleteCommitMessage(id, removedHeading, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content section-delete",
				"err", err, "id", id, "sec", addr)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:   id,
			Kind: userContentKind,
			Data: ve.Data,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content section-delete",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(userContentSectionDeleteResponse{
			OK:         true,
			ID:         id,
			RemovedIdx: removedIdx,
		}); err != nil {
			logger.ErrorContext(r.Context(),
				"encode /v1/user-content/{id}/sections/{sec} (DELETE) response",
				"err", err, "id", id, "sec", addr)
		}
	}
}

// userContentSectionDeleteResponse is the 200 envelope returned by
// DELETE /v1/user-content/{id}/sections/{sec}. Echoes the removed
// section's index so the agent's audit log shows what was deleted.
type userContentSectionDeleteResponse struct {
	OK         bool   `json:"ok"`
	ID         string `json:"id"`
	RemovedIdx int    `json:"removed_idx"`
}

// handleUserContentFrontmatterEdit implements PUT
// /v1/user-content/{id}/frontmatter per yaad-index. Replaces
// the entity's `data` map (full replacement, not patch) and
// re-derives canonical-label edges via the shared
// applyCanonicalTypeEdges helper from — the helper's
// DeleteEdgesByTypeFrom + CreateEdge cycle gives idempotent
// re-edit semantics: re-edit-with-same-values produces the same
// edges, re-edit-with-changed-values replaces cleanly, edge
// graph end-state matches the new fill exactly.
//
// Auth: same author/operator gate as the section-replace path.
// No If-Match header gate — frontmatter edits are full
// replacements, so concurrency-induced data loss is bounded to
// last-writer-wins; the body etag (from /sections) doesn't apply
// to frontmatter content.
//
// Vault-then-DB ordering per ADR-0008. Auto-commit prefix
// `edit-frontmatter: <id>`.
func handleUserContentFrontmatterEdit(
	logger *slog.Logger,
	st store.Store,
	vaultReader *vault.Reader,
	vaultWriter *vault.Writer,
	canonicalKindReg map[string]config.CanonicalKindConfig,
	frontmatterEdges map[string]config.UserContentFrontmatterEdgeMapping,
	bus eventbus.Bus,
	writeLocks *writelocks.Manager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		// Whole-entity write-lock for frontmatter edits (yaad-index
		// #23 + ADR-0024). Section-scoped key isn't applicable —
		// frontmatter spans the whole file. Keyed on entity ID;
		// section writers using `<id>#<idx>` don't collide here,
		// which matches the documented v1 trade-off in
		// internal/writelocks's TestAcquire_UGCSectionVsWholeFileDontConflict.
		release, ok := acquireWriteLock(w, r, writeLocks, id)
		if !ok {
			return
		}
		// #154: release first, then publish — LIFO defer ordering.
		var pending eventbus.PendingEvents
		defer pending.Drain(r.Context(), bus)
		defer release()
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content frontmatter-edit reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may edit this user-content entity")
			return
		}

		var req userContentFrontmatterEditRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		// Parse declared frontmatter-edge fields up front so
		// failures reject the whole edit before any vault/db
		// write. UGC is operator-authored content (the operator
		// IS the writer), so pre-formed canonical-label strings
		// are accepted same as on operator-fill per
		// yaad-index.
		operatorAllKinds := make([]string, 0, len(canonicalKindReg))
		for k := range canonicalKindReg {
			operatorAllKinds = append(operatorAllKinds, k)
		}
		sort.Strings(operatorAllKinds)

		ucEdgeOps, opErr := parseUserContentFrontmatterEdges(req.Data, frontmatterEdges, operatorAllKinds)
		if opErr != nil {
			writeError(w, opErr.status, opErr.code, opErr.message)
			return
		}

		// Build the new Data map Per the prior design,. Identity-bearing
		// fields (id, title, author, operator) carry forward
		// from the existing entity — title is set-once, author/
		// operator stamp the create-time identity. Everything
		// else replaces from req.Data.
		newData := make(map[string]any, len(req.Data)+4)
		for _, key := range []string{"id", "title", "author", "operator"} {
			if v, ok := ve.Data[key]; ok {
				newData[key] = v
			}
		}
		for fieldName, raw := range req.Data {
			if _, isEdgeField := frontmatterEdges[fieldName]; isEdgeField {
				continue // canonical-label list set below from ucEdgeOps
			}
			if fieldName == "id" || fieldName == "title" || fieldName == "author" || fieldName == "operator" {
				// Reject attempts to mutate identity-bearing
				// fields via frontmatter-edit. Title is set-once
				// per the create contract; id is derived from
				// title; author/operator are stamped from the
				// auth claim at create time.
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("data.%s is set-once at create time and cannot be edited", fieldName))
				return
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument",
					fmt.Sprintf("data.%s: invalid JSON value: %v", fieldName, err))
				return
			}
			newData[fieldName] = v
		}
		for _, op := range ucEdgeOps {
			newData[op.Field] = canonicalLabelEntryIDs(op.Value)
		}
		ve.Data = newData

		// Provenance: append a `user` row marking this
		// frontmatter edit. Mirrors the section-replace
		// pattern's audit-trail — every operator-side mutation
		// shows up.
		now := clock.Now().Truncate(time.Second)
		ve.Provenance = append(ve.Provenance, vault.ProvenanceEntry{
			Source: "user",
			FilledAt: &now,
			OK: true,
		})

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := userContentFrontmatterEditCommitMessage(id, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteWithCommit from user-content frontmatter-edit",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		// Mirror to DB.
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: id,
			Kind: userContentKind,
			Data: ve.Data,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from user-content frontmatter-edit",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		// Edge re-derivation Per the prior design, /. applyCanonicalTypeEdges'
		// DeleteEdgesByTypeFrom step wipes the prior fill's edges
		// for each declared edge_type before CreateEdge lands the
		// new fill. Idempotent semantics: re-edit-with-same-values
		// produces the same edge graph; re-edit-with-changed
		// replaces cleanly.
		//
		// Always run the helper — even when ucEdgeOps is empty,
		// the agent may have cleared a previously-set edge field
		// by omitting it from req.Data; in that case we'd want to
		// wipe those edges. To handle that, walk every declared
		// mapping and synthesize a no-op opSet (empty list)
		// for fields the agent didn't include — DeleteEdgesByTypeFrom
		// fires, no new edges land.
		fullOps := buildFullEditOpsFromMappings(ucEdgeOps, frontmatterEdges, req.Data)
		if len(fullOps) > 0 {
			ucGaps := userContentEdgeGapsFromMappings(frontmatterEdges)
			// Phase 2.2.C: SourceOperator (UGC is operator-
			// authored per ADR-0012). No entity.created here —
			// this is the edit path; the entity already exists.
			// The helper still emits entity.edge_added on each
			// new edge (and entity.created on any newly-
			// materialized thin canonical-label rows).
			if err := applyCanonicalTypeEdges(r.Context(), st, id, fullOps, ucGaps, logger, bus, eventbus.SourceOperator, &pending); err != nil {
				logger.ErrorContext(r.Context(), "user-content frontmatter-edit: canonical-edge re-derivation",
					"err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to re-materialize canonical-edges")
				return
			}
		}

		sections := vault.ParseSections(ve.CleanContent)
		page := buildSectionsPage(sections, 0, sectionsDefaultLimit)
		out := userContentEntityResponse{
			OK: true,
			ID: ve.ID,
			Kind: ve.Kind,
			Data: ve.Data,
			Tags: ve.Tags,
			Provenance: vaultProvenanceToAPI(ve.Provenance),
			Sections: page,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content/{id}/frontmatter (PUT) response",
				"err", err, "id", id)
		}
	}
}

// buildFullEditOpsFromMappings synthesizes the per-edge-type op
// set the frontmatter-edit path needs to hand applyCanonicalTypeEdges.
// Every declared mapping becomes an op — fields the agent
// included in the request body land their parsed canonical-label
// list; fields the agent omitted land an empty list so
// DeleteEdgesByTypeFrom wipes any prior edges of that type
// without creating new ones. The "edit by omission clears"
// semantic mirrors operator-fill's `null` clear shape.
//
// Returns nil when there are no declared mappings — the caller
// skips applyCanonicalTypeEdges entirely (no-op).
func buildFullEditOpsFromMappings(
	parsedOps []operatorFillOp,
	mappings map[string]config.UserContentFrontmatterEdgeMapping,
	requestData map[string]json.RawMessage,
) []operatorFillOp {
	if len(mappings) == 0 {
		return nil
	}
	parsedByEdgeType := make(map[string]operatorFillOp, len(parsedOps))
	for _, op := range parsedOps {
		parsedByEdgeType[op.Field] = op
	}
	out := make([]operatorFillOp, 0, len(mappings))
	for fieldName, mapping := range mappings {
		if op, has := parsedByEdgeType[mapping.EdgeType]; has {
			out = append(out, op)
			continue
		}
		// Field omitted from request — synthesize an empty-list
		// op so DeleteEdgesByTypeFrom wipes any prior edges of
		// that type without creating new ones.
		_ = fieldName // referenced via mapping; explicit to satisfy go vet
		out = append(out, operatorFillOp{
			Field: mapping.EdgeType,
			Kind: opSet,
			Value: []canonicalLabelEntry{},
		})
	}
	return out
}

// handleUserContentDelete implements DELETE /v1/user-content/{id}.
// Validates author/operator, removes the vault file (with auto-
// commit), drops the store rows via DeleteEntityCascade.
func handleUserContentDelete(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, writeLocks *writelocks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}
		// Whole-entity write-lock (yaad-index #23 + ADR-0024).
		release, ok := acquireWriteLock(w, r, writeLocks, id)
		if !ok {
			return
		}
		defer release()
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"user-content delete reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		// Authorization first — a cross-author intruder must learn
		// 403, not the lifecycle hint (knowing whether *someone else's*
		// entity is archived is an information leak).
		if !canEditUserContent(claim, ve) {
			writeError(w, http.StatusForbidden, "author_mismatch",
				"only the original author or the entity's operator may delete this user-content entity")
			return
		}

		// ADR-0018 step 4 state-machine: DELETE only valid on
		// archived entities. UGC entities follow the same contract
		// — the operator must explicitly archive first. Re-load
		// from the store side to get the authoritative archived_at;
		// the vault entity loaded above is the body, not the lifecycle.
		// The archive endpoint is on the shared `/v1/entities/{id}/archive`
		// route — UGC has no separate archive sub-route.
		row, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no user-content with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity from user-content delete", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to load entity")
			return
		}
		if row.ArchivedAt == nil {
			writeError(w, http.StatusConflict, "must archive before delete",
				fmt.Sprintf("POST /v1/entities/%s/archive first; DELETE only destroys archived entities (ADR-0018)", id))
			return
		}

		var author string
		if !IsAnonymousClaim(claim) {
			author = claim.Subject
		}
		commitMsg := entityDestroyCommitMessage(id, userContentKind, author)
		commitAuthor := agentAuthorRef(author)
		if err := vaultWriter.DestroyArchivedWithCommit(r.Context(), userContentKind, id, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.DestroyArchivedWithCommit from user-content delete",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to remove archived vault file")
			return
		}

		if err := st.DeleteEntityCascade(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
			// Vault file is gone but DB row failed to drop. Reindex's
			// disappear-pass will reconcile on next walk; flag it loudly
			// so the operator can investigate.
			logger.ErrorContext(r.Context(), "store.DeleteEntityCascade from user-content delete (vault file already removed)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to drop entity from DB; vault file is already removed (reindex will reconcile)")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(userContentDeleteResponse{
			OK: true, ID: id, Deleted: true,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content (delete) response", "err", err, "id", id)
		}
	}
}

// canEditUserContent returns true if the claim is allowed to mutate
// the entity Per the prior design,'s auth contract: same agent OR same operator.
// Anonymous claims (dev-mode AnonymousAuth bypass) always return
// true so existing tests + non-auth deploys keep working.
func canEditUserContent(claim *auth.Claim, ve *vault.Entity) bool {
	if IsAnonymousClaim(claim) {
		return true
	}
	author, _ := ve.Data["author"].(string)
	operator, _ := ve.Data["operator"].(string)
	if claim.Subject != "" && claim.Subject == author {
		return true
	}
	if operator != "" && claim.Operator == operator {
		return true
	}
	return false
}

// parseUserContentFrontmatterEdges walks the operator-config-
// declared frontmatter-edge mappings and parses each declared
// field's value from the request's `data` map into a list of
// canonical-label ids. Mirrors the canonical_type fill parsing
// from /: dual shape (object form + pre-formed-label
// string), `allowPreformedLabels=true` since UGC is operator-
// authored.
//
// Each declared mapping's `target_kind` becomes the gap's `Kinds`
// allowlist (as a single-element list) so kind validation in
// parseCanonicalLabelList enforces "the value's kind must equal
// this mapping's target_kind." Missing fields skip silently —
// the operator can leave them unset.
//
// Returns the per-field edge ops (Field=mapping.EdgeType,
// Value=[]string of canonical-label ids) ready for
// applyCanonicalTypeEdges.
func parseUserContentFrontmatterEdges(
	data map[string]json.RawMessage,
	mappings map[string]config.UserContentFrontmatterEdgeMapping,
	operatorAllKinds []string,
) ([]operatorFillOp, *opError) {
	if len(mappings) == 0 || len(data) == 0 {
		return nil, nil
	}
	ops := make([]operatorFillOp, 0, len(mappings))
	for fieldName, mapping := range mappings {
		raw, ok := data[fieldName]
		if !ok {
			continue
		}
		// Wrap a bare object/string in a single-element array so
		// the shared list parser handles all four shapes
		// uniformly (single object, list of objects, single
		// string, list of strings).
		listRaw := wrapAsList(raw)
		gap := config.GapSpec{
			Type: config.CanonicalTypeName,
			Kinds: []string{mapping.TargetKind},
		}
		labels, perr := parseCanonicalLabelList(fieldName, listRaw, gap, operatorAllKinds, true)
		if perr != nil {
			return nil, perr
		}
		ops = append(ops, operatorFillOp{
			Field: mapping.EdgeType,
			Kind: opSet,
			Value: labels,
		})
	}
	return ops, nil
}

// wrapAsList rewrites a bare object `{...}` or string `"..."` JSON
// value as a single-element array `[{...}]` / `["..."]`. Already-
// arrayed inputs round-trip unchanged. Lets UGC frontmatter accept
// both `data.publisher: "Roxley"` and `data.designed_by: ["X", "Y"]`
// shapes via the same shared list parser.
func wrapAsList(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 {
		return raw
	}
	if trimmed[0] == '[' {
		return raw
	}
	wrapped := []byte("[")
	wrapped = append(wrapped, []byte(trimmed)...)
	wrapped = append(wrapped, ']')
	return wrapped
}

// userContentEdgeGapsFromMappings synthesizes a `gaps` map keyed
// by edge_type so applyCanonicalTypeEdges (which expects
// `map[string]config.GapSpec` keyed by op.Field) sees a
// canonical_type gap-spec for each declared mapping. The synthesized
// gap-spec carries the mapping's target_kind in `Kinds` so the
// helper's spec.Type check passes.
func userContentEdgeGapsFromMappings(
	mappings map[string]config.UserContentFrontmatterEdgeMapping,
) map[string]config.GapSpec {
	out := make(map[string]config.GapSpec, len(mappings))
	for _, m := range mappings {
		out[m.EdgeType] = config.GapSpec{
			Type: config.CanonicalTypeName,
			Kinds: []string{m.TargetKind},
		}
	}
	return out
}
