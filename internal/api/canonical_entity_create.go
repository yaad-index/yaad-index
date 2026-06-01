// Direct canonical-entity creation per #389. Lets an authenticated
// caller create a canonical entity (`<kind>:<slug>`) on its own —
// independent of plugin ingestion, edge side-effects, or a UGC
// frontmatter-edge stub. The three indirect paths still exist; this is
// the first-class "I just want the entity to exist" surface.
//
// Side-effect-free by design (the issue's core ask): it writes the
// vault file + DB row + publishes entity.created, and nothing else. No
// edges are materialized — `data` may seed scalar gap fields but a
// `canonical_type` (edge-producing) gap is rejected, since materializing
// its edge would be exactly the side-effect this surface avoids.
//
// Auth is any-authenticated (not operator-only): agents already stub
// canonicals indirectly via edge side-effects, so a direct path must
// not be MORE restricted. `data` seeding reuses the operator-fill
// parse/apply validation, so seeded values get the same typed checks +
// gap_state stamping the rest of the fill surface enforces — including
// the fill_strategy gates (an agent-trigger can't seed an
// operator-strategy gap).

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"

	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/writelocks"
)

// createCanonicalEntityRequest is the POST /v1/canonical-entities body.
// `data` mirrors the operator-fill body shape (field → scalar value) so
// the same parse/apply validation can seed gap fields at creation.
type createCanonicalEntityRequest struct {
	Kind string                     `json:"kind"`
	Slug string                     `json:"slug"`
	Data map[string]json.RawMessage `json:"data,omitempty"`
}

// canonicalSlugPattern is the ADR-0008 slug shape: lowercase
// alphanumeric segments joined by single hyphens. Mirrors the slugs
// SlugFromTitle produces, but here the slug is operator-supplied so we
// validate rather than derive.
var canonicalSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// handleCreateCanonicalEntity implements POST /v1/canonical-entities.
func handleCreateCanonicalEntity(logger *slog.Logger, st store.Store, vaultWriter *vault.Writer, canonicalKindReg map[string]config.CanonicalKindConfig, writeLocks *writelocks.Manager, bus eventbus.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if vaultWriter == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_required",
				"canonical-entity creation requires vault.path configuration; the entity body lives in vault files")
			return
		}

		var req createCanonicalEntityRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("request body is not valid JSON: %v", err))
			return
		}

		kindCfg, ok := canonicalKindReg[req.Kind]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("kind %q is not in the canonical_kinds registry", req.Kind))
			return
		}
		if !canonicalSlugPattern.MatchString(req.Slug) {
			writeError(w, http.StatusBadRequest, "invalid_argument",
				fmt.Sprintf("slug %q must be lowercase alphanumeric segments joined by single hyphens (ADR-0008)", req.Slug))
			return
		}
		id := req.Kind + ":" + req.Slug

		claim, ok := ClaimFromContext(r.Context())
		if !ok || claim == nil {
			logger.ErrorContext(r.Context(),
				"create-canonical-entity reached without an auth claim", "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"auth claim missing on request — server misconfiguration")
			return
		}

		// Entity-level write-lock spans the collision check + write so a
		// concurrent create of the same id can't both pass the 409 gate.
		release, lockOK := acquireWriteLock(w, r, writeLocks, id)
		if !lockOK {
			return
		}
		var pending eventbus.PendingEvents
		defer pending.Drain(r.Context(), bus)
		defer release()

		switch _, err := st.GetEntity(r.Context(), id); {
		case err == nil:
			writeError(w, http.StatusConflict, "conflict",
				fmt.Sprintf("entity %s already exists", id))
			return
		case !errors.Is(err, store.ErrNotFound):
			logger.ErrorContext(r.Context(), "store.GetEntity from create-canonical-entity",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to check for an existing entity")
			return
		}

		ve := canonical.NewCanonicalLabelEntity(id, req.Kind, kindCfg)

		now := clock.Now().UTC()
		triggerMode := "agent"
		if claim.Subject != "" && claim.Subject == claim.Operator {
			triggerMode = "operator"
		}
		if len(req.Data) > 0 {
			effectiveGaps := resolveEffectiveGaps(kindCfg.Gaps, ve.GapState)
			for field := range req.Data {
				// `data` seeds DECLARED gap fields only. Reject any other
				// key before parsing — parseOperatorFillOps's ad-hoc
				// branch would otherwise accept unknown fields under an
				// operator token (triggerMode=="operator"), letting a
				// caller seed arbitrary frontmatter past the registered-
				// gap contract.
				spec, declared := effectiveGaps[field]
				if !declared {
					writeError(w, http.StatusBadRequest, "invalid_argument",
						fmt.Sprintf("field %q is not a declared gap on kind %q; create_canonical_entity seeds only registered gap fields", field, req.Kind))
					return
				}
				// Reject edge-producing seeds: materializing a
				// canonical_type gap's edge is the side-effect this
				// surface exists to avoid.
				if spec.Type == config.CanonicalTypeName {
					writeError(w, http.StatusBadRequest, "invalid_argument",
						fmt.Sprintf("field %q is a canonical_type (edge) gap; create_canonical_entity is edge-side-effect-free — seed it with fill_field after create", field))
					return
				}
			}
			operatorAllKinds := make([]string, 0, len(canonicalKindReg))
			for k := range canonicalKindReg {
				operatorAllKinds = append(operatorAllKinds, k)
			}
			sort.Strings(operatorAllKinds)

			ops, opErr := parseOperatorFillOps(req.Data, effectiveGaps, operatorAllKinds, ve.Data, ve.Gaps, triggerMode, false)
			if opErr != nil {
				writeError(w, opErr.status, opErr.code, opErr.message)
				return
			}
			applyOperatorFillOps(ve, ops, now, effectiveGaps)
		}

		commitMsg := fmt.Sprintf("create canonical entity %s", id)
		commitAuthor := agentAuthorRef(claim.Subject)
		if err := vaultWriter.WriteCanonicalLabelWithCommit(r.Context(), ve, commitMsg, commitAuthor); err != nil {
			logger.ErrorContext(r.Context(), "vault.Writer.WriteCanonicalLabelWithCommit from create-canonical-entity",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to write vault file")
			return
		}

		if err := st.UpsertEntity(r.Context(), &store.Entity{
			ID:        ve.ID,
			Kind:      ve.Kind,
			Data:      vaultEntityDataForDB(ve),
			GapState:  vaultGapStateToStore(ve.GapState),
			CreatedAt: now,
		}); err != nil {
			logger.ErrorContext(r.Context(), "store.UpsertEntity from create-canonical-entity",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to mirror entity to DB")
			return
		}

		// entity.created per ADR-0024 — direct creation fires the same
		// trigger every other create path does. SourceTag reflects the
		// caller's trigger mode; CausedByEntityID self-references (the
		// create is its own cause, mirroring a source-plugin ingest).
		source := eventbus.SourceAgent
		if triggerMode == "operator" {
			source = eventbus.SourceOperator
		}
		eventbus.QueueOrPublish(r.Context(), bus, &pending, eventbus.EntityCreatedEvent{
			ID:               ve.ID,
			Kind:             ve.Kind,
			SourceTag:        source,
			At:               now,
			Chain:            eventbus.WorkflowChainFromContext(r.Context()),
			CausedByEntityID: ve.ID,
		})

		fresh, err := st.GetEntity(r.Context(), id)
		if err != nil {
			logger.ErrorContext(r.Context(), "store.GetEntity post-create reread",
				"err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal_error",
				"failed to reload created entity")
			return
		}

		remainingGaps := ve.Gaps
		if remainingGaps == nil {
			remainingGaps = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/entities/"+id)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(operatorFillResponse{
			OK:     true,
			Entity: toAPIEntity(fresh),
			Gaps:   remainingGaps,
		}); err != nil {
			logger.ErrorContext(r.Context(), "encode /v1/canonical-entities response",
				"err", err, "id", id)
		}
	}
}
