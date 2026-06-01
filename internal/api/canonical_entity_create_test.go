package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/eventbus"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
	"github.com/yaad-index/yaad-index/internal/workflow/actions"
	"github.com/yaad-index/yaad-index/internal/workflow/engine"
	"github.com/yaad-index/yaad-index/internal/workflow/parser"
)

// recordingCreateRunner captures which workflows fired, for the
// entity.created assertion.
type recordingCreateRunner struct {
	mu    sync.Mutex
	fired []string
}

func (r *recordingCreateRunner) Run(_ context.Context, wf *parser.Workflow, _ actions.Decision, _ actions.Activation) []actions.ActionResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fired = append(r.fired, wf.Name)
	return nil
}

func (r *recordingCreateRunner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fired...)
}

// newCreateCanonicalFixture wires the create endpoint with a canonical
// registry (person with a scalar `relation` gap + a `knows`
// canonical_type edge gap; boardgame as a bare kind) and a workflow
// engine on the shared bus so the entity.created path is observable.
func newCreateCanonicalFixture(t *testing.T) (http.Handler, store.Store, string, auth.Signer, *recordingCreateRunner, *triggerFakeResolver, *engine.Engine) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	rd, err := vault.NewReader(root)
	require.NoError(t, err)

	keyDir := t.TempDir()
	require.NoError(t, auth.GenerateKeypair(keyDir, false))
	signer, err := auth.LoadSigner(keyDir)
	require.NoError(t, err)
	verifier, err := auth.LoadVerifier(keyDir)
	require.NoError(t, err)

	reg := map[string]config.CanonicalKindConfig{
		"person": {
			Gaps: map[string]config.GapSpec{
				"relation": {Type: "string", Description: "relation to the operator"},
				"knows":    {Type: config.CanonicalTypeName, Kinds: []string{"person"}, Description: "people this person knows"},
			},
		},
		"boardgame": {},
	}

	bus := eventbus.NewMemoryBus()
	runner := &recordingCreateRunner{}
	resolver := &triggerFakeResolver{entities: map[string]map[string]any{}}
	eng, err := engine.New(engine.Options{
		Bus:      bus,
		Resolver: resolver,
		Runner:   runner,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, rd),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
		WithEventBus(bus),
		WithWorkflowEngine(eng),
	)
	return h, st, root, signer, runner, resolver, eng
}

// TestCreateCanonicalEntity_HappyPath_NoData: a bare create lands the
// vault file (ct/ layout, ugc:true) + DB row with the kind's gaps open.
func TestCreateCanonicalEntity_HappyPath_NoData(t *testing.T) {
	t.Parallel()
	h, st, root, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "alex-example"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "/v1/entities/person:alex-example", rec.Header().Get("Location"))

	var got operatorFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "person:alex-example", got.Entity.ID)
	assert.ElementsMatch(t, []string{"relation", "knows"}, got.Gaps)

	v := readVaultByID(t, root, "person", "person:alex-example")
	assert.True(t, v.UGC, "created canonical carries the ugc body flag")

	dbe, err := st.GetEntity(context.Background(), "person:alex-example")
	require.NoError(t, err)
	assert.Equal(t, "person", dbe.Kind)
}

// TestCreateCanonicalEntity_SeedsScalarData: `data` seeds a scalar gap,
// reusing the operator-fill validation (gap_state stamped, gap closed).
func TestCreateCanonicalEntity_SeedsScalarData(t *testing.T) {
	t.Parallel()
	h, _, root, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "bob", "data": map[string]any{"relation": "friend"}}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var got operatorFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.NotContains(t, got.Gaps, "relation", "seeded gap is no longer open")

	v := readVaultByID(t, root, "person", "person:bob")
	assert.Equal(t, "friend", v.Data["relation"])
	require.Contains(t, v.GapState, "relation")
	assert.NotNil(t, v.GapState["relation"].FilledAt, "seeded value stamps gap_state")
}

// TestCreateCanonicalEntity_400OnUnknownKind: kind not in the registry.
func TestCreateCanonicalEntity_400OnUnknownKind(t *testing.T) {
	t.Parallel()
	h, _, _, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "wikipedia-article", "slug": "x"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not in the canonical_kinds registry")
}

// TestCreateCanonicalEntity_409OnCollision: a second create of the same
// id conflicts.
func TestCreateCanonicalEntity_409OnCollision(t *testing.T) {
	t.Parallel()
	h, _, _, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	body := map[string]any{"kind": "person", "slug": "dup"}
	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok, body, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "first body=%s", rec.Body.String())

	rec = ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok, body, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "second body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "already exists")
}

// TestCreateCanonicalEntity_400OnBadSlug: slug must be ADR-0008 shaped.
func TestCreateCanonicalEntity_400OnBadSlug(t *testing.T) {
	t.Parallel()
	h, _, _, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "Bad Slug!"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "slug")
}

// TestCreateCanonicalEntity_400OnCanonicalTypeSeed: seeding a
// canonical_type (edge) gap is rejected — create is side-effect-free.
func TestCreateCanonicalEntity_400OnCanonicalTypeSeed(t *testing.T) {
	t.Parallel()
	h, st, _, signer, _, _, _ := newCreateCanonicalFixture(t)
	tok := mintToken(t, signer, "alice-agent", "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{
			"kind": "person", "slug": "carol",
			"data": map[string]any{"knows": []string{"person:dave"}},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "edge-side-effect-free")

	// And nothing was created (the reject happens before the write).
	_, err := st.GetEntity(context.Background(), "person:carol")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateCanonicalEntity_400OnUndeclaredField_OperatorToken pins the
// review fix: an operator token (triggerMode=="operator") must NOT be
// able to seed a frontmatter field that isn't a declared gap. Without
// the pre-parse guard, parseOperatorFillOps's ad-hoc branch would
// silently accept it under an operator trigger.
func TestCreateCanonicalEntity_400OnUndeclaredField_OperatorToken(t *testing.T) {
	t.Parallel()
	h, st, _, signer, _, _, _ := newCreateCanonicalFixture(t)
	// Subject == Operator → triggerMode == "operator".
	tok := mintOperatorToken(t, signer, "alice")

	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{
			"kind": "person", "slug": "intruder",
			"data": map[string]any{"arbitrary_frontmatter": "smuggled"},
		}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not a declared gap")

	// Reject happens before any write — no entity, no smuggled field.
	_, err := st.GetEntity(context.Background(), "person:intruder")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateCanonicalEntity_FiresEntityCreated: a direct create fires
// entity_created workflows, same as every other create path.
func TestCreateCanonicalEntity_FiresEntityCreated(t *testing.T) {
	t.Parallel()
	h, _, _, signer, runner, resolver, eng := newCreateCanonicalFixture(t)

	// Pre-seed the engine's resolver with the entity the create lands,
	// so the engine can resolve it when the event fires.
	resolver.entities["person:event-test"] = map[string]any{"id": "person:event-test", "kind": "person"}

	wf := &parser.Workflow{
		Name:           "on-new-person",
		Version:        1,
		Status:         parser.StatusActive,
		AllowedPlugins: []string{"operator-fill"},
		Trigger: parser.Trigger{
			Type:  parser.TriggerTypeEntityCreated,
			Match: parser.TriggerMatch{Kinds: []string{"person"}},
		},
		Subject: "entity.id",
		Actions: []parser.Action{{AddNote: &parser.AddNoteAction{Content: "'seen'"}}},
	}
	require.NoError(t, eng.Reconcile([]*parser.Workflow{wf}))

	tok := mintToken(t, signer, "alice-agent", "alice")
	rec := ugcReq(t, h, http.MethodPost, "/v1/canonical-entities", tok,
		map[string]any{"kind": "person", "slug": "event-test"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	eng.WaitForIdle()
	assert.Contains(t, runner.snapshot(), "on-new-person",
		"direct create must fire entity_created workflows")
}
