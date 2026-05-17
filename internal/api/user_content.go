// User-content (UGC) read endpoints per yaad-index PR-B of 3.
//
// PR-B scope: GET surface only. Writes (POST create, PUT section
// replace, DELETE entity) ship in PR-C with the auth-author / operator-
// override / If-Match concurrency contract.
//
// The body of a user-content entity rides on Entity.CleanContent —
// same field plugin-fetched bodies use, since the file-on-disk shape
// (frontmatter + clean_content + standard `## Edges` / `## Notes`
// sections) is uniform across entity sources. The UGC story diverges
// at write time (PR-C: the agent supplies the body directly rather
// than a plugin fetching it); on the read side both paths converge.
//
// Section parsing uses vault.ParseSections (ADR-0012 / yaad-index
//'s containment model) — every ATX heading is one addressable
// section, deeper headings are textually contained in their parent's
// body, pre-heading body is the implicit section 0.

package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

const (
	// userContentKind is the entity kind UGC files declare in their
	// frontmatter. Routes carved out of /v1/user-content/* enforce that
	// the resolved entity carries this kind so a stray id collision
	// (e.g. someone created `user-content:foo` via the generic ingest
	// path) doesn't leak through the UGC-specific surface.
	userContentKind = "user-content"

	// userContentIDPrefix is the required `<prefix>:` shape for UGC ids.
	// Mirrors the kind for symmetry with plugin sources (where prefix
	// equals plugin name, e.g. `wikipedia:`).
	userContentIDPrefix = userContentKind + ":"

	// sectionsDefaultLimit is the default page size for embedded /
	// standalone section listings. Picked as "enough to fit a typical
	// UGC note in one page without pagination" while keeping the wire
	// payload bounded for outliers.
	sectionsDefaultLimit = 20

	// sectionsMaxLimit caps the effective page size. Larger asks land
	// at this value; the agent paginates the rest.
	sectionsMaxLimit = 100
)

// userContentSection is the wire shape of one parsed section.
// Mirrors vault.Section field-for-field plus a HeadingSlug helper so
// agents don't have to re-derive the slug client-side.
type userContentSection struct {
	Index int `json:"index"`
	Depth int `json:"depth"`
	Heading string `json:"heading,omitempty"`
	HeadingSlug string `json:"heading_slug,omitempty"`
	Body string `json:"body"`
	ByteOffset int `json:"byte_offset"`
}

// userContentSectionsPage is the paginated section list shape used by
// both the embedded `sections` field on GET /id and the standalone
// GET /id/sections endpoint. NextCursor is omitted when the prior
// page was the last one — the agent stops paginating on the missing
// field.
type userContentSectionsPage struct {
	Entries []userContentSection `json:"entries"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// userContentEntityResponse is the GET /v1/user-content/{id} envelope.
// Smaller than the generic GET /v1/entities/{id} shape — UGC has no
// edge expansion, no notations, no aliases overlay — but adds the
// embedded paginated sections so the agent can read entity metadata
// + the first page of body content in one round trip.
type userContentEntityResponse struct {
	OK bool `json:"ok"`
	ID string `json:"id"`
	Kind string `json:"kind"`
	Data map[string]any `json:"data,omitempty"`
	Tags []string `json:"tags,omitempty"`
	Provenance []provenanceEntry `json:"provenance"`
	Sections userContentSectionsPage `json:"sections"`
}

// userContentSectionResponse is the GET /v1/user-content/{id}/sections/{sec}
// envelope — single section with the matched address echoed back for
// the agent's audit trail.
type userContentSectionResponse struct {
	OK bool `json:"ok"`
	ID string `json:"id"`
	Section userContentSection `json:"section"`
}

// handleUserContentRead implements GET /v1/user-content/{id}: load
// the entity, parse its body, return entity metadata + first-page
// sections. 404 when the id doesn't resolve OR resolves to a
// non-user-content kind. 503 vault_required when WithVaultIO is
// missing (UGC is fundamentally a vault-shape feature).
func handleUserContentRead(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !strings.HasPrefix(id, userContentIDPrefix) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("id must start with %q", userContentIDPrefix))
			return
		}
		if vaultReader == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"user-content endpoints require vault.path configuration; the body lives in vault files")
			return
		}

		limit, ok := parseSectionsLimit(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("limit must be a positive integer; max %d", sectionsMaxLimit))
			return
		}
		offset, ok := decodeSectionsCursor(r.URL.Query().Get("cursor"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument", "invalid cursor")
			return
		}

		got, err := st.GetEntity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no entity with id %s", id))
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity from user-content read", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to look up entity")
			return
		}
		if got.Kind != userContentKind {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("entity %s exists but is not a user-content entity (kind=%s)", id, got.Kind))
			return
		}

		ve, err := vaultReader.ReadByID(got.Kind, id)
		if err != nil {
			if vault.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					fmt.Sprintf("no vault file for id %s", id))
				return
			}
			logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from user-content read", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to read vault file")
			return
		}

		sections := vault.ParseSections(ve.CleanContent)
		page := buildSectionsPage(sections, offset, limit)

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
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content/{id} response", "err", err, "id", id)
		}
	}
}

// handleUserContentSectionsList implements GET
// /v1/user-content/{id}/sections — same paginated section shape as
// the embedded one on the entity GET, but with no entity envelope.
func handleUserContentSectionsList(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
			return
		}

		limit, ok := parseSectionsLimit(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("limit must be a positive integer; max %d", sectionsMaxLimit))
			return
		}
		offset, ok := decodeSectionsCursor(r.URL.Query().Get("cursor"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument", "invalid cursor")
			return
		}

		sections := vault.ParseSections(ve.CleanContent)
		page := buildSectionsPage(sections, offset, limit)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusOK)
		out := struct {
			OK bool `json:"ok"`
			userContentSectionsPage
		}{OK: true, userContentSectionsPage: page}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content/{id}/sections response", "err", err, "id", id)
		}
	}
}

// handleUserContentSection implements GET
// /v1/user-content/{id}/sections/{sec} — one section by positional
// index OR heading slug. Returns 404 when the address doesn't
// resolve (out-of-range index, unknown slug, or duplicate slug —
// duplicates require the agent to address by positional index).
func handleUserContentSection(logger *slog.Logger, st store.Store, vaultReader *vault.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ve, status, errCode, errMsg := loadUserContentVaultEntity(logger, r, st, vaultReader, id)
		if status != 0 {
			writeError(w, status, errCode, errMsg)
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

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", userContentEtag(ve.CleanContent))
		w.WriteHeader(http.StatusOK)
		out := userContentSectionResponse{
			OK: true,
			ID: ve.ID,
			Section: vaultSectionToAPI(sections[idx]),
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/user-content/{id}/sections/{sec} response",
				"err", err, "id", id, "sec", addr)
		}
	}
}

// loadUserContentVaultEntity centralizes the id-prefix check + store
// lookup + kind verification + vault read used by both the sections-
// list and single-section handlers. Returns (entity, 0, "", "") on
// success; (nil, status, code, msg) on failure for the caller to
// hand to writeError.
//
// The logger is required so the helper can stamp the underlying err
// before returning a 500 envelope — without it (the original shape)
// the operator saw the 500 status code with no breadcrumb explaining
// which call failed (the cold-reviewer's review on a prior PR).
func loadUserContentVaultEntity(logger *slog.Logger, r *http.Request, st store.Store, vaultReader *vault.Reader, id string) (*vault.Entity, int, string, string) {
	if !strings.HasPrefix(id, userContentIDPrefix) {
		return nil, http.StatusBadRequest, "invalid_argument",
			fmt.Sprintf("id must start with %q", userContentIDPrefix)
	}
	if vaultReader == nil {
		return nil, http.StatusServiceUnavailable, "vault_required",
			"user-content endpoints require vault.path configuration; the body lives in vault files"
	}
	got, err := st.GetEntity(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, http.StatusNotFound, "not_found",
			fmt.Sprintf("no entity with id %s", id)
	}
	if err != nil {
		logger.ErrorContext(r.Context(), "store.GetEntity from user-content lookup",
			"err", err, "id", id)
		return nil, http.StatusInternalServerError, "internal_error",
			"failed to look up entity"
	}
	if got.Kind != userContentKind {
		return nil, http.StatusNotFound, "not_found",
			fmt.Sprintf("entity %s exists but is not a user-content entity (kind=%s)", id, got.Kind)
	}
	ve, err := vaultReader.ReadByID(got.Kind, id)
	if err != nil {
		if vault.IsNotExist(err) {
			return nil, http.StatusNotFound, "not_found",
				fmt.Sprintf("no vault file for id %s", id)
		}
		logger.ErrorContext(r.Context(), "vault.Reader.ReadByID from user-content lookup",
			"err", err, "id", id)
		return nil, http.StatusInternalServerError, "internal_error",
			"failed to read vault file"
	}
	return ve, 0, "", ""
}

// buildSectionsPage paginates the parsed section list. The cursor
// encodes the next start-index; out-of-range offsets return an empty
// page (rather than a 404 — pagination over a stable list shouldn't
// fail loudly when the agent overshoots).
func buildSectionsPage(sections []vault.Section, offset, limit int) userContentSectionsPage {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(sections) {
		return userContentSectionsPage{Entries: []userContentSection{}}
	}
	end := offset + limit
	if end > len(sections) {
		end = len(sections)
	}
	out := userContentSectionsPage{
		Entries: make([]userContentSection, 0, end-offset),
	}
	for _, s := range sections[offset:end] {
		out.Entries = append(out.Entries, vaultSectionToAPI(s))
	}
	if end < len(sections) {
		out.NextCursor = encodeSectionsCursor(end)
	}
	return out
}

func vaultSectionToAPI(s vault.Section) userContentSection {
	return userContentSection{
		Index: s.Index,
		Depth: s.Depth,
		Heading: s.Heading,
		HeadingSlug: s.HeadingSlug(),
		Body: s.Body,
		ByteOffset: s.ByteOffset,
	}
}

func parseSectionsLimit(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return sectionsDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > sectionsMaxLimit {
		n = sectionsMaxLimit
	}
	return n, true
}

func encodeSectionsCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeSectionsCursor(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// userContentEtag is the entity-level concurrency token for the
// PUT /v1/user-content/{id}/sections/{sec} If-Match header. We hash
// the WHOLE document body (CleanContent), not a per-section slice —
// any edit anywhere in the entity invalidates pending edits, which
// keeps the lost-update window tight at the cost of extra 412s when
// two agents edit different sections simultaneously. v1 trade-off
// Per the prior design,; per-section etag scheme is a future-PR option if write
// concurrency becomes a real problem in practice.
func userContentEtag(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// vaultProvenanceToAPI mirrors the existing toAPIProvenance helper
// from entities.go but operates on the vault-frontmatter shape
// (vault.ProvenanceEntry) directly. Kept separate so PR-B doesn't
// need to round-trip through store.Entity to render provenance on
// the response.
func vaultProvenanceToAPI(in []vault.ProvenanceEntry) []provenanceEntry {
	if in == nil {
		return []provenanceEntry{}
	}
	out := make([]provenanceEntry, len(in))
	for i, p := range in {
		entry := provenanceEntry{
			Source: p.Source,
			OK: p.OK,
			Error: p.Error,
			ErrorMessage: p.ErrorMessage,
		}
		if p.FetchedAt != nil {
			entry.FetchedAt = p.FetchedAt.UTC().Format(time.RFC3339)
		}
		if p.FilledAt != nil {
			entry.FilledAt = p.FilledAt.UTC().Format(time.RFC3339)
		}
		out[i] = entry
	}
	return out
}
