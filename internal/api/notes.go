package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// commentsRequest is the POST /v1/entities/{id}/notes body. The
// server stamps `date` (UTC) — clients never send it.
//
// Per yaad-index a prior PR, `author` must match the JWT subject
// attached by the auth middleware. Empty `author` is filled from the
// claim's Subject for client convenience; a non-empty `author` that
// disagrees with the claim returns 403 author_mismatch. Clients have
// no way to set `operator` — that field is stamped server-side from
// the claim's Operator and is read-only on the wire.
type commentsRequest struct {
	Text string `json:"text"`
	Author string `json:"author,omitempty"`
}

// noteEntry is the wire shape for a single note on the response.
// Matches the vault.Note frontmatter shape; Date is RFC3339 UTC so
// clients can do their own timezone rendering.
//
// Operator (added a prior PR) names the human resource owner from the
// pair-claim. Empty for legacy notes (vault entries written before
// had no operator stamp). Always populated on new notes.
type noteEntry struct {
	Date string `json:"date"`
	Text string `json:"text"`
	Author string `json:"author,omitempty"`
	Operator string `json:"operator,omitempty"`
	// Field is the optional per-field scope per #186 (e.g.
	// `birth_date`). Empty → entity-level note (legacy behavior).
	Field string `json:"field,omitempty"`
	// Kind discriminates everyday notes from agent-feedback
	// annotations per #186. Empty / `note` → operator-level
	// commentary; `annotation` → agent observation that wants
	// operator attention. Omitempty so legacy notes (no kind
	// stamped at write time) decode as zero-value without the
	// JSON field appearing in the response.
	Kind string `json:"kind,omitempty"`
}

// commentsResponse is the 201 envelope: the just-appended note plus
// the merged entity (so the caller can refresh local state without a
// follow-up GET). Mirrors the fillResponse shape.
type commentsResponse struct {
	OK bool `json:"ok"`
	Note noteEntry `json:"note"`
	Entity entity `json:"entity"`
}

// handleNotes implements POST /v1/entities/{id}/notes.
//
// Vault-first append (per ADR-0008): the new note is added to the
// entity's body `## Notes` table (Per the prior design, — notes live in the
// body, frontmatter just carries `note_count: N`); the DB row is
// updated to mirror the new state. The vault file is the source of
// truth; the DB is a derived index.
//
// **No AppendProvenance call** (deliberate, do not "fix"). The
// provenance log is for fetch/fill events — "where did this entity's
// data come from" — and a note-append isn't a fetch or a structural
// data change. The body notes table IS the audit trail (date +
// author + text per entry); duplicating that into the provenance log
// would pollute it. The provenance shape (`{source, fetched_at, ok}`)
// also doesn't fit a note-append cleanly. Keep these surfaces
// separate.
//
// Append-only in v1 — edit/delete-by-note-id is a follow-up per
// ADR-0008's Open Questions section. Server stamps `date` (UTC); the
// client never supplies it.
//
// Input normalization: `text` is `strings.TrimSpace`-d before storage
// to address the cold-reviewer's a prior PR review note about the vault parser
// trimming leading whitespace from body note blocks. After this
// trim, the API → vault → reindex round-trip is lossless for non-
// whitespace text content; pure-whitespace inputs surface as 400
// invalid_argument.
//
// Asymmetry with a prior PR ingest: like the fill endpoint (a prior PR),
// notes require vault wiring — there's no "DB-only fallback"
// for notes because the canonical note list lives in vault
// frontmatter. Returns 503 vault_required when WithVaultIO is
// omitted.
// canonicalKindReg widens the carve-out per yaad-index: notes
// targeting a canonical-label thin row (`<kind>:<slug>` where `kind`
// is in the operator's canonical_kinds registry) auto-materialize
// the vault file at `{ROOT}/ct/<kind>/<slug>.md` when the caller
// holds operator authority. Without operator authority, or when the
// id isn't canonical-label-shaped, the existing 404 paths apply.
func handleNotes(logger *slog.Logger, st store.Store, vaultReader *vault.Reader, vaultWriter *vault.Writer, canonicalKindReg map[string]config.CanonicalKindConfig) http.HandlerFunc {
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

		// Per yaad-index a prior PR: enforce author == JWT subject.
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
				"notes handler reached without an auth claim — middleware misconfigured",
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
				"notes require vault.path configuration; the note list lives in vault frontmatter")
			return
		}

		// autoMaterialize covers the "thin DB row exists, vault file
		// missing" case for canonical-label entities (per yaad-index
		//). The note write then creates the vault file via
		// WriteCanonicalLabelWithCommit. Per the operator's scope
		// tightening, notes do NOT create the entity row from
		// nothing — that path stays a 404 to prevent dangling
		// notes on entities that don't exist. Operator-fill is the
		// deliberate-create path; notes are casual and need an
		// existing entity to attach to.
		var autoMaterialize bool

		got, err := st.GetEntity(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no entity with id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "store.GetEntity from notes", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to look up entity")
			return
		}

		ve, err := vaultReader.ReadByID(got.Kind, id)
		if err != nil {
			if !vault.IsNotExist(err) {
				logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from notes", "err", err, "id", id)
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
			ve = canonical.NewCanonicalLabelEntity(got.ID, got.Kind, kindCfg)
			autoMaterialize = true
		}

		// Truncate to second precision so the YAML frontmatter encoding
		// and the body `## Notes` section header (which uses
		// RFC3339, second-precision) round-trip to the SAME Date value
		// post-Unmarshal. Without the truncate, frontmatter retains
		// nanos but body loses them — the vault.mergeNotes dedup
		// then fails to collapse the two reads of the same note,
		// producing duplicate entries on every read-modify-write
		// cycle.
		now := clock.Now().Truncate(time.Second)
		newNote := vault.Note{
			Date: now,
			Text: text,
			Author: author,
			Operator: operator,
		}
		ve.Notes = append(ve.Notes, newNote)

		commitMsg := noteCommitMessage(ve.ID, author)
		commitAuthor := agentAuthorRef(author)
		writeErr := error(nil)
		if autoMaterialize {
			writeErr = vaultWriter.WriteCanonicalLabelWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		} else {
			writeErr = vaultWriter.WriteWithCommit(r.Context(), ve, commitMsg, commitAuthor)
		}
		if writeErr != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.Write from notes",
				"err", writeErr, "id", id, "auto_materialize", autoMaterialize)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		// Mirror the note-augmented data shape into the DB so
		// search (LIKE on data) finds the new note text. No
		// AppendProvenance call (see handler doc).
		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID: ve.ID,
			Kind: ve.Kind,
			Data: vaultEntityDataForDB(ve),
			CreatedAt: got.CreatedAt,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from notes (vault already written)",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror note to DB")
			return
		}

		// Re-read merged entity so the response includes the canonical
		// shape — same pattern as fill.
		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-note reread", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to reload entity")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(commentsResponse{
			OK: true,
			Note: noteEntry{
				Date: newNote.Date.Format(time.RFC3339),
				Text: newNote.Text,
				Author: newNote.Author,
				Operator: newNote.Operator,
			},
			Entity: toAPIEntity(fresh),
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/entities/{id}/notes response", "err", err)
		}
	}
}
