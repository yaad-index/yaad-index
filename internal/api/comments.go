package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// commentsRequest is the POST /v1/entities/{id}/comments body. The
// server stamps `date` (UTC) — clients never send it.
//
// Per alice2-index a prior PR, `author` must match the JWT subject
// attached by the auth middleware. Empty `author` is filled from the
// claim's Subject for client convenience; a non-empty `author` that
// disagrees with the claim returns 403 author_mismatch. Clients have
// no way to set `operator` — that field is stamped server-side from
// the claim's Operator and is read-only on the wire.
type commentsRequest struct {
	Text string `json:"text"`
	Author string `json:"author,omitempty"`
}

// commentEntry is the wire shape for a single comment on the response.
// Matches the vault.Comment frontmatter shape; Date is RFC3339 UTC so
// clients can do their own timezone rendering.
//
// Operator (added a prior PR) names the human resource owner from the
// pair-claim. Empty for legacy comments (vault entries written before
// had no operator stamp). Always populated on new comments.
type commentEntry struct {
	Date string `json:"date"`
	Text string `json:"text"`
	Author string `json:"author,omitempty"`
	Operator string `json:"operator,omitempty"`
}

// commentsResponse is the 201 envelope: the just-appended comment plus
// the merged entity (so the caller can refresh local state without a
// follow-up GET). Mirrors the fillResponse shape.
type commentsResponse struct {
	OK bool `json:"ok"`
	Comment commentEntry `json:"comment"`
	Entity entity `json:"entity"`
}

// handleComments implements POST /v1/entities/{id}/comments.
//
// Vault-first append (per ADR-0008): the new comment is added to the
// entity's body `## Comments` table (Per the prior design, — comments live in the
// body, frontmatter just carries `comment_count: N`); the DB row is
// updated to mirror the new state. The vault file is the source of
// truth; the DB is a derived index.
//
// **No AppendProvenance call** (deliberate, do not "fix"). The
// provenance log is for fetch/fill events — "where did this entity's
// data come from" — and a comment-append isn't a fetch or a structural
// data change. The body comments table IS the audit trail (date +
// author + text per entry); duplicating that into the provenance log
// would pollute it. The provenance shape (`{source, fetched_at, ok}`)
// also doesn't fit a comment-append cleanly. Keep these surfaces
// separate.
//
// Append-only in v1 — edit/delete-by-comment-id is a follow-up per
// ADR-0008's Open Questions section. Server stamps `date` (UTC); the
// client never supplies it.
//
// Input normalization: `text` is `strings.TrimSpace`-d before storage
// to address the cold-reviewer's a prior PR review note about the vault parser
// trimming leading whitespace from body comment blocks. After this
// trim, the API → vault → reindex round-trip is lossless for non-
// whitespace text content; pure-whitespace inputs surface as 400
// invalid_argument.
//
// Asymmetry with a prior PR ingest: like the fill endpoint (a prior PR),
// comments require vault wiring — there's no "DB-only fallback"
// for comments because the canonical comment list lives in vault
// frontmatter. Returns 503 vault_required when WithVaultIO is
// omitted.
// canonicalKindReg widens the carve-out per alice2-index: comments
// targeting a canonical-label thin row (`<kind>:<slug>` where `kind`
// is in the operator's canonical_kinds registry) auto-materialize
// the vault file at `{ROOT}/ct/<kind>/<slug>.md` when the caller
// holds operator authority. Without operator authority, or when the
// id isn't canonical-label-shaped, the existing 404 paths apply.
func handleComments(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, canonicalKindReg map[string]config.CanonicalKindConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req commentsRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		text := strings.TrimSpace(req.Text)
		if text == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				"text is required and must be non-empty after whitespace trim")
			return
		}
		author := strings.TrimSpace(req.Author)

		// Per alice2-index a prior PR: enforce author == JWT subject.
		// Empty author → fill from claim (client-convenience). Non-
		// empty author that disagrees with the claim → 403. The
		// claim is always present at this point: RequireAuth lands a
		// real claim, AnonymousAuth lands the synthetic anon shape;
		// the only path with no claim at all is a misconfigured
		// handler chain (not reachable in production wiring).
		//
		// In AnonymousAuth dev-mode (auth.required=false) the synthetic
		// claim has no real identity to enforce against, so the
		// author + operator stay client-controlled and unstamped —
		// preserves the legacy behavior for unauthenticated dev
		// binaries and existing dev-mode tests.
		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"comments handler reached without an auth claim — middleware misconfigured",
				"id", r.PathValue("id"))
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}
		var operator string
		if !IsAnonymousClaim(claim) {
			if author == "" {
				author = claim.Subject
			} else if author != claim.Subject {
				writeError(w, http.StatusForbidden, "author_mismatch",
					"author claim does not match authenticated agent")
				return
			}
			operator = claim.Operator
		}

		id := r.PathValue("id")

		if vaultReader == nil || vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"comments require vault.path configuration; the comment list lives in vault frontmatter")
			return
		}

		// autoMaterialize covers the "thin DB row exists, vault file
		// missing" case for canonical-label entities (per alice2-index
		//). The comment write then creates the vault file via
		// WriteCanonicalLabelWithCommit. Per the operator's scope
		// tightening, comments do NOT create the entity row from
		// nothing — that path stays a 404 to prevent dangling
		// comments on entities that don't exist. Operator-fill is the
		// deliberate-create path; comments are casual and need an
		// existing entity to attach to.
		var autoMaterialize bool

		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no entity with id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "store.GetEntity from comments", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to look up entity")
			return
		}

		ve, err := vaultReader.ReadByID(got.Kind, id)
		if err != nil {
			if !vault.IsNotExist(err) {
				logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from comments", "err", err, "id", id)
				writeError(w, http.StatusInternalServerError, "internal_error",
					"failed to read vault file")
				return
			}
			// DB row exists (thin label Per the prior design, phase B) but no
			// vault file. Auto-materialize the canonical-label
			// vault file when the caller holds operator authority
			// AND the kind is in the canonical-kind registry.
			// Source-shape entities with a missing vault file
			// remain a 404 — the daemon never auto-creates
			// source-shape vault files; that path is plugin-driven.
			kindCfg, kindOK := canonicalKindReg[got.Kind]
			if !kindOK || !ClaimHasOperatorAuthority(claim) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for id %s (kind=%s)", id, got.Kind))
				return
			}
			ve = newCanonicalLabelEntity(got.ID, got.Kind, kindCfg)
			autoMaterialize = true
		}

		// Truncate to second precision so the YAML frontmatter encoding
		// and the body `## Comments` section header (which uses
		// RFC3339, second-precision) round-trip to the SAME Date value
		// post-Unmarshal. Without the truncate, frontmatter retains
		// nanos but body loses them — the vault.mergeComments dedup
		// then fails to collapse the two reads of the same comment,
		// producing duplicate entries on every read-modify-write
		// cycle.
		now := clock.Now().Truncate(time.Second)
		newComment := vault.Comment{
			Date: now,
			Text: text,
			Author: author,
			Operator: operator,
		}
		ve.Comments = append(ve.Comments, newComment)

		commitMsg := commentCommitMessage(ve.ID, author)
		commitAuthor := agentAuthorRef(author)
		writeErr := error(nil)
		if autoMaterialize {
			writeErr = vaultWriter.WriteCanonicalLabelWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		} else {
			writeErr = vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		}
		if writeErr != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.Write from comments",
				"err", writeErr, "id", id, "auto_materialize", autoMaterialize)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		// Mirror the comment-augmented data shape into the DB so
		// search (LIKE on data) finds the new comment text. No
		// AppendProvenance call (see handler doc).
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: ve.ID,
			Kind: ve.Kind,
			Data: vaultEntityDataForDB(ve),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from comments (vault already written)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror comment to DB")
			return
		}

		// Re-read merged entity so the response includes the canonical
		// shape — same pattern as fill.
		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-comment reread", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to reload entity")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(commentsResponse{
			OK: true,
			Comment: commentEntry{
				Date: newComment.Date.Format(time.RFC3339),
				Text: newComment.Text,
				Author: newComment.Author,
				Operator: newComment.Operator,
			},
			Entity: toAPIEntity(fresh),
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/{id}/comments response", "err", err)
		}
	}
}
