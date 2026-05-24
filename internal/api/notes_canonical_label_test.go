// Tests for the yaad-index add_note auto-materialize path:
// a note targeting a canonical-label thin row creates the
// vault file at `{ROOT}/ct/<kind>/<slug>.md` when the caller holds
// operator authority. ADR-0021 §3 carve-out widened from "first
// operator-fill" to "first operator action" — notes authored by
// the operator (whether typed directly or relayed through an agent
// acting on the operator's behalf) count as operator action.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/canonical"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newCommentsCanonicalLabelFixture builds a vault-wired handler with
// auth + the boardgame canonical-kind registry, ready for the
// add_note auto-materialize tests. Mirrors
// newOperatorFillFixture's shape; kept separate so the two test
// surfaces don't share fixture state.
func newCommentsCanonicalLabelFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	r, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	reg := config.MergeCanonicalRegistry(
		nil,
		[]string{"boardgame"},
		config.CanonicalKindConfig{},
		nil,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)
	return h, st, root, signer
}

// TestAddNote_AutoMaterialize_AgentOnBehalf pins the headline
// contract: an agent JWT with operator-claim populated can
// post a note to a canonical-label thin row, and the daemon
// auto-creates the vault file at `{ROOT}/ct/<kind>/<slug>.md`
// with the note as the first attached content.
func TestAddNote_AutoMaterialize_AgentOnBehalf(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCommentsCanonicalLabelFixture(t)
	const id = "boardgame:caverna"

	// Seed a thin DB row only — no vault file. Mirrors the
	// post-ingest state when a plugin-emitted canonical-label
	// edge target had its row materialized but not its vault
	// file.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "boardgame",
	}))

	// Agent-on-behalf-of-operator: charlie subject, alice operator.
	agentTok := mintToken(t, signer, "the implementer", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", agentTok,
		map[string]any{"text": "operator's note via agent"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got commentsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "operator's note via agent", got.Note.Text)
	assert.Equal(t, "the implementer", got.Note.Author, "author stamps the agent (subject)")
	assert.Equal(t, "alice", got.Note.Operator, "operator stamps the human (claim.Operator)")

	// Vault file landed at `<root>/ct/boardgame/caverna.md` (canonical-
	// label layout per ADR-0021 §3 carve-out).
	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ve, err := r.ReadByID("boardgame", id)
	require.NoError(t, err, "vault file must be readable post-note")
	require.Len(t, ve.Notes, 1, "note landed in vault")
	assert.Equal(t, "operator's note via agent", ve.Notes[0].Text)
	assert.Equal(t, "the implementer", ve.Notes[0].Author)
	assert.Equal(t, "alice", ve.Notes[0].Operator)
}

// TestAddNote_AutoMaterialize_OperatorDirect: the operator-direct
// path (Subject == Operator) also auto-materializes — the gate
// accepts both shapes uniformly.
func TestAddNote_AutoMaterialize_OperatorDirect(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCommentsCanonicalLabelFixture(t)
	const id = "boardgame:brass-birmingham"

	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "boardgame",
	}))

	tok := mintToken(t, signer, "alice", "alice") // subject == operator

	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", tok,
		map[string]any{"text": "first thoughts"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ve, err := r.ReadByID("boardgame", id)
	require.NoError(t, err)
	require.Len(t, ve.Notes, 1)
	assert.Equal(t, "alice", ve.Notes[0].Author)
	assert.Equal(t, "alice", ve.Notes[0].Operator)
}

// TestAddNote_NoAutoMaterialize_NoRow: per the operator's scope
// tightening, notes do NOT create entities from nothing —
// even with operator authority + canonical-label-shaped id,
// when the entity row doesn't exist the request returns 404.
// Operator-fill is the deliberate-create path; notes are
// casual and need an existing entity to attach to (otherwise
// dangling notes accumulate on entities that don't exist).
func TestAddNote_NoAutoMaterialize_NoRow(t *testing.T) {
	t.Parallel()
	h, st, _, signer := newCommentsCanonicalLabelFixture(t)
	const id = "boardgame:operator-invented-game"

	// No seed at all — agent attempts to note on a canonical
	// label that has no DB row.
	tok := mintToken(t, signer, "the implementer", "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", tok,
		map[string]any{"text": "a hand-crafted entry"}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no entity with id")

	// And the DB stays empty — the 404 path doesn't side-effect.
	_, err := st.GetEntity(context.Background(), id)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row created on the 404 path")
}

// TestAddNote_NoAutoMaterialize_NonCanonicalLabelID: an id that
// doesn't parse as a canonical-label (kind not in the operator's
// canonical_kinds registry) keeps the existing 404 behavior.
// Source-namespace prefixes like `wikipedia-article:foo` would
// land here when the operator hasn't enabled `wikipedia-article`
// as a canonical kind (which is the typical case — source kinds
// aren't usually canonical).
func TestAddNote_NoAutoMaterialize_NonCanonicalLabelID(t *testing.T) {
	t.Parallel()
	h, _, _, signer := newCommentsCanonicalLabelFixture(t)
	const id = "wikipedia-article:nonexistent" // wikipedia-article not in canonical_kinds

	tok := mintToken(t, signer, "the implementer", "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", tok,
		map[string]any{"text": "should 404"}, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no entity with id")
}

// TestAddNote_ExistingVaultFile_NoAutoMaterializePath: when the
// vault file already exists, the auto-materialize path doesn't
// fire — the note appends via the existing flow. Pinned so a
// regression in the autoMaterialize flag doesn't accidentally
// re-materialize an existing entity (which would overwrite Data).
func TestAddNote_ExistingVaultFile_NoAutoMaterializePath(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newCommentsCanonicalLabelFixture(t)
	const id = "boardgame:already-materialized"

	// Pre-existing entity + vault file (post-operator-fill state).
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "boardgame", Data: map[string]any{"name": "Already Materialized", "rating": int64(7)},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "boardgame",
		Source: []string{canonical.CanonicalLabelPlugin + "/default"},
		Data: map[string]any{"name": "Already Materialized", "rating": int64(7)},
	}))

	tok := mintToken(t, signer, "the implementer", "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/notes", tok,
		map[string]any{"text": "follow-up note"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	r, err := vault.NewReader(root)
	require.NoError(t, err)
	ve, err := r.ReadByID("boardgame", id)
	require.NoError(t, err)
	// Pre-existing Data preserved — the existing-vault path read+merged,
	// not the auto-materialize path which would synthesize empty Data.
	// YAML decode coerces ints to int(7) (not int64), so assert with
	// the post-roundtrip type.
	assert.Equal(t, "Already Materialized", ve.Data["name"])
	assert.EqualValues(t, 7, ve.Data["rating"])
	require.Len(t, ve.Notes, 1)
}
