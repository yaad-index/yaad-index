package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/plugins/fixture"
	"github.com/yaad-index/yaad-index/internal/store"
)

func ingestRequestBody(t *testing.T, body any) io.Reader {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err, "marshal ingest request")
	return strings.NewReader(string(b))
}

func postIngest(t *testing.T, h http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", ingestRequestBody(t, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postIngestRaw(t *testing.T, h http.Handler, raw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", strings.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeQueued(t *testing.T, rec *httptest.ResponseRecorder) ingestQueuedResponse {
	t.Helper()
	var got ingestQueuedResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode queued response")
	return got
}

func decodeComplete(t *testing.T, rec *httptest.ResponseRecorder) ingestCompleteResponse {
	t.Helper()
	var got ingestCompleteResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode complete response")
	return got
}

func decodeNeedsFill(t *testing.T, rec *httptest.ResponseRecorder) ingestNeedsFillResponse {
	t.Helper()
	var got ingestNeedsFillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got), "decode needs_fill response")
	return got
}

// Test_Ingest_LongPoll_BrassBirminghamCompletes_200 exercises the
// happy path: simulated extraction finishes within wait_seconds, the
// handler observes complete, and returns the 200 inline-entity shape
// with the entity persisted in the store.
func Test_Ingest_LongPoll_BrassBirminghamCompletes_200(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)

	// 50ms simulated delay, 2s wait_seconds — handler observes within
	// the window.
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := decodeComplete(t, rec)
	assert.True(t, got.OK)
	assert.Equal(t, "complete", got.Status)
	assert.Equal(t, "boardgame:brass-birmingham", got.Entity.ID)
	assert.Equal(t, "Brass: Birmingham", got.Entity.Data["title"])

	persisted, err := st.GetEntity(context.Background(), "boardgame:brass-birmingham")
	require.NoError(t, err, "GetEntity")
	assert.Len(t, persisted.Provenance, 1, "provenance after one ingest")
}

// Test_Ingest_LongPoll_QueuedTestTimesOut_202 fires the timeout path:
// the queued-test fixture sleeps far longer than the test's
// wait_seconds, so the handler returns 202 queued.
func Test_Ingest_LongPoll_QueuedTestTimesOut_202(t *testing.T) {
	t.Parallel()

	h := newAPI(t)

	start := time.Now()
	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/queued-test/foo",
		"wait_seconds": 1,
	})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusAccepted, rec.Code, "want 202 (timed out), body=%s", rec.Body.String())
	got := decodeQueued(t, rec)
	assert.Equal(t, "queued", got.Status)
	assert.NotEmpty(t, got.EstimatedEntityID, "estimated_entity_id: want non-empty on timeout")

	// Sanity: we should have waited roughly wait_seconds, not the full
	// simulation delay (which is ~60s for queued-test).
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond, "elapsed: want ~1s (wait_seconds)")
	assert.LessOrEqual(t, elapsed, 3*time.Second, "elapsed: want ~1s (wait_seconds)")
}

// Test_Ingest_LongPoll_NeedsFillTest_202_GapsExposed: the needs-fill
// fixture transitions to needs_fill within wait_seconds; the handler
// returns 202 with the gap set so the agent can fill any subset via
// POST /v1/entities/{id}/fill — the entity ID itself is the durable
// callback handle (per ADR-0008's "Callback ID = entity ID").
func Test_Ingest_LongPoll_NeedsFillTest_202_GapsExposed(t *testing.T) {
	t.Parallel()

	h := newAPI(t)

	rec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	got := decodeNeedsFill(t, rec)
	assert.Equal(t, "needs_fill", got.Status)
	assert.Equal(t, stubNeedsFillEntityID, got.Entity.ID)
	assert.NotEmpty(t, got.CleanContent, "clean_content: want non-empty")
	assert.False(t, got.CleanContentTruncated, "clean_content_truncated")
	// Gaps is a {field → description} object (per ADR-0002
	// universal-state amendment). Assert the field-name set; the
	// descriptions are operator-facing prose checked separately by
	// the plugin-path tests.
	gotNames := make([]string, 0, len(got.Gaps))
	for k := range got.Gaps {
		gotNames = append(gotNames, k)
	}
	sort.Strings(gotNames)
	assert.Equal(t, []string{"complexity_assessment", "summary", "tags"}, gotNames, "gaps (field names)")
}

// Test_Ingest_AsyncOnlyMode_Bypasses_LongPoll: wait_seconds=0 returns
// 202 queued immediately without blocking, regardless of which fixture
// the URL points at.
func Test_Ingest_AsyncOnlyMode_Bypasses_LongPoll(t *testing.T) {
	t.Parallel()

	h := newAPI(t)

	start := time.Now()
	rec := postIngest(t, h, map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": 0,
	})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusAccepted, rec.Code, "want 202 (async-only)")
	got := decodeQueued(t, rec)
	assert.Equal(t, "queued", got.Status)

	// Should return basically instantly — no waiting on the simulator.
	assert.LessOrEqual(t, elapsed, 250*time.Millisecond,
		"async-only mode took %s — want immediate return (no long-poll)", elapsed)
}

// Test_Ingest_EndToEnd_NeedsFill_Then_Fill walks the full ingest →
// fill → merged loop using the durable-callback model from ADR-0008.
// The entity ID returned by ingest is the agent's handle for the
// fill call; no separate fill_token is exchanged. A second fill
// against an already-filled gap surfaces as 409 conflict (not 410
// expired_token, which is the dropped semantic).
func Test_Ingest_EndToEnd_NeedsFill_Then_Fill(t *testing.T) {
	t.Parallel()

	h, _, _ := newAPIWithVault(t)

	// 1. Ingest the needs-fill fixture; the entity ID is the durable
	// callback handle.
	ingestRec := postIngest(t, h, map[string]any{
		"url": "https://example.test/needs-fill-test/foo",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusAccepted, ingestRec.Code, "ingest body=%s", ingestRec.Body.String())
	ingestBody := decodeNeedsFill(t, ingestRec)
	require.NotEmpty(t, ingestBody.Entity.ID, "entity.id: durable callback handle")

	// 2. Fill against the entity id with the gap set the response advertised.
	fillBody := map[string]any{
		"fields": map[string]any{
			"summary": "An end-to-end-tested partial entity, now filled.",
			"tags": []string{"e2e"},
			"complexity_assessment": "moderate",
		},
	}
	fillRec := postFill(t, h, ingestBody.Entity.ID, fillBody)
	require.Equal(t, http.StatusOK, fillRec.Code, "fill body=%s", fillRec.Body.String())

	var fillResp fillResponse
	require.NoError(t, json.NewDecoder(fillRec.Body).Decode(&fillResp), "decode fill response")
	assert.Equal(t, ingestBody.Entity.ID, fillResp.Entity.ID)
	for _, k := range []string{"summary", "tags", "complexity_assessment"} {
		assert.Contains(t, fillResp.Entity.Data, k, "merged entity.data missing %q", k)
	}

	// 3. Re-fill of an already-filled field surfaces as 409 conflict
	// (the gap was removed from the entity's gap set on the prior
	// fill).
	redo := postFill(t, h, ingestBody.Entity.ID, fillBody)
	assert.Equal(t, http.StatusConflict, redo.Code, "re-fill of already-filled fields: want 409")
}

// Test_Ingest_NoPlugin_Returns422_UnsupportedURL is the empty-registry
// case: no plugin's url_patterns match any URL because there are no
// plugins. The fixture-fallback path also rejects this URL (no
// brass-birmingham / queued-test / needs-fill-test substring), so the
// dispatcher falls through to the canonical 422 unsupported_url
// envelope per ADR-0002 + ADR-0006. Replaces the legacy
// Test_Ingest_UnsupportedURLReturnsStubError which asserted the
// pre-fix 400 invalid_argument shape (an envelope shape the operator's
// review of a prior PR explicitly called wrong).
func Test_Ingest_NoPlugin_Returns422_UnsupportedURL(t *testing.T) {
	t.Parallel()
	const target = "https://example.test/unrelated/page"
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": target,
		"wait_seconds": 0,
	})
	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "unsupported_url", target)
}

// Test_Ingest_PluginMismatch_Returns422_UnsupportedURL covers the
// "plugin loaded but doesn't claim this URL" case: a plugin matching
// wikipedia.org URLs is registered, but the request targets
// example.com. registry.Lookup returns no match → fixture fallback
// rejects (no sentinel match) → 422 unsupported_url.
func Test_Ingest_PluginMismatch_Returns422_UnsupportedURL(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("wikipedia", "wikipedia.org", &plugins.FetchResult{
		Entity: &store.Entity{ID: "wikipedia:x", Kind: "wikipedia-article"},
	}))
	h, _ := newAPIWithRegistry(t, registry)

	const target = "https://example.com/foo/bar"
	rec := postIngest(t, h, map[string]any{
		"url": target,
		"wait_seconds": 0,
	})
	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "unsupported_url", target)
}

// Test_Ingest_PluginMatch_NotAffected is the regression guard: the
// 422 shortcut must NOT trip when a registered plugin's url_patterns
// claim the URL. A naive fix that always returns 422 on no-fixture-
// match would break the plugin-match path; this test fails loudly on
// that regression.
func Test_Ingest_PluginMatch_NotAffected(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(fixture.New("wikipedia", "wikipedia.org", &plugins.FetchResult{
		Entity: &store.Entity{
			ID: "wikipedia:go-programming-language",
			Kind: "boardgame", // bootstrapKind constraint, see ingest_plugin_test.go fakeWikipediaResult
			Data: map[string]any{"title": "Go (programming language)"},
		},
		Provenance: []store.ProvenanceEntry{
			{Source: "fixture:wikipedia", OK: true},
		},
	}))
	h, _ := newAPIWithRegistry(t, registry)

	rec := postIngest(t, h, map[string]any{
		"url": "https://en.wikipedia.org/wiki/Go_(programming_language)",
		"wait_seconds": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, "matched-plugin URL body=%s", rec.Body.String())
}

// Test_Ingest_CommandShapeDispatchesToNamedPlugin pins the fix for
// issue #52: a command-shape input (`<plugin>: !<command>`) where the
// plugin advertises empty url_patterns must reach the named plugin
// via LookupByName, NOT via the Match-based registry.Lookup walk.
// Pre-fix the gmail plugin (URLPatterns=[], Commands=["fetch"]) was
// invisible at dispatch — the same surface validateRouting had
// already approved — and ingest returned 422 unsupported_url.
func Test_Ingest_CommandShapeDispatchesToNamedPlugin(t *testing.T) {
	t.Parallel()

	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		// Empty url_patterns mirror the real gmail plugin's shape —
		// Match must NOT be the route here.
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "gmail",
			SourceNamespace: "gmail",
			EntityKinds: []plugins.KindSpec{{Name: "boardgame"}},
			Commands: []plugins.CommandSpec{{Name: "fetch"}},
		},
		FetchValue: &plugins.FetchResult{
			Entity: &store.Entity{
				ID: "gmail:fetch-result",
				Kind: "boardgame", // satisfies bootstrap-kind constraint per fakeWikipediaResult
				Data: map[string]any{"title": "fetched"},
			},
			Provenance: []store.ProvenanceEntry{
				{Source: "fixture:gmail", OK: true},
			},
		},
	})
	h, _ := newAPIWithRegistry(t, registry)

	rec := postIngest(t, h, map[string]any{
		"url": "gmail: !fetch",
		"wait_seconds": 2,
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"command-shape input must reach the named plugin; body=%s", rec.Body.String())
	got := decodeComplete(t, rec)
	assert.Equal(t, "gmail:fetch-result", got.Entity.ID,
		"the gmail plugin's FetchResult must surface through dispatch")
}

// Test_Ingest_CommandShape_UnknownPluginReturns404 pins the
// validateRouting rejection path: a command-shape input naming an
// unregistered plugin must fail at validation with 404
// plugin_not_found, never reaching the dispatch fork.
func Test_Ingest_CommandShape_UnknownPluginReturns404(t *testing.T) {
	t.Parallel()
	h := newAPI(t)
	rec := postIngest(t, h, map[string]any{
		"url": "no-such-plugin: !fetch",
		"wait_seconds": 0,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "plugin_not_found")
}

// Test_Ingest_CommandShape_UnknownCommandReturns400 pins the
// validateRouting rejection for a registered plugin that doesn't
// advertise the named command. 400 invalid_input is the existing
// validator contract (input-shape error); the dispatch fix doesn't
// change this — it only routes the inputs validateRouting passes.
func Test_Ingest_CommandShape_UnknownCommandReturns400(t *testing.T) {
	t.Parallel()
	registry := plugins.NewRegistry()
	registry.Register(&fixture.Plugin{
		NameValue: "gmail",
		MatchFunc: func(string) bool { return false },
		CapabilitiesValue: plugins.Capabilities{
			Name: "gmail",
			SourceNamespace: "gmail",
			EntityKinds: []plugins.KindSpec{{Name: "source"}},
			Commands: []plugins.CommandSpec{{Name: "fetch"}},
		},
	})
	h, _ := newAPIWithRegistry(t, registry)

	rec := postIngest(t, h, map[string]any{
		"url": "gmail: !unknown",
		"wait_seconds": 0,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_input")
	assert.Contains(t, rec.Body.String(), "no command")
}

func Test_Ingest_MissingURL(t *testing.T) {
	t.Parallel()
	rec := postIngest(t, newAPI(t), map[string]any{"wait_seconds": 0})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "url is required")
}

// Test_Ingest_NonHttpURL_FallsThroughToPluginMatcher locks the
// post- contract: non-http(s) schemes (or any URL-shape) pass
// through the validator and reach the plugin / fixture matchers.
// When no plugin and no fixture sentinel claims the URL, the
// canonical 422 unsupported_url envelope fires per ADR-0006 — the
// request was well-formed, just not actionable.
//
// Replaces the legacy Test_Ingest_InvalidURLScheme which asserted
// 400 invalid_argument from the dropped scheme check.
func Test_Ingest_NonHttpURL_FallsThroughToPluginMatcher(t *testing.T) {
	t.Parallel()
	const target = "ftp://example.test/unrelated-path"
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": target,
		"wait_seconds": 0,
	})
	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "unsupported_url", target)
}

// Test_Ingest_GarbageURL_FallsThroughToPluginMatcher — the URL
// validator no longer rejects malformed URL shapes; they fall
// through to the plugin matcher and surface as 422 unsupported_url
// when no plugin claims them. Replaces the legacy
// Test_Ingest_UnparseableURL which asserted 400 invalid_argument.
func Test_Ingest_GarbageURL_FallsThroughToPluginMatcher(t *testing.T) {
	t.Parallel()
	const target = "not a url"
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": target,
		"wait_seconds": 0,
	})
	assertErrorEnvelope(t, rec, http.StatusUnprocessableEntity, "unsupported_url", target)
}

func Test_Ingest_WaitSeconds_OverMax(t *testing.T) {
	t.Parallel()
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": "https://example.test/queued-test/foo",
		"wait_seconds": 301,
	})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "wait_seconds")
}

func Test_Ingest_WaitSeconds_Negative(t *testing.T) {
	t.Parallel()
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": "https://example.test/queued-test/foo",
		"wait_seconds": -1,
	})
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "wait_seconds")
}

func Test_Ingest_WaitSeconds_NonInteger(t *testing.T) {
	t.Parallel()
	rec := postIngestRaw(t, newAPI(t),
		`{"url":"https://example.test/queued-test/foo","wait_seconds":"60"}`)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

func Test_Ingest_WaitSeconds_AtMax(t *testing.T) {
	t.Parallel()
	// async-only via wait_seconds=0 trick: at-max validation runs after
	// JSON parse; we only need 300 to pass through. Use it with the
	// brass-birmingham fixture so the simulator's 50ms delay completes
	// inside the wait window — still 200 complete.
	rec := postIngest(t, newAPI(t), map[string]any{
		"url": "https://boardgamegeek.com/boardgame/224517/brass-birmingham",
		"wait_seconds": ingestMaxWaitSeconds,
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"wait_seconds=%d: want 200 (brass-birmingham completes fast)", ingestMaxWaitSeconds)
}

func Test_Ingest_MalformedJSON(t *testing.T) {
	t.Parallel()
	rec := postIngestRaw(t, newAPI(t), `{`)
	assertErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_argument", "JSON")
}

// Test_Ingest_PersistedEntitiesPassClosureInvariant: the entities that
// /v1/ingest persists must reference a registered entity_kind. Mirrors
// the closure-invariant tests on PRs / / /. After the
// long-poll wiring this asserts via GetEntity round-trip — for fixtures
// that complete within wait_seconds (brass-birmingham, needs-fill-test).
func Test_Ingest_PersistedEntitiesPassClosureInvariant(t *testing.T) {
	t.Parallel()

	h, st := newAPIWithStore(t)
	declared := make(map[string]struct{}, len(testSeedEntityKinds))
	for _, k := range testSeedEntityKinds {
		declared[k] = struct{}{}
	}

	check := func(t *testing.T, label, urlSubstring, expectedID string) {
		t.Helper()
		rec := postIngest(t, h, map[string]any{
			"url": "https://example.test/" + urlSubstring + "/x",
			"wait_seconds": 2,
		})
		require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, rec.Code,
			"%s status: want 200/202, body=%s", label, rec.Body.String())
		entity, err := st.GetEntity(context.Background(), expectedID)
		require.NoError(t, err, "%s GetEntity", label)
		_, ok := declared[entity.Kind]
		assert.True(t, ok, "%s entity.kind=%q not in testSeedEntityKinds", label, entity.Kind)
	}

	check(t, "brass-birmingham", "brass-birmingham", "boardgame:brass-birmingham")
	check(t, "needs-fill-test", "needs-fill-test", stubNeedsFillEntityID)
}
