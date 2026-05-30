package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/config"
	"github.com/yaad-index/yaad-index/internal/store"
	"github.com/yaad-index/yaad-index/internal/vault"
)

// newAgentFillCanonicalTypeFixture wires a vault-backed handler
// covering a `source` kind with one canonical_type gap `subjects`,
// configured with the caller-supplied kinds allowlist (specific
// list OR `["*"]` for the wildcard variant). Used by the agent-
// fill canonical_type tests below — the handler enforces
// auth-required so callers mint anonymous tokens via mintToken.
func newAgentFillCanonicalTypeFixture(t *testing.T, gapKinds []string) (http.Handler, store.Store, string, auth.Signer) {
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

	opPerKind := map[string]config.CanonicalKindConfig{
		"source": {
			Gaps: map[string]config.GapSpec{
				"subjects": {
					Type: config.CanonicalTypeName,
					Description: "Canonical entities mentioned in this source.",
					FillStrategy: "both",
					Kinds: gapKinds,
				},
			},
		},
		"boardgame": {},
		"person": {},
	}
	reg := config.MergeCanonicalRegistry(
		nil,
		nil,
		config.CanonicalKindConfig{},
		opPerKind,
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

// seedSourceForAgentCanonicalTypeFill creates a vault entity + DB
// row for a `source`-kind entity ready to receive an agent-fill
// canonical_type op on the `subjects` gap.
func seedSourceForAgentCanonicalTypeFill(t *testing.T, st store.Store, root, id string) {
	t.Helper()
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "source",
		Data: map[string]any{"name": "Test Source"},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "source",
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Test Source"},
		Gaps: []string{"subjects"},
	}))
}

// agentFillReq POSTs to /v1/entities/{id}/fill with the canonical
// agent-fill body shape `{"fields": {...}}` + a bearer token. A
// thin wrapper around the existing ugcReq helper.
func agentFillReq(t *testing.T, h http.Handler, id, tok string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return ugcReq(t, h, http.MethodPost, "/v1/entities/"+id+"/fill", tok,
		map[string]any{"fields": fields}, nil)
}

// TestAgentFill_CanonicalType_HappyPath_ObjectForm covers the
// primary canonical_type path on agent-fill per yaad-index:
// agent submits a list of `{name, kind}` objects, daemon
// slugifies each via slug.Slug, edges land from the source entity
// to the derived canonical-label endpoints. Mirrors the
// operator-fill canonical_type happy-path test from.
func TestAgentFill_CanonicalType_HappyPath_ObjectForm(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:newsletter-2026-04"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Brass: Birmingham (2018)", "kind": "boardgame"},
			map[string]any{"name": "Martin Wallace", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, edges, 2)
	targets := make([]string, len(edges))
	for i, e := range edges {
		targets[i] = e.To
	}
	assert.ElementsMatch(t,
		[]string{"boardgame:brass-birmingham-2018", "person:martin-wallace"},
		targets,
		"daemon's slug.Slug derives canonical-label slugs from descriptive names",
	)
}

// TestAgentFill_CanonicalType_RejectsPreformedLabels covers the
// agent-only-shape gate per spec §Operator-fill: pre-formed
// canonical-label strings are operator-only. An agent-fill body
// containing `["<kind>:<slug>", ...]` rejects with
// type_mismatch + the "pre-formed canonical-label string only
// accepted on operator-fill" hint.
func TestAgentFill_CanonicalType_RejectsPreformedLabels(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-rejects-preformed"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{"boardgame:brass-pittsburgh"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
	assert.Contains(t, rec.Body.String(), "operator-fill")
}

// TestAgentFill_CanonicalType_EmptyList covers the explicit
// empty-list path per spec §Edge cases: `[]` transitions the gap
// to filled state with no edges.
func TestAgentFill_CanonicalType_EmptyList(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-empty-list"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{"subjects": []any{}})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges, "empty-list fill must not create edges")

	ve := readVaultByID(t, root, "source", id)
	assert.NotContains(t, ve.Gaps, "subjects",
		"empty-list fill transitions gap to filled")
}

// TestAgentFill_CanonicalType_RefillReplacesEdges covers the
// idempotent-replace semantic per spec §Re-fill: a second
// agent-fill deletes the prior edges and creates the new fill's
// edges. Uses two distinct subjects between fills to confirm.
func TestAgentFill_CanonicalType_RefillReplacesEdges(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-refill"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Caverna", "kind": "boardgame"},
			map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "first fill body=%s", rec.Body.String())

	first, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, first, 2)

	// Re-add `subjects` to the open-gap list so the agent-fill
	// handler accepts a second call. (The vault gap-set tracks
	// what's currently open; re-fill from the agent path requires
	// the field to be re-opened, e.g. via a re-ingest. This test
	// drives the re-open via a direct vault mutation to keep the
	// scope on the edge-replace semantics, not the re-open flow.)
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	rec = agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Agricola", "kind": "boardgame"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "second fill body=%s", rec.Body.String())

	second, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, second, 1, "re-fill must replace, not append")
	assert.Equal(t, "boardgame:agricola", second[0].To)
}

// TestAgentFill_CanonicalType_KindNotInResolution covers the
// gap's allowlist enforcement on the agent path: a fill whose
// kind isn't in `gap.Kinds` rejects.
func TestAgentFill_CanonicalType_KindNotInResolution(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-kind-not-allowed"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Martin Wallace", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	assert.Empty(t, edges, "no edges should land on a rejected fill")
}

// TestAgentFill_CanonicalType_WildcardKinds covers the wildcard
// resolution: `kinds: "*"` accepts any kind in the operator's
// canonical_kinds registry; kinds outside the registry reject.
func TestAgentFill_CanonicalType_WildcardKinds(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{config.CanonicalTypeWildcard})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-wildcard"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	// `boardgame` and `person` are in the operator's
	// canonical_kinds → both pass.
	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Caverna", "kind": "boardgame"},
			map[string]any{"name": "Uwe Rosenberg", "kind": "person"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "wildcard registered kinds body=%s", rec.Body.String())

	// `country` is NOT in the operator's canonical_kinds —
	// wildcard rejects per ADR-0008's "any THIS operator declared"
	// semantic.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	rec = agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{"name": "Germany", "kind": "country"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"wildcard rejects kinds not in operator registry; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kind_not_allowed")
}

// TestAgentFill_CanonicalType_NotArray covers the wire-shape
// gate: a bare object (single-item without array wrapping)
// rejects per spec §Edge cases.
func TestAgentFill_CanonicalType_NotArray(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-not-array"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	body := strings.NewReader(`{"fields": {"subjects": {"name": "X", "kind": "boardgame"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/entities/"+id+"/fill", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type_mismatch")
}

// TestAgentFill_CanonicalType_AcceptsDataPerEntry pins
// yaad-index #119 Cut 1: a canonical_type entry MAY carry an
// optional `data: {...}` map. The parser accepts it without
// rejecting; frontmatter `data[<field>]` persists label IDs
// only — the per-entry data does NOT bleed into the source
// entity's frontmatter. Cut 2 will use the carried data to
// append dataview paragraphs on the target canonical entity.
func TestAgentFill_CanonicalType_AcceptsDataPerEntry(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-data-per-entry"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Brass: Birmingham",
				"kind": "boardgame",
				"data": map[string]any{
					"my_rating":   "9",
					"played_at":   "essen-2024",
					"co_player":   "alice",
				},
			},
			map[string]any{
				"name": "Martin Wallace",
				"kind": "person",
				// No data — older entries stay valid.
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"data field must be accepted; body=%s", rec.Body.String())

	// Edges still land — the existing canonical_type semantics
	// are preserved.
	edges, err := st.GetEdgesFor(context.Background(), id, []string{"subjects"})
	require.NoError(t, err)
	require.Len(t, edges, 2)

	// Frontmatter `data.subjects` persists label IDs only —
	// the per-entry `data` payload doesn't bleed into the
	// source entity's frontmatter. Cut 2 records it as a
	// dataview paragraph on the target instead.
	ve := readVaultByID(t, root, "source", id)
	storedRaw, ok := ve.Data["subjects"]
	require.True(t, ok, "subjects must persist on frontmatter")
	stored, ok := storedRaw.([]any)
	require.True(t, ok, "subjects must persist as a list; got %T", storedRaw)
	gotIDs := make([]string, len(stored))
	for i, v := range stored {
		gotIDs[i], _ = v.(string)
	}
	assert.ElementsMatch(t,
		[]string{"boardgame:brass-birmingham", "person:martin-wallace"},
		gotIDs,
		"frontmatter must store label-IDs only, NOT the per-entry data payload")
}

// TestAgentFill_CanonicalType_DataAppendsParagraphOnTarget
// pins yaad-index #119 dataview-append behavior: an entry
// carrying `data` lands as a dataview paragraph on the target
// canonical entity's body (auto-materialized when missing per
// the policy call).
func TestAgentFill_CanonicalType_DataAppendsParagraphOnTarget(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-dataview-target"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Brass: Birmingham",
				"kind": "boardgame",
				"data": map[string]any{
					"my_rating":  "9",
					"co_player":  "alice",
					"played_at":  "essen-2024",
				},
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Target canonical-label vault file was auto-materialized.
	target := readVaultByID(t, root, "boardgame", "boardgame:brass-birmingham")
	require.Len(t, target.Dataview, 1,
		"target must carry one dataview paragraph after the fill")
	got := target.Dataview[0].Fields
	assert.Equal(t, "9", got["my_rating"])
	assert.Equal(t, "alice", got["co_player"])
	assert.Equal(t, "essen-2024", got["played_at"])
}

// TestAgentFill_CanonicalType_DataDedupesIdenticalParagraph
// pins the dedup contract: re-filling with the exact same
// data payload produces no duplicate paragraph on the target
// (sorted-key content-hash equality skips the append).
func TestAgentFill_CanonicalType_DataDedupesIdenticalParagraph(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-dataview-dedup"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	payload := map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Caverna",
				"kind": "boardgame",
				"data": map[string]any{"my_rating": "8"},
			},
		},
	}
	require.Equal(t, http.StatusOK, agentFillReq(t, h, id, tok, payload).Code,
		"first fill must succeed")

	// Re-open the gap + re-fill with the same data payload.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	require.Equal(t, http.StatusOK, agentFillReq(t, h, id, tok, payload).Code,
		"second fill (same data) must succeed")

	target := readVaultByID(t, root, "boardgame", "boardgame:caverna")
	assert.Len(t, target.Dataview, 1,
		"sorted-key dedup must skip the duplicate; one paragraph total")
}

// TestAgentFill_CanonicalType_DataAppendsSecondParagraphOnDifferentValue
// pins the non-dedup case: a second fill with a different
// `data` payload appends as a fresh paragraph rather than
// replacing the prior one.
func TestAgentFill_CanonicalType_DataAppendsSecondParagraphOnDifferentValue(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-dataview-append-second"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	require.Equal(t, http.StatusOK, agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Spirit Island",
				"kind": "boardgame",
				"data": map[string]any{"my_rating": "8"},
			},
		},
	}).Code, "first fill must succeed")

	// Re-open + re-fill with a different value.
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	ve := readVaultByID(t, root, "source", id)
	ve.Gaps = append(ve.Gaps, "subjects")
	require.NoError(t, w.Write(ve))

	require.Equal(t, http.StatusOK, agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Spirit Island",
				"kind": "boardgame",
				"data": map[string]any{"my_rating": "9"},
			},
		},
	}).Code, "second fill must succeed")

	target := readVaultByID(t, root, "boardgame", "boardgame:spirit-island")
	require.Len(t, target.Dataview, 2,
		"two distinct paragraphs accumulate (history-as-event-log)")
}

// TestAgentFill_CanonicalType_DataExtraFieldsAccepted: the
// per-entry `data` map is free-form — daemon does not type
// the keys. Any string-keyed map value the agent emits
// passes through unchanged.
func TestAgentFill_CanonicalType_DataExtraFieldsAccepted(t *testing.T) {
	t.Parallel()
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-data-extras"
	seedSourceForAgentCanonicalTypeFill(t, st, root, id)

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"subjects": []any{
			map[string]any{
				"name": "Caverna",
				"kind": "boardgame",
				"data": map[string]any{
					"any_key":     "any-value",
					"nested_map":  map[string]any{"deep": "ok"},
					"numeric":     42,
					"empty_value": "",
				},
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"free-form data keys must pass; body=%s", rec.Body.String())
}

// TestAgentFill_LegacyFieldsStillWork covers the back-compat
// guarantee: fields without a typed gap-spec entry (e.g. summary
// + tags on a source-shape entity that doesn't declare them in
// the canonical-kind registry) keep flowing through the
// untyped applyFieldsToVaultEntity path. The agent-fill canonical_type
// branch is purely additive.
func TestAgentFill_LegacyFieldsStillWork(t *testing.T) {
	t.Parallel()
	t.Skip("#355 Cut 2b: legacy fill shape; re-adaptation tracked separately")
	h, st, root, signer := newAgentFillCanonicalTypeFixture(t, []string{"person", "boardgame"})
	tok := mintToken(t, signer, "agent", "alice")
	const id = "source:agent-legacy-fields"

	// Seed an entity whose open-gap list includes both the typed
	// `subjects` canonical_type gap AND the legacy untyped
	// `summary` + `tags` fields. The `subjects` field is left
	// unfilled in this test; the legacy fields are the focus.
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id,
		Kind: "source",
		Data: map[string]any{"name": "Test Source"},
	}))
	w, err := vault.NewWriter(root)
	require.NoError(t, err)
	require.NoError(t, w.Write(&vault.Entity{
		ID: id,
		Kind: "source",
		Source: []string{"fixture/default"},
		Data: map[string]any{"name": "Test Source"},
		Gaps: []string{"subjects", "summary", "tags"},
	}))

	rec := agentFillReq(t, h, id, tok, map[string]any{
		"summary": "A short narrative.",
		"tags": []any{"alpha", "beta"},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	ve := readVaultByID(t, root, "source", id)
	assert.Equal(t, "A short narrative.", ve.Summary,
		"legacy summary lands on top-level vault.Entity.Summary, not Data")
	assert.Equal(t, []string{"alpha", "beta"}, ve.Tags,
		"legacy tags lands on top-level vault.Entity.Tags")
}

// TestNeedsFill_GapMetadataKindsSurfaces pins the nice-to-have
// per yaad-index: when a canonical_type gap is open, the
// `gap_metadata` wire field surfaces the gap's `kinds` allowlist
// so the agent's UI can render the resolution set at fill-prompt
// time.
func TestNeedsFill_GapMetadataKindsSurfaces(t *testing.T) {
	t.Parallel()
	gap := config.GapSpec{
		Type: config.CanonicalTypeName,
		Description: "subjects",
		FillStrategy: "both",
		Kinds: []string{"person", "boardgame"},
	}
	reg := map[string]config.CanonicalKindConfig{
		"source": {Gaps: map[string]config.GapSpec{"subjects": gap}},
	}
	ve := &vault.Entity{
		ID: "source:gap-metadata-kinds",
		Kind: "source",
		Source: []string{"fixture/default"},
		Gaps: []string{"subjects"},
	}
	entry, ok := buildNeedsFillEntry(ve.ID, ve.Kind, ve, "", reg, false)
	require.True(t, ok, "buildNeedsFillEntry: want true for an open canonical_type gap")

	require.NotNil(t, entry.GapMetadata)
	meta, has := entry.GapMetadata["subjects"]
	require.True(t, has, "gap_metadata must contain the subjects gap")
	assert.Equal(t, config.CanonicalTypeName, meta.Type)
	assert.Equal(t, []string{"person", "boardgame"}, meta.Kinds,
		"agent-side UI uses meta.Kinds to render the allowlist at fill-prompt time")
}

// TestAgentFill_CanonicalType_WorkflowInjectedSpec_CreatesEdge
// is the #158 regression test. A source-shape entity (kind=gmail,
// NOT in the operator canonical_kinds registry) with a
// workflow-injected canonical_type gap (via add_gap per #142)
// must route through the canonical_type code path on agent-fill,
// landing a real edge to the target canonical entity. Prior to
// #158 the fill silently downgraded to "store untyped data" — no
// edge was created, no canonical entity materialized.
//
// Mirrors the linkedin-hiring-classify flow that surfaced the
// bug: workflow injects `hiring_alert_for` (canonical_type,
// kinds=[company], fill_strategy=agent) on a gmail email; agent
// fills with `[{name: "Acme", kind: "company"}]`; daemon must
// create the `hiring_alert_for → company:acme` edge.
func TestAgentFill_CanonicalType_WorkflowInjectedSpec_CreatesEdge(t *testing.T) {
	t.Parallel()
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

	// Operator config has `company` registered (it's the target
	// canonical-label kind) but NOT `gmail` (source-shape).
	reg := map[string]config.CanonicalKindConfig{
		"company": {},
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)

	const id = "gmail:msg-linkedin-classify"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "gmail",
		Data: map[string]any{"id": id, "subject": "We're hiring at Acme"},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "gmail", Source: []string{"gmail/default"},
		Data: map[string]any{"id": id, "subject": "We're hiring at Acme"},
		Gaps: []string{"hiring_alert_for"},
		GapState: map[string]vault.GapStateEntry{
			"hiring_alert_for": {
				Type:         config.CanonicalTypeName,
				FillStrategy: "agent",
				Kinds:        []string{"company"},
				Description:  "the company that's hiring",
			},
		},
		CleanContent: "Acme is hiring engineers.",
	}))

	tok := mintToken(t, signer, "agent", "alice")
	rec := agentFillReq(t, h, id, tok, map[string]any{
		"hiring_alert_for": []any{
			map[string]any{"name": "Acme", "kind": "company"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	edges, err := st.GetEdgesFor(context.Background(), id, []string{"hiring_alert_for"})
	require.NoError(t, err)
	require.Len(t, edges, 1,
		"workflow-injected canonical_type gap must create the edge on agent-fill (per #158)")
	assert.Equal(t, "company:acme", edges[0].To)

	// Thin canonical-label row for the target must materialize so
	// the edge FK is satisfied.
	target, err := st.GetEntity(context.Background(), "company:acme")
	require.NoError(t, err, "canonical-label target must materialize")
	assert.Equal(t, "company", target.Kind)
}

// TestAgentFill_CanonicalType_WorkflowInjectedSpec_RoutedNotStoredAsUntyped
// pins the negative shape complementary to the above: the prior
// shape would store the raw JSON list as `entity.data.<field>`
// (legacy untyped path). Post-#158 the value should land as a
// canonicalLabelEntryIDs `[]string` of resolved label ids, NOT
// the raw object form.
func TestAgentFill_CanonicalType_WorkflowInjectedSpec_RoutedNotStoredAsUntyped(t *testing.T) {
	t.Parallel()
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

	reg := map[string]config.CanonicalKindConfig{"company": {}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewHandlerWithRegistry(logger, st, testRegistryWithSeed(),
		WithVaultIO(w, r),
		WithAuthVerifier(verifier),
		WithAuthRequired(true),
		WithCanonicalKindRegistry(reg),
	)

	const id = "gmail:msg-routing-check"
	require.NoError(t, st.UpsertEntity(context.Background(), &store.Entity{
		ID: id, Kind: "gmail",
		Data: map[string]any{"id": id},
	}))
	require.NoError(t, w.Write(&vault.Entity{
		ID: id, Kind: "gmail", Source: []string{"gmail/default"},
		Data: map[string]any{"id": id},
		Gaps: []string{"hiring_alert_for"},
		GapState: map[string]vault.GapStateEntry{
			"hiring_alert_for": {
				Type:         config.CanonicalTypeName,
				FillStrategy: "agent",
				Kinds:        []string{"company"},
				Description:  "the hiring company",
			},
		},
	}))

	tok := mintToken(t, signer, "agent", "alice")
	rec := agentFillReq(t, h, id, tok, map[string]any{
		"hiring_alert_for": []any{
			map[string]any{"name": "Foo Corp", "kind": "company"},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	stored, err := r.ReadByID("gmail", id)
	require.NoError(t, err)
	got := stored.Data["hiring_alert_for"]
	// Effective canonical_type routing stores the resolved id list,
	// not the raw object form. After YAML round-trip through vault
	// the slice surfaces as []interface{} of strings.
	switch v := got.(type) {
	case []string:
		assert.Equal(t, []string{"company:foo-corp"}, v)
	case []any:
		require.Len(t, v, 1)
		assert.Equal(t, "company:foo-corp", v[0])
	default:
		t.Fatalf("expected []string or []any of canonical-label ids; got %T: %#v", got, got)
	}
}
