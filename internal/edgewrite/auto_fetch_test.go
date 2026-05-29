// Tests for the #325 resolver-plugin auto-fetch hook on
// canonical-edge writes. Covers: the four short-circuit
// branches (no resolver wired, no resolver_plugin for kind,
// source-shape from-kind suppression, source-connection
// suppression) + the three dispatch outcomes (single-match
// success, disambiguation → resolution-task spawn, error →
// err-task spawn) + the cross-cutting concerns (clock
// injection, idempotent re-fire on already-source-connected
// canonicals).

package edgewrite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/store"
)

// fakeNameResolver records ResolveCanonicalEntity calls + lets
// tests script the response. Concurrency-safe; the auto-fetch
// hook is called inline so production never hits concurrent
// dispatch from a single CreateEdge call.
type fakeNameResolver struct {
	mu sync.Mutex
	// resp determines the next ResolveCanonicalEntity outcome:
	// nil → returns (resolvedID, nil, nil); err → returns
	// ("", nil, err); options non-nil → returns ("", options,
	// nil).
	resolvedID string
	options    map[string]plugins.DisambiguationOption
	err        error
	// calls records every (pluginName, targetKind, name) tuple
	// the auto-fetch hook routed to ResolveCanonicalEntity.
	calls []fakeNameResolverCall
}

type fakeNameResolverCall struct {
	pluginName, targetKind, name string
}

func (f *fakeNameResolver) ResolveCanonicalEntity(_ context.Context, pluginName, targetKind, name string) (string, map[string]plugins.DisambiguationOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeNameResolverCall{pluginName: pluginName, targetKind: targetKind, name: name})
	if f.err != nil {
		return "", nil, f.err
	}
	if len(f.options) > 0 {
		return "", f.options, nil
	}
	return f.resolvedID, nil, nil
}

func (f *fakeNameResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeNameResolver) lastCall() (fakeNameResolverCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeNameResolverCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// fakeResolutionTaskWriter records WriteResolutionTask calls
// for assertion on the disambiguation outcome path.
type fakeResolutionTaskWriter struct {
	mu    sync.Mutex
	calls []*ResolutionDeferred
	err   error
}

func (f *fakeResolutionTaskWriter) WriteResolutionTask(_ context.Context, d *ResolutionDeferred) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	cp := *d
	f.calls = append(f.calls, &cp)
	return "tasks/resolution/" + d.TargetKind + "-" + d.RawTarget + ".md", true, nil
}

func (f *fakeResolutionTaskWriter) snapshot() []*ResolutionDeferred {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*ResolutionDeferred, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeErrTaskWriter records AppendErrTask calls for assertion
// on the dispatch-error outcome path.
type fakeErrTaskWriter struct {
	mu    sync.Mutex
	calls []fakeErrTaskCall
	err   error
}

type fakeErrTaskCall struct {
	workflow string
	when     time.Time
	entityID string
	errMsg   string
}

func (f *fakeErrTaskWriter) AppendErrTask(_ context.Context, workflow string, when time.Time, entityID, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, fakeErrTaskCall{workflow: workflow, when: when, entityID: entityID, errMsg: errMsg})
	return nil
}

func (f *fakeErrTaskWriter) snapshot() []fakeErrTaskCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeErrTaskCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// autoFetchFixture assembles a Service with the auto-fetch
// dependencies wired (resolver + canonical-kinds + both
// writers + fixed clock + quiet logger). Tests dial individual
// fields up/down between cases.
type autoFetchFixture struct {
	svc       *Service
	st        store.Store
	resolver  *fakeNameResolver
	resWriter *fakeResolutionTaskWriter
	errWriter *fakeErrTaskWriter
	clockAt   time.Time
}

func newAutoFetchFixture(t *testing.T, resolvers map[string][]string, canonicalKinds []string) *autoFetchFixture {
	t.Helper()
	st := newStore(t)
	svc, err := New(st, resolvers)
	require.NoError(t, err)

	kindsSet := make(map[string]struct{}, len(canonicalKinds))
	for _, k := range canonicalKinds {
		kindsSet[k] = struct{}{}
	}
	svc.SetCanonicalKinds(kindsSet)

	resolver := &fakeNameResolver{}
	svc.SetNameResolver(resolver)

	resWriter := &fakeResolutionTaskWriter{}
	svc.SetResolutionTaskWriter(resWriter)

	errWriter := &fakeErrTaskWriter{}
	svc.SetErrTaskWriter(errWriter)

	clockAt := time.Date(2026, 5, 28, 21, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return clockAt })

	// Quiet logger so test output stays clean.
	svc.SetLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))

	return &autoFetchFixture{
		svc: svc, st: st, resolver: resolver,
		resWriter: resWriter, errWriter: errWriter, clockAt: clockAt,
	}
}

// TestAutoFetch_NoResolverPluginForKind_NoOp pins the gate
// short-circuit: a kind without a resolver_plugin in operator
// config doesn't trigger the hook even when every other piece
// is wired. Mirrors the "kinds without `resolver_plugin` keep
// current behavior" acceptance line in the #325 issue.
func TestAutoFetch_NoResolverPluginForKind_NoOp(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame", "person"},
	)
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "person:alice", "person")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "person:alice",
	}))

	assert.Zero(t, f.resolver.callCount(),
		"person has no resolver_plugin → auto-fetch must not fire")
	assert.Empty(t, f.resWriter.snapshot())
	assert.Empty(t, f.errWriter.snapshot())
}

// TestAutoFetch_HappyPath_SingleMatchDispatches pins the
// single-match success branch: a workflow edge to a canonical
// kind with resolver_plugin set + no existing source connection
// triggers the resolver, which returns a matching entity id.
// The hook records the single dispatch + creates no tasks.
func TestAutoFetch_HappyPath_SingleMatchDispatches(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	f.resolver.resolvedID = "boardgame:brass-birmingham"
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "boardgame:brass-birmingham", "boardgame")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass-birmingham",
	}))

	require.Equal(t, 1, f.resolver.callCount(), "exactly one dispatch")
	got, _ := f.resolver.lastCall()
	assert.Equal(t, "yaad-bgg", got.pluginName)
	assert.Equal(t, "boardgame", got.targetKind)
	assert.Equal(t, "brass-birmingham", got.name,
		"the slug portion of the canonical id is passed as the name arg")
	assert.Empty(t, f.resWriter.snapshot(), "no disambiguation → no resolution-task")
	assert.Empty(t, f.errWriter.snapshot(), "no error → no err-task")
}

// TestAutoFetch_Disambiguation_SpawnsResolutionTask pins the
// disambiguation outcome: resolver returns options → the hook
// spawns a resolution-task with the ResolutionDeferred shape
// carrying every field the task builder needs.
func TestAutoFetch_Disambiguation_SpawnsResolutionTask(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	f.resolver.options = map[string]plugins.DisambiguationOption{
		"boardgame:brass-birmingham": {Label: "Brass: Birmingham", Summary: "2018 release"},
		"boardgame:brass-lancashire": {Label: "Brass: Lancashire", Summary: "2007 release"},
	}
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))

	tasks := f.resWriter.snapshot()
	require.Len(t, tasks, 1)
	d := tasks[0]
	assert.Equal(t, "email:m1", d.From)
	assert.Equal(t, "mentions", d.EdgeType)
	assert.Equal(t, "boardgame", d.TargetKind)
	assert.Equal(t, "brass", d.RawTarget,
		"slug of the canonical id is the RawTarget; ResolutionTaskKey idempotency derives from it")
	assert.Equal(t, "yaad-bgg", d.ResolverPlugin)
	assert.Len(t, d.Options, 2)
	assert.Empty(t, f.errWriter.snapshot(), "disambiguation isn't an error")
}

// TestAutoFetch_Error_SpawnsErrTask pins the error/timeout
// outcome: resolver returns an err → err-task spawned via the
// `resolver-auto-fetch` workflow string with the canonical id +
// dispatch error in the body.
func TestAutoFetch_Error_SpawnsErrTask(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	f.resolver.err = errors.New("plugin timeout after 60s")
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))

	errs := f.errWriter.snapshot()
	require.Len(t, errs, 1)
	assert.Equal(t, "resolver-auto-fetch", errs[0].workflow)
	assert.Equal(t, "boardgame:brass", errs[0].entityID)
	assert.Equal(t, f.clockAt, errs[0].when, "clock injection deterministic")
	assert.Contains(t, errs[0].errMsg, "boardgame:brass")
	assert.Contains(t, errs[0].errMsg, "yaad-bgg")
	assert.Contains(t, errs[0].errMsg, "plugin timeout after 60s")
	assert.Empty(t, f.resWriter.snapshot())
}

// TestAutoFetch_EmptyResolvedAndNoOptions_SpawnsErrTask pins
// the failure case where the resolver returns an empty entity
// id AND no options — anomalous shape that shouldn't silently
// drop. The hook treats it as an error (the contract carved
// out the same shape inside syncIngester.IngestByName already).
func TestAutoFetch_EmptyResolvedAndNoOptions_SpawnsErrTask(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	// Default fakeNameResolver returns ("", nil, nil) — exactly
	// the malformed-shape branch.
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))

	errs := f.errWriter.snapshot()
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].errMsg, "empty entity id without disambiguation options")
}

// TestAutoFetch_SourceShapeFromKind_NoOp pins the recursion
// break: when the from-entity's kind is NOT in the canonical
// set (i.e., it's plugin-source-shape like `bgg:13`), the
// CreateEdge call-site guard short-circuits the dispatch.
// Critical for stopping the plugin's own
// `<plugin-source> -> <canonical>` edge writes from
// re-triggering plugin ingest on every fire.
//
// Note (#330): the guard lives at the CreateEdge call site,
// NOT inside MaybeDispatchResolverAutoFetch — see
// TestAutoFetch_MaybeDispatchDirectInvocation_FromKindSourceShape_StillDispatches
// for the direct-invocation contract used by the fill-API gate.
func TestAutoFetch_SourceShapeFromKind_NoOp(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"}, // `bgg` deliberately NOT in the set
	)
	seedEntity(t, f.st, "bgg:13", "bgg")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "is_about", From: "bgg:13", To: "boardgame:brass",
	}))

	assert.Zero(t, f.resolver.callCount(),
		"bgg:13 → boardgame:brass is a plugin's own edge write; auto-fetch must not re-trigger the plugin")
	assert.Empty(t, f.resWriter.snapshot())
	assert.Empty(t, f.errWriter.snapshot())
}

// TestAutoFetch_MaybeDispatchDirectInvocation_FromKindSourceShape_StillDispatches
// is the #330 regression test. The fill-API gate
// (api.checkCanonicalTypeResolverPlugins) calls
// MaybeDispatchResolverAutoFetch DIRECTLY (not via CreateEdge)
// and passes the entity-being-filled as fromID — which is
// usually source-shape (e.g., `gmail:<msg-id>`). The pre-#330
// recursion break inside the method suppressed the dispatch
// in that case; the fix moved the break to the CreateEdge
// call site so direct callers aren't blocked.
//
// Without this guarantee, the fill-gate stays at HTTP 422
// even though resolver_plugin is set and the plugin would
// resolve the target — the failure mode #330 was filed for
// after PR-328 deployed.
func TestAutoFetch_MaybeDispatchDirectInvocation_FromKindSourceShape_StillDispatches(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"}, // `gmail` deliberately NOT in the set
	)
	f.resolver.resolvedID = "boardgame:lost-expedition"
	seedEntity(t, f.st, "gmail:msg-1", "gmail")
	seedEntity(t, f.st, "boardgame:lost-expedition", "boardgame")

	// Direct invocation, mimicking the fill-API gate caller.
	// fromID is source-shape (gmail:msg-1); pre-#330 this would
	// short-circuit. Post-#330 the dispatch fires.
	f.svc.MaybeDispatchResolverAutoFetch(context.Background(), "gmail:msg-1", "mentions", "boardgame:lost-expedition")

	assert.Equal(t, 1, f.resolver.callCount(),
		"direct invocation with source-shape fromID must dispatch (the fill-gate caller path)")
}

// TestAutoFetch_SourceConnected_NoOp pins the
// already-source-connected suppression: a canonical that
// already has an incoming edge from a plugin-source-shape
// entity doesn't trigger a re-fetch. Models the second-edge-
// to-same-canonical case (e.g. two emails both mention the
// same boardgame; the first triggered the plugin, the second
// must not).
func TestAutoFetch_SourceConnected_NoOp(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	seedEntity(t, f.st, "bgg:13", "bgg")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")
	seedEntity(t, f.st, "email:m1", "email")
	// Pre-existing source connection — bgg:13 is_about boardgame:brass.
	require.NoError(t, f.st.CreateEdge(context.Background(), &store.Edge{
		Type: "is_about", From: "bgg:13", To: "boardgame:brass",
	}))

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))

	assert.Zero(t, f.resolver.callCount(),
		"boardgame:brass already has a `bgg:` source-shape incoming edge; auto-fetch must skip")
}

// TestAutoFetch_OnlyCanonicalIncomingEdges_StillFires pins the
// inverse: incoming edges only from canonical-shape entities
// (e.g., another email already mentioned this boardgame) do
// NOT count as source-connection. Plugin auto-fetch still
// fires.
func TestAutoFetch_OnlyCanonicalIncomingEdges_StillFires(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	f.resolver.resolvedID = "boardgame:brass"
	seedEntity(t, f.st, "email:m0", "email")
	seedEntity(t, f.st, "email:m1", "email")
	seedEntity(t, f.st, "boardgame:brass", "boardgame")
	// Pre-existing canonical incoming edge (email -> boardgame).
	require.NoError(t, f.st.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m0", To: "boardgame:brass",
	}))

	require.NoError(t, f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))

	assert.Equal(t, 1, f.resolver.callCount(),
		"canonical-only incoming edges aren't source-connections; plugin still dispatches")
}

// TestAutoFetch_NoResolverWired_NoOp pins the test-fixture
// degraded-mode short-circuit: a Service without a NameResolver
// (e.g. test that only exercises the CreateEdge passthrough
// contract) leaves auto-fetch off. No need for tests to wire a
// fake resolver unless they specifically exercise auto-fetch.
func TestAutoFetch_NoResolverWired_NoOp(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	svc, err := New(st, map[string][]string{"boardgame": {"yaad-bgg"}})
	require.NoError(t, err)
	svc.SetCanonicalKinds(map[string]struct{}{"email": {}, "boardgame": {}})
	// Intentionally do NOT call SetNameResolver / SetResolutionTaskWriter / SetErrTaskWriter.

	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass", "boardgame")

	// Should not panic + should not write anything beyond the
	// edge itself.
	require.NoError(t, svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))
}

// TestAutoFetch_CreateCanonicalEdgeByName_LegacyPath fires on
// the slugify branch of CreateCanonicalEdgeByName: the workflow
// add_canonical_edge auto-mode path doesn't need the hook
// (resolver already invoked), but the legacy slugify fall-
// through (interactive mode or already-resolved name) needs to
// trigger auto-fetch identically to CreateEdge.
func TestAutoFetch_CreateCanonicalEdgeByName_LegacyPath(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	// Use Interactive mode so the auto-resolve branch is
	// skipped and the legacy slugify path takes over.
	ctx := WithMode(context.Background(), Interactive)
	f.resolver.resolvedID = "boardgame:brass"
	seedEntity(t, f.st, "email:m1", "email")

	targetID, _, err := f.svc.CreateCanonicalEdgeByName(ctx, "email:m1", "mentions", "boardgame", "Brass", nil)
	require.NoError(t, err)
	assert.Equal(t, "boardgame:brass", targetID)
	// The legacy path materialized boardgame:brass + wrote the
	// edge. Auto-fetch must then dispatch.
	assert.Equal(t, 1, f.resolver.callCount(),
		"slugify-path edge write fires #325 auto-fetch on the materialized canonical")
}

// TestAutoFetch_ClockNotInjected_DefaultsToTimeNow asserts
// the clock fallback fires when no SetClock call happened.
// Production main.go intentionally leaves SetClock unset; the
// test pins the no-injection branch + the err-task timestamp
// being roughly time.Now (not zero / panic).
func TestAutoFetch_ClockNotInjected_DefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	svc, err := New(st, map[string][]string{"boardgame": {"yaad-bgg"}})
	require.NoError(t, err)
	svc.SetCanonicalKinds(map[string]struct{}{"email": {}, "boardgame": {}})
	resolver := &fakeNameResolver{err: errors.New("boom")}
	svc.SetNameResolver(resolver)
	errWriter := &fakeErrTaskWriter{}
	svc.SetErrTaskWriter(errWriter)
	svc.SetLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))

	seedEntity(t, st, "email:m1", "email")
	seedEntity(t, st, "boardgame:brass", "boardgame")

	before := time.Now().UTC()
	require.NoError(t, svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:brass",
	}))
	after := time.Now().UTC()

	errs := errWriter.snapshot()
	require.Len(t, errs, 1)
	assert.GreaterOrEqual(t, errs[0].when.Unix(), before.Unix())
	assert.LessOrEqual(t, errs[0].when.Unix(), after.Unix())
}

// TestAutoFetch_DispatchedBeforeStoreError pins the ordering
// contract: when the underlying store.CreateEdge fails, the
// auto-fetch hook does NOT fire. The edge didn't land, so
// invoking a plugin for it would create state inconsistent
// with the failed write.
func TestAutoFetch_DispatchedBeforeStoreError(t *testing.T) {
	t.Parallel()
	f := newAutoFetchFixture(t,
		map[string][]string{"boardgame": {"yaad-bgg"}},
		[]string{"email", "boardgame"},
	)
	// Don't seed boardgame:brass — CreateEdge's FK probe will
	// reject (the target row doesn't exist). NOTE: this also
	// covers the from-row-missing case the same way.
	seedEntity(t, f.st, "email:m1", "email")

	err := f.svc.CreateEdge(context.Background(), &store.Edge{
		Type: "mentions", From: "email:m1", To: "boardgame:nonexistent",
	})
	require.Error(t, err)
	assert.Zero(t, f.resolver.callCount(),
		"store.CreateEdge failed → auto-fetch must not dispatch (no edge landed)")
}

// TestSplitCanonicalID covers the helper's shape rejection so
// the auto-fetch hook never tries to slug-extract from an id
// that doesn't match `<kind>:<slug>`.
func TestSplitCanonicalID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantKind  string
		wantSlug  string
		wantOK    bool
	}{
		{"boardgame:brass-birmingham", "boardgame", "brass-birmingham", true},
		{"day:2026-05-28", "day", "2026-05-28", true},
		{"no-colon-here", "", "", false},
		{":empty-kind", "", "", false},
		{"empty-slug:", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, slug, ok := splitCanonicalID(c.in)
		assert.Equal(t, c.wantOK, ok, "ok for %q", c.in)
		assert.Equal(t, c.wantKind, kind, "kind for %q", c.in)
		assert.Equal(t, c.wantSlug, slug, "slug for %q", c.in)
	}
}
