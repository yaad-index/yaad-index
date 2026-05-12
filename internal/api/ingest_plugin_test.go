package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

// newAPIWithRegistry returns a handler whose ingest path is wired to a
// custom plugins.Registry. Used for the plugin-integration tests below
// to register a fixture plugin without compiling a binary.
func newAPIWithRegistry(t *testing.T, registry *plugins.Registry) (http.Handler, store.Store) {
	t.Helper()
	st, err := store.New(":memory:")
	require.NoError(t, err, "store.New")
	t.Cleanup(func() { _ = st.Close() })
	return NewHandlerWithRegistry(slog.New(slog.NewJSONHandler(io.Discard, nil)), st, registry), st
}

// decodeDisambiguation is the test helper for the new 200 shape
// added in ADR-0006. Mirrors decodeComplete/decodeNeedsFill in
// ingest_test.go.
func decodeDisambiguation(t *testing.T, rec *httptest.ResponseRecorder) ingestDisambiguationResponse {
	t.Helper()
	var got ingestDisambiguationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode disambiguation response")
	return got
}

// fakeWikipediaResult is the FetchResult a fixture plugin returns for
// the happy-path complete test — chosen so the assertion matches a
// real production-shaped entity even though no subprocess is spawned.
func fakeWikipediaResult() *plugins.FetchResult {
	return &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "wikipedia:go-programming-language",
			Kind: "boardgame", // Kind must be in the test-seed registered kinds to satisfy
			// the closure-invariant test downstream. boardgame is the
			// nearest registered kind for this fixture; the production
			// `wikipedia-article` kind would be added when the real
			// `yaad-wikipedia` binary plus an ADR-0005 kind-registration
			// PR land. The test only cares about the wiring.
			Data: map[string]any{
				"title": "Go (programming language)",
				"extract": "Go is a statically typed, compiled programming language.",
				"lang": "en",
				"url": "https://en.wikipedia.org/wiki/Go_(programming_language)",
			},
		},
		Provenance: []store.ProvenanceEntry{
			{
				Source: "fixture:wikipedia",
				OK: true,
			},
		},
	}
}

func Test_Ingest_PluginPath_Complete200(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("wikipedia", "wikipedia.org", fakeWikipediaResult()))

	h, st := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go_(programming_language)",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeComplete(t, rec)
	assert.Equal(t, "complete", got.Status)
	assert.Equal(t, "wikipedia:go-programming-language", got.Entity.ID)
	assert.Equal(t, "Go (programming language)", got.Entity.Data["title"])

	// Round-trip through the store proves persistence happened.
	persisted, err := st.GetEntity(context.Background(), "wikipedia:go-programming-language")
	require.NoError(t, err, "GetEntity after plugin ingest")
	assert.Len(t, persisted.Provenance, 1, "persisted.Provenance")
}

// Test_Ingest_PluginPath_EmptyProvenance_SynthesizesEntry covers
// the cold-reviewer's a prior PR finding: a plugin (here fixture.Plugin) returning
// a successful FetchResult with empty Provenance must still produce
// at least one persisted provenance entry. Before the tracker-side
// synthesis fix the test fails with len == 0; after, the synthesized
// entry's source matches the plugin name.
func Test_Ingest_PluginPath_EmptyProvenance_SynthesizesEntry(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("forgetful", "forgetful-test", &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "boardgame:forgetful-fixture",
			Kind: "boardgame",
			Data: map[string]any{"title": "Forgetful"},
		},
		// Provenance intentionally nil — the regression we're guarding.
	}))

	h, st := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/forgetful-test/x",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	persisted, err := st.GetEntity(context.Background(), "boardgame:forgetful-fixture")
	require.NoError(t, err, "GetEntity after plugin ingest")
	require.Len(t, persisted.Provenance, 1, "persisted.Provenance: want 1 synthesized entry")
	got := persisted.Provenance[0]
	assert.Equal(t, "forgetful", got.Source, "synthesized provenance source: want plugin name")
	assert.True(t, got.OK, "synthesized provenance OK")
	assert.NotNil(t, got.FetchedAt, "synthesized provenance FetchedAt")
	assert.Nil(t, got.FilledAt, "synthesized provenance FilledAt: this is a fetch entry")
}

func Test_Ingest_PluginPath_NeedsFill_IssuesToken(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("partial", "partial-test", &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "boardgame:partial-fixture",
			Kind: "boardgame",
			Data: map[string]any{"title": "Partial"},
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "fixture:partial", OK: true},
		},
		RawContent: "<cleaned content>",
		RawContentTruncated: false,
		Gaps: map[string]string{
			"summary": "one paragraph summary",
			"tags": "topic tags relevant to this entry",
		},
	}))

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/partial-test/x",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := decodeNeedsFill(t, rec)
	assert.Equal(t, "needs_fill", got.Status)
	assert.NotEmpty(t, got.Entity.ID, "entity.id: durable callback handle (no fill_token issued post a prior PR)")
	assert.Len(t, got.Gaps, 2)
	// Spot-check the description value flows through — the AI filling
	// gaps reads these to know what each field is for.
	assert.NotEmpty(t, got.Gaps["summary"], "gaps[summary] description")
}

func Test_Ingest_PluginPath_FetchErrorMapsToFailed500(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "always-errors",
		MatchFunc: func(string) bool { return true },
		FetchError: errors.New("simulated fetch failure"),
	})

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/anything",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode envelope")
	assert.Equal(t, "fetch_failed", body.Error)
	assert.NotEmpty(t, body.Message)
}

func Test_Ingest_PluginPath_FallsBackToFixturesForUnmatchedURL(t *testing.T) {
	t.Parallel()

	// Registry has a plugin whose Match never claims the URL.
	registry := plugins.NewRegistry()
	registry.Register(fixture.New("never-matches", "definitely-not-in-url", fakeWikipediaResult()))

	h, _ := newAPIWithRegistry(t, registry)
	// Non-matching URL → falls back to the URL-fixture path. The
	// brass-birmingham sentinel still drives the legacy long-poll
	// fixture flow.
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeComplete(t, rec)
	assert.Equal(t, "boardgame:brass-birmingham", got.Entity.ID, "fallback id")
}

// --- Disambiguation protocol (ADR-0006 amendment) ---

// disambiguationOptions returns a 2-option fixture mirroring the
// canonical "Go" example from yaad's design chat: programming
// language vs. board game. Map keys are the plugin's canonical ids
// (the caller re-invokes ingest via `wikipedia: <id>` shorthand to
// fetch the chosen option).
func disambiguationOptions() map[string]plugins.DisambiguationOption {
	return map[string]plugins.DisambiguationOption{
		"Go_(programming_language)": {
			Label: "Go (programming language)",
			Summary: "Open-source language by Google",
		},
		"Go_(game)": {
			Label: "Go (game)",
			Summary: "Ancient Chinese strategy board game",
		},
	}
}

// Test_Ingest_Disambiguation_Returns200_WithStateAndOptions covers
// the multi-option happy path: a plugin that populates Options on
// the FetchResult produces a 200 response with state="disambiguation"
// and the options array passed through. No entity is persisted —
// the caller picks one option's URL and re-invokes ingest.
func Test_Ingest_Disambiguation_Returns200_WithStateAndOptions(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(string) bool { return true },
		FetchValue: &plugins.FetchResult{
			Options: disambiguationOptions(),
		},
	})

	h, st := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeDisambiguation(t, rec)
	assert.Equal(t, "disambiguation", got.State)
	assert.Equal(t, "disambiguation", got.Status, "legacy alias")
	require.Len(t, got.Options, 2)

	progLang, ok := got.Options["Go_(programming_language)"]
	require.True(t, ok, "options missing key %q", "Go_(programming_language)")
	assert.Equal(t, "Go (programming language)", progLang.Label)
	assert.NotEmpty(t, progLang.Summary, "options[programming_language].summary (plugin SHOULD include)")

	game, ok := got.Options["Go_(game)"]
	require.True(t, ok, "options missing key %q", "Go_(game)")
	assert.Equal(t, "Go (game)", game.Label)

	// No entity should have been persisted — disambiguation doesn't
	// upsert. Probe the store to confirm neither candidate id exists.
	_, err := st.GetEntity(context.Background(), "wikipedia:Go_(programming_language)")
	assert.Error(t, err, "disambiguation must not persist any candidate entity")
}

// Test_Ingest_Disambiguation_SingleOption_Returns200_NotAutoResolved
// guards the single-option-allowed invariant: one option means
// "is this what you meant?" — NOT auto-resolved to that one option.
// A naive optimization that auto-resolves single-option results
// would silently flip the response to `complete` and break the
// caller's expectation of getting a confirmation prompt.
func Test_Ingest_Disambiguation_SingleOption_Returns200_NotAutoResolved(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(string) bool { return true },
		FetchValue: &plugins.FetchResult{
			Options: map[string]plugins.DisambiguationOption{
				"Go_(programming_language)": {
					Label: "Go (programming language)",
					Summary: "Open-source language by Google",
				},
			},
		},
	})

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeDisambiguation(t, rec)
	assert.Equal(t, "disambiguation", got.State,
		"single-option must NOT auto-resolve to complete")
	assert.Len(t, got.Options, 1)
}

// Test_Ingest_AllEmpty_Returns404_NotFound covers the "plugin
// returned nothing" terminal: empty FetchResult (no Entity, no
// Options, no Gaps) maps to 404 not_found via the error envelope.
// Distinct from "Options=[]" (which is structurally indistinguishable
// from "no Options") — both shapes mean "no result for this URL"
// and produce the same 404. Per ADR-0006, disambiguation never
// carries zero candidates.
func Test_Ingest_AllEmpty_Returns404_NotFound(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "empty",
		MatchFunc: func(string) bool { return true },
		FetchValue: &plugins.FetchResult{}, // intentionally all-empty
	})

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Definitely_Not_An_Article",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	var body errorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body), "decode error envelope")
	assert.Equal(t, "not_found", body.Error)
	assert.NotEmpty(t, body.Message, "operator-readable message")
}

// Test_Ingest_Complete_HasStateField is the backwards-compat assertion
// for the universal `state` field added in ADR-0002: the existing
// 200 `complete` response now carries `state: "complete"` alongside
// the legacy `status` field. Clients reading either keep working.
func Test_Ingest_Complete_HasStateField(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("wikipedia", "wikipedia.org", fakeWikipediaResult()))

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go_(programming_language)",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeComplete(t, rec)
	assert.Equal(t, "complete", got.State, "universal state field added in ADR-0002")
	assert.Equal(t, "complete", got.Status, "legacy alias")
}

// Test_Ingest_NeedsFill_HasStateField is the backwards-compat
// assertion for the 202 needs_fill shape — same shape as the
// complete-state test above, exercising the second existing 2xx
// shape that gains the universal `state` field.
func Test_Ingest_NeedsFill_HasStateField(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("partial", "partial-test", &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "boardgame:partial-state-test",
			Kind: "boardgame",
			Data: map[string]any{"title": "Partial"},
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "fixture:partial", OK: true},
		},
		RawContent: "<cleaned content>",
		Gaps: map[string]string{
			"summary": "one paragraph summary",
			"tags": "topic tags relevant to this entry",
		},
	}))

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/partial-test/x",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := decodeNeedsFill(t, rec)
	assert.Equal(t, "needs_fill", got.State, "universal state field added in ADR-0002")
	assert.Equal(t, "needs_fill", got.Status, "legacy alias")
}

// --- Issue: shorthand-by-id input + disambiguation round-trip ---

// Test_Ingest_PluginShorthand_RoutesToPlugin pins the post-
// validator behavior: a shorthand input like `wikipedia: Tehran`
// is NOT URL-shaped (no scheme by canonical-URL semantics, contains
// a space) but plugins' `url_patterns` regex matchers can claim it.
// Legacy the strict scheme + ParseRequestURI checks rejected the
// input at the API boundary; post- it flows through to the
// plugin matcher and the request succeeds.
func Test_Ingest_PluginShorthand_RoutesToPlugin(t *testing.T) {
	t.Parallel()

	// Plugin Match returns true for any string starting with the
	// `wikipedia:` shorthand. The fixture's FetchValue resolves to a
	// single entity (the chosen disambiguation candidate).
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(in string) bool {
			return strings.HasPrefix(in, "wikipedia:")
		},
		FetchValue: fakeWikipediaResult(),
	})

	h, _ := newAPIWithRegistry(t, registry)
	rec := postIngest(t, h, map[string]any{
		"url": "wikipedia: Tehran",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"shorthand input must route to the plugin matcher, not 400 at the API boundary; body=%s",
		rec.Body.String())

	got := decodeComplete(t, rec)
	assert.Equal(t, "complete", got.State)
	assert.Equal(t, "wikipedia:go-programming-language", got.Entity.ID,
		"plugin's FetchValue resolved the shorthand to a single entity")
}

// Test_Ingest_DisambiguationRoundTrip_ShorthandResolves walks the
// full ADR-0006 disambiguation flow end-to-end: bare-word input
// returns options, then the shorthand re-invocation resolves to a
// single entity. Legacy the second leg was rejected at the
// validator with 400 invalid_argument; post- it routes correctly.
func Test_Ingest_DisambiguationRoundTrip_ShorthandResolves(t *testing.T) {
	t.Parallel()

	// One plugin handles both legs of the round trip:
	// - bare word `https://en.wikipedia.org/wiki/Go` → returns
	// Options (disambiguation state).
	// - shorthand `wikipedia: Go_(programming_language)` → returns
	// Entity (complete state).
	// MatchFunc claims both shapes; FetchFunc dispatches by input.
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "wikipedia",
		MatchFunc: func(in string) bool {
			return strings.HasPrefix(in, "wikipedia:") ||
				strings.Contains(in, "wikipedia.org")
		},
		FetchFunc: func(_ context.Context, in string) (*plugins.FetchResult, error) {
			if strings.HasPrefix(in, "wikipedia:") {
				return fakeWikipediaResult(), nil
			}
			return &plugins.FetchResult{Options: disambiguationOptions()}, nil
		},
	})

	h, _ := newAPIWithRegistry(t, registry)

	// Leg 1: ambiguous query → disambiguation response.
	first := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, first.Code, "leg 1 body=%s", first.Body.String())
	gotFirst := decodeDisambiguation(t, first)
	assert.Equal(t, "disambiguation", gotFirst.State)
	require.Contains(t, gotFirst.Options, "Go_(programming_language)")

	// Leg 2: caller picks one option and re-invokes via shorthand.
	// Legacy the validator rejected this with 400; post- it
	// flows through.
	second := postIngest(t, h, map[string]any{
		"url": "wikipedia: Go_(programming_language)",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, second.Code,
		"leg 2 (shorthand re-invocation) must route to plugin; body=%s", second.Body.String())
	gotSecond := decodeComplete(t, second)
	assert.Equal(t, "complete", gotSecond.State)
	assert.Equal(t, "wikipedia:go-programming-language", gotSecond.Entity.ID)
}
